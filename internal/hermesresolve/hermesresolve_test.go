package hermesresolve

import (
	"context"
	"encoding/base64"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/secretbox"
	"lumina/internal/ssrfguard"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

type mod struct {
	reg func(httpserver.Router)
}

func (m mod) Register(r httpserver.Router, _ echo.MiddlewareFunc) { m.reg(r) }

func stubResolver(answers map[string][]string) ssrfguard.Resolver {
	return func(_ context.Context, host string) ([]string, error) { return answers[host], nil }
}

// TestResolver ports HermesEndpointResolverTests (4 cases, each named below).
func TestResolver(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i)
	}
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(master))
	require.NoError(t, err)
	defaultURL, _ := url.Parse("http://default.hermes.test")
	answers := map[string][]string{"user.hermes.test": {"93.184.216.34"}, "evil.test": {"10.0.0.1"}}
	r := New(pool, box, ssrfguard.Guard{Resolver: stubResolver(answers)}, defaultURL)

	newTenant := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.New()
		_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
			VALUES ($1, $2, 'x', $3, now(), now())`, id, id.String()+"@t", id.String())
		require.NoError(t, err)
		return id
	}
	row := func(t *testing.T, tenant uuid.UUID, base string, ct, nonce []byte) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO user_hermes_config (id, tenant_id, base_url, auth_header_ciphertext, auth_header_nonce)
			VALUES ($1, $2, $3, $4, $5)`, uuid.New(), tenant, base, ct, nonce)
		require.NoError(t, err)
	}
	failure := func(t *testing.T, tenant uuid.UUID) *ResolutionError {
		t.Helper()
		_, err := r.Resolve(ctx, tenant)
		var f *ResolutionError
		require.ErrorAs(t, err, &f)
		return f
	}

	// Swift "returns managed default when no row exists".
	t.Run("default", func(t *testing.T) {
		res, err := r.Resolve(ctx, uuid.New())
		require.NoError(t, err)
		require.False(t, res.IsUserOverride)
		require.Equal(t, defaultURL, res.BaseURL)
		require.Nil(t, res.AuthHeader)
	})

	// Swift "returns override when row exists and decrypts auth header".
	t.Run("override", func(t *testing.T) {
		id := newTenant(t)
		sealed, err := box.Seal("Bearer abc-123", id)
		require.NoError(t, err)
		row(t, id, "https://user.hermes.test", sealed.Ciphertext, sealed.Nonce)
		res, err := r.Resolve(ctx, id)
		require.NoError(t, err)
		require.True(t, res.IsUserOverride)
		require.Equal(t, "https://user.hermes.test", res.BaseURL.String())
		require.Equal(t, "Bearer abc-123", *res.AuthHeader)
	})

	t.Run("override without auth header", func(t *testing.T) {
		id := newTenant(t)
		row(t, id, "https://user.hermes.test", nil, nil)
		res, err := r.Resolve(ctx, id)
		require.NoError(t, err)
		require.True(t, res.IsUserOverride)
		require.Nil(t, res.AuthHeader)
	})

	// A cron-dashboard-only row must not break routing.
	t.Run("blank base url is no override", func(t *testing.T) {
		id := newTenant(t)
		row(t, id, " \t ", nil, nil)
		res, err := r.Resolve(ctx, id)
		require.NoError(t, err)
		require.False(t, res.IsUserOverride)
		require.Equal(t, defaultURL, res.BaseURL)
	})

	// Swift "throws ssrfRejected when stored URL fails revalidation".
	t.Run("ssrf", func(t *testing.T) {
		id := newTenant(t)
		row(t, id, "https://evil.test", nil, nil)
		f := failure(t, id)
		require.Equal(t, SSRFRejected, f.Kind)
		require.Equal(t, `privateAddress("10.0.0.1")`, f.Reason)
	})

	// Swift "throws decryptFailed when ciphertext is corrupt".
	t.Run("corrupt ciphertext", func(t *testing.T) {
		id := newTenant(t)
		sealed, err := box.Seal("Bearer abc", id)
		require.NoError(t, err)
		sealed.Ciphertext[0] ^= 0xFF
		row(t, id, "https://user.hermes.test", sealed.Ciphertext, sealed.Nonce)
		require.Equal(t, DecryptFailed, failure(t, id).Kind)
	})

	t.Run("half an auth header is malformed", func(t *testing.T) {
		id := newTenant(t)
		row(t, id, "https://user.hermes.test", []byte{1}, nil)
		require.Equal(t, MalformedRow, failure(t, id).Kind)
	})

	t.Run("middleware", func(t *testing.T) {
		var got *Resolution
		e := httpserver.New(config.Config{}, httpserver.Deps{Modules: []httpserver.Module{mod{func(rt httpserver.Router) {
			rt.POST("/x", func(c *echo.Context) error {
				got = nil
				if res, ok := FromContext(c); ok {
					got = &res
				}
				return c.NoContent(http.StatusNoContent)
			}, auth.Identify(keys, auth.PGUsers(pool)), r.Middleware())
		}}}})
		do := func(t *testing.T, tenant uuid.UUID) *httptest.ResponseRecorder {
			t.Helper()
			req := httptest.NewRequest(http.MethodPost, "/x", nil)
			if tenant != uuid.Nil {
				tok, err := scenario.Mint(keys, tenant)
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			return rec
		}

		// No identity: skip resolution, the route answers for itself.
		got = &Resolution{}
		require.Equal(t, http.StatusNoContent, do(t, uuid.Nil).Code)
		require.Nil(t, got)

		id := newTenant(t)
		require.Equal(t, http.StatusNoContent, do(t, id).Code)
		require.NotNil(t, got)
		require.False(t, got.IsUserOverride)

		bad := newTenant(t)
		row(t, bad, "https://evil.test", nil, nil)
		rec := do(t, bad)
		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		require.Equal(t, `{"error":{"code":"hermes_unreachable","message":"Your Hermes address could not be used (privateAddress(\"10.0.0.1\")). Check the URL in Settings — a hostname that only resolves on your own network will not resolve from here; the tailnet IP does."}}`, rec.Body.String())

		corrupt := newTenant(t)
		sealed, err := box.Seal("Bearer abc", corrupt)
		require.NoError(t, err)
		sealed.Ciphertext[0] ^= 0xFF
		row(t, corrupt, "https://user.hermes.test", sealed.Ciphertext, sealed.Nonce)
		require.Equal(t, `{"error":{"code":"hermes_unreachable","message":"Your saved Hermes credentials could not be read. Re-enter the auth header in Settings."}}`, do(t, corrupt).Body.String())

		half := newTenant(t)
		row(t, half, "https://user.hermes.test", nil, []byte{1})
		require.Equal(t, `{"error":{"code":"hermes_unreachable","message":"Your Hermes configuration is incomplete. Re-save it in Settings."}}`, do(t, half).Body.String())
	})

	// Swift installs no middleware without a usable master key.
	t.Run("nil resolver passes through", func(t *testing.T) {
		var none *Resolver
		called := false
		h := none.Middleware()(func(*echo.Context) error { called = true; return nil })
		require.NoError(t, h(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())))
		require.True(t, called)
	})
}
