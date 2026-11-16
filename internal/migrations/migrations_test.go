package migrations_test

import (
	"context"
	"io/fs"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// A stand-in for the real baseline until Task 0.5 generates it.
var testFS = fstest.MapFS{
	"00001_baseline.sql": {Data: []byte("-- +goose Up\nCREATE EXTENSION IF NOT EXISTS vector;\nCREATE TABLE users (id uuid PRIMARY KEY, email text);\n-- +goose Down\nSELECT 1/0;\n")},
	"00002_notes.sql":    {Data: []byte("-- mirrors: M100_CreateNotes\n-- +goose Up\nCREATE TABLE notes (id uuid PRIMARY KEY, embedding vector(3));\n-- +goose Down\nDROP TABLE notes;\n")},
}

func fluentDB(t *testing.T, pool *pgxpool.Pool, names ...string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `CREATE TABLE _fluent_migrations (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), name text NOT NULL, batch int NOT NULL DEFAULT 1)`)
	require.NoError(t, err)
	for _, n := range names {
		_, err := pool.Exec(ctx, `INSERT INTO _fluent_migrations (name) VALUES ($1)`, n)
		require.NoError(t, err)
	}
}

func TestUpOnEmptyDB(t *testing.T) {
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, testFS))
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM notes`).Scan(&n))
	require.NoError(t, migrations.Up(context.Background(), pool, testFS), "second Up is a no-op")
}

func TestMigrateRefusesFluentDB(t *testing.T) {
	pool := dbtest.Postgres(t)
	fluentDB(t, pool, "M00_EnableExtensions")
	require.ErrorIs(t, migrations.Up(context.Background(), pool, testFS), migrations.ErrFluentManaged)
}

func TestAdoptFluentRequiresExactManifest(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	fluentDB(t, pool, "M00_EnableExtensions", "M01_CreateUser")

	err := migrations.AdoptFluent(ctx, pool, testFS, []string{"M00_EnableExtensions"})
	require.ErrorContains(t, err, "M01_CreateUser")
	err = migrations.AdoptFluent(ctx, pool, testFS, []string{"M00_EnableExtensions", "M01_CreateUser", "M02_Extra"})
	require.ErrorContains(t, err, "M02_Extra")

	require.NoError(t, migrations.AdoptFluent(ctx, pool, testFS, []string{"M00_EnableExtensions", "M01_CreateUser"}))
	// goose now sees everything as applied: Up runs no DDL, so the tables the
	// stand-in migrations would create must still be absent.
	require.NoError(t, migrations.Up(ctx, pool, testFS))
	var exists bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('public.notes') IS NOT NULL`).Scan(&exists))
	require.False(t, exists, "adopt must not execute migration SQL")
}

// A database adopted from Fluent has every embedded version recorded and none
// of their DDL run. A table that only goose creates must therefore arrive in
// a version written after the adoption, or it never arrives.
func TestUpAfterAdoptRunsVersionsAddedLater(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	fluentDB(t, pool, "M00_EnableExtensions")
	require.NoError(t, migrations.AdoptFluent(ctx, pool, testFS, []string{"M00_EnableExtensions"}))

	later := fstest.MapFS{"00003_notes_ensure.sql": {Data: []byte("-- +goose Up\nCREATE TABLE IF NOT EXISTS notes (id uuid PRIMARY KEY);\n-- +goose Down\nSELECT 1;\n")}}
	for name, f := range testFS {
		later[name] = f
	}
	require.NoError(t, migrations.Up(ctx, pool, later))
	var exists bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('public.notes') IS NOT NULL`).Scan(&exists))
	require.True(t, exists, "the version added after adoption must run")
}

// 00004 exists only to give adopted databases the table 00002 defines, so the
// two must describe the same table: same statements, word for word.
func TestPendingActionsEnsureRepeats00002(t *testing.T) {
	up := func(name string) string {
		b, err := fs.ReadFile(migrations.SQL(), name)
		require.NoError(t, err)
		_, rest, ok := strings.Cut(string(b), "-- +goose Up\n")
		require.True(t, ok, name)
		body, _, ok := strings.Cut(rest, "-- +goose Down")
		require.True(t, ok, name)
		return strings.TrimSpace(body)
	}
	require.Equal(t, up("00002_assistant_pending_actions.sql"), up("00004_assistant_pending_actions_ensure.sql"))
}

// Both files run on a fresh database; the second must be a no-op there.
func TestPendingActionsTableOnFreshDB(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	var exists bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('public.assistant_pending_actions') IS NOT NULL`).Scan(&exists))
	require.True(t, exists)
}

func TestEmbeddedFSLoads(t *testing.T) {
	_, err := migrations.SQL().Open("README.md")
	require.NoError(t, err)
}
