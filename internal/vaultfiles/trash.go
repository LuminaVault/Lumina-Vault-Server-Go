package vaultfiles

import (
	"context"
	"errors"
	"fmt"
	"lumina/internal/ids"
	"lumina/internal/vaultgit"
	"lumina/internal/wire"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// TrashEntry is a vault_trash row: one deleted file, renamed into the tenant's
// raw root as TrashName, with what its vault_files row held so a restore can
// rebuild it (backlog E3.1).
type TrashEntry struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	VaultFileID     uuid.UUID
	SpaceID         *uuid.UUID
	OriginalPath    string
	TrashName       string
	ContentType     string
	SizeBytes       int64
	SHA256          string
	Metadata        *Metadata
	CreatedByUserID *uuid.UUID
	DeletedByUserID *uuid.UUID
	DeletedAt       time.Time
}

// trashBaseMaxBytes caps the original file name kept in a trash name, so a
// long name can't push the result past the filesystem's 255-byte limit.
const trashBaseMaxBytes = 120

// TrashName is where a delete puts a file, relative to the raw root:
// _deleted_<unix seconds>_<trash id, 32 hex>_<file name>.
//
// The id makes every name unique. The old name flattened the whole path
// ("a/b.md" and "a_b.md" both became "a_b.md") and collided on two deletes in
// one second. Only the last path component is kept, cut to trashBaseMaxBytes;
// the full path lives in vault_trash.original_path. The `_deleted_` prefix keeps
// the file out of export and the link index, as before.
func TrashName(id uuid.UUID, relative string, now time.Time) string {
	base := LastPathComponent(relative)
	if len(base) > trashBaseMaxBytes {
		cut := trashBaseMaxBytes
		for cut > 0 && !utf8.RuneStart(base[cut]) {
			cut--
		}
		base = base[:cut]
	}
	return "_deleted_" + strconv.FormatInt(now.Unix(), 10) + "_" +
		strings.ReplaceAll(id.String(), "-", "") + "_" + base
}

// MoveFileToTrash renames target (from ResolveInside) to <rawRoot>/<name>.
// moved is false when there is no file: fileExists follows links, so a
// dangling link stays where it is, as SoftDeleteFile did.
func MoveFileToTrash(rawRoot, target, name string) (moved bool, err error) {
	if !fileExists(target) {
		return false, nil
	}
	if err := moveItem(target, appendingPath(rawRoot, name)); err != nil {
		return false, err
	}
	return true, nil
}

// Trash moves row's file into the trash and records it in vault_trash. It
// touches no vault_files row; the caller deletes that.
//
// It returns a nil entry and no error when the file is missing on disk, since
// there is nothing to restore. If the insert fails, the file stays in the
// trash under its unique name, so nothing is lost, and the error says where.
func (s *Store) Trash(ctx context.Context, rawRoot, target string, row *VaultFile, deletedBy *uuid.UUID, now time.Time) (*TrashEntry, error) {
	e := &TrashEntry{
		ID:              uuid.New(),
		TenantID:        row.TenantID,
		VaultFileID:     row.ID,
		SpaceID:         row.SpaceID,
		OriginalPath:    row.Path,
		ContentType:     row.ContentType,
		SizeBytes:       row.SizeBytes,
		SHA256:          row.SHA256,
		Metadata:        row.Metadata,
		CreatedByUserID: row.CreatedByUserID,
		DeletedByUserID: deletedBy,
		DeletedAt:       now,
	}
	e.TrashName = TrashName(e.ID, row.Path, now)
	moved, err := MoveFileToTrash(rawRoot, target, e.TrashName)
	if err != nil || !moved {
		return nil, err
	}
	actor := ""
	if deletedBy != nil {
		actor = ids.Upper(*deletedBy)
	}
	vaultgit.Touch(row.TenantID, "delete", actor, "")
	metadata, err := encodeColumn(row.Metadata)
	if err == nil {
		_, err = s.pool.Exec(ctx, `INSERT INTO vault_trash (id, tenant_id, vault_file_id, space_id, original_path,
			trash_name, content_type, size_bytes, sha256, metadata, created_by_user_id, deleted_by_user_id, deleted_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $13)`,
			e.ID, e.TenantID, e.VaultFileID, e.SpaceID, e.OriginalPath, e.TrashName, e.ContentType,
			e.SizeBytes, e.SHA256, metadata, e.CreatedByUserID, e.DeletedByUserID, e.DeletedAt)
	}
	if err != nil {
		return nil, fmt.Errorf("vault_trash insert (file kept as %s): %w", e.TrashName, err)
	}
	return e, nil
}

const trashColumns = `id, tenant_id, vault_file_id, space_id, original_path, trash_name, content_type,
	size_bytes, sha256, metadata, created_by_user_id, deleted_by_user_id, deleted_at`

// ListTrash returns the tenant's trash, newest first.
func (s *Store) ListTrash(ctx context.Context, tenant uuid.UUID) ([]TrashEntry, error) {
	return s.queryTrash(ctx, `WHERE tenant_id = $1 ORDER BY deleted_at DESC, id`, tenant)
}

// FindTrash returns the tenant's trash entry with id, or nil.
func (s *Store) FindTrash(ctx context.Context, tenant, id uuid.UUID) (*TrashEntry, error) {
	rows, err := s.queryTrash(ctx, `WHERE tenant_id = $1 AND id = $2`, tenant, id)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// ExpiredTrash returns up to limit entries of any tenant deleted before cutoff,
// oldest first, for the purge job.
func (s *Store) ExpiredTrash(ctx context.Context, cutoff time.Time, limit int) ([]TrashEntry, error) {
	return s.queryTrash(ctx, `WHERE deleted_at < $1 ORDER BY deleted_at, id LIMIT $2`, cutoff, limit)
}

// DeleteTrashRow removes a vault_trash row. A row that has gone is not an error.
func (s *Store) DeleteTrashRow(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM vault_trash WHERE id = $1`, id)
	return err
}

// ErrTrashFileGone is Restore's answer when the trashed file is no longer on
// disk, so there is nothing to bring back.
var ErrTrashFileGone = errors.New("trashed file is gone")

// RemoveTrashedFile deletes <rawRoot>/<name> for good. name must be a direct
// child with the `_deleted_` prefix; a symlink is unlinked, never followed. A
// missing file is not an error.
func RemoveTrashedFile(rawRoot, name string) error {
	if !strings.HasPrefix(name, "_deleted_") || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return fmt.Errorf("not a trash name: %q", name)
	}
	err := os.Remove(appendingPath(rawRoot, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// restoreAttempts bounds the "-restored-N" names Restore tries.
const restoreAttempts = 50

// RestoredPath is the n-th name Restore tries for original: the path itself,
// then "note-restored.md", then "note-restored-2.md" and so on. SanitizePath
// allows no spaces or brackets, so the suffix is plain.
func RestoredPath(original string, n int) string {
	if n == 0 {
		return original
	}
	dir, base := "", original
	if i := strings.LastIndex(original, "/"); i >= 0 {
		dir, base = original[:i+1], original[i+1:]
	}
	stem, ext := base, ""
	if i := strings.LastIndex(base, "."); i > 0 {
		stem, ext = base[:i], base[i:]
	}
	suffix := "-restored"
	if n > 1 {
		suffix = fmt.Sprintf("-restored-%d", n)
	}
	return dir + stem + suffix + ext
}

// Restore moves e's file back and recreates its vault_files row, then drops the
// trash row. The file goes to its original path, or to the first free
// RestoredPath when a file or row now holds it. The Space comes from the path's
// folder, as on upload, so a Space deleted since then leaves the file unfiled.
// The row is unprocessed, so the compile pipeline picks it up again.
//
// The caller rebuilds the note's memory: Restore touches only the file and rows.
func (s *Store) Restore(ctx context.Context, rawRoot string, e *TrashEntry, actor uuid.UUID) (*VaultFile, error) {
	src := appendingPath(rawRoot, e.TrashName)
	if _, err := os.Lstat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrTrashFileGone
		}
		return nil, err
	}
	var path, target string
	for n := range restoreAttempts {
		candidate, err := SanitizePath(RestoredPath(e.OriginalPath, n))
		if err != nil {
			return nil, err
		}
		taken, err := s.FindByPath(ctx, e.TenantID, candidate)
		if err != nil {
			return nil, err
		}
		resolved, err := ResolveInside(rawRoot, candidate)
		if err != nil {
			return nil, err
		}
		if _, statErr := os.Lstat(resolved); taken == nil && errors.Is(statErr, os.ErrNotExist) {
			path, target = candidate, resolved
			break
		}
	}
	if path == "" {
		return nil, wire.NewError(http.StatusConflict, "no free name to restore to")
	}
	if err := createParent(target); err != nil {
		return nil, err
	}
	if err := moveItem(src, target); err != nil {
		return nil, err
	}
	vaultgit.Touch(e.TenantID, "restore", ids.Upper(actor), "")
	spaceID, err := s.SpaceIDForPath(ctx, e.TenantID, path)
	if err == nil {
		var id uuid.UUID
		id, err = s.Insert(ctx, UpsertParams{
			TenantID: e.TenantID, SpaceID: spaceID, Path: path, ContentType: e.ContentType,
			SizeBytes: e.SizeBytes, SHA256: e.SHA256, Metadata: e.Metadata,
		})
		if err == nil {
			if err = s.StampActors(ctx, id, actor); err == nil {
				if err = s.DeleteTrashRow(ctx, e.ID); err == nil {
					return s.FindByID(ctx, e.TenantID, id)
				}
			}
			// The row exists; the file is where it belongs. Report, don't undo.
			return nil, err
		}
	}
	if back := moveItem(target, src); back != nil {
		return nil, errors.Join(err, back)
	}
	return nil, err
}

func (s *Store) queryTrash(ctx context.Context, tail string, args ...any) ([]TrashEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+trashColumns+` FROM vault_trash `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrashEntry
	for rows.Next() {
		var e TrashEntry
		var metadata []byte
		if err := rows.Scan(&e.ID, &e.TenantID, &e.VaultFileID, &e.SpaceID, &e.OriginalPath, &e.TrashName,
			&e.ContentType, &e.SizeBytes, &e.SHA256, &metadata, &e.CreatedByUserID, &e.DeletedByUserID,
			&e.DeletedAt); err != nil {
			return nil, err
		}
		if e.Metadata, err = decodeColumn(metadata); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
