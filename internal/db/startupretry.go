package db

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// DefaultStartupDelays is StartupRetry.defaultDelays.
var DefaultStartupDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second}

// StartupRetryOptions are StartupRetry.run's defaulted parameters. The zero value
// is Swift's defaults.
type StartupRetryOptions struct {
	Delays      []time.Duration
	IsTransient func(error) bool
	Sleep       func(context.Context, time.Duration) error
	Log         *slog.Logger
}

// StartupRetry is StartupRetry.run: it retries boot-time database work when
// Postgres drops the connection. A deploy runs the migrate Job right before the
// new pod starts, and the pod's first connection is sometimes reset as that work
// winds down. Only connection-level failures are retried, once per delay: a
// failed query or bad credentials still stop the boot on the first attempt.
func StartupRetry[T any](ctx context.Context, label string, opt StartupRetryOptions, body func() (T, error)) (T, error) {
	if opt.Delays == nil {
		opt.Delays = DefaultStartupDelays
	}
	if opt.IsTransient == nil {
		opt.IsTransient = IsTransientConnectionError
	}
	if opt.Sleep == nil {
		opt.Sleep = sleep
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	for attempt := 0; ; {
		v, err := body()
		if err == nil || !opt.IsTransient(err) || attempt >= len(opt.Delays) {
			return v, err
		}
		delay := opt.Delays[attempt]
		attempt++
		opt.Log.Warn(label+" lost its database connection; retrying",
			"attempt", attempt, "delay", delay.String(), "error", fmt.Sprintf("%#v", err))
		if err := opt.Sleep(ctx, delay); err != nil {
			return v, err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// IsTransientConnectionError is StartupRetry.isTransientConnectionError: true for
// PSQLError's connectionError, serverClosedConnection and uncleanShutdown, which
// in pgx are a failed dial or a connection closed under it. An error the server
// answered with (a *pgconn.PgError, which includes bad credentials) is not.
func IsTransientConnectionError(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*pgconn.PgError](err); ok {
		return false
	}
	if _, ok := errors.AsType[*pgconn.ConnectError](err); ok {
		return true
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
