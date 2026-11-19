package connectedapps

import (
	"context"
	"errors"
	"lumina/internal/secretbox"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotConnected means the user has no account row for the app.
var ErrNotConnected = errors.New("connectedapps: not connected")

// Account is one user's link to one connected app. Tokens are plaintext and
// live only in memory; the database holds them sealed under the user id.
type Account struct {
	AppID, Status             string // Status: "connected" | "needs_reauth"
	AccessToken, RefreshToken string
	ExpiresAt                 *time.Time
	Scope                     *string
	ConnectedAt               time.Time
}

// Store keeps connected-app tokens, sealed per user: the secretbox key is the
// user id, so a row copied to another user cannot be opened.
type Store struct {
	pool *pgxpool.Pool
	box  *secretbox.Box
	now  func() time.Time
}

func NewStore(pool *pgxpool.Pool, box *secretbox.Box) *Store {
	return &Store{pool: pool, box: box, now: time.Now}
}

// sealed is an account's tokens sealed under the user id. An empty refresh
// token gives nil ciphertext and nonce, stored as NULLs.
type sealed struct{ accCT, accNonce, refCT, refNonce []byte }

func (s *Store) seal(user uuid.UUID, a Account) (sealed, error) {
	access, err := s.box.Seal(a.AccessToken, user)
	if err != nil {
		return sealed{}, err
	}
	out := sealed{accCT: access.Ciphertext, accNonce: access.Nonce}
	if a.RefreshToken != "" {
		refresh, err := s.box.Seal(a.RefreshToken, user)
		if err != nil {
			return sealed{}, err
		}
		out.refCT, out.refNonce = refresh.Ciphertext, refresh.Nonce
	}
	return out, nil
}

// Save upserts the (user, app) row as connected. An empty RefreshToken stores
// NULLs. Never logs tokens.
func (s *Store) Save(ctx context.Context, user uuid.UUID, a Account) error {
	t, err := s.seal(user, a)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO connected_app_accounts
    (id, user_id, app_id, access_ciphertext, access_nonce, refresh_ciphertext, refresh_nonce,
     access_expires_at, scope, status, last_failure_code, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'connected', NULL, $10, $10)
ON CONFLICT (user_id, app_id) DO UPDATE SET
    access_ciphertext = EXCLUDED.access_ciphertext,
    access_nonce = EXCLUDED.access_nonce,
    refresh_ciphertext = EXCLUDED.refresh_ciphertext,
    refresh_nonce = EXCLUDED.refresh_nonce,
    access_expires_at = EXCLUDED.access_expires_at,
    scope = EXCLUDED.scope,
    status = 'connected',
    last_failure_code = NULL,
    updated_at = EXCLUDED.updated_at`,
		uuid.New(), user, a.AppID, t.accCT, t.accNonce, t.refCT, t.refNonce,
		a.ExpiresAt, a.Scope, s.now())
	return err
}

// UpdateTokens replaces the tokens of an existing (user, app) row and never
// inserts one: a refresh that answers after a disconnect must not bring the
// row back. ErrNotConnected when the row is gone. Never logs tokens.
func (s *Store) UpdateTokens(ctx context.Context, user uuid.UUID, a Account) error {
	t, err := s.seal(user, a)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE connected_app_accounts SET
    access_ciphertext = $3,
    access_nonce = $4,
    refresh_ciphertext = $5,
    refresh_nonce = $6,
    access_expires_at = $7,
    scope = $8,
    status = 'connected',
    last_failure_code = NULL,
    updated_at = $9
WHERE user_id = $1 AND app_id = $2`,
		user, a.AppID, t.accCT, t.accNonce, t.refCT, t.refNonce, a.ExpiresAt, a.Scope, s.now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotConnected
	}
	return nil
}

// Get returns the account with tokens opened. ErrNotConnected when absent; a
// row sealed for a different user fails with secretbox.ErrDecryptionFailed.
func (s *Store) Get(ctx context.Context, user uuid.UUID, appID string) (Account, error) {
	var (
		a                                Account
		accCT, accNonce, refCT, refNonce []byte
	)
	err := s.pool.QueryRow(ctx, `
SELECT app_id, status, access_ciphertext, access_nonce, refresh_ciphertext, refresh_nonce,
       access_expires_at, scope, created_at
FROM connected_app_accounts WHERE user_id = $1 AND app_id = $2`, user, appID).
		Scan(&a.AppID, &a.Status, &accCT, &accNonce, &refCT, &refNonce, &a.ExpiresAt, &a.Scope, &a.ConnectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotConnected
	}
	if err != nil {
		return Account{}, err
	}
	if accCT != nil && accNonce != nil {
		if a.AccessToken, err = s.box.Open(secretbox.Sealed{Ciphertext: accCT, Nonce: accNonce}, user); err != nil {
			return Account{}, err
		}
	}
	if refCT != nil && refNonce != nil {
		if a.RefreshToken, err = s.box.Open(secretbox.Sealed{Ciphertext: refCT, Nonce: refNonce}, user); err != nil {
			return Account{}, err
		}
	}
	return a, nil
}

// List returns the user's accounts without tokens (nothing is decrypted).
func (s *Store) List(ctx context.Context, user uuid.UUID) ([]Account, error) {
	rows, err := s.pool.Query(ctx, `
SELECT app_id, status, access_expires_at, scope, created_at
FROM connected_app_accounts WHERE user_id = $1 ORDER BY app_id`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.AppID, &a.Status, &a.ExpiresAt, &a.Scope, &a.ConnectedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Delete removes the account row; deleting an absent row is not an error.
func (s *Store) Delete(ctx context.Context, user uuid.UUID, appID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM connected_app_accounts WHERE user_id = $1 AND app_id = $2`, user, appID)
	return err
}

// MarkNeedsReauth flags the account so the user is asked to reconnect.
func (s *Store) MarkNeedsReauth(ctx context.Context, user uuid.UUID, appID, code string) error {
	tag, err := s.pool.Exec(ctx, `
UPDATE connected_app_accounts SET status = 'needs_reauth', last_failure_code = $3, updated_at = $4
WHERE user_id = $1 AND app_id = $2`, user, appID, code, s.now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotConnected
	}
	return nil
}
