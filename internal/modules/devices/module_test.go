package devices_test

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/devices"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

const configured = "com.lumina.fernando"

var (
	alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
	keys  = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
}

func setup(t *testing.T, bundleID string) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	seed, err := os.ReadFile("../../../parity/seed/base.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(seed))
	require.NoError(t, err)
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{devices.New(pool, bundleID)},
	})
	return env{t: t, e: e, pool: pool}
}

type resp struct {
	code int
	ct   string
	body []byte
}

func (v env) do(user uuid.UUID, method, path, body string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()}
}

func (v env) register(user uuid.UUID, token string, bundleID *string) int {
	v.t.Helper()
	body := fmt.Sprintf(`{"token":%q,"platform":"ios"}`, token)
	if bundleID != nil {
		body = fmt.Sprintf(`{"token":%q,"platform":"ios","bundleId":%q}`, token, *bundleID)
	}
	return v.do(user, http.MethodPost, "/v1/devices", body).code
}

func (v env) storedBundleID(token string) *string {
	v.t.Helper()
	var out *string
	require.NoError(v.t, v.pool.QueryRow(context.Background(), `SELECT bundle_id FROM device_tokens WHERE token = $1`, token).Scan(&out))
	return out
}

func str(s string) *string { return &s }

// Swift DeviceBundleIDTests "stores an allowed bundle id and drops a foreign one".
func TestStoresAllowedBundleIDAndDropsForeign(t *testing.T) {
	v := setup(t, configured)
	beta, prod, foreign, legacy := "beta-"+uuid.NewString(), "prod-"+uuid.NewString(), "foreign-"+uuid.NewString(), "legacy-"+uuid.NewString()

	require.Equal(t, http.StatusOK, v.register(alice, beta, str("com.lumina.fernando.beta")))
	require.Equal(t, http.StatusOK, v.register(alice, prod, str(configured)))
	require.Equal(t, http.StatusOK, v.register(alice, foreign, str("com.lumina.fernandox")))
	require.Equal(t, http.StatusOK, v.register(alice, legacy, nil))

	require.Equal(t, "com.lumina.fernando.beta", *v.storedBundleID(beta))
	require.Equal(t, configured, *v.storedBundleID(prod))
	require.Nil(t, v.storedBundleID(foreign))
	require.Nil(t, v.storedBundleID(legacy))

	// An older client re-registering without a bundle id keeps the stored one.
	require.Equal(t, http.StatusOK, v.register(alice, beta, nil))
	require.Equal(t, "com.lumina.fernando.beta", *v.storedBundleID(beta))
	// A newer client sending a foreign id clears it.
	require.Equal(t, http.StatusOK, v.register(alice, beta, str("com.other.app")))
	require.Nil(t, v.storedBundleID(beta))
}

// Swift DeviceBundleIDTests "allowed bundle ids are the configured one and its variants".
func TestAllowedBundleIDsAreConfiguredAndVariants(t *testing.T) {
	require.True(t, devices.IsAllowedBundleID("com.lumina.fernando", configured))
	require.True(t, devices.IsAllowedBundleID("com.lumina.fernando.beta", configured))
	require.True(t, devices.IsAllowedBundleID("com.lumina.fernando.test", configured))
	require.False(t, devices.IsAllowedBundleID("com.lumina.fernandox", configured))
	require.False(t, devices.IsAllowedBundleID("com.other.app", configured))
	require.False(t, devices.IsAllowedBundleID("com.lumina.fernando.beta", ""))
	require.Equal(t, configured, devices.Topic(nil, configured))
	require.Equal(t, configured, devices.Topic(str("com.other.app"), configured))
	require.Equal(t, "com.lumina.fernando.beta", devices.Topic(str("com.lumina.fernando.beta"), configured))
}

func TestRegisterUpdatesExistingAndUnregisterIsTenantScoped(t *testing.T) {
	v := setup(t, "")
	r := v.do(alice, http.MethodPost, "/v1/devices", `{"token":"tok-1","platform":"ios"}`)
	require.Equal(t, http.StatusOK, r.code)
	require.Equal(t, "application/json; charset=utf-8", r.ct)
	var first struct{ ID, Token, Platform string }
	require.NoError(t, json.Unmarshal(r.body, &first))
	require.Equal(t, strings.ToUpper(first.ID), first.ID)

	r = v.do(alice, http.MethodPost, "/v1/devices", `{"token":"tok-1","platform":"android"}`)
	var second struct{ ID, Token, Platform string }
	require.NoError(t, json.Unmarshal(r.body, &second))
	require.Equal(t, first.ID, second.ID, "same row")
	require.Equal(t, "android", second.Platform)

	// device_tokens.token is globally unique: bob cannot take alice's token.
	r = v.do(bob, http.MethodPost, "/v1/devices", `{"token":"tok-1","platform":"ios"}`)
	require.Equal(t, http.StatusInternalServerError, r.code)
	require.Empty(t, r.body)

	r = v.do(bob, http.MethodDelete, "/v1/devices/tok-1", "")
	require.Equal(t, http.StatusNoContent, r.code)
	var n int
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM device_tokens`).Scan(&n))
	require.Equal(t, 1, n, "bob's unregister did not touch alice's row")

	r = v.do(alice, http.MethodDelete, "/v1/devices/tok-1", "")
	require.Equal(t, http.StatusNoContent, r.code)
	require.Empty(t, r.body)
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM device_tokens`).Scan(&n))
	require.Zero(t, n)
}

func TestRegisterErrorsMatchSwift(t *testing.T) {
	v := setup(t, "")
	cases := []struct{ name, body, message string }{
		{"empty token", `{"token":"","platform":"ios"}`, "token required"},
		{"unknown platform", `{"token":"x","platform":"windows"}`, "The given data was not valid input: Cannot initialize DevicePlatform from invalid String value windows"},
		{"missing platform", `{"token":"x"}`, "Coding key `platform` not found."},
		{"platform wrong type", `{"token":"x","platform":1}`, "Type mismatch for `platform` key, expected `String` type."},
	}
	for _, tc := range cases {
		r := v.do(alice, http.MethodPost, "/v1/devices", tc.body)
		require.Equal(t, http.StatusBadRequest, r.code, tc.name)
		require.JSONEq(t, `{"error":{"message":`+mustJSON(tc.message)+`}}`, string(r.body), tc.name)
	}
	require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodPost, "/v1/devices", `{"token":"x","platform":"ios"}`).code)
}

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t, "")
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/devices", `{"token":"spec","platform":"ios"}`},
		{http.MethodDelete, "/v1/devices/spec", ""},
	} {
		r := v.do(alice, c.method, c.path, c.body)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: r.code, Header: http.Header{"Content-Type": {r.ct}}}
		out.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, r.body)
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
