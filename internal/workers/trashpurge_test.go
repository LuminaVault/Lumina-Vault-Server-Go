package workers

import (
	"context"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Backlog E3.2: entries past retention lose their file and row; newer ones stay.
func TestVaultTrashPurge(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	tenant := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
		VALUES ($1, 'p@test', 'x', true, 'purgeuser', 'trial', now(), now())`, tenant)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
		VALUES ($1, $1, 'v', now(), now())`, tenant)
	require.NoError(t, err)

	paths := vaultpaths.Paths{Root: t.TempDir()}
	require.NoError(t, paths.EnsureTenantDirectories(tenant))
	store := vaultfiles.NewStore(pool, paths)
	raw := paths.Raw(tenant)
	now := time.Date(2026, 11, 15, 5, 0, 0, 0, time.UTC)

	trash := func(name string, age time.Duration) *vaultfiles.TrashEntry {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(raw, name), []byte("x"), 0o644))
		id, err := store.Insert(ctx, vaultfiles.UpsertParams{TenantID: tenant, Path: name, ContentType: "text/markdown", SizeBytes: 1, SHA256: "s"})
		require.NoError(t, err)
		row, err := store.FindByID(ctx, tenant, id)
		require.NoError(t, err)
		target, err := vaultfiles.ResolveInside(raw, name)
		require.NoError(t, err)
		e, err := store.Trash(ctx, raw, target, row, nil, now.Add(-age))
		require.NoError(t, err)
		require.NoError(t, store.DeleteRow(ctx, id))
		return e
	}
	old := trash("old.md", 31*24*time.Hour)
	fresh := trash("fresh.md", 29*24*time.Hour)
	gone := trash("gone.md", 40*24*time.Hour)
	require.NoError(t, os.Remove(filepath.Join(raw, gone.TrashName)))

	purged, failed, err := NewVaultTrashPurge(store, paths, 30, nil).Run(ctx, now)
	require.NoError(t, err)
	require.Equal(t, 2, purged, "a file already gone still drops its row")
	require.Zero(t, failed)
	require.NoFileExists(t, filepath.Join(raw, old.TrashName))
	require.FileExists(t, filepath.Join(raw, fresh.TrashName))

	left, err := store.ListTrash(ctx, tenant)
	require.NoError(t, err)
	require.Len(t, left, 1)
	require.Equal(t, fresh.ID, left[0].ID)
}

func TestRemoveTrashedFileRefusesOtherNames(t *testing.T) {
	raw := t.TempDir()
	for _, name := range []string{"note.md", "_deleted_1_x/../../etc", "..", "inbox/_deleted_1_a.md"} {
		require.Error(t, vaultfiles.RemoveTrashedFile(raw, name), name)
	}
	require.NoError(t, vaultfiles.RemoveTrashedFile(raw, "_deleted_1_missing.md"))
}
