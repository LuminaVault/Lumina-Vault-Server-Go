package migrations_test

import (
	"context"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
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

func TestEmbeddedFSLoads(t *testing.T) {
	_, err := migrations.SQL().Open("README.md")
	require.NoError(t, err)
}
