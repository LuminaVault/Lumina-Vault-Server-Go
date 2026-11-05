package kb_test

import (
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/kb"
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

var (
	amy  = uuid.MustParse("6B0B0000-0000-4000-8000-0000000000A1")
	ben  = uuid.MustParse("6B0B0000-0000-4000-8000-0000000000A2")
	keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

const path = "/v1/memory-compile/pending"

func setup(t *testing.T) (*echo.Echo, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "kb.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	return httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{kb.New(pool)},
	}), pool
}

func get(t *testing.T, e *echo.Echo, user uuid.UUID, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func pending(t *testing.T, e *echo.Echo, user uuid.UUID) int {
	t.Helper()
	rec := get(t, e, user, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		PendingFiles *int `json:"pendingFiles"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotNil(t, out.PendingFiles)
	return *out.PendingFiles
}

// The four cases of KB/KBCompilePendingTests.swift.
func TestUnauthenticatedPendingReturns401(t *testing.T) {
	e, _ := setup(t)
	require.Equal(t, http.StatusUnauthorized, get(t, e, uuid.Nil, nil).Code)
	require.Equal(t, http.StatusUnauthorized, get(t, e, uuid.Nil, map[string]string{"Authorization": "Bearer not.a.jwt"}).Code)
}

func TestFreshTenantWithNoVaultRowsReturnsZero(t *testing.T) {
	e, _ := setup(t)
	require.Equal(t, 0, pending(t, e, ben))
}

func TestCountsOnlyRowsWithProcessedAtNil(t *testing.T) {
	e, _ := setup(t)
	require.Equal(t, 2, pending(t, e, amy), "two unprocessed and one processed")
}

func TestAnotherTenantsPendingRowsAreInvisible(t *testing.T) {
	e, _ := setup(t)
	require.Equal(t, 0, pending(t, e, ben))
	require.Equal(t, 2, pending(t, e, amy))
}

func TestVaultHeaderIsNotRead(t *testing.T) {
	e, _ := setup(t)
	rec := get(t, e, ben, map[string]string{"X-Vault-ID": amy.String()})
	require.JSONEq(t, `{"pendingFiles":0}`, rec.Body.String())
	rec = get(t, e, ben, map[string]string{"X-Vault-ID": "nope"})
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestResponseConformsToSpec(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)
	e, _ := setup(t)
	rec := get(t, e, amy, nil)
	req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
	route, params, err := router.FindRoute(req)
	require.NoError(t, err)
	out := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		},
		Status: rec.Code,
		Header: http.Header{"Content-Type": {rec.Header().Get("Content-Type")}},
	}
	out.SetBodyBytes(rec.Body.Bytes())
	require.NoError(t, openapi3filter.ValidateResponse(ctx, out))
}
