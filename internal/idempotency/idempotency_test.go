package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/wire"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

// everyone says every token's user exists, so tests need no users rows:
// idempotency_keys has no foreign key to them.
type everyone struct{}

func (everyone) Exists(context.Context, uuid.UUID) (bool, error) { return true, nil }

// testModule mounts handlers behind an identity and the middleware, as Swift's
// test app does.
type testModule struct {
	mw     echo.MiddlewareFunc
	status int
	calls  int
	err    error
}

func (m *testModule) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	// Identify, not Middleware: as in Swift, only a request with a key needs
	// the identity at this point.
	identify := auth.Identify(keys, everyone{})
	r.POST("/idem", m.handle, identify, m.mw)
	r.POST("/idem/*", m.handle, identify, m.mw)
}

func (m *testModule) handle(c *echo.Context) error {
	m.calls++
	if m.err != nil {
		return m.err
	}
	if m.status >= 500 {
		return c.NoContent(m.status)
	}
	if m.status == http.StatusNoContent {
		return c.NoContent(http.StatusNoContent)
	}
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return err
	}
	c.Response().Header().Set("X-Handler", "live")
	// Echo back a counter so a test can tell which call produced the response.
	return c.Blob(m.status, "text/plain", fmt.Appendf(nil, "%d:%d", m.calls, len(body)))
}

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	m    *testModule
	e    http.Handler
}

func setup(t *testing.T, pool *pgxpool.Pool, opts Options) *env {
	t.Helper()
	m := &testModule{status: http.StatusOK, mw: New(pool, opts)}
	return &env{t: t, pool: pool, m: m, e: httpserver.New(config.Config{}, httpserver.Deps{Modules: []httpserver.Module{m}})}
}

func (v *env) post(path string, user uuid.UUID, key, body string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if key != "" {
		req.Header.Set(headerKey, key)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec
}

func TestIdempotency(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	count := func(t *testing.T, tenant uuid.UUID) int {
		t.Helper()
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1`, tenant).Scan(&n))
		return n
	}

	// Swift "replay returns cached response without re-executing handler".
	t.Run("replay", func(t *testing.T) {
		v, user, key := setup(t, pool, Options{}), uuid.New(), uuid.NewString()
		first := v.post("/idem", user, key, "payload")
		require.Equal(t, http.StatusOK, first.Code)
		require.Empty(t, first.Header().Get(headerReplayed))
		require.Equal(t, "live", first.Header().Get("X-Handler"))

		second := v.post("/idem", user, key, "payload")
		require.Equal(t, http.StatusOK, second.Code)
		require.Equal(t, first.Body.String(), second.Body.String())
		require.Equal(t, "true", second.Header().Get(headerReplayed))
		require.Equal(t, first.Header().Get("Content-Type"), second.Header().Get("Content-Type"))
		require.Empty(t, second.Header().Get("X-Handler"), "a replay has only its Content-Type and the marker")
		require.Equal(t, 1, v.m.calls)
	})

	// Swift "body change with same key returns 409".
	t.Run("body change", func(t *testing.T) {
		v, user, key := setup(t, pool, Options{}), uuid.New(), uuid.NewString()
		require.Equal(t, http.StatusOK, v.post("/idem", user, key, "a").Code)
		r := v.post("/idem", user, key, "b")
		require.Equal(t, http.StatusConflict, r.Code)
		require.Contains(t, r.Body.String(), "Idempotency-Key reused with different request")
		require.Equal(t, 1, v.m.calls)
	})

	// Swift "same key across different tenants are isolated".
	t.Run("tenants", func(t *testing.T) {
		v, key := setup(t, pool, Options{}), uuid.NewString()
		require.Equal(t, http.StatusOK, v.post("/idem", uuid.New(), key, "payload").Code)
		r := v.post("/idem", uuid.New(), key, "payload")
		require.Equal(t, http.StatusOK, r.Code)
		require.Empty(t, r.Header().Get(headerReplayed))
		require.Equal(t, 2, v.m.calls)
	})

	// Swift "missing idempotency key is a no op".
	t.Run("no key", func(t *testing.T) {
		v, user := setup(t, pool, Options{}), uuid.New()
		for range 3 {
			require.Equal(t, http.StatusOK, v.post("/idem", user, "", "x").Code)
		}
		require.Equal(t, 3, v.m.calls)
		require.Zero(t, count(t, user))
	})

	// Swift "5xx responses are not cached".
	t.Run("5xx", func(t *testing.T) {
		v, user, key := setup(t, pool, Options{}), uuid.New(), uuid.NewString()
		v.m.status = http.StatusInternalServerError
		for range 2 {
			require.Equal(t, http.StatusInternalServerError, v.post("/idem", user, key, "x").Code)
		}
		require.Equal(t, 2, v.m.calls)
		require.Zero(t, count(t, user))
	})

	// A key that isn't Swift's 36-character form is no key at all.
	t.Run("malformed keys bypass", func(t *testing.T) {
		v, user := setup(t, pool, Options{}), uuid.New()
		id := uuid.New()
		for _, key := range []string{"nope", strings.ReplaceAll(id.String(), "-", ""), "{" + id.String() + "}", "urn:uuid:" + id.String()} {
			require.Equal(t, http.StatusOK, v.post("/idem", user, key, "x").Code, key)
		}
		require.Equal(t, 4, v.m.calls)
		require.Zero(t, count(t, user))
	})

	t.Run("key is case-insensitive", func(t *testing.T) {
		v, user, key := setup(t, pool, Options{}), uuid.New(), uuid.NewString()
		require.Equal(t, http.StatusOK, v.post("/idem", user, strings.ToLower(key), "x").Code)
		require.Equal(t, "true", v.post("/idem", user, strings.ToUpper(key), "x").Header().Get(headerReplayed))
	})

	t.Run("no identity is a 401 only with a key", func(t *testing.T) {
		v := setup(t, pool, Options{})
		require.Equal(t, http.StatusOK, v.post("/idem", uuid.Nil, "", "x").Code)
		require.Equal(t, http.StatusUnauthorized, v.post("/idem", uuid.Nil, uuid.NewString(), "x").Code)
		require.Equal(t, 1, v.m.calls)
	})

	// Returned responses are cached through 4xx, except 408.
	t.Run("status rules", func(t *testing.T) {
		for status, cached := range map[int]bool{
			http.StatusCreated: true, http.StatusBadRequest: true, http.StatusNotFound: true,
			http.StatusNoContent: true, http.StatusRequestTimeout: false,
		} {
			v, user, key := setup(t, pool, Options{}), uuid.New(), uuid.NewString()
			v.m.status = status
			require.Equal(t, status, v.post("/idem", user, key, "x").Code)
			r := v.post("/idem", user, key, "x")
			require.Equal(t, status, r.Code)
			if cached {
				require.Equal(t, 1, v.m.calls, strconv.Itoa(status))
				require.Equal(t, "true", r.Header().Get(headerReplayed))
			} else {
				require.Equal(t, 2, v.m.calls, strconv.Itoa(status))
			}
		}
	})

	// A 204 stores an empty body, with no Content-Type, and replays the same.
	t.Run("empty body", func(t *testing.T) {
		v, user, key := setup(t, pool, Options{}), uuid.New(), uuid.NewString()
		v.m.status = http.StatusNoContent
		require.Equal(t, http.StatusNoContent, v.post("/idem", user, key, "").Code)
		r := v.post("/idem", user, key, "")
		require.Equal(t, http.StatusNoContent, r.Code)
		require.Empty(t, r.Body.String())
		var ct *string
		require.NoError(t, pool.QueryRow(ctx, `SELECT response_content_type FROM idempotency_keys WHERE tenant_id = $1`, user).Scan(&ct))
		require.Nil(t, ct)
	})

	// An error the handler returns isn't a response: Swift's thrown HTTPError
	// skips persistence, whatever its status.
	t.Run("returned errors are not stored", func(t *testing.T) {
		v, user, key := setup(t, pool, Options{}), uuid.New(), uuid.NewString()
		v.m.err = wire.NewError(http.StatusNotFound, "nope")
		for range 2 {
			require.Equal(t, http.StatusNotFound, v.post("/idem", user, key, "x").Code)
		}
		require.Equal(t, 2, v.m.calls)
		require.Zero(t, count(t, user))
	})

	t.Run("expired row runs again and is replaced", func(t *testing.T) {
		now := time.Now()
		v, user, key := setup(t, pool, Options{Now: func() time.Time { return now }}), uuid.New(), uuid.NewString()
		require.Equal(t, http.StatusOK, v.post("/idem", user, key, "a").Code)
		now = now.Add(25 * time.Hour)
		r := v.post("/idem", user, key, "different body is fine once expired")
		require.Equal(t, http.StatusOK, r.Code)
		require.Empty(t, r.Header().Get(headerReplayed))
		require.Equal(t, 2, v.m.calls)
		require.Equal(t, 1, count(t, user))
		require.Equal(t, "true", v.post("/idem", user, key, "different body is fine once expired").Header().Get(headerReplayed))
	})

	t.Run("expiry is 24 hours", func(t *testing.T) {
		v, user := setup(t, pool, Options{}), uuid.New()
		require.Equal(t, http.StatusOK, v.post("/idem", user, uuid.NewString(), "a").Code)
		var ttl time.Duration
		require.NoError(t, pool.QueryRow(ctx, `SELECT expires_at - created_at FROM idempotency_keys WHERE tenant_id = $1`, user).Scan(&ttl))
		require.Equal(t, 24*time.Hour, ttl)
	})

	// Too large to hash: the handler runs on every byte, and nothing is stored.
	t.Run("oversized request bypasses with its whole body", func(t *testing.T) {
		v, user, key := setup(t, pool, Options{MaxRequestBody: 4}), uuid.New(), uuid.NewString()
		r := v.post("/idem", user, key, "123456789")
		require.Equal(t, http.StatusOK, r.Code)
		require.Equal(t, "1:9", r.Body.String())
		require.Zero(t, count(t, user))
	})

	// Forwarded but not stored.
	t.Run("oversized response", func(t *testing.T) {
		v, user, key := setup(t, pool, Options{MaxResponseBody: 2}), uuid.New(), uuid.NewString()
		r := v.post("/idem", user, key, "x")
		require.Equal(t, "1:1", r.Body.String())
		require.Zero(t, count(t, user))
	})

	// The first execution's row, as both servers read it.
	t.Run("stored row", func(t *testing.T) {
		v, user, key := setup(t, pool, Options{}), uuid.New(), uuid.NewString()
		require.Equal(t, http.StatusOK, v.post("/idem/a%20b?x=1", user, key, "payload").Code)
		var (
			hash, ct string
			status   int
			body     []byte
		)
		require.NoError(t, pool.QueryRow(ctx, `SELECT request_hash, response_status, response_content_type, response_body
			FROM idempotency_keys WHERE tenant_id = $1 AND key = $2`, user, key).Scan(&hash, &status, &ct, &body))
		sum := sha256.Sum256([]byte("payloadPOST/idem/a%20b"))
		require.Equal(t, hex.EncodeToString(sum[:]), hash, "body, method and raw path, without the query")
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "text/plain", ct)
		require.Equal(t, "1:7", string(body))
	})
}

// The golden value pins Swift's format: SHA-256 over body + method + path with
// no separators, lowercase hex. Computed independently with
// `printf 'payloadPOST/v1/vault/create' | shasum -a 256`.
func TestHashRequestFormat(t *testing.T) {
	require.Equal(t, "23eb235f26dcc3a6ae514cb065c83955e22d4b786940f84f1f85dcc58f32f26d", hashRequest([]byte("payload"), "POST", "/v1/vault/create"))
}
