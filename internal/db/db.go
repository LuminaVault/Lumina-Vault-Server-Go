// Package db opens the shared Postgres pool. Until Phase 12 the schema is
// owned by the Swift server's Fluent migrations; Go reads and writes the same
// tables.
package db

import (
	"context"
	"fmt"
	"lumina/internal/config"
	"net"
	"net/url"
	"strconv"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// DSN builds a connection string from the POSTGRES_* variables Swift uses.
func DSN(cfg config.Config) string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.PostgresUser, cfg.PostgresPassword),
		Host:   net.JoinHostPort(cfg.PostgresHost, strconv.Itoa(cfg.PostgresPort)),
		Path:   "/" + cfg.PostgresDatabase,
	}
	return u.String()
}

// Open connects, registers the pgvector types on every connection and traces
// queries through OpenTelemetry.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	pcfg.ConnConfig.Tracer = otelpgx.NewTracer()
	pcfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return registerVector(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: %w", err)
	}
	return pool, nil
}

// registerVector is a no-op on a database without the vector extension (a
// fresh test DB before migrations), so it never blocks a connection.
func registerVector(ctx context.Context, conn *pgx.Conn) error {
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'vector')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	return pgxvec.RegisterTypes(ctx, conn)
}
