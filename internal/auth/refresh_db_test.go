package auth_test

import (
	"context"
	"encoding/base64"
	"lumina/internal/auth"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The refresh half of AuthService: issue, rotate (revoke then the caller
// issues), revoke. Not transactional, no reuse detection, as Swift.
func TestRefreshTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	user := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username) VALUES ($1, $2, 'x', $2)`, user, user.String())
	require.NoError(t, err)
	now := time.Now()

	raw, err := auth.IssueRefreshToken(ctx, pool, user, now)
	require.NoError(t, err)
	b, err := base64.RawURLEncoding.DecodeString(raw)
	require.NoError(t, err)
	require.Len(t, b, 32)
	require.Len(t, raw, 43, "unpadded")

	var hash string
	var expires time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT token_hash, expires_at FROM refresh_tokens WHERE tenant_id = $1`, user).Scan(&hash, &expires))
	require.Equal(t, auth.HashRefreshToken(raw), hash)
	require.Len(t, hash, 64)
	require.WithinDuration(t, now.Add(30*24*time.Hour), expires, time.Millisecond)

	got, err := auth.ConsumeRefreshToken(ctx, pool, raw, now)
	require.NoError(t, err)
	require.Equal(t, user, got)
	_, err = auth.ConsumeRefreshToken(ctx, pool, raw, now)
	require.ErrorIs(t, err, auth.ErrInvalidRefresh, "rotated: revoked")

	_, err = auth.ConsumeRefreshToken(ctx, pool, "never-issued", now)
	require.ErrorIs(t, err, auth.ErrInvalidRefresh)

	old, err := auth.IssueRefreshToken(ctx, pool, user, now.Add(-31*24*time.Hour))
	require.NoError(t, err)
	_, err = auth.ConsumeRefreshToken(ctx, pool, old, now)
	require.ErrorIs(t, err, auth.ErrInvalidRefresh, "expired")

	live, err := auth.IssueRefreshToken(ctx, pool, user, now)
	require.NoError(t, err)
	require.NoError(t, auth.RevokeRefreshToken(ctx, pool, live, now))
	require.NoError(t, auth.RevokeRefreshToken(ctx, pool, "unknown", now))
	_, err = auth.ConsumeRefreshToken(ctx, pool, live, now)
	require.ErrorIs(t, err, auth.ErrInvalidRefresh, "logged out")
}

// sha256Hex is lowercase hex of the UTF-8 bytes.
func TestHashRefreshTokenIsSHA256Hex(t *testing.T) {
	require.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", auth.HashRefreshToken("abc"))
}
