package vaultfiles

import (
	"context"
	"fmt"
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
	rows, err := s.pool.Query(ctx, `SELECT `+trashColumns+` FROM vault_trash
		WHERE tenant_id = $1 ORDER BY deleted_at DESC, id`, tenant)
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
