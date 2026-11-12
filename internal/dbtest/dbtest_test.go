package dbtest_test

import (
	"context"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPostgres: Swift's centralized test Postgres config (host, port, user,
// password, database read from env, TLS off). Go's equivalent is
// dbtest.Postgres: a throwaway container on the CI image, reached as lv on
// lv_test, whose pool the test can query at once.
func TestPostgresHelperConnects(t *testing.T) {
	pool := dbtest.Postgres(t)
	ctx := context.Background()
	var db, user string
	require.NoError(t, pool.QueryRow(ctx, `SELECT current_database(), current_user`).Scan(&db, &user))
	require.Equal(t, "lv_test", db)
	require.Equal(t, "lv", user)

	// The image is pgvector's, which stock Postgres is not: the migrations need it.
	var available bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector')`).Scan(&available))
	require.True(t, available, "image %s must ship the vector extension", dbtest.Image)
}

// TestPostgres: each call gets its own database, so suites never share state
// (Swift's TestDatabaseIsolation clones a per-suite database for the same reason).
func TestPostgresHelperIsolatesTests(t *testing.T) {
	ctx := context.Background()
	a := dbtest.Postgres(t)
	_, err := a.Exec(ctx, `CREATE TABLE only_in_a (id int)`)
	require.NoError(t, err)
	b := dbtest.Postgres(t)
	var exists bool
	require.NoError(t, b.QueryRow(ctx, `SELECT to_regclass('public.only_in_a') IS NOT NULL`).Scan(&exists))
	require.False(t, exists)
}

// PrimeBaseDatabaseTests: "migrate base database". Swift migrated the base
// database once so per-suite clones started from the full schema. Go has no
// template clone: a test migrates its own container. This pins that the full
// embedded migration list applies cleanly to a fresh dbtest database.
func TestPrimeBaseDatabaseMigrates(t *testing.T) {
	pool := dbtest.Postgres(t)
	ctx := context.Background()
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	var users bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('public.users') IS NOT NULL`).Scan(&users))
	require.True(t, users, "the migrated database has the full schema")
}
