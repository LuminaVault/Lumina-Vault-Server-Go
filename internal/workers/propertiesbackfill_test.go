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

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Backlog E8.2: notes written while properties were off are indexed once the
// flag is on; other files, gone files and invalid YAML don't stall the run.
func TestVaultPropertiesBackfill(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	tenant := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
		VALUES ($1, 'b@test', 'x', true, 'backfilluser', 'trial', now(), now())`, tenant)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
		VALUES ($1, $1, 'v', now(), now())`, tenant)
	require.NoError(t, err)

	paths := vaultpaths.Paths{Root: t.TempDir()}
	require.NoError(t, paths.EnsureTenantDirectories(tenant))
	store := vaultfiles.NewStore(pool, paths)
	raw := paths.Raw(tenant)

	// Written while off: nothing indexed.
	write := func(name, body string, onDisk bool) uuid.UUID {
		t.Helper()
		if onDisk {
			require.NoError(t, os.WriteFile(filepath.Join(raw, name), []byte(body), 0o600))
		}
		id, err := store.Insert(ctx, vaultfiles.UpsertParams{TenantID: tenant, Path: name, ContentType: "text/markdown", SizeBytes: int64(len(body)), SHA256: "s"})
		require.NoError(t, err)
		return id
	}
	plan := write("plan.md", "---\nstatus: Doing\ntitle: Plan\n---\nx\n", true)
	bad := write("bad.md", "---\na: [x\n---\n", true)
	gone := write("gone.md", "", false)
	photo := write("photo.jpg", "jpg", true)

	job := &VaultPropertiesBackfill{Store: store}
	indexed, failed, err := job.Run(ctx)
	require.NoError(t, err)
	require.Zero(t, indexed, "a no-op while the flag is off")
	require.Zero(t, failed)

	vaultfiles.EnableProperties(true)
	t.Cleanup(func() { vaultfiles.EnableProperties(false) })
	indexed, failed, err = job.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, indexed, "plan, bad (error recorded) and gone (marked)")
	require.Zero(t, failed)

	row, err := store.FindByID(ctx, tenant, plan)
	require.NoError(t, err)
	require.Equal(t, "Doing", row.Properties["status"])
	require.Equal(t, "Plan", *row.Metadata.Title, "projected into the sidecar")
	row, err = store.FindByID(ctx, tenant, bad)
	require.NoError(t, err)
	require.NotNil(t, row.PropertiesError)

	var unindexed []uuid.UUID
	for _, id := range []uuid.UUID{plan, bad, gone, photo} {
		var isNull bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT properties_indexed_at IS NULL FROM vault_files WHERE id = $1`, id).Scan(&isNull))
		if isNull {
			unindexed = append(unindexed, id)
		}
	}
	require.Equal(t, []uuid.UUID{photo}, unindexed, "only notes are indexed")

	indexed, _, err = job.Run(ctx)
	require.NoError(t, err)
	require.Zero(t, indexed, "the next run finds nothing")
}
