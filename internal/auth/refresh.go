package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
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
// row, expiring RefreshTokenLifetime from now. The row has no family.
func IssueRefreshToken(ctx context.Context, db DB, tenant uuid.UUID, now time.Time) (string, error) {
	return IssueRefreshTokenInFamily(ctx, db, tenant, uuid.Nil, false, now)
}

// IssueRefreshTokenInFamily is IssueRefreshToken for a login chain. families
// says refresh_tokens.family_id exists (RefreshFamilyColumn); without it the
// family is ignored. uuid.Nil starts a new family: sign-in passes it, and only
// a rotation passes the family of the token it consumed.
func IssueRefreshTokenInFamily(ctx context.Context, db DB, tenant, family uuid.UUID, families bool, now time.Time) (string, error) {
	raw, err := NewRefreshToken()
	if err != nil {
		return "", err
	}
	if !families {
		_, err = db.Exec(ctx, `INSERT INTO refresh_tokens (id, tenant_id, token_hash, expires_at, revoked_at, created_at)
			VALUES ($1, $2, $3, $4, NULL, $5)`, uuid.New(), tenant, HashRefreshToken(raw), now.Add(RefreshTokenLifetime), now)
	} else {
		if family == uuid.Nil {
			family = uuid.New()
		}
		_, err = db.Exec(ctx, `INSERT INTO refresh_tokens (id, tenant_id, token_hash, expires_at, revoked_at, created_at, family_id)
			VALUES ($1, $2, $3, $4, NULL, $5, $6)`, uuid.New(), tenant, HashRefreshToken(raw), now.Add(RefreshTokenLifetime), now, family)
	}
	if err != nil {
		return "", err
	}
	return raw, nil
}

// RefreshFamilyColumn reports whether refresh_tokens.family_id exists. goose
// migration 00009 adds it; a database Swift migrated (parity) has none, and
// there reuse keeps revoking the whole tenant. Probe it outside any
// transaction: it reads through the pool.
func RefreshFamilyColumn(ctx context.Context, pool DB) (bool, error) {
	var ok bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'refresh_tokens' AND column_name = 'family_id')`).Scan(&ok)
	return ok, err
}

// RefreshReuseGrace is how long after its revocation a refresh token may be
// presented again without counting as reuse. It covers a client racing itself
// (two refreshes of the same token from one device); later reuse of a rotated,
// logged-out or reset token means it leaked.
const RefreshReuseGrace = 30 * time.Second

// ConsumeRefreshToken is ConsumeRefreshTokenFamily without families: reuse
// revokes every live refresh token of the tenant.
func ConsumeRefreshToken(ctx context.Context, db DB, raw string, now time.Time) (uuid.UUID, error) {
	tenant, _, err := ConsumeRefreshTokenFamily(ctx, db, raw, now, false)
	return tenant, err
}

// ConsumeRefreshTokenFamily is the first half of AuthService.refresh: the
// matching unrevoked row must be unexpired and its user must exist; it is
// revoked in the same statement and its tenant and family (uuid.Nil when it has
// none) returned for issueTokens. Two refreshes of one token cannot both win.
//
// A token revoked more than RefreshReuseGrace ago is reuse, and whoever holds
// the stolen chain has to sign in again. With families (RefreshFamilyColumn),
// only the reused token's login chain is revoked and the tenant's other
// sessions stay signed in; a token without a family, or no families at all,
// revokes every live refresh token of the tenant. Swift read, then revoked,
// with no reuse detection; both fixed after the archive. db may be a
// transaction.
func ConsumeRefreshTokenFamily(ctx context.Context, db DB, raw string, now time.Time, families bool) (uuid.UUID, uuid.UUID, error) {
	hash := HashRefreshToken(raw)
	familyCol := "NULL::uuid"
	if families {
		familyCol = "r.family_id"
	}
	var tenant uuid.UUID
	var family *uuid.UUID
	err := db.QueryRow(ctx, `UPDATE refresh_tokens r SET revoked_at = $2
		WHERE r.token_hash = $1 AND r.revoked_at IS NULL AND r.expires_at > $2
		  AND EXISTS (SELECT 1 FROM users u WHERE u.id = r.tenant_id)
		RETURNING r.tenant_id, `+familyCol, hash, now).Scan(&tenant, &family)
	if err == nil {
		return tenant, deref(family), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, err
	}
	var revokedAt *time.Time
	err = db.QueryRow(ctx, `SELECT r.tenant_id, r.revoked_at, `+familyCol+` FROM refresh_tokens r WHERE r.token_hash = $1
		ORDER BY r.revoked_at DESC NULLS LAST LIMIT 1`, hash).Scan(&tenant, &revokedAt, &family)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, ErrInvalidRefresh
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if revokedAt != nil && revokedAt.Before(now.Add(-RefreshReuseGrace)) {
		if family != nil {
			_, err = db.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = $3 WHERE tenant_id = $1 AND family_id = $2 AND revoked_at IS NULL`,
				tenant, *family, now)
		} else {
			_, err = db.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = $2 WHERE tenant_id = $1 AND revoked_at IS NULL`,
				tenant, now)
		}
		if err != nil {
			return uuid.Nil, uuid.Nil, err
		}
		return tenant, deref(family), ErrRefreshReused
	}
	return uuid.Nil, uuid.Nil, ErrInvalidRefresh
}

func deref(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

// RefreshTokenTenant is the tenant a refresh token was issued to, whatever its
// state; false when the token is unknown. It decides nothing: only
// ConsumeRefreshToken says whether the token may be used.
func RefreshTokenTenant(ctx context.Context, db DB, raw string) (uuid.UUID, bool, error) {
	var tenant uuid.UUID
	err := db.QueryRow(ctx, `SELECT tenant_id FROM refresh_tokens WHERE token_hash = $1 LIMIT 1`, HashRefreshToken(raw)).Scan(&tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return tenant, true, nil
}

// ErrRefreshReused is ErrInvalidRefresh after reuse was detected and the
// reused token's family (or, without one, the tenant's refresh tokens) was revoked. errors.Is(ErrRefreshReused,
// ErrInvalidRefresh) holds, so callers answer the same 401.
var ErrRefreshReused = fmt.Errorf("%w: reused after revocation", ErrInvalidRefresh)

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
