package news_test

import (
	"context"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/news"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var (
	alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
	keys  = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
	// The parity env's key: base64 of 32 bytes of 0x01 (never a real key).
	masterKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
)

const feedBody = `{"items":[{"id":"a","url":"https://bbc.example/a","title":"Ceasefire talks resume","date_published":"2026-09-17T11:00:00.700Z","_feeds":{"source_name":"BBC News","feed_url":"https://feeds.bbci.co.uk/news/rss.xml"}}],"_feeds":{"warming":[],"stale":[]}}`

func dbSetup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "news.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	// alice: enabled, empty (unopenable) config; bob: disabled.
	_, err := pool.Exec(ctx, `INSERT INTO plugin_installs (id, tenant_id, plugin_slug, status, config_ciphertext, config_nonce)
		VALUES (gen_random_uuid(), $1, 'news-ticker', 'enabled', '', ''), (gen_random_uuid(), $2, 'news-ticker', 'disabled', '', '')`, alice, bob)
	require.NoError(t, err)
	return pool
}

func newsModule(pool *pgxpool.Pool, cfg config.Config) *news.Module {
	return news.New(pool, cfg, auth.Identify(keys, auth.PGUsers(pool)), ratelimit.NewStore())
}

func server(pool *pgxpool.Pool, m *news.Module) *echo.Echo {
	mods := []httpserver.Module{}
	if m != nil {
		mods = append(mods, m)
	}
	return httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: mods})
}

type resp struct {
	code int
	ct   string
	body string
}

func get(t *testing.T, e *echo.Echo, user uuid.UUID, path string) resp {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.String()}
}

func stubAggregator(t *testing.T, calls *atomic.Int32, lastFeeds *atomic.Value) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		lastFeeds.Store(r.URL.Query().Get("feeds"))
		_, _ = w.Write([]byte(feedBody))
	}))
	t.Cleanup(s.Close)
	return s.URL
}

// One container for every DB-backed case (each dbtest.Postgres is a fresh one).
func TestRoutes(t *testing.T) {
	pool := dbSetup(t)
	ctx := context.Background()

	t.Run("no master key: route not mounted, like Swift", func(t *testing.T) {
		require.Nil(t, newsModule(pool, config.Config{}))
		require.Nil(t, newsModule(pool, config.Config{LVSecretMasterKey: "not-base64"}))
		e := server(pool, nil)
		r := get(t, e, alice, "/v1/news/ticker")
		require.Equal(t, http.StatusNotFound, r.code)
		require.Empty(t, r.body)
	})

	t.Run("parity env: 401 without a token, 503 for everyone with one", func(t *testing.T) {
		m := newsModule(pool, config.Config{LVSecretMasterKey: masterKey})
		require.NotNil(t, m)
		e := server(pool, m)
		r := get(t, e, uuid.Nil, "/v1/news/ticker")
		require.Equal(t, http.StatusUnauthorized, r.code)
		require.JSONEq(t, `{"error":{"message":"Authenticated identity is unavailable"}}`, r.body)
		for _, u := range []uuid.UUID{alice, bob} {
			for _, q := range []string{"", "?limit=0", "?limit=abc"} {
				r := get(t, e, u, "/v1/news/ticker"+q)
				require.Equal(t, http.StatusServiceUnavailable, r.code)
				require.Equal(t, "application/json; charset=utf-8", r.ct)
				require.JSONEq(t, `{"error":{"message":"news_ticker_not_configured"}}`, r.body)
			}
		}
	})

	var calls atomic.Int32
	var lastFeeds atomic.Value
	base := stubAggregator(t, &calls, &lastFeeds)
	cfg := config.Config{LVSecretMasterKey: masterKey, FeedsBaseURL: base, FeedsDefaultFeeds: "https://curated.example/rss"}
	e := server(pool, newsModule(pool, cfg))

	t.Run("configured: install state from plugin_installs", func(t *testing.T) {
		r := get(t, e, bob, "/v1/news/ticker")
		require.Equal(t, http.StatusConflict, r.code)
		require.JSONEq(t, `{"error":{"message":"plugin_disabled"}}`, r.body)

		_, err := pool.Exec(ctx, `DELETE FROM plugin_installs WHERE tenant_id = $1`, bob)
		require.NoError(t, err)
		r = get(t, e, bob, "/v1/news/ticker")
		require.Equal(t, http.StatusNotFound, r.code)
		require.JSONEq(t, `{"error":{"message":"plugin_not_installed"}}`, r.body)

		// alice: enabled, unopenable config → curated feeds only.
		r = get(t, e, alice, "/v1/news/ticker")
		require.Equal(t, http.StatusOK, r.code, r.body)
		require.Equal(t, "application/json; charset=utf-8", r.ct)
		require.Equal(t, "https://curated.example/rss", lastFeeds.Load())
		require.Contains(t, r.body, `"publishedAt":"2026-09-17T11:00:00Z"`, "whole seconds")
		require.NotContains(t, r.body, "sourceUrl", "nil optionals omitted")
		require.Contains(t, r.body, `"stale":false`)
	})

	t.Run("configured: sealed feed_urls are appended, tenant-bound", func(t *testing.T) {
		box, err := secretbox.New(masterKey)
		require.NoError(t, err)
		sealed, err := box.Seal(`{"feed_urls":"https://mine.example/rss, http://192.168.0.2/x"}`, alice)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `UPDATE plugin_installs SET config_ciphertext = $2, config_nonce = $3 WHERE tenant_id = $1`,
			alice, sealed.Ciphertext, sealed.Nonce)
		require.NoError(t, err)
		r := get(t, e, alice, "/v1/news/ticker?limit=7")
		require.Equal(t, http.StatusOK, r.code, r.body)
		require.Equal(t, "https://curated.example/rss,https://mine.example/rss", lastFeeds.Load())

		// The same blob installed for bob does not open (key is per tenant).
		_, err = pool.Exec(ctx, `INSERT INTO plugin_installs (id, tenant_id, plugin_slug, status, config_ciphertext, config_nonce)
			VALUES ($1, $2, 'news-ticker', 'enabled', $3, $4)`, uuid.New(), bob, sealed.Ciphertext, sealed.Nonce)
		require.NoError(t, err)
		r = get(t, e, bob, "/v1/news/ticker?limit=7")
		require.Equal(t, http.StatusOK, r.code, r.body)
		require.Equal(t, "https://curated.example/rss", lastFeeds.Load())
	})

	t.Run("settingsByUser: 30 per minute per user, unauthenticated by IP, 429 before 401", func(t *testing.T) {
		limits := ratelimit.NewStore()
		e := server(pool, news.New(pool, cfg, auth.Identify(keys, auth.PGUsers(pool)), limits))
		for i := range 30 {
			require.Equal(t, http.StatusOK, get(t, e, alice, "/v1/news/ticker").code, i)
		}
		r := get(t, e, alice, "/v1/news/ticker")
		require.Equal(t, http.StatusTooManyRequests, r.code)
		require.JSONEq(t, `{"error":{"message":"rate limit exceeded"}}`, r.body)
		require.Equal(t, http.StatusOK, get(t, e, bob, "/v1/news/ticker").code, "bob's bucket is his own")

		// The same store already counted for another settingsByUser module:
		// news shares it (Swift buckets are per policy name).
		shared := ratelimit.NewStore()
		for range 30 {
			shared.Hit(ratelimit.SettingsByUser, "ip:anon")
		}
		e = server(pool, news.New(pool, cfg, auth.Identify(keys, auth.PGUsers(pool)), shared))
		r = get(t, e, uuid.Nil, "/v1/news/ticker")
		require.Equal(t, http.StatusTooManyRequests, r.code, "no token still counts against ip:anon, so 429 wins over 401")
	})

	t.Run("200 conforms to api/openapi.yaml", func(t *testing.T) {
		doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
		require.NoError(t, err)
		doc.Servers = nil
		router, err := gorillamux.NewRouter(doc)
		require.NoError(t, err)
		for _, p := range []string{"/v1/news/ticker", "/v1/news/ticker?limit=3"} {
			r := get(t, e, alice, p)
			require.Equal(t, http.StatusOK, r.code)
			req := httptest.NewRequest(http.MethodGet, p, nil)
			route, params, err := router.FindRoute(req)
			require.NoError(t, err)
			out := &openapi3filter.ResponseValidationInput{
				RequestValidationInput: &openapi3filter.RequestValidationInput{
					Request: req, PathParams: params, Route: route,
					Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
				},
				Status: r.code, Header: http.Header{"Content-Type": {r.ct}},
			}
			out.SetBodyBytes([]byte(r.body))
			require.NoError(t, openapi3filter.ValidateResponse(context.Background(), out), strings.TrimSpace(r.body))
		}
	})
}
