package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RefreshTokenLifetime is AuthService.refreshTokenLifetime.
const RefreshTokenLifetime = 30 * 24 * time.Hour

// ErrInvalidRefresh is AuthError.invalidRefresh: unknown, revoked or expired
// token, or a user that no longer exists.
var ErrInvalidRefresh = errors.New("auth: invalid refresh token")

// NewRefreshToken is AuthService.randomToken: 32 random bytes, base64url
// without padding.
func NewRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashRefreshToken is AuthService.sha256Hex: lowercase hex SHA-256 of the
// UTF-8 token. refresh_tokens.token_hash holds this, never the token.
func HashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// DB is the part of pgxpool.Pool and pgx.Tx the refresh store uses.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// IssueRefreshToken is the refresh half of issueTokens: a new token and its
// row, expiring RefreshTokenLifetime from now.
func IssueRefreshToken(ctx context.Context, db DB, tenant uuid.UUID, now time.Time) (string, error) {
	raw, err := NewRefreshToken()
	if err != nil {
		return "", err
	}
	_, err = db.Exec(ctx, `INSERT INTO refresh_tokens (id, tenant_id, token_hash, expires_at, revoked_at, created_at)
		VALUES ($1, $2, $3, $4, NULL, $5)`, uuid.New(), tenant, HashRefreshToken(raw), now.Add(RefreshTokenLifetime), now)
	if err != nil {
		return "", err
	}
	return raw, nil
}

// ConsumeRefreshToken is the first half of AuthService.refresh: the newest
// matching unrevoked row must be unexpired and its user must exist; it is then
// revoked and its tenant returned for issueTokens. As in Swift this is not
// transactional and there is no reuse detection: a revoked token is simply invalid.
func ConsumeRefreshToken(ctx context.Context, db DB, raw string, now time.Time) (uuid.UUID, error) {
	var (
		id, tenant uuid.UUID
		expires    time.Time
		userExists bool
	)
	err := db.QueryRow(ctx, `SELECT r.id, r.tenant_id, r.expires_at, EXISTS (SELECT 1 FROM users u WHERE u.id = r.tenant_id)
		FROM refresh_tokens r WHERE r.token_hash = $1 AND r.revoked_at IS NULL LIMIT 1`,
		HashRefreshToken(raw)).Scan(&id, &tenant, &expires, &userExists)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrInvalidRefresh
	}
	if err != nil {
		return uuid.Nil, err
	}
	if !expires.After(now) || !userExists {
		return uuid.Nil, ErrInvalidRefresh
	}
	if _, err := db.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = $2 WHERE id = $1`, id, now); err != nil {
		return uuid.Nil, err
	}
	return tenant, nil
}

// RevokeRefreshToken is AuthService.revokeRefresh: revoke the row with this
// hash, revoked or not. An unknown token is not an error.
func RevokeRefreshToken(ctx context.Context, db DB, raw string, now time.Time) error {
	var id uuid.UUID
	err := db.QueryRow(ctx, `SELECT id FROM refresh_tokens WHERE token_hash = $1 LIMIT 1`, HashRefreshToken(raw)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = $2 WHERE id = $1`, id, now)
	return err
}
