package ratelimit_test

import (
	"context"
	"lumina/internal/auth"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"lumina/internal/ratelimit"
	"lumina/internal/wire"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// A tiered policy end to end: users.tier decides the ceiling, the override doesn't,
// and an unidentified caller gets the baseline.
func TestPGTierScalesATieredPolicy(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	keys := auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

	e := echo.New()
	e.HTTPErrorHandler = wire.HTTPErrorHandler
	e.POST("/p", func(c *echo.Context) error { return c.NoContent(http.StatusOK) },
		auth.Identify(keys, auth.PGUsers(pool)),
		ratelimit.TieredMiddleware(ratelimit.NewStore(), ratelimit.ChatByUser, ratelimit.UserOrIPKey, ratelimit.PGTier(pool)))

	allowed := func(user uuid.UUID, ip string) int {
		n := 0
		for range 130 {
			req := httptest.NewRequest(http.MethodPost, "/p", nil)
			req.Header.Set("X-Real-Ip", ip)
			if user != uuid.Nil {
				tok, err := scenario.Mint(keys, user)
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				n++
			}
		}
		return n
	}
	user := func(tier, override string) uuid.UUID {
		id := uuid.New()
		_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, tier, tier_override) VALUES ($1, $2, 'x', $2, $3, $4)`,
			id, id.String(), tier, override)
		require.NoError(t, err)
		return id
	}

	require.Equal(t, 30, allowed(user("trial", "none"), "1.1.1.1"))
	require.Equal(t, 60, allowed(user("pro", "none"), "1.1.1.2"))
	require.Equal(t, 120, allowed(user("ultimate", "none"), "1.1.1.3"))
	require.Equal(t, 10, allowed(user("free", "none"), "1.1.1.4"))
	require.Equal(t, 10, allowed(user("lapsed", "ultimate"), "1.1.1.5"), "an override doesn't reach the rate limit")
	require.Equal(t, 30, allowed(uuid.Nil, "1.1.1.6"), "no identity gets the baseline")
}
