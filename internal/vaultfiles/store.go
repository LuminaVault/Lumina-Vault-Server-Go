package vaultfiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// VaultFile is a vault_files row: the index of one file under
// <rawRoot>/<path>. The path is unique per tenant.
type VaultFile struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	CreatedByUserID *uuid.UUID
	UpdatedByUserID *uuid.UUID
	SpaceID         *uuid.UUID
	Path            string
	ContentType     string
	SizeBytes       int64
	SHA256          string
	ProcessedAt     *time.Time
	Metadata        *Metadata
	CreatedAt       *time.Time
	UpdatedAt       *time.Time
}

// Columns lists vault_files in the order Scan reads them, for a module's own
// list queries.
const Columns = `id, tenant_id, created_by_user_id, updated_by_user_id, space_id, path, content_type,
	size_bytes, sha256, processed_at, metadata, created_at, updated_at`

// Scan reads one row selected with Columns.
func Scan(row pgx.Row) (*VaultFile, error) {
	var f VaultFile
	var metadata []byte
	if err := row.Scan(&f.ID, &f.TenantID, &f.CreatedByUserID, &f.UpdatedByUserID, &f.SpaceID, &f.Path,
		&f.ContentType, &f.SizeBytes, &f.SHA256, &f.ProcessedAt, &metadata, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return nil, err
	}
	var err error
	if f.Metadata, err = decodeColumn(metadata); err != nil {
		return nil, err
	}
	return &f, nil
}

// Store reads and writes vault_files and moves the files behind them.
type Store struct {
	pool  *pgxpool.Pool
	paths vaultpaths.Paths
}

func NewStore(pool *pgxpool.Pool, paths vaultpaths.Paths) *Store {
	return &Store{pool: pool, paths: paths}
}

// FindByPath returns the tenant's row at path, or nil.
func (s *Store) FindByPath(ctx context.Context, tenant uuid.UUID, path string) (*VaultFile, error) {
	return s.find(ctx, `tenant_id = $1 AND path = $2`, tenant, path)
}

// FindByID returns the tenant's row with id, or nil.
func (s *Store) FindByID(ctx context.Context, tenant, id uuid.UUID) (*VaultFile, error) {
	return s.find(ctx, `tenant_id = $1 AND id = $2`, tenant, id)
}

func (s *Store) find(ctx context.Context, where string, args ...any) (*VaultFile, error) {
	f, err := Scan(s.pool.QueryRow(ctx, `SELECT `+Columns+` FROM vault_files WHERE `+where+` LIMIT 1`, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // nil row: not found, as Fluent's first()
	}
	return f, err
}

// UpsertParams are upsertVaultFileRow's arguments.
type UpsertParams struct {
	TenantID uuid.UUID
	// SpaceID nil leaves an existing row's Space alone, so a content update
	// doesn't unfile it.
	SpaceID     *uuid.UUID
	Path        string
	ContentType string
	SizeBytes   int64
	SHA256      string
	// Processed marks the row already compiled. A re-upload without it clears
	// processed_at, so the file is compiled again.
	Processed bool
	// Metadata nil leaves an existing row's metadata alone. Otherwise it's
	// merged over the old value (MergeMetadata), or stored as is on a new row.
	Metadata *Metadata
}

// Upsert is VaultController.upsertVaultFileRow. Like Swift it's a read and then
// a write, not ON CONFLICT: two concurrent first uploads of one path end with
// one unique violation, and two re-uploads can lose a metadata merge.
func (s *Store) Upsert(ctx context.Context, p UpsertParams) (uuid.UUID, error) {
	now := time.Now()
	var processedAt *time.Time
	if p.Processed {
		processedAt = &now
	}
	existing, err := s.FindByPath(ctx, p.TenantID, p.Path)
	if err != nil {
		return uuid.Nil, err
	}
	if existing == nil {
		return s.insert(ctx, p, processedAt, now)
	}
	var merged *Metadata
	if p.Metadata != nil {
		m := MergeMetadata(existing.Metadata, *p.Metadata)
		merged = &m
	}
	metadata, err := encodeColumn(merged)
	if err != nil {
		return uuid.Nil, err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE vault_files
		SET content_type = $2, size_bytes = $3, sha256 = $4, processed_at = $5,
			space_id = COALESCE($6, space_id), metadata = COALESCE($7::jsonb, metadata), updated_at = $8
		WHERE id = $1`,
		existing.ID, p.ContentType, p.SizeBytes, p.SHA256, processedAt, p.SpaceID, metadata, now)
	return existing.ID, err
}

// Insert is `VaultFile(...).save`: a new row, with no read before it, so a path the
// tenant already has is a unique violation.
func (s *Store) Insert(ctx context.Context, p UpsertParams) (uuid.UUID, error) {
	now := time.Now()
	var processedAt *time.Time
	if p.Processed {
		processedAt = &now
	}
	return s.insert(ctx, p, processedAt, now)
}

func (s *Store) insert(ctx context.Context, p UpsertParams, processedAt *time.Time, now time.Time) (uuid.UUID, error) {
	id := uuid.New()
	metadata, err := encodeColumn(p.Metadata)
	if err != nil {
		return uuid.Nil, err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO vault_files (id, tenant_id, space_id, path, content_type, size_bytes, sha256,
			processed_at, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $10)`,
		id, p.TenantID, p.SpaceID, p.Path, p.ContentType, p.SizeBytes, p.SHA256, processedAt, metadata, now)
	return id, err
}

// StampActors is the step after the upsert in VaultController.upload: the first
// writer stays as creator, the latest as updater, and updated_at moves. A row
// that has gone is not an error.
func (s *Store) StampActors(ctx context.Context, id, actor uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE vault_files
		SET created_by_user_id = COALESCE(created_by_user_id, $2), updated_by_user_id = $2, updated_at = $3
		WHERE id = $1`, id, actor, time.Now())
	return err
}

// UpdateMetadata stores metadata whole, as `row.metadata = m; row.save` does in
// TodosController, CaptureFilingService and URLEnrichmentService. It does not
// merge.
func (s *Store) UpdateMetadata(ctx context.Context, id uuid.UUID, m *Metadata) error {
	metadata, err := encodeColumn(m)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE vault_files SET metadata = $2::jsonb, updated_at = $3 WHERE id = $1`,
		id, metadata, time.Now())
	return err
}

// UpdateContent stores a rewritten file's size and hash and its metadata whole, as
// URLEnrichmentService's `row.save` does. It does not merge.
func (s *Store) UpdateContent(ctx context.Context, id uuid.UUID, sizeBytes int64, sha256 string, m *Metadata) error {
	metadata, err := encodeColumn(m)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE vault_files SET size_bytes = $2, sha256 = $3, metadata = $4::jsonb, updated_at = $5 WHERE id = $1`,
		id, sizeBytes, sha256, metadata, time.Now())
	return err
}

// DeleteRow removes the row for good. vault_files has no soft delete of its
// own: the `_deleted_*` name is the only trace of a deleted file, and that's on
// disk. Cascade the note's memory first, because the foreign key would only
// set its source to NULL.
func (s *Store) DeleteRow(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM vault_files WHERE id = $1`, id)
	return err
}

// SpaceIDForPath is VaultController.spaceID(forPath:): the path's first folder
// matched against the tenant's Space slugs. A file at the top level, "inbox/",
// and any folder that's not a slug all mean unfiled, which is nil.
func (s *Store) SpaceIDForPath(ctx context.Context, tenant uuid.UUID, path string) (*uuid.UUID, error) {
	var components []string
	for _, c := range splitCharacter(path, "/") {
		if c != "" {
			components = append(components, c)
		}
	}
	if len(components) <= 1 || components[0] == "inbox" {
		return nil, nil // nil Space: unfiled
	}
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM spaces WHERE tenant_id = $1 AND slug = $2 LIMIT 1`, tenant, components[0]).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // nil Space: unfiled
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// MoveFile is VaultFileMover.move: it moves one file to a new path and re-files
// everything that copies its location, namely the row's Space, the memories
// compiled from it and their chunks. to must already be sanitized. Answers 409
// when a row holds to. row is updated for the caller's response.
//
// Like Swift the disk move comes first and isn't undone: if the transaction
// fails, the file has moved and the row still names the old path. A missing
// source file is not an error, and the row moves anyway. A file at to with no
// row is an error (a 500), because moveItem doesn't overwrite.
func (s *Store) MoveFile(ctx context.Context, row *VaultFile, to string, tenant uuid.UUID) error {
	conflict, err := s.FindByPath(ctx, tenant, to)
	if err != nil {
		return err
	}
	if conflict != nil {
		return wire.NewError(http.StatusConflict, "destination path already exists")
	}
	rawRoot := s.paths.Raw(tenant)
	src, err := ResolveInside(rawRoot, row.Path)
	if err != nil {
		return err
	}
	dst, err := ResolveInside(rawRoot, to)
	if err != nil {
		return err
	}
	if err := createParent(dst); err != nil {
		return err
	}
	if fileExists(src) {
		if err := moveItem(src, dst); err != nil {
			return err
		}
	}
	spaceID, err := s.SpaceIDForPath(ctx, tenant, to)
	if err != nil {
		return err
	}
	now := time.Now()
	row.Path, row.SpaceID, row.UpdatedAt = to, spaceID, &now
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE vault_files SET path = $2, space_id = $3, updated_at = $4 WHERE id = $1`,
			row.ID, to, spaceID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE memories SET space_id = $1, updated_at = NOW()
			WHERE tenant_id = $2 AND source_vault_file_id = $3`, spaceID, tenant, row.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE memory_chunks SET space_id = $1, source_path = $2
			WHERE tenant_id = $3 AND vault_file_id = $4`, spaceID, to, tenant, row.ID)
		return err
	})
}

// MoveDestination is VaultFileMover.path(forFile:inSpaceSlug:): where a file
// lands when re-filed into a Space, or into the inbox when slug is nil.
func MoveDestination(path string, slug *string) (string, error) {
	folder := "inbox"
	if slug != nil {
		folder = *slug
	}
	return SanitizePath(folder + "/" + LastPathComponent(path))
}

// TrashPath is where a soft delete puts relative at time now:
// <rawRoot>/_deleted_<unix seconds>_<path with "/" as "_">.
func TrashPath(rawRoot, relative string, now time.Time) string {
	flattened := strings.ReplaceAll(relative, "/", "_")
	return appendingPath(rawRoot, "_deleted_"+strconv.FormatInt(now.Unix(), 10)+"_"+flattened)
}

// SoftDeleteFile renames target (from ResolveInside) to its TrashPath, if it
// exists. It touches no row.
//
// Swift treats the error as best effort. VaultController.delete logs it and
// TodosController.delete ignores it, and both then delete the row, so a failed
// rename leaves a file with no row. Failure is real: moveItem doesn't overwrite,
// so two deletes of one path in one second collide, and so do "a/b.md" and
// "a_b.md"; a deep path can also make a name longer than the filesystem allows.
func SoftDeleteFile(rawRoot, target, relative string, now time.Time) error {
	if !fileExists(target) {
		return nil
	}
	return moveItem(target, TrashPath(rawRoot, relative, now))
}

func decodeColumn(b []byte) (*Metadata, error) {
	if b == nil || string(b) == "null" {
		return nil, nil // nil metadata: the column is optional
	}
	var m Metadata
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("vault_files.metadata: %w", err)
	}
	return &m, nil
}

// encodeColumn returns a nil pointer for nil, which pgx binds as NULL.
func encodeColumn(m *Metadata) (*string, error) {
	if m == nil {
		return nil, nil // nil metadata: NULL
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	s := string(b)
	return &s, nil
}
