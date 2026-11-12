package migrations_test

import (
	"context"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// The Swift migration suites (M00, M39, M41, M114) each ran Fluent's migration
// list and asserted the schema it left. In Go those migrations are folded into
// the goose baseline, so each test below applies the embedded migrations and
// asserts the same schema effect, then applies them again to show the second
// run is a no-op.

func migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	return pool
}

func strs(t *testing.T, pool *pgxpool.Pool, query string, args ...any) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

func columns(t *testing.T, pool *pgxpool.Pool, table string) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT column_name, data_type FROM information_schema.columns WHERE table_name = $1`, table)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, typ string
		require.NoError(t, rows.Scan(&name, &typ))
		out[name] = typ
	}
	require.NoError(t, rows.Err())
	return out
}

func indexDefs(t *testing.T, pool *pgxpool.Pool, table string) []string {
	t.Helper()
	return strs(t, pool, `SELECT indexdef FROM pg_indexes WHERE tablename = $1`, table)
}

func anyIndex(defs []string, match func(string) bool) bool {
	return slices.ContainsFunc(defs, match)
}

func has(def string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(def, p) {
			return false
		}
	}
	return true
}

// M00EnableExtensionsTests: "prepare is safe under concurrent invocation". The
// extensions are created by the baseline; eight concurrent migrate runs must all
// succeed and leave uuid-ossp, pgcrypto, vector and pg_trgm installed.
func TestM00ExtensionsUnderConcurrentMigrate(t *testing.T) {
	pool := dbtest.Postgres(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { errs[i] = migrations.Up(ctx, pool, migrations.SQL()) })
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "migrate run %d", i)
	}
	names := strs(t, pool, `SELECT extname FROM pg_extension WHERE extname IN ('uuid-ossp', 'pgcrypto', 'vector', 'pg_trgm')`)
	require.ElementsMatch(t, []string{"uuid-ossp", "pgcrypto", "vector", "pg_trgm"}, names)
}

// M39HnswAndTsvectorTests: "migration produces hnsw + tsv schema" and
// "migrations are idempotent".
func TestM39HnswAndTsvectorSchema(t *testing.T) {
	pool := migrated(t)
	indexes := strs(t, pool, `SELECT indexname FROM pg_indexes WHERE tablename = 'memories'`)
	require.Contains(t, indexes, "idx_memories_embedding_hnsw")
	require.Contains(t, indexes, "idx_memories_content_tsv")
	require.NotContains(t, indexes, "idx_memories_embedding", "the old IVFFlat index is gone")

	attrs := strs(t, pool, `SELECT attname::text FROM pg_attribute
		WHERE attrelid = 'memories'::regclass AND attnum > 0 AND NOT attisdropped`)
	require.Contains(t, attrs, "content_tsv")

	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()), "second run is a no-op")
}

// M41HermesTenantContainersTests: "migration creates table and indexes" and
// "migration is idempotent".
func TestM41HermesTenantContainersSchema(t *testing.T) {
	pool := migrated(t)
	cols := columns(t, pool, "hermes_tenant_containers")
	for _, c := range []string{
		"id", "tenant_id", "container_name", "port",
		"api_server_key_ciphertext", "api_server_key_nonce",
		"xai_connected_at", "created_at", "last_used_at",
	} {
		require.Contains(t, cols, c, "missing column")
	}
	indexes := strs(t, pool, `SELECT indexname FROM pg_indexes WHERE tablename = 'hermes_tenant_containers'`)
	require.Contains(t, indexes, "idx_hermes_tenant_containers_last_used_at")
	require.Contains(t, indexes, "idx_hermes_tenant_containers_port")

	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()), "second run is a no-op")
}

// M114HermesMirrorTests: "migrations create the mirror tables with unique tenant
// keys" (M114-M116 and M120) and "migrations are idempotent".
func TestM114HermesMirrorSchema(t *testing.T) {
	pool := migrated(t)

	state := columns(t, pool, "hermes_mirror_state")
	for _, c := range []string{
		"tenant_id", "last_sync_at", "last_status", "last_error", "skills_count", "jobs_count",
		"vault_files_count", "vault_path", "vault_state", "vault_cursor", "sessions_cursor",
		"sessions_imported", "compile_job_id",
	} {
		require.Contains(t, state, c, "hermes_mirror_state missing column")
	}
	require.True(t, anyIndex(indexDefs(t, pool, "hermes_mirror_state"), func(d string) bool { return has(d, "UNIQUE", "tenant_id") }))
	require.True(t, anyIndex(indexDefs(t, pool, "hermes_mirrored_skills"), func(d string) bool { return has(d, "UNIQUE", "tenant_id", "name") }))

	jobs := columns(t, pool, "hermes_mirrored_jobs")
	require.Equal(t, "jsonb", jobs["raw"])
	require.True(t, anyIndex(indexDefs(t, pool, "hermes_mirrored_jobs"), func(d string) bool { return has(d, "UNIQUE", "hermes_job_id") }))
	require.Contains(t, jobs, "runs_high_water_at")
	require.Contains(t, jobs, "runs_collected_at")

	runs := columns(t, pool, "hermes_job_runs")
	for _, c := range []string{
		"tenant_id", "hermes_job_id", "hermes_run_key", "status", "started_at", "finished_at",
		"output", "error", "tokens", "vault_file_id", "skill_run_log_id", "collected_at",
	} {
		require.Contains(t, runs, c, "hermes_job_runs missing column")
	}
	require.Equal(t, "jsonb", runs["tokens"])
	runIdx := indexDefs(t, pool, "hermes_job_runs")
	require.True(t, anyIndex(runIdx, func(d string) bool { return has(d, "UNIQUE", "hermes_run_key", "tenant_id") }))
	require.True(t, anyIndex(runIdx, func(d string) bool { return has(d, "started_at DESC") && !strings.Contains(d, "hermes_job_id") }))
	require.True(t, anyIndex(runIdx, func(d string) bool { return has(d, "started_at DESC", "hermes_job_id") }))

	cascade := strs(t, pool, `SELECT confdeltype::text FROM pg_constraint
		WHERE conrelid = 'hermes_job_runs'::regclass AND contype = 'f' AND confrelid = 'users'::regclass`)
	require.Equal(t, []string{"c"}, cascade, "deleting a user removes their run history")

	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()), "second run is a no-op")
}
