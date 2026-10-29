// Package dbtest starts a throwaway pgvector Postgres for integration tests.
package dbtest

import (
	"context"
	"lumina/internal/db"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image matches the Swift CI service image (LuminaVaultServer ci.yml).
const Image = "pgvector/pgvector:pg18"

// Postgres returns a pool on a fresh database; the container is removed when
// the test ends. Tests are skipped under -short.
func Postgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test (needs Docker); skipped with -short")
	}
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase("lv_test"),
		tcpostgres.WithUsername("lv"),
		tcpostgres.WithPassword("lv"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
