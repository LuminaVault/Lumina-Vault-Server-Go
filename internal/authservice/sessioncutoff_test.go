package authservice_test

import (
	"context"
	"lumina/internal/auth"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// A password reset invalidates the access tokens issued before it, on ordinary
// and audio routes (post-archive fix: Swift left them valid until they expired).
// On a database with no sessions_valid_after (Swift-migrated, as parity's) the
// reset still works and the check is off.
func TestPasswordResetInvalidatesAccessTokens(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	e := echo.New()
	ok := func(c *echo.Context) error { return c.NoContent(http.StatusOK) }
	e.GET("/p", ok, auth.Middleware(keys, auth.PGUsers(pool)))
	e.GET("/audio", ok, auth.AudioIdentify(keys, auth.PGAudioUsers(pool)), auth.Require)
	get := func(path, token string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}

	h := build(t, pool, harness{})
	email := randomEmail("cutoff")
	_, err := h.svc.Register(ctx, email, randomUsername("cutoff"), password)
	require.NoError(t, err)
	before, err := h.svc.Login(ctx, email, password, false)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, get("/p", before.AccessToken))
	require.Equal(t, http.StatusOK, get("/audio", before.AccessToken))

	// iat is kept to the second; let the reset fall in a later one.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	require.NoError(t, h.svc.ForgotPassword(ctx, email))
	after, err := h.svc.ResetPassword(ctx, email, "654321", "NewPasswordCorrectHorse1!")
	require.NoError(t, err)

	require.Equal(t, http.StatusUnauthorized, get("/p", before.AccessToken))
	require.Equal(t, http.StatusUnauthorized, get("/audio", before.AccessToken))
	require.Equal(t, http.StatusOK, get("/p", after.AccessToken))
	require.Equal(t, http.StatusOK, get("/audio", after.AccessToken))
}

func TestPasswordResetWithoutTheCutoffColumn(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `ALTER TABLE users DROP COLUMN sessions_valid_after`)
	require.NoError(t, err)
	has, err := auth.SessionsCutoffColumn(ctx, pool)
	require.NoError(t, err)
	require.False(t, has)

	e := echo.New()
	e.GET("/p", func(c *echo.Context) error { return c.NoContent(http.StatusOK) }, auth.Middleware(keys, auth.PGUsers(pool)))
	h := build(t, pool, harness{})
	email := randomEmail("nocol")
	_, err = h.svc.Register(ctx, email, randomUsername("nocol"), password)
	require.NoError(t, err)
	before, err := h.svc.Login(ctx, email, password, false)
	require.NoError(t, err)
	require.NoError(t, h.svc.ForgotPassword(ctx, email))
	_, err = h.svc.ResetPassword(ctx, email, "654321", "NewPasswordCorrectHorse1!")
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/p", nil)
	req.Header.Set("Authorization", "Bearer "+before.AccessToken)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "no column: the check is off, as in Swift")
}
