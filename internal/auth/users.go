package auth

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type pgUsers struct{ pool *pgxpool.Pool }

// PGUsers checks the users table, as Swift's JWTAuthenticator does with
// User.find (the table has no soft-delete column).
func PGUsers(pool *pgxpool.Pool) UserLoader { return pgUsers{pool} }

func (u pgUsers) Exists(ctx context.Context, id uuid.UUID) (bool, error) {
	var ok bool
	err := u.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, id).Scan(&ok)
	return ok, err
}
