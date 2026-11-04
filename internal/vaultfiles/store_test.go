package vaultfiles_test

import (
	"context"
	"encoding/json"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

var (
	alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
)

type fixture struct {
	pool  *pgxpool.Pool
	store *vaultfiles.Store
	paths vaultpaths.Paths
}

func setup(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	seed, err := os.ReadFile("../../parity/seed/base.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(seed))
	require.NoError(t, err)
	paths := vaultpaths.Paths{Root: t.TempDir()}
	require.NoError(t, paths.EnsureTenantDirectories(alice))
	return fixture{pool: pool, store: vaultfiles.NewStore(pool, paths), paths: paths}
}

func (f fixture) space(t *testing.T, tenant uuid.UUID, slug string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO spaces (id, tenant_id, name, slug, created_at, updated_at) VALUES ($1, $2, $3, $3, now(), now())`, id, tenant, slug)
	require.NoError(t, err)
	return id
}

func (f fixture) upsert(t *testing.T, p vaultfiles.UpsertParams) uuid.UUID {
	t.Helper()
	if p.TenantID == uuid.Nil {
		p.TenantID = alice
	}
	if p.ContentType == "" {
		p.ContentType = "text/markdown"
	}
	id, err := f.store.Upsert(context.Background(), p)
	require.NoError(t, err)
	return id
}

func (f fixture) row(t *testing.T, tenant, id uuid.UUID) *vaultfiles.VaultFile {
	t.Helper()
	row, err := f.store.FindByID(context.Background(), tenant, id)
	require.NoError(t, err)
	require.NotNil(t, row)
	return row
}

func TestUpsertCreates(t *testing.T) {
	f := setup(t)
	space := f.space(t, alice, "work")
	before := time.Now().Add(-time.Second)
	id := f.upsert(t, vaultfiles.UpsertParams{
		SpaceID: &space, Path: "work/a.md", ContentType: "text/markdown; charset=utf-8",
		SizeBytes: 3, SHA256: "abc", Processed: true, Metadata: &vaultfiles.Metadata{Title: ptr("t")},
	})
	row := f.row(t, alice, id)
	require.Equal(t, alice, row.TenantID)
	require.Equal(t, &space, row.SpaceID)
	require.Equal(t, "work/a.md", row.Path)
	require.Equal(t, "text/markdown; charset=utf-8", row.ContentType, "the raw header value is stored")
	require.Equal(t, int64(3), row.SizeBytes)
	require.Equal(t, "abc", row.SHA256)
	require.NotNil(t, row.ProcessedAt)
	require.Equal(t, "t", *row.Metadata.Title)
	require.Equal(t, row.CreatedAt, row.UpdatedAt)
	require.True(t, row.CreatedAt.After(before))
	require.Nil(t, row.CreatedByUserID)
	require.Nil(t, row.UpdatedByUserID)

	found, err := f.store.FindByPath(context.Background(), alice, "work/a.md")
	require.NoError(t, err)
	require.Equal(t, id, found.ID)
}

func TestUpsertCreatesWithNullMetadata(t *testing.T) {
	f := setup(t)
	id := f.upsert(t, vaultfiles.UpsertParams{Path: "a.md"})
	require.Nil(t, f.row(t, alice, id).Metadata)
	var isNull bool
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT metadata IS NULL FROM vault_files WHERE id = $1`, id).Scan(&isNull))
	require.True(t, isNull, "SQL NULL, not a jsonb null")
}

func TestUpsertUpdatesInPlace(t *testing.T) {
	f := setup(t)
	id := f.upsert(t, vaultfiles.UpsertParams{Path: "a.md", SizeBytes: 1, SHA256: "one", Processed: true})
	first := f.row(t, alice, id)
	time.Sleep(2 * time.Millisecond)

	again := f.upsert(t, vaultfiles.UpsertParams{Path: "a.md", ContentType: "text/plain", SizeBytes: 2, SHA256: "two"})
	require.Equal(t, id, again, "same path, same row")
	row := f.row(t, alice, id)
	require.Equal(t, "text/plain", row.ContentType)
	require.Equal(t, int64(2), row.SizeBytes)
	require.Equal(t, "two", row.SHA256)
	require.Nil(t, row.ProcessedAt, "a re-upload without processed clears it")
	require.Equal(t, first.CreatedAt, row.CreatedAt)
	require.True(t, row.UpdatedAt.After(*first.UpdatedAt))
}

// VaultUploadSpaceTests "re-upload omitting space_id leaves the filed copy alone".
func TestUpsertWithoutSpaceLeavesSpace(t *testing.T) {
	f := setup(t)
	space := f.space(t, alice, "work")
	id := f.upsert(t, vaultfiles.UpsertParams{SpaceID: &space, Path: "work/a.md"})
	f.upsert(t, vaultfiles.UpsertParams{Path: "work/a.md"})
	require.Equal(t, &space, f.row(t, alice, id).SpaceID)

	other := f.space(t, alice, "home")
	f.upsert(t, vaultfiles.UpsertParams{SpaceID: &other, Path: "work/a.md"})
	require.Equal(t, &other, f.row(t, alice, id).SpaceID, "a provided space overwrites")
}

// VaultUploadSpaceTests "uploading the same name to a second space files it
// under that space": two folders are two rows.
func TestUpsertSameNameInTwoFolders(t *testing.T) {
	f := setup(t)
	a := f.upsert(t, vaultfiles.UpsertParams{Path: "work/a.md"})
	b := f.upsert(t, vaultfiles.UpsertParams{Path: "home/a.md"})
	require.NotEqual(t, a, b)
}

func TestUpsertMergesMetadata(t *testing.T) {
	f := setup(t)
	id := f.upsert(t, vaultfiles.UpsertParams{Path: "a.md", Metadata: &vaultfiles.Metadata{
		EnrichmentStatus: ptr("done"), Title: ptr("old"), Provenance: ptr("hermes-mirror"), ProjectID: &project,
	}})

	f.upsert(t, vaultfiles.UpsertParams{Path: "a.md"})
	require.Equal(t, "old", *f.row(t, alice, id).Metadata.Title, "no sidecar: untouched")

	f.upsert(t, vaultfiles.UpsertParams{Path: "a.md", Metadata: &vaultfiles.Metadata{Done: ptr(true)}})
	require.Equal(t, &vaultfiles.Metadata{EnrichmentStatus: ptr("done"), ProjectID: &project, Done: ptr(true)},
		f.row(t, alice, id).Metadata)
}

func TestUpsertIsTenantScoped(t *testing.T) {
	f := setup(t)
	a := f.upsert(t, vaultfiles.UpsertParams{TenantID: alice, Path: "a.md"})
	b := f.upsert(t, vaultfiles.UpsertParams{TenantID: bob, Path: "a.md"})
	require.NotEqual(t, a, b)

	row, err := f.store.FindByID(context.Background(), bob, a)
	require.NoError(t, err)
	require.Nil(t, row)
	row, err = f.store.FindByPath(context.Background(), alice, "nope.md")
	require.NoError(t, err)
	require.Nil(t, row)
}

func TestStampActorsKeepsFirstCreator(t *testing.T) {
	f := setup(t)
	id := f.upsert(t, vaultfiles.UpsertParams{Path: "a.md"})
	ctx := context.Background()
	require.NoError(t, f.store.StampActors(ctx, id, alice))
	row := f.row(t, alice, id)
	require.Equal(t, &alice, row.CreatedByUserID)
	require.Equal(t, &alice, row.UpdatedByUserID)

	require.NoError(t, f.store.StampActors(ctx, id, bob))
	row = f.row(t, alice, id)
	require.Equal(t, &alice, row.CreatedByUserID)
	require.Equal(t, &bob, row.UpdatedByUserID)

	require.NoError(t, f.store.StampActors(ctx, uuid.New(), alice), "a missing row isn't an error")
}

func TestUpdateMetadataReplacesWhole(t *testing.T) {
	f := setup(t)
	id := f.upsert(t, vaultfiles.UpsertParams{Path: "a.md", Metadata: &vaultfiles.Metadata{Title: ptr("t"), Provenance: ptr("p")}})
	require.NoError(t, f.store.UpdateMetadata(context.Background(), id, &vaultfiles.Metadata{Done: ptr(true)}))
	require.Equal(t, &vaultfiles.Metadata{Done: ptr(true)}, f.row(t, alice, id).Metadata)
}

func TestDeleteRow(t *testing.T) {
	f := setup(t)
	id := f.upsert(t, vaultfiles.UpsertParams{Path: "a.md"})
	require.NoError(t, f.store.DeleteRow(context.Background(), id))
	row, err := f.store.FindByID(context.Background(), alice, id)
	require.NoError(t, err)
	require.Nil(t, row)
}

// The bytes Swift writes and the bytes Go writes must be one jsonb value.
func TestColumnMatchesSwiftInJSONB(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var m vaultfiles.Metadata
	require.NoError(t, json.Unmarshal([]byte(swiftJSON), &m))
	id := f.upsert(t, vaultfiles.UpsertParams{Path: "a.md", Metadata: &m})

	var goText, swiftText string
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT metadata::text, $2::jsonb::text FROM vault_files WHERE id = $1`, id, swiftJSON).Scan(&goText, &swiftText))
	require.Equal(t, swiftText, goText)

	var numberType string
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT jsonb_typeof(metadata->'dueAt') FROM vault_files WHERE id = $1`, id).Scan(&numberType))
	require.Equal(t, "number", numberType)
}

// DashboardController counts todos with metadata->>'isTodo' = 'true'.
func TestColumnAnswersSwiftsJSONQueries(t *testing.T) {
	f := setup(t)
	id := f.upsert(t, vaultfiles.UpsertParams{Path: "a.md", Metadata: &vaultfiles.Metadata{IsTodo: ptr(true), Title: ptr("t")}})
	var isTodo, title string
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT metadata->>'isTodo', metadata->>'title' FROM vault_files WHERE id = $1`, id).Scan(&isTodo, &title))
	require.Equal(t, "true", isTodo)
	require.Equal(t, "t", title)
}

// A row Swift wrote that Go can't decode fails the read, as Fluent's does.
func TestUndecodableMetadataFailsTheRead(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id := f.upsert(t, vaultfiles.UpsertParams{Path: "a.md"})
	_, err := f.pool.Exec(ctx, `UPDATE vault_files SET metadata = '{"isTodo":"yes"}' WHERE id = $1`, id)
	require.NoError(t, err)
	_, err = f.store.FindByID(ctx, alice, id)
	require.Error(t, err)
	_, err = f.pool.Exec(ctx, `UPDATE vault_files SET metadata = 'null'::jsonb WHERE id = $1`, id)
	require.NoError(t, err)
	row, err := f.store.FindByID(ctx, alice, id)
	require.NoError(t, err)
	require.Nil(t, row.Metadata)
}

func TestSpaceIDForPath(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	work := f.space(t, alice, "work")
	f.space(t, alice, "inbox")
	f.space(t, bob, "bobs")

	got, err := f.store.SpaceIDForPath(ctx, alice, "work/deep/a.md")
	require.NoError(t, err)
	require.Equal(t, &work, got)
	for _, path := range []string{"a.md", "work", "inbox/a.md", "unknown/a.md", "bobs/a.md"} {
		got, err := f.store.SpaceIDForPath(ctx, alice, path)
		require.NoError(t, err)
		require.Nil(t, got, path)
	}
}

// moveFixture is a filed note with one memory and one chunk.
func moveFixture(t *testing.T, f fixture, path string) (*vaultfiles.VaultFile, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	id := f.upsert(t, vaultfiles.UpsertParams{Path: path, SizeBytes: 4, SHA256: "s"})
	abs := filepath.Join(f.paths.Raw(alice), path)
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	write(t, abs, "body")
	memory := uuid.New()
	_, err := f.pool.Exec(ctx, `INSERT INTO memories (id, tenant_id, content, source_vault_file_id, created_at, updated_at)
		VALUES ($1, $2, 'c', $3, now() - interval '1 day', now() - interval '1 day')`, memory, alice, id)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `INSERT INTO memory_chunks (chunk_id, document_id, tenant_id, memory_id, vault_file_id, source_path, ordinal, start_line, end_line, text, content_sha256)
		VALUES ('c1', 'd1', $1, $2, $3, $4, 0, 0, 1, 't', 's')`, alice, memory, id, path)
	require.NoError(t, err)
	return f.row(t, alice, id), memory
}

// VaultCRUDTests "move renames file and updates row", and the re-filing in
// VaultFileMover.
func TestMoveFileRefilesEverything(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	space := f.space(t, alice, "work")
	row, memory := moveFixture(t, f, "inbox/a.md")
	before := row.UpdatedAt

	time.Sleep(2 * time.Millisecond)
	require.NoError(t, f.store.MoveFile(ctx, row, "work/sub/new.md", alice))
	require.Equal(t, "work/sub/new.md", row.Path, "the row is updated for the response")
	require.Equal(t, &space, row.SpaceID)

	raw := f.paths.Raw(alice)
	require.NoFileExists(t, filepath.Join(raw, "inbox", "a.md"))
	body, err := os.ReadFile(filepath.Join(raw, "work", "sub", "new.md"))
	require.NoError(t, err)
	require.Equal(t, "body", string(body))

	stored := f.row(t, alice, row.ID)
	require.Equal(t, "work/sub/new.md", stored.Path)
	require.Equal(t, &space, stored.SpaceID)
	require.True(t, stored.UpdatedAt.After(*before))

	var memorySpace *uuid.UUID
	var memoryUpdated time.Time
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT space_id, updated_at FROM memories WHERE id = $1`, memory).Scan(&memorySpace, &memoryUpdated))
	require.Equal(t, &space, memorySpace)
	require.WithinDuration(t, time.Now(), memoryUpdated, time.Minute)

	var chunkSpace *uuid.UUID
	var chunkPath string
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT space_id, source_path FROM memory_chunks WHERE chunk_id = 'c1'`).Scan(&chunkSpace, &chunkPath))
	require.Equal(t, &space, chunkSpace)
	require.Equal(t, "work/sub/new.md", chunkPath)
}

func TestMoveFileToInboxUnfiles(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.space(t, alice, "work")
	row, memory := moveFixture(t, f, "work/a.md")
	require.NoError(t, f.store.MoveFile(ctx, row, "work/b.md", alice))
	require.NotNil(t, row.SpaceID)
	require.NoError(t, f.store.MoveFile(ctx, row, "inbox/b.md", alice))
	require.Nil(t, row.SpaceID)
	require.Nil(t, f.row(t, alice, row.ID).SpaceID)
	var memorySpace *uuid.UUID
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT space_id FROM memories WHERE id = $1`, memory).Scan(&memorySpace))
	require.Nil(t, memorySpace)
}

// VaultCRUDTests "move rejects conflict".
func TestMoveFileConflict(t *testing.T) {
	f := setup(t)
	row, _ := moveFixture(t, f, "a.md")
	f.upsert(t, vaultfiles.UpsertParams{Path: "b.md"})
	err := f.store.MoveFile(context.Background(), row, "b.md", alice)
	requireWireError(t, err, http.StatusConflict, "destination path already exists")
	require.Equal(t, "a.md", row.Path)
	require.FileExists(t, filepath.Join(f.paths.Raw(alice), "a.md"))
}

// VaultCRUDTests "move rejects traversal": the route sanitizes newPath first, and
// MoveFile's ResolveInside holds even when a caller forgets.
func TestMoveFileTraversal(t *testing.T) {
	f := setup(t)
	row, _ := moveFixture(t, f, "in.md")
	_, err := vaultfiles.SanitizePath("../escape.md")
	requireWireError(t, err, http.StatusBadRequest, "path contains illegal segment")
	err = f.store.MoveFile(context.Background(), row, "../escape.md", alice)
	requireWireError(t, err, http.StatusBadRequest, escapes)
	require.FileExists(t, filepath.Join(f.paths.Raw(alice), "in.md"))
}

func TestMoveFileMissingSourceStillMovesRow(t *testing.T) {
	f := setup(t)
	row, _ := moveFixture(t, f, "a.md")
	require.NoError(t, os.Remove(filepath.Join(f.paths.Raw(alice), "a.md")))
	require.NoError(t, f.store.MoveFile(context.Background(), row, "new/b.md", alice))
	require.Equal(t, "new/b.md", f.row(t, alice, row.ID).Path)
	require.DirExists(t, filepath.Join(f.paths.Raw(alice), "new"), "the parent is created either way")
}

// A file with no row at the destination isn't a 409 but a plain failure, and
// nothing is overwritten.
func TestMoveFileWontOverwriteOrphan(t *testing.T) {
	f := setup(t)
	row, _ := moveFixture(t, f, "a.md")
	orphan := filepath.Join(f.paths.Raw(alice), "orphan.md")
	write(t, orphan, "orphan")

	err := f.store.MoveFile(context.Background(), row, "orphan.md", alice)
	require.ErrorIs(t, err, os.ErrExist)
	body, readErr := os.ReadFile(orphan)
	require.NoError(t, readErr)
	require.Equal(t, "orphan", string(body))
	require.Equal(t, "a.md", f.row(t, alice, row.ID).Path)
}

// QUIRK from Swift: the disk move comes first and isn't undone.
func TestMoveFileDatabaseFailureLeavesFileMoved(t *testing.T) {
	f := setup(t)
	row, _ := moveFixture(t, f, "a.md")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A trigger stands in for the database failing inside the transaction.
	_, err := f.pool.Exec(ctx, `
		CREATE FUNCTION fail_move() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'boom'; END $$;
		CREATE TRIGGER fail_move BEFORE UPDATE ON memory_chunks FOR EACH ROW EXECUTE FUNCTION fail_move();`)
	require.NoError(t, err)

	require.Error(t, f.store.MoveFile(ctx, row, "b.md", alice))
	raw := f.paths.Raw(alice)
	require.NoFileExists(t, filepath.Join(raw, "a.md"))
	require.FileExists(t, filepath.Join(raw, "b.md"))
	require.Equal(t, "a.md", f.row(t, alice, row.ID).Path, "the transaction rolled back")
}
