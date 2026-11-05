package entitlement_test

import (
	"context"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/entitlement"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// The vision and knowledge cases of Billing/BYOEntitlementMiddlewareTests.swift
// that entitlement_test.go doesn't hold: the gate as /v1/vision and /v1/knowledge mount it.
type gateEnv struct {
	t    *testing.T
	pool *pgxpool.Pool
}

var gateKeys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

func newGateEnv(t *testing.T) gateEnv {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	return gateEnv{t: t, pool: pool}
}

func (g gateEnv) user(tier, override string) uuid.UUID {
	g.t.Helper()
	id := uuid.New()
	_, err := g.pool.Exec(context.Background(),
		`INSERT INTO users (id, email, password_hash, username, tier, tier_override) VALUES ($1, $2, 'x', $3, $4, $5)`,
		id, id.String(), id.String(), tier, override)
	require.NoError(g.t, err)
	return id
}

type probeModule struct {
	mw       echo.MiddlewareFunc
	identify echo.MiddlewareFunc
}

func (m probeModule) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	r.GET("/probe", func(c *echo.Context) error { return c.String(http.StatusOK, "ok") }, m.identify, m.mw)
}

// probe runs one request through mw as user; uuid.Nil is anonymous.
func (g gateEnv) probe(mw echo.MiddlewareFunc, user uuid.UUID) (int, string) {
	g.t.Helper()
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Modules: []httpserver.Module{probeModule{mw: mw, identify: auth.Identify(gateKeys, auth.PGUsers(g.pool))}},
	})
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	if user != uuid.Nil {
		tok, err := scenario.Mint(gateKeys, user)
		require.NoError(g.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestPlatformFundedFloor(t *testing.T) {
	g := newGateEnv(t)
	gate := entitlement.PlatformFundedMiddleware(g.pool, entitlement.MemoryQuery, true)

	t.Run("closed to non-paying tiers, even though they hold the capability", func(t *testing.T) {
		for _, tier := range []string{"free", "lapsed", "archived"} {
			code, body := g.probe(gate, g.user(tier, "none"))
			require.Equal(t, http.StatusPaymentRequired, code, tier)
			require.JSONEq(t, `{"paywall":true,"paywallId":"default","requiredTier":"pro"}`, body)
		}
		chat := entitlement.PlatformFundedMiddleware(g.pool, entitlement.Chat, true)
		code, _ := g.probe(chat, g.user("lapsed", "none"))
		require.Equal(t, http.StatusPaymentRequired, code, "chat is the same on a platform-funded route")
	})
	t.Run("open to trial and above", func(t *testing.T) {
		for _, tier := range []string{"trial", "pro", "ultimate"} {
			code, _ := g.probe(gate, g.user(tier, "none"))
			require.Equal(t, http.StatusOK, code, tier)
		}
	})
	t.Run("an override clears the floor", func(t *testing.T) {
		code, _ := g.probe(gate, g.user("free", "ultimate"))
		require.Equal(t, http.StatusOK, code)
		code, _ = g.probe(gate, g.user("lapsed", "pro"))
		require.Equal(t, http.StatusOK, code)
	})
	t.Run("an archived tenant is not saved by an override below the capability", func(t *testing.T) {
		code, _ := g.probe(gate, g.user("archived", "none"))
		require.Equal(t, http.StatusPaymentRequired, code)
	})
	t.Run("no identity is a 401 first", func(t *testing.T) {
		code, _ := g.probe(gate, uuid.Nil)
		require.Equal(t, http.StatusUnauthorized, code)
	})
	t.Run("enforcement off bypasses everything", func(t *testing.T) {
		off := entitlement.PlatformFundedMiddleware(g.pool, entitlement.MemoryQuery, false)
		code, _ := g.probe(off, uuid.Nil)
		require.Equal(t, http.StatusOK, code)
		code, _ = g.probe(off, g.user("archived", "none"))
		require.Equal(t, http.StatusOK, code)
	})
}

// The same tenant, same signals, on the user-funded route /v1/knowledge mounts.
func TestUserFundedRouteExemption(t *testing.T) {
	g := newGateEnv(t)
	own := func(context.Context, uuid.UUID) bool { return true }
	none := func(context.Context, uuid.UUID) bool { return false }

	t.Run("a lapsed user with their own key is not charged", func(t *testing.T) {
		code, _ := g.probe(entitlement.Middleware(g.pool, entitlement.MemoryCompile, true, own), g.user("lapsed", "none"))
		require.Equal(t, http.StatusOK, code)
	})
	t.Run("the same capability passes on a user-funded route", func(t *testing.T) {
		code, _ := g.probe(entitlement.Middleware(g.pool, entitlement.MemoGenerator, true, own), g.user("lapsed", "none"))
		require.Equal(t, http.StatusOK, code)
		code, _ = g.probe(entitlement.Middleware(g.pool, entitlement.MemoGenerator, true, none), g.user("lapsed", "none"))
		require.Equal(t, http.StatusPaymentRequired, code)
	})
	t.Run("ultimate-only capabilities stay gated for BYO", func(t *testing.T) {
		code, body := g.probe(entitlement.Middleware(g.pool, entitlement.SkillVaultRun, true, own), g.user("lapsed", "none"))
		require.Equal(t, http.StatusPaymentRequired, code)
		require.JSONEq(t, `{"paywall":true,"paywallId":"ultimate_upsell","requiredTier":"ultimate"}`, body)
	})
	t.Run("archived stays closed even with BYO", func(t *testing.T) {
		code, _ := g.probe(entitlement.Middleware(g.pool, entitlement.MemoryQuery, true, own), g.user("archived", "none"))
		require.Equal(t, http.StatusPaymentRequired, code)
	})
	t.Run("memoryQuery reaches everyone but archived, so the exemption never decides /v1/knowledge", func(t *testing.T) {
		for _, tier := range []string{"free", "lapsed", "trial", "pro", "ultimate"} {
			code, _ := g.probe(entitlement.Middleware(g.pool, entitlement.MemoryQuery, true, none), g.user(tier, "none"))
			require.Equal(t, http.StatusOK, code, tier)
		}
	})
	t.Run("a paying tier passes without consulting the credential store", func(t *testing.T) {
		consulted := false
		spy := func(context.Context, uuid.UUID) bool { consulted = true; return false }
		code, _ := g.probe(entitlement.Middleware(g.pool, entitlement.MemoryCompile, true, spy), g.user("pro", "none"))
		require.Equal(t, http.StatusOK, code)
		require.False(t, consulted)
	})
	t.Run("enforcement disabled still bypasses everything", func(t *testing.T) {
		code, _ := g.probe(entitlement.Middleware(g.pool, entitlement.Chat, false, nil), g.user("lapsed", "none"))
		require.Equal(t, http.StatusOK, code)
	})
}
