package db_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"lumina/internal/db"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

var (
	errNotAConnection = errors.New("not a connection error")
	errDropped        = errors.New("dropped connection")
)

func droppedOnly(err error) bool { return errors.Is(err, errDropped) }

func noSleep(context.Context, time.Duration) error { return nil }

// StartupRetryTests: "a dropped connection is retried until the work succeeds".
func TestStartupRetryRetriesADroppedConnection(t *testing.T) {
	calls := 0
	got, err := db.StartupRetry(context.Background(), "test", db.StartupRetryOptions{IsTransient: droppedOnly, Sleep: noSleep}, func() (string, error) {
		calls++
		if calls < 3 {
			return "", errDropped
		}
		return "migrated", nil
	})
	require.NoError(t, err)
	require.Equal(t, "migrated", got)
	require.Equal(t, 3, calls)
}

// StartupRetryTests: "any other error stops the boot on the first attempt".
func TestStartupRetryStopsOnAnyOtherError(t *testing.T) {
	calls := 0
	_, err := db.StartupRetry(context.Background(), "test", db.StartupRetryOptions{IsTransient: droppedOnly, Sleep: noSleep}, func() (int, error) {
		calls++
		return 0, errNotAConnection
	})
	require.ErrorIs(t, err, errNotAConnection)
	require.Equal(t, 1, calls)
}

// StartupRetryTests: "retries are bounded".
func TestStartupRetryIsBounded(t *testing.T) {
	calls := 0
	var slept []time.Duration
	_, err := db.StartupRetry(context.Background(), "test", db.StartupRetryOptions{
		Delays: []time.Duration{0, 0}, IsTransient: droppedOnly,
		Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
	}, func() (int, error) {
		calls++
		return 0, errDropped
	})
	require.ErrorIs(t, err, errDropped)
	require.Equal(t, 3, calls)
	require.Len(t, slept, 2)
}

// StartupRetryTests: "only Postgres connection errors count as transient by default".
func TestStartupRetryOnlyConnectionErrorsAreTransient(t *testing.T) {
	require.False(t, db.IsTransientConnectionError(errNotAConnection))
	require.False(t, db.IsTransientConnectionError(&pgconn.PgError{Code: "28P01"}), "bad credentials stop the boot")
	require.False(t, db.IsTransientConnectionError(&pgconn.PgError{Code: "42P01"}), "a failed query stops the boot")
	require.True(t, db.IsTransientConnectionError(fmt.Errorf("db: %w", io.ErrUnexpectedEOF)), "server closed the connection")

	// A real failed dial: nothing listens on port 1.
	_, err := pgconn.Connect(context.Background(), "postgres://u:p@127.0.0.1:1/x?sslmode=disable&connect_timeout=2")
	require.Error(t, err)
	require.True(t, db.IsTransientConnectionError(err), "%v", err)
	require.Equal(t, db.DefaultStartupDelays, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second})
}
