package auth

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionChecker is a UserLoader that also knows when a user's session tokens
// were last invalidated (users.sessions_valid_after, set by a password reset).
// Identify uses it when the loader offers it.
type SessionChecker interface {
	// SessionsValidAfter returns the user's cutoff (nil when none) and whether the
	// user exists.
	SessionsValidAfter(ctx context.Context, id uuid.UUID) (cutoff *time.Time, found bool, err error)
}

type pgUsers struct {
	pool *pgxpool.Pool
	// hasCutoff probes once for users.sessions_valid_after. goose migration 00005
	// adds it; a database Swift migrated (parity) has none, and there the check
	// is off rather than every request failing.
	hasCutoff func() (bool, error)
}

// PGUsers checks the users table, as Swift's JWTAuthenticator does with
// User.find (the table has no soft-delete column).
func PGUsers(pool *pgxpool.Pool) UserLoader {
	return pgUsers{pool: pool, hasCutoff: sync.OnceValues(func() (bool, error) {
		return SessionsCutoffColumn(context.Background(), pool)
	})}
}

// SessionsCutoffColumn reports whether users.sessions_valid_after exists.
func SessionsCutoffColumn(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var ok bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'users' AND column_name = 'sessions_valid_after')`).Scan(&ok)
	return ok, err
}

func (u pgUsers) Exists(ctx context.Context, id uuid.UUID) (bool, error) {
	var ok bool
	err := u.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, id).Scan(&ok)
	return ok, err
}

func (u pgUsers) SessionsValidAfter(ctx context.Context, id uuid.UUID) (*time.Time, bool, error) {
	has, err := u.hasCutoff()
	if err != nil {
		return nil, false, err
	}
	if !has {
		ok, err := u.Exists(ctx, id)
		return nil, ok, err
	}
	var cutoff *time.Time
	err = u.pool.QueryRow(ctx, `SELECT sessions_valid_after FROM users WHERE id = $1`, id).Scan(&cutoff)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return cutoff, err == nil, err
}
