package ratelimit_test

import (
	"encoding/json"
	"lumina/internal/ratelimit"
	"lumina/internal/wire"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var policy = ratelimit.Policy{Name: "p", Max: 3, Window: 60 * time.Second}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newStore() (*ratelimit.Store, *clock) {
	c := &clock{t: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	s := ratelimit.NewStore()
	s.SetClock(c.now)
	return s, c
}

func TestFixedWindow(t *testing.T) {
	s, c := newStore()
	for range 3 {
		limited, _ := s.Hit(policy, "u:A")
		require.False(t, limited)
	}
	c.t = c.t.Add(20*time.Second + 500*time.Millisecond)
	limited, retry := s.Hit(policy, "u:A")
	require.True(t, limited)
	require.Equal(t, 39, retry, "Int(60 - 20.5) truncates")

	limited, _ = s.Hit(policy, "u:B")
	require.False(t, limited, "keys are independent")
	limited, _ = s.Hit(ratelimit.Policy{Name: "other", Max: 3, Window: time.Minute}, "u:A")
	require.False(t, limited, "policy names are separate namespaces")

	c.t = c.t.Add(39 * time.Second) // 59.5 s after the window opened
	limited, _ = s.Hit(policy, "u:A")
	require.True(t, limited, "still inside the window; rejected hits count too")

	c.t = c.t.Add(time.Second) // 60.5 s: a new window
	limited, _ = s.Hit(policy, "u:A")
	require.False(t, limited)
}

func TestRetryAfterFloorIsOne(t *testing.T) {
	s, c := newStore()
	for range 3 {
		s.Hit(policy, "k")
	}
	c.t = c.t.Add(59*time.Second + 900*time.Millisecond)
	_, retry := s.Hit(policy, "k")
	require.Equal(t, 1, retry)
}

func TestMiddlewareAndIPKey(t *testing.T) {
	s, _ := newStore()
	e := echo.New()
	e.HTTPErrorHandler = wire.HTTPErrorHandler
	var keys []string
	e.GET("/x", func(c *echo.Context) error { return c.NoContent(http.StatusNoContent) },
		ratelimit.Middleware(s, policy, func(c *echo.Context) string {
			k := ratelimit.UserOrIPKey(c)
			keys = append(keys, k)
			return k
		}))
	call := func(h map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}
	for range 3 {
		require.Equal(t, http.StatusNoContent, call(nil).Code)
	}
	rec := call(nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "60", rec.Header().Get("Retry-After"))
	require.Equal(t, wire.ContentTypeJSON, rec.Header().Get("Content-Type"))
	var body map[string]map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "rate limit exceeded", body["error"]["message"])

	call(map[string]string{"X-Forwarded-For": " 10.0.0.1 , 10.0.0.2"})
	call(map[string]string{"X-Forwarded-For": ",10.0.0.3"})
	call(map[string]string{"X-Forwarded-For": ","})
	call(map[string]string{"X-Real-IP": "10.0.0.4"})
	require.Equal(t, []string{"ip:anon", "ip:anon", "ip:anon", "ip:anon", "ip:10.0.0.1", "ip:10.0.0.3", "ip:anon", "ip:10.0.0.4"}, keys)
}

// The numbers follow RateLimitPolicy.effectiveMax with standardTierScale.
func TestEffectiveMax(t *testing.T) {
	for _, c := range []struct {
		tier              string
		query, capture    int
		kb, unscaledToday int
	}{
		{"ultimate", 120, 240, 20, 60},
		{"pro", 60, 120, 10, 60},
		{"trial", 30, 60, 5, 60},
		{"free", 10, 20, 2, 60},
		{"lapsed", 10, 20, 2, 60},
		{"archived", 3, 6, 1, 60}, // 5 * 0.1 rounds to 1
		{"not-a-tier", 10, 20, 2, 60},
	} {
		kb := ratelimit.Policy{Name: "kb", Max: 5, Window: time.Minute, TierScale: ratelimit.StandardTierScale}
		require.Equal(t, c.query, ratelimit.QueryByUser.EffectiveMax(c.tier), c.tier)
		require.Equal(t, c.capture, ratelimit.CaptureByUser.EffectiveMax(c.tier), c.tier)
		require.Equal(t, c.kb, kb.EffectiveMax(c.tier), c.tier)
		require.Equal(t, c.unscaledToday, ratelimit.MeTodayByUser.EffectiveMax(c.tier), c.tier)
	}
}

func TestPolicyNamesMatchSwift(t *testing.T) {
	require.Equal(t, "queryByUser", ratelimit.QueryByUser.Name)
	require.Equal(t, "captureByUser", ratelimit.CaptureByUser.Name)
	require.Equal(t, "meTodayByUser", ratelimit.MeTodayByUser.Name)
}

func TestTieredMiddleware(t *testing.T) {
	store := ratelimit.NewStore()
	tier, known := "archived", true
	mw := ratelimit.TieredMiddleware(store, ratelimit.QueryByUser,
		func(*echo.Context) string { return "k" },
		func(*echo.Context) (string, bool) { return tier, known })
	call := func() error {
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
		return mw(func(*echo.Context) error { return nil })(c)
	}
	for range 3 {
		require.NoError(t, call())
	}
	require.ErrorIs(t, call(), ratelimit.ErrLimited, "archived allows 3")

	// One bucket per caller: a tier change reads the same count (4 so far),
	// and no tier at all is the baseline 30.
	known = false
	for range 26 {
		require.NoError(t, call())
	}
	require.ErrorIs(t, call(), ratelimit.ErrLimited)
}
