package sessions_test

import (
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/sessions"
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
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// The Swift repo has no SessionsController tests; these pin what the
// controller's SQL and mapping do, against parity/seed/sessions.sql.

var (
	alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
	keys  = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
	space = "5ACE0000-0000-4000-8000-0000000000B1"
)

type env struct {
	t *testing.T
	e *echo.Echo
}

func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "sessions.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err)
	}
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{sessions.New(pool)},
	})
	return env{t: t, e: e}
}

type session struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Preview       string  `json:"preview"`
	MessageCount  int     `json:"messageCount"`
	LastMessageAt string  `json:"lastMessageAt"`
	WorkspaceID   *string `json:"workspaceID"`
	Pinned        bool    `json:"pinned"`
	Archived      bool    `json:"archived"`
}

func (v env) get(user uuid.UUID, query string) (int, string, []byte) {
	v.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions"+query, nil)
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()
}

func (v env) list(user uuid.UUID, query string) []session {
	v.t.Helper()
	code, _, body := v.get(user, query)
	require.Equal(v.t, http.StatusOK, code, string(body))
	var out struct {
		Sessions []session `json:"sessions"`
	}
	require.NoError(v.t, json.Unmarshal(body, &out))
	return out.Sessions
}

func titles(ss []session) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Title
	}
	return out
}

func TestListNewestFirstWithCountsAndPreview(t *testing.T) {
	v := setup(t)
	code, ct, raw := v.get(alice, "")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "application/json; charset=utf-8", ct)
	require.NotContains(t, string(raw), "nextCursor", "nil cursor is omitted")
	got := v.list(alice, "")
	require.Equal(t, []string{"Long ASCII", "Combining mark", "No messages", "Short", "Emoji", "CRLF"}, titles(got))

	long := got[0]
	require.Equal(t, "C0000000-0000-4000-8000-0000000000C1", long.ID)
	require.Equal(t, 4, long.MessageCount)
	require.Equal(t, strings.Repeat("0123456789", 14), long.Preview, "latest message by created_at, 140 characters")
	require.Equal(t, "2026-02-07T00:00:00Z", long.LastMessageAt, "conversation updated_at")
	require.Nil(t, long.WorkspaceID)
	require.False(t, long.Pinned)
	require.False(t, long.Archived)

	comb := got[1]
	require.Equal(t, strings.Repeat("a", 139)+"e\u0301", comb.Preview, "the combining accent belongs to the 140th character")
	require.Equal(t, "2026-02-06T12:30:45Z", comb.LastMessageAt, "whole seconds")
	require.Equal(t, space, *comb.WorkspaceID)

	empty := got[2]
	require.Equal(t, 0, empty.MessageCount)
	require.Empty(t, empty.Preview)

	require.Equal(t, "hi there", got[3].Preview)
	require.Equal(t, 2, got[3].MessageCount)
	require.Equal(t, strings.Repeat("b", 137)+"\U0001F468\u200d\U0001F469\u200d\U0001F467\U0001F1F5\U0001F1F9\U0001F44D\U0001F3FD", got[4].Preview)
	require.Equal(t, strings.Repeat("c", 139)+"\r\n", got[5].Preview)
}

func TestLimit(t *testing.T) {
	v := setup(t)
	for q, n := range map[string]int{
		"?limit=2": 2, "?limit=0": 1, "?limit=-4": 1, "?limit=999": 6, "?limit=abc": 6,
		"?limit=2.0": 6, "?limit=%2B3": 3, "?limit=99999999999999999999": 6, "?limit=": 6,
	} {
		require.Len(t, v.list(alice, q), n, q)
	}
}

func TestWorkspaceFilter(t *testing.T) {
	v := setup(t)
	require.Equal(t, []string{"Combining mark", "Short"}, titles(v.list(alice, "?workspace="+space)))
	require.Equal(t, []string{"Combining mark", "Short"}, titles(v.list(alice, "?workspace="+strings.ToLower(space))))
	require.Equal(t, []string{"Combining mark"}, titles(v.list(alice, "?workspace="+space+"&limit=1")))
	require.Empty(t, v.list(alice, "?workspace=00000000-0000-4000-8000-000000000000"))
	for _, q := range []string{"nope", "", strings.ReplaceAll(space, "-", ""), "%7B" + space + "%7D"} {
		require.Len(t, v.list(alice, "?workspace="+q), 6, "unparseable workspace %q means all", q)
	}
}

func TestTenantIsolation(t *testing.T) {
	v := setup(t)
	require.Equal(t, []string{"Bob chat"}, titles(v.list(bob, "")))
	require.Empty(t, v.list(bob, "?workspace="+space), "the filter never crosses tenants")
	code, _, body := v.get(uuid.Nil, "")
	require.Equal(t, http.StatusUnauthorized, code)
	require.JSONEq(t, `{"error":{"message":"Authenticated identity is unavailable"}}`, string(body))
}

func TestResponseConformsToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)
	v := setup(t)
	for _, q := range []string{"", "?limit=2&workspace=" + space} {
		code, ct, body := v.get(alice, q)
		req := httptest.NewRequest(http.MethodGet, "/v1/sessions"+q, nil)
		route, params, err := router.FindRoute(req)
		require.NoError(t, err)
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{
				Request: req, PathParams: params, Route: route,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
			},
			Status: code, Header: http.Header{"Content-Type": {ct}},
		}
		out.SetBodyBytes(body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), string(body))
	}
}
