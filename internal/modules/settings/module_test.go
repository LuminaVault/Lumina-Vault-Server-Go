package settings_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/hermescaps"
	"lumina/internal/hermesresolve"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/settings"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/ssrfguard"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

var keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	srv  http.Handler
}

// setup mirrors dbTestReader: private ranges allowed, plain http accepted.
func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i)
	}
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(master))
	require.NoError(t, err)
	guard := ssrfguard.Guard{AllowPrivateRanges: true}
	defaultURL, _ := url.Parse("http://default.hermes.test")
	resolver := hermesresolve.New(pool, box, guard, defaultURL)
	module := settings.New(pool, settings.Options{
		Identify: auth.Identify(keys, auth.PGUsers(pool)), Limits: ratelimit.NewStore(),
		Box: box, Guard: guard, Capabilities: hermescaps.New(pool, resolver, box, guard),
	})
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: []httpserver.Module{module}})
	return env{t: t, pool: pool, srv: e}
}

func (v env) user() uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
		VALUES ($1, $2, 'x', $3, now(), now())`, id, id.String()+"@t", "u"+strings.ReplaceAll(id.String(), "-", "")[:12])
	require.NoError(v.t, err)
	return id
}

type resp struct {
	code int
	body []byte
}

func (v env) do(user uuid.UUID, method, path, body string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	tok, err := scenario.Mint(keys, user)
	require.NoError(v.t, err)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	v.srv.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Body.Bytes()}
}

type getResponse struct {
	BaseURL       string  `json:"baseUrl"`
	HasAuthHeader bool    `json:"hasAuthHeader"`
	VerifiedAt    *string `json:"verifiedAt"`
	Name          *string `json:"name"`
}

func decode(t *testing.T, r resp) getResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	var out getResponse
	require.NoError(t, json.Unmarshal(r.body, &out))
	return out
}

// Ported from HermesConfigControllerTests: GET 404 when no row exists.
func TestGet404WhenNoRow(t *testing.T) {
	v := setup(t)
	r := v.do(v.user(), http.MethodGet, "/v1/settings/hermes", "")
	require.Equal(t, http.StatusNotFound, r.code)
}

// Ported from HermesConfigControllerTests: PUT then GET round-trips and reports hasAuthHeader.
func TestPutThenGetRoundTrips(t *testing.T) {
	v := setup(t)
	u := v.user()
	put := decode(t, v.do(u, http.MethodPut, "/v1/settings/hermes", `{"baseUrl":"http://127.0.0.1:9999","authHeader":"Bearer abc"}`))
	require.Equal(t, getResponse{BaseURL: "http://127.0.0.1:9999", HasAuthHeader: true}, put)
	got := decode(t, v.do(u, http.MethodGet, "/v1/settings/hermes", ""))
	require.Equal(t, put, got)
}

// Ported from HermesConfigControllerTests: GET response never echoes plaintext auth header.
func TestGetNeverEchoesAuthHeader(t *testing.T) {
	v := setup(t)
	u := v.user()
	v.do(u, http.MethodPut, "/v1/settings/hermes", `{"baseUrl":"http://127.0.0.1:9999","authHeader":"Bearer SECRET-TOKEN-XYZ"}`)
	r := v.do(u, http.MethodGet, "/v1/settings/hermes", "")
	require.NotContains(t, string(r.body), "SECRET-TOKEN-XYZ")
	require.NotContains(t, string(r.body), "authHeader")
	var ct []byte
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT auth_header_ciphertext FROM user_hermes_config WHERE tenant_id = $1`, u).Scan(&ct))
	require.NotContains(t, string(ct), "SECRET-TOKEN-XYZ")
}

// Ported from HermesConfigControllerTests: PUT without authHeader reports hasAuthHeader false.
func TestPutWithoutAuthHeader(t *testing.T) {
	v := setup(t)
	put := decode(t, v.do(v.user(), http.MethodPut, "/v1/settings/hermes", `{"baseUrl":"http://127.0.0.1:9999"}`))
	require.False(t, put.HasAuthHeader)
}

// Ported from HermesConfigControllerTests: PUT rejects loopback when allowPrivate disabled by SSRFGuard.
func TestGuardRejectsLoopbackWhenPrivateDisabled(t *testing.T) {
	g := ssrfguard.Guard{RequireHTTPS: true, Resolver: func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }}
	_, err := g.Validate(context.Background(), "https://127.0.0.1")
	var rej *ssrfguard.Rejection
	require.ErrorAs(t, err, &rej)
}

// Ported from HermesConfigControllerTests: DELETE drops the row and GET returns 404.
func TestDeleteDropsRow(t *testing.T) {
	v := setup(t)
	u := v.user()
	v.do(u, http.MethodPut, "/v1/settings/hermes", `{"baseUrl":"http://127.0.0.1:9999"}`)
	require.Equal(t, http.StatusNoContent, v.do(u, http.MethodDelete, "/v1/settings/hermes", "").code)
	require.Equal(t, http.StatusNotFound, v.do(u, http.MethodGet, "/v1/settings/hermes", "").code)
}

// Ported from HermesConfigControllerTests: PUT resets verifiedAt.
func TestPutResetsVerifiedAt(t *testing.T) {
	v := setup(t)
	u := v.user()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	v.do(u, http.MethodPut, "/v1/settings/hermes", `{"baseUrl":"`+upstream.URL+`"}`)
	test := v.do(u, http.MethodPost, "/v1/settings/hermes/test", "")
	require.Equal(t, http.StatusOK, test.code, string(test.body))
	require.Contains(t, string(test.body), "verifiedAt")
	require.NotNil(t, decode(t, v.do(u, http.MethodGet, "/v1/settings/hermes", "")).VerifiedAt)

	put := decode(t, v.do(u, http.MethodPut, "/v1/settings/hermes", `{"baseUrl":"http://127.0.0.1:8888","authHeader":"Bearer new"}`))
	require.Equal(t, "http://127.0.0.1:8888", put.BaseURL)
	require.True(t, put.HasAuthHeader)
	require.Nil(t, put.VerifiedAt)
}

// Ported from HermesConfigControllerTests: tenant A cannot read tenant B config.
func TestTenantIsolation(t *testing.T) {
	v := setup(t)
	a, b := v.user(), v.user()
	v.do(a, http.MethodPut, "/v1/settings/hermes", `{"baseUrl":"http://127.0.0.1:9999"}`)
	require.Equal(t, http.StatusNotFound, v.do(b, http.MethodGet, "/v1/settings/hermes", "").code)
}

func TestPutRejectsInvalidBaseURL(t *testing.T) {
	v := setup(t)
	r := v.do(v.user(), http.MethodPut, "/v1/settings/hermes", `{"baseUrl":"ftp://example.com"}`)
	require.Equal(t, http.StatusBadRequest, r.code, string(r.body))
	require.Contains(t, string(r.body), "invalid_base_url:")
}

func TestTestUnreachableIs502(t *testing.T) {
	v := setup(t)
	u := v.user()
	v.do(u, http.MethodPut, "/v1/settings/hermes", `{"baseUrl":"http://127.0.0.1:1"}`)
	r := v.do(u, http.MethodPost, "/v1/settings/hermes/test", "")
	require.Equal(t, http.StatusBadGateway, r.code, string(r.body))
	require.Contains(t, string(r.body), "unreachable")
}

func TestCapabilitiesManagedDefaultAndOverrideProbe(t *testing.T) {
	v := setup(t)
	u := v.user()
	var managed hermescaps.Response
	r := v.do(u, http.MethodGet, "/v1/me/hermes/capabilities", "")
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	require.NoError(t, json.Unmarshal(r.body, &managed))
	require.Equal(t, hermescaps.ManagedDefault(), managed.Capabilities)
	require.Nil(t, managed.CheckedAt)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			_, _ = w.Write([]byte(`{"features":{"session_resources":true,"skills_api":true}}`))
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	v.do(u, http.MethodPut, "/v1/settings/hermes", `{"baseUrl":"`+upstream.URL+`"}`)
	var over hermescaps.Response
	r = v.do(u, http.MethodGet, "/v1/me/hermes/capabilities?refresh=true", "")
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	require.NoError(t, json.Unmarshal(r.body, &over))
	require.True(t, over.Capabilities.IsUserOverride)
	require.Equal(t, hermescaps.Live, over.Capabilities.Sessions)
	require.Equal(t, hermescaps.ReadOnly, over.Capabilities.Skills, "Swift: skills_api reads read_only")
	require.NotNil(t, over.CheckedAt)
}
