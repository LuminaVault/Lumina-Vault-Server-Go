package spaces_test

import (
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/spaces"
	"lumina/internal/vaultpaths"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
	keys  = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

type env struct {
	t     *testing.T
	e     *echo.Echo
	pool  *pgxpool.Pool
	paths vaultpaths.Paths
	m     *spaces.Module
}

func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "spaces.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err)
	}
	paths := vaultpaths.Paths{Root: t.TempDir()}
	m := spaces.New(pool, paths, auth.Identify(keys, auth.PGUsers(pool)))
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{m},
	})
	return env{t: t, e: e, pool: pool, paths: paths, m: m}
}

type resp struct {
	code int
	ct   string
	body []byte
}

func (v env) do(user uuid.UUID, method, path, body string, hdr ...string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
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
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Slug      string  `json:"slug"`
	Icon      *string `json:"icon"`
	Category  *string `json:"category"`
	NoteCount int     `json:"noteCount"`
	CreatedAt string  `json:"createdAt"`
}

func (v env) create(user uuid.UUID, body string) dto {
	v.t.Helper()
	r := v.do(user, http.MethodPost, "/v1/spaces", body)
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var d dto
	require.NoError(v.t, json.Unmarshal(r.body, &d))
	return d
}

func (v env) list(user uuid.UUID) []dto {
	v.t.Helper()
	r := v.do(user, http.MethodGet, "/v1/spaces", "")
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var out struct{ Spaces []dto }
	require.NoError(v.t, json.Unmarshal(r.body, &out))
	return out.Spaces
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Swift SpacesCategoryTests "create space accepts and persists category".
func TestCreatePersistsCategory(t *testing.T) {
	v := setup(t)
	d := v.create(alice, `{"name":"Crypto","slug":"crypto","icon":"bitcoinsign.circle","category":"stocks"}`)
	require.Equal(t, "stocks", *d.Category)
	require.Equal(t, 0, d.NoteCount)
	require.Equal(t, "bitcoinsign.circle", *d.Icon)
	require.Equal(t, strings.ToUpper(d.ID), d.ID)

	st, err := os.Stat(filepath.Join(v.paths.Raw(alice), "crypto"))
	require.NoError(t, err, "folder raw/<slug> is created")
	require.True(t, st.IsDir())
	for _, sub := range []string{"wiki", "memories"} {
		_, err := os.Stat(filepath.Join(v.paths.TenantRoot(alice), sub))
		require.NoError(t, err)
	}

	var by, upd uuid.UUID
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT created_by_user_id, updated_by_user_id FROM spaces WHERE slug = 'crypto'`).Scan(&by, &upd))
	require.Equal(t, alice, by)
	require.Equal(t, alice, upd)
}

// Swift SpacesCategoryTests "update space changes category and clears via empty string".
func TestUpdateCategoryAndClear(t *testing.T) {
	v := setup(t)
	d := v.create(alice, `{"name":"Reading","slug":"reading","icon":"book.fill","category":"ideas"}`)
	r := v.do(alice, http.MethodPut, "/v1/spaces/"+d.ID, `{"category":"work"}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	var got dto
	require.NoError(t, json.Unmarshal(r.body, &got))
	require.Equal(t, "work", *got.Category)

	r = v.do(alice, http.MethodPut, "/v1/spaces/"+d.ID, `{"category":""}`)
	require.Equal(t, http.StatusOK, r.code)
	require.NotContains(t, string(r.body), `"category"`, "cleared field is omitted")
	require.Equal(t, "book.fill", *v.list(alice)[1].Icon, "untouched fields stay")
}

// Swift SpacesCategoryTests "seeded defaults expose slug as category"
// (through SeedDefaults; POST /v1/vault/create is wave 4).
func TestSeedDefaultsAndIdempotence(t *testing.T) {
	v := setup(t)
	created, err := v.m.SeedDefaults(context.Background(), bob)
	require.NoError(t, err)
	require.Equal(t, []string{"ai", "stocks", "health", "work", "ideas"}, created)
	for _, s := range v.list(bob) {
		require.Equal(t, s.Slug, *s.Category)
	}
	again, err := v.m.SeedDefaults(context.Background(), bob)
	require.NoError(t, err)
	require.Empty(t, again)
}

// SpaceSlugDerivationTests integration case: a name-only create derives the slug.
func TestCreateWithoutSlugDerives(t *testing.T) {
	v := setup(t)
	d := v.create(alice, `{"name":"Reading List"}`)
	require.Equal(t, "Reading List", d.Name)
	require.Equal(t, "reading-list", d.Slug)
}

func TestListSortedByNameWithLiveNoteCounts(t *testing.T) {
	v := setup(t)
	got := v.list(alice)
	require.Len(t, got, 2)
	require.Equal(t, "Alpha", got[0].Name)
	require.Equal(t, 2, got[0].NoteCount, "counted from vault_files, not the stored 7")
	require.Equal(t, "Zeta", got[1].Name)
	require.Equal(t, 0, got[1].NoteCount)
	require.Equal(t, "2026-01-04T00:00:00Z", got[0].CreatedAt)
	require.Empty(t, v.list(bob))
}

func TestUpdatePersistsLiveNoteCount(t *testing.T) {
	v := setup(t)
	r := v.do(alice, http.MethodPut, "/v1/spaces/5BACE000-0000-4000-8000-0000000000A1", `{"name":"  Alpha 2 "}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	var n int
	var name string
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT name, note_count FROM spaces WHERE id = '5bace000-0000-4000-8000-0000000000a1'`).Scan(&name, &n))
	require.Equal(t, "Alpha 2", name)
	require.Equal(t, 2, n, "Swift saves the model after stamping the live count")
}

func TestDeleteSoftMovesFolder(t *testing.T) {
	v := setup(t)
	d := v.create(alice, `{"name":"Temp","slug":"temp"}`)
	r := v.do(alice, http.MethodDelete, "/v1/spaces/"+d.ID, "")
	require.Equal(t, http.StatusNoContent, r.code)
	require.Empty(t, r.body)
	_, err := os.Stat(filepath.Join(v.paths.Raw(alice), "temp"))
	require.True(t, os.IsNotExist(err))
	moved, err := filepath.Glob(filepath.Join(v.paths.Raw(alice), "_deleted_*_temp"))
	require.NoError(t, err)
	require.Len(t, moved, 1)
	require.Equal(t, http.StatusNotFound, v.do(alice, http.MethodDelete, "/v1/spaces/"+d.ID, "").code)

	// A seeded space with no folder deletes fine.
	require.Equal(t, http.StatusNoContent, v.do(alice, http.MethodDelete, "/v1/spaces/5BACE000-0000-4000-8000-0000000000A2", "").code)
}

func TestActivityRecorded(t *testing.T) {
	v := setup(t)
	d := v.create(alice, `{"name":"Logged","slug":"logged"}`)
	v.do(alice, http.MethodPut, "/v1/spaces/"+d.ID, `{"name":"Logged 2"}`)
	v.do(alice, http.MethodDelete, "/v1/spaces/"+d.ID, "")
	rows, err := v.pool.Query(context.Background(),
		`SELECT action, target_title, actor_name, target_type FROM vault_activity_events ORDER BY created_at, action`)
	require.NoError(t, err)
	var got []string
	for rows.Next() {
		var a, title, actor, typ string
		require.NoError(t, rows.Scan(&a, &title, &actor, &typ))
		got = append(got, a+":"+title+":"+actor+":"+typ)
	}
	require.NoError(t, rows.Err())
	require.ElementsMatch(t, []string{
		"space.created:Logged:alice:space", "space.updated:Logged 2:alice:space", "space.deleted:Logged 2:alice:space",
	}, got)
}

func TestErrorsMatchSwift(t *testing.T) {
	v := setup(t)
	v.create(alice, `{"name":"Dup","slug":"dup"}`)
	seeded := "/v1/spaces/5BACE000-0000-4000-8000-0000000000A1"
	cases := []struct {
		name, method, path, body string
		user                     uuid.UUID
		hdr                      []string
		status                   int
		message                  string
	}{
		{"no-token", http.MethodGet, "/v1/spaces", "", uuid.Nil, nil, 401, "Authenticated identity is unavailable"},
		{"bad-header-no-token", http.MethodGet, "/v1/spaces", "", uuid.Nil, []string{"X-Vault-ID", "nope"}, 400, "invalid X-Vault-ID"},
		{"foreign-vault", http.MethodGet, "/v1/spaces", "", bob, []string{"X-Vault-ID", alice.String()}, 403, "vault access denied"},
		{"unknown-vault", http.MethodGet, "/v1/spaces", "", alice, []string{"X-Vault-ID", uuid.NewString()}, 404, "vault not found"},
		{"bad-id", http.MethodGet, "/v1/spaces/nope", "", alice, nil, 400, "invalid space id"},
		{"no-token-bad-id", http.MethodGet, "/v1/spaces/nope", "", uuid.Nil, nil, 401, "Authenticated identity is unavailable"},
		{"missing", http.MethodGet, "/v1/spaces/00000000-0000-4000-8000-000000000000", "", alice, nil, 404, "space not found"},
		{"other-tenant", http.MethodGet, seeded, "", bob, nil, 404, "space not found"},
		{"blank-name", http.MethodPost, "/v1/spaces", `{"name":"  ","slug":"ok"}`, alice, nil, 400, "name required"},
		{"bad-slug", http.MethodPost, "/v1/spaces", `{"name":"x","slug":"-x"}`, alice, nil, 400, "slug must be 2-31 chars, lowercase a-z/0-9/-, no leading dash"},
		{"reserved-slug", http.MethodPost, "/v1/spaces", `{"name":"x","slug":"raw"}`, alice, nil, 400, "slug must be 2-31 chars, lowercase a-z/0-9/-, no leading dash"},
		{"taken", http.MethodPost, "/v1/spaces", `{"name":"x","slug":" DUP "}`, alice, nil, 409, "slug already used in this vault"},
		{"missing-name", http.MethodPost, "/v1/spaces", `{}`, alice, nil, 400, "Coding key `name` not found."},
		{"update-blank-name", http.MethodPut, seeded, `{"name":""}`, alice, nil, 400, "name required"},
		{"update-missing-first", http.MethodPut, "/v1/spaces/00000000-0000-4000-8000-000000000000", `{"name":""}`, alice, nil, 404, "space not found"},
		{"delete-other-tenant", http.MethodDelete, seeded, "", bob, nil, 404, "space not found"},
	}
	for _, tc := range cases {
		r := v.do(tc.user, tc.method, tc.path, tc.body, tc.hdr...)
		require.Equal(t, tc.status, r.code, tc.name+": "+string(r.body))
		require.JSONEq(t, `{"error":{"message":`+mustJSON(tc.message)+`}}`, string(r.body), tc.name)
	}
}

func TestTeamVaultViewerCannotWrite(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at) VALUES ('7ea70000-0000-4000-8000-000000000001', 'T', 'a11ce000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001', now(), now())`,
		`INSERT INTO vaults (id, team_id, name, created_at, updated_at) VALUES ('7ea70000-0000-4000-8000-0000000000f1', '7ea70000-0000-4000-8000-000000000001', 'TV', now(), now())`,
		`INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at) VALUES (gen_random_uuid(), '7ea70000-0000-4000-8000-0000000000f1', 'b0b00000-0000-4000-8000-000000000002', 'viewer', false, 'a11ce000-0000-4000-8000-000000000001', now(), now())`,
	} {
		_, err := v.pool.Exec(ctx, q)
		require.NoError(t, err)
	}
	var err error
	h := []string{"X-Vault-ID", "7EA70000-0000-4000-8000-0000000000F1"}
	require.Equal(t, http.StatusOK, v.do(bob, http.MethodGet, "/v1/spaces", "", h...).code)
	r := v.do(bob, http.MethodPost, "/v1/spaces", `{"name":"T"}`, h...)
	require.Equal(t, http.StatusForbidden, r.code)
	require.Contains(t, string(r.body), "vault write permission required")
	// Owning the team grants nothing without a membership (as in Swift).
	require.Equal(t, http.StatusForbidden, v.do(alice, http.MethodGet, "/v1/spaces", "", h...).code)
	// An editor writes; the folder lives under the vault id.
	_, err = v.pool.Exec(ctx, `UPDATE vault_memberships SET role = 'editor'`)
	require.NoError(t, err)
	r = v.do(bob, http.MethodPost, "/v1/spaces", `{"name":"Team Space"}`, h...)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	_, err = os.Stat(filepath.Join(v.paths.Raw(uuid.MustParse("7EA70000-0000-4000-8000-0000000000F1")), "team-space"))
	require.NoError(t, err)
}

// Every success body must validate against api/openapi.yaml (decisions.md D2).
func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	d := v.create(alice, `{"name":"Spec","slug":"spec","color":"#fff"}`)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/spaces", ""},
		{http.MethodPost, "/v1/spaces", `{"name":"Spec2","description":"d","category":"ai"}`},
		{http.MethodGet, "/v1/spaces/" + d.ID, ""},
		{http.MethodPut, "/v1/spaces/" + d.ID, `{"icon":"star"}`},
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
