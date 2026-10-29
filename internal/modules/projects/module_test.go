package projects_test

import (
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/projects"
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

var (
	alice  = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob    = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
	seeded = "5EED0000-0000-4000-8000-0000000000A1"
	keys   = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
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
	seed, err := os.ReadFile("../../../parity/seed/base.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(seed))
	require.NoError(t, err)
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{projects.New(pool)},
	})
	return env{t: t, e: e}
}

type resp struct {
	code int
	ct   string
	body []byte
}

func (v env) do(user uuid.UUID, method, path, body string) resp {
	v.t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
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

type dto struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Archived    bool    `json:"archived"`
	TodoCount   *int    `json:"todoCount"`
	CreatedAt   string  `json:"createdAt"`
}

func list(t *testing.T, v env, user uuid.UUID, query string) []dto {
	t.Helper()
	r := v.do(user, http.MethodGet, "/v1/projects"+query, "")
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	var out struct{ Projects []dto }
	require.NoError(t, json.Unmarshal(r.body, &out))
	return out.Projects
}

func TestListNewestFirstWithTodoCount(t *testing.T) {
	v := setup(t)
	r := v.do(alice, http.MethodPost, "/v1/projects", `{"name":"Newer"}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	got := list(t, v, alice, "")
	require.Len(t, got, 2)
	require.Equal(t, "Newer", got[0].Name, "created_at DESC")
	require.Equal(t, seeded, got[1].ID)
	require.Equal(t, 2, *got[1].TodoCount, "two isTodo rows (one done) point at Seeded; a non-todo note does not count")
	require.Equal(t, 0, *got[0].TodoCount)
	require.Equal(t, "2026-01-02T00:00:00Z", got[1].CreatedAt)
}

func TestLimitClamped(t *testing.T) {
	v := setup(t)
	v.do(alice, http.MethodPost, "/v1/projects", `{"name":"Two"}`)
	require.Len(t, list(t, v, alice, "?limit=0"), 1)
	require.Len(t, list(t, v, alice, "?limit=-5"), 1)
	require.Len(t, list(t, v, alice, "?limit=999"), 2)
	require.Len(t, list(t, v, alice, "?limit=abc"), 2)
}

func TestCreateTrimsAndUppercasesID(t *testing.T) {
	v := setup(t)
	r := v.do(alice, http.MethodPost, "/v1/projects", `{"name":"  Home  ","description":"chores"}`)
	require.Equal(t, http.StatusOK, r.code)
	require.Equal(t, "application/json; charset=utf-8", r.ct)
	var p dto
	require.NoError(t, json.Unmarshal(r.body, &p))
	require.Equal(t, "Home", p.Name)
	require.Equal(t, strings.ToUpper(p.ID), p.ID)
	require.Equal(t, "chores", *p.Description)
	require.Equal(t, 0, *p.TodoCount)
	require.False(t, p.Archived) // Swift ProjectModelTests "project round-trips and maps to DTO"
}

// Swift ProjectModelTests "projects are tenant-scoped".
func TestProjectsAreTenantScoped(t *testing.T) {
	v := setup(t)
	require.Empty(t, list(t, v, bob, ""))
	v.do(bob, http.MethodPost, "/v1/projects", `{"name":"B-only"}`)
	require.Len(t, list(t, v, bob, ""), 1)
	require.Len(t, list(t, v, alice, ""), 1, "alice still sees only Seeded")
}

func TestPatchSemantics(t *testing.T) {
	v := setup(t)
	path := "/v1/projects/" + seeded
	r := v.do(alice, http.MethodPatch, path, `{"description":""}`)
	require.Equal(t, http.StatusOK, r.code)
	var p dto
	require.NoError(t, json.Unmarshal(r.body, &p))
	require.Nil(t, p.Description, `"" clears the description`)
	require.NotContains(t, string(r.body), `"description"`, "nil is omitted, as Swift does")
	require.Equal(t, 2, *p.TodoCount)

	r = v.do(alice, http.MethodPatch, strings.ToLower(path), `{"name":"  Renamed ","archived":true}`)
	require.Equal(t, http.StatusOK, r.code, "lowercase ids are accepted")
	require.NoError(t, json.Unmarshal(r.body, &p))
	require.Equal(t, "Renamed", p.Name)
	require.True(t, p.Archived)

	r = v.do(alice, http.MethodPatch, path, `{"name":null}`)
	require.Equal(t, http.StatusOK, r.code, "null optional = unchanged")
}

// Statuses and messages for every error case the golden capture recorded
// against the Swift binary (testdata/golden/projects-http.json).
func TestErrorsMatchSwift(t *testing.T) {
	v := setup(t)
	path := "/v1/projects/" + seeded
	cases := []struct {
		name, method, path, body string
		user                     uuid.UUID
		status                   int
		message                  string
	}{
		{"create-empty-name", http.MethodPost, "/v1/projects", `{"name":" "}`, alice, 400, "project name required"},
		{"patch-empty-name", http.MethodPatch, path, `{"name":"   "}`, alice, 400, "project name cannot be empty"},
		{"patch-missing", http.MethodPatch, "/v1/projects/00000000-0000-4000-8000-000000000000", `{"name":"x"}`, alice, 404, "project not found"},
		{"patch-bad-id", http.MethodPatch, "/v1/projects/nope", `{"name":"x"}`, alice, 400, "invalid project id"},
		{"other-tenant-patch", http.MethodPatch, path, `{"name":"x"}`, bob, 404, "project not found"},
		{"other-tenant-delete", http.MethodDelete, path, "", bob, 404, "project not found"},
		{"no-token", http.MethodGet, "/v1/projects", "", uuid.Nil, 401, "Authenticated identity is unavailable"},
		{"missing-key", http.MethodPost, "/v1/projects", `{}`, alice, 400, "Coding key `name` not found."},
	}
	for _, tc := range cases {
		r := v.do(tc.user, tc.method, tc.path, tc.body)
		require.Equal(t, tc.status, r.code, tc.name)
		require.JSONEq(t, `{"error":{"message":`+mustJSON(tc.message)+`}}`, string(r.body), tc.name)
	}
	require.Len(t, list(t, v, alice, ""), 1, "bob's attempts changed nothing")
}

func TestDeleteTwiceIs404(t *testing.T) {
	v := setup(t)
	path := "/v1/projects/" + seeded
	r := v.do(alice, http.MethodDelete, path, "")
	require.Equal(t, http.StatusNoContent, r.code)
	require.Empty(t, r.body)
	require.Empty(t, r.ct)
	require.Equal(t, http.StatusNotFound, v.do(alice, http.MethodDelete, path, "").code)
}

// The golden statuses for the same flows must match what Go returns.
func TestGoldenStatuses(t *testing.T) {
	raw, err := os.ReadFile("../../../testdata/golden/projects-http.json")
	require.NoError(t, err)
	var golden []struct {
		Name   string `json:"name"`
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	require.NoError(t, json.Unmarshal(raw, &golden))
	want := map[string]int{}
	for _, g := range golden {
		want[g.Name] = g.Status
	}
	require.Equal(t, 200, want["create"], "Swift answers create with 200, not 201")
	require.Equal(t, 204, want["delete"])
	require.Equal(t, 404, want["delete-again"])
	require.Equal(t, 400, want["patch-empty-name"])
}

// Every success body must validate against api/openapi.yaml (decisions.md D2).
func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	created := v.do(alice, http.MethodPost, "/v1/projects", `{"name":"Spec"}`)
	var p dto
	require.NoError(t, json.Unmarshal(created.body, &p))
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/projects", ""},
		{http.MethodPost, "/v1/projects", `{"name":"Spec2","description":"d"}`},
		{http.MethodPatch, "/v1/projects/" + p.ID, `{"archived":true}`},
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
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: r.code,
			Header: http.Header{"Content-Type": {r.ct}},
		}
		out.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, r.body)
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
