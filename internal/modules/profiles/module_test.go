package profiles_test

import (
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/profiles"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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
	bob   = uuid.MustParse("B0B00000-0000-4000-8000-000000000002") // no personas: a fresh tenant
	keys  = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
}

func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "profiles.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err)
	}
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{profiles.New(pool)},
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

type dto struct {
	ID            string   `json:"id"`
	Slug          string   `json:"slug"`
	Label         string   `json:"label"`
	SystemPrompt  string   `json:"systemPrompt"`
	IsDefault     bool     `json:"isDefault"`
	SkillsEnabled []string `json:"skillsEnabled"`
	CreatedAt     string   `json:"createdAt"`
	UpdatedAt     string   `json:"updatedAt"`
}

type listResp struct {
	Items      []dto   `json:"items"`
	ActiveSlug *string `json:"activeSlug"`
}

func (v env) mustDTO(user uuid.UUID, method, path, body string) dto {
	v.t.Helper()
	r := v.do(user, method, path, body)
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var d dto
	require.NoError(v.t, json.Unmarshal(r.body, &d))
	return d
}

func (v env) list(user uuid.UUID) (listResp, []byte) {
	v.t.Helper()
	r := v.do(user, http.MethodGet, "/v1/profiles", "")
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var l listResp
	require.NoError(v.t, json.Unmarshal(r.body, &l))
	return l, r.body
}

func errBody(msg string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"message": msg}})
	return string(b)
}

// --- Swift ProfilesControllerTests, one Go test per Swift test ---

func TestListOnFreshTenantIsEmptyWithNilActiveSlug(t *testing.T) {
	v := setup(t)
	l, raw := v.list(bob)
	require.Empty(t, l.Items)
	require.Nil(t, l.ActiveSlug)
	require.JSONEq(t, `{"items":[]}`, string(raw), "nil activeSlug is omitted, items is [] not null")
}

func TestFirstProfileBecomesDefaultSecondDoesNot(t *testing.T) {
	v := setup(t)
	first := v.mustDTO(bob, http.MethodPost, "/v1/profiles", `{"slug":"stocks","label":"Stocks","templateSlug":"stocks-tracker"}`)
	require.True(t, first.IsDefault)
	require.Equal(t, "stocks", first.Slug)
	require.Contains(t, first.SystemPrompt, "equities")
	second := v.mustDTO(bob, http.MethodPost, "/v1/profiles", `{"slug":"news","label":"News"}`)
	require.False(t, second.IsDefault)
}

func TestDuplicateSlugIs409(t *testing.T) {
	v := setup(t)
	v.mustDTO(bob, http.MethodPost, "/v1/profiles", `{"slug":"stocks","label":"Stocks"}`)
	r := v.do(bob, http.MethodPost, "/v1/profiles", `{"slug":"stocks","label":"Stocks 2"}`)
	require.Equal(t, http.StatusConflict, r.code)
	require.JSONEq(t, errBody("slug_already_exists"), string(r.body))
}

func TestInvalidSlugIs400(t *testing.T) {
	v := setup(t)
	r := v.do(bob, http.MethodPost, "/v1/profiles", `{"slug":"Has Spaces!","label":"x"}`)
	require.Equal(t, http.StatusBadRequest, r.code)
	require.JSONEq(t, errBody("invalid_slug"), string(r.body))
}

func TestPatchUpdatesLabelAndSystemPrompt(t *testing.T) {
	v := setup(t)
	v.mustDTO(bob, http.MethodPost, "/v1/profiles", `{"slug":"stocks","label":"Stocks"}`)
	d := v.mustDTO(bob, http.MethodPatch, "/v1/profiles/stocks", `{"label":"Stocks Pro","systemPrompt":"Focus on options flow."}`)
	require.Equal(t, "Stocks Pro", d.Label)
	require.Equal(t, "Focus on options flow.", d.SystemPrompt)
}

func TestActivateSwitchesDefault(t *testing.T) {
	v := setup(t)
	v.mustDTO(bob, http.MethodPost, "/v1/profiles", `{"slug":"stocks","label":"Stocks"}`)
	v.mustDTO(bob, http.MethodPost, "/v1/profiles", `{"slug":"news","label":"News"}`)
	r := v.do(bob, http.MethodPost, "/v1/profiles/news/activate", "")
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	var ack struct {
		Slug        string `json:"slug"`
		ActivatedAt string `json:"activatedAt"`
	}
	require.NoError(t, json.Unmarshal(r.body, &ack))
	require.Equal(t, "news", ack.Slug)
	l, _ := v.list(bob)
	require.Equal(t, "news", *l.ActiveSlug)
	for _, p := range l.Items {
		require.Equal(t, p.Slug == "news", p.IsDefault, p.Slug)
	}
}

func TestDeleteDefaultIs409(t *testing.T) {
	v := setup(t)
	v.mustDTO(bob, http.MethodPost, "/v1/profiles", `{"slug":"stocks","label":"Stocks"}`)
	r := v.do(bob, http.MethodDelete, "/v1/profiles/stocks", "")
	require.Equal(t, http.StatusConflict, r.code)
	require.JSONEq(t, errBody("cannot_delete_default"), string(r.body))
}

func TestDeleteNonDefaultIs204(t *testing.T) {
	v := setup(t)
	v.mustDTO(bob, http.MethodPost, "/v1/profiles", `{"slug":"stocks","label":"Stocks"}`)
	v.mustDTO(bob, http.MethodPost, "/v1/profiles", `{"slug":"news","label":"News"}`)
	r := v.do(bob, http.MethodDelete, "/v1/profiles/news", "")
	require.Equal(t, http.StatusNoContent, r.code)
	require.Empty(t, r.body)
	require.Empty(t, r.ct)
}

func TestGetUnknownSlugIs404(t *testing.T) {
	v := setup(t)
	r := v.do(bob, http.MethodGet, "/v1/profiles/ghost", "")
	require.Equal(t, http.StatusNotFound, r.code)
	require.JSONEq(t, errBody("profile_not_found"), string(r.body))
}

// --- Go-only: quirks read from the Swift controller and Foundation ---

func TestListOrderAndActiveSlug(t *testing.T) {
	v := setup(t)
	l, _ := v.list(alice)
	require.Len(t, l.Items, 3)
	require.Equal(t, []string{"assistant", "research", "legacy"},
		[]string{l.Items[0].Slug, l.Items[1].Slug, l.Items[2].Slug}, "created_at ASC, NULL last")
	require.Equal(t, "assistant", *l.ActiveSlug)
	a := l.Items[0]
	require.Equal(t, "9F000000-0000-4000-8000-0000000000A1", a.ID)
	require.Equal(t, "2026-01-05T00:00:00Z", a.CreatedAt)
	require.Equal(t, "2026-01-05T10:00:00Z", a.UpdatedAt, "whole seconds")
	require.Equal(t, []string{"kb-ask", "kb-compile"}, a.SkillsEnabled)
	require.Equal(t, []string{}, l.Items[1].SkillsEnabled)
	// NULL timestamps fall back to Date().
	_, err := time.Parse(time.RFC3339, l.Items[2].CreatedAt)
	require.NoError(t, err)
}

func TestCreateDefaultsAndTemplates(t *testing.T) {
	v := setup(t)
	d := v.mustDTO(alice, http.MethodPost, "/v1/profiles", `{"slug":"plain","label":"  Plain  "}`)
	require.False(t, d.IsDefault, "alice already has personas")
	require.Equal(t, "  Plain  ", d.Label, "validated trimmed, stored untrimmed")
	require.True(t, strings.HasPrefix(d.SystemPrompt, "You are Hermes \u2014 a personal assistant."))
	require.Equal(t, []string{}, d.SkillsEnabled)
	require.Equal(t, strings.ToUpper(d.ID), d.ID)
	require.Equal(t, d.CreatedAt, d.UpdatedAt)

	d = v.mustDTO(alice, http.MethodPost, "/v1/profiles", `{"slug":"e","label":"E","systemPrompt":"Be brief.","templateSlug":""}`)
	require.Equal(t, "Be brief.", d.SystemPrompt, "empty templateSlug falls back to systemPrompt")
	d = v.mustDTO(alice, http.MethodPost, "/v1/profiles", `{"slug":"n","label":"N","systemPrompt":"x","templateSlug":"news-curator","skillsEnabled":["a","a"]}`)
	require.True(t, strings.HasPrefix(d.SystemPrompt, "You are Hermes, a high-signal news curator."), "template wins")
	require.Equal(t, []string{"a", "a"}, d.SkillsEnabled)
	d = v.mustDTO(alice, http.MethodPost, "/v1/profiles", `{"slug":"pa","label":"PA","templateSlug":"personal-assistant"}`)
	plain := v.mustDTO(alice, http.MethodGet, "/v1/profiles/plain", "")
	require.Equal(t, plain.SystemPrompt, d.SystemPrompt, "personal-assistant is the default seed")
}

func TestSlugRules(t *testing.T) {
	v := setup(t)
	for _, ok := range []string{"9", "x--", "a" + strings.Repeat("b", 30) + "9", "trail\n", "crlf\r\n"} {
		body, _ := json.Marshal(map[string]string{"slug": ok, "label": "x"})
		r := v.do(alice, http.MethodPost, "/v1/profiles", string(body))
		require.Equal(t, http.StatusOK, r.code, "%q: %s", ok, r.body)
	}
	for _, bad := range []string{"", "-lead", "UPPER", strings.Repeat("a", 33), "\u00fcml", "a_b", " abc", "ab\ncd", "a\n\n"} {
		body, _ := json.Marshal(map[string]string{"slug": bad, "label": "x"})
		r := v.do(alice, http.MethodPost, "/v1/profiles", string(body))
		require.Equal(t, http.StatusBadRequest, r.code, "%q", bad)
		require.JSONEq(t, errBody("invalid_slug"), string(r.body))
	}
}

func TestLabelCountsGraphemesAfterTrimming(t *testing.T) {
	v := setup(t)
	cases := []struct {
		label string
		ok    bool
	}{
		{strings.Repeat("e\u0301", 80), true},
		{strings.Repeat("\U0001F1F5\U0001F1F9", 80), true},
		{strings.Repeat("l", 80), true},
		{"\u200b", true},
		{strings.Repeat("l", 81), false},
		{strings.Repeat("e\u0301", 81), false},
		{"", false},
		{" \t\n\r", false},
		{"\u00a0", false},
		{"\u3000", false},
		{"\u2028", false},
	}
	for i, c := range cases {
		body, _ := json.Marshal(map[string]string{"slug": "l" + string(rune('a'+i)), "label": c.label})
		r := v.do(alice, http.MethodPost, "/v1/profiles", string(body))
		if c.ok {
			require.Equal(t, http.StatusOK, r.code, "%+q: %s", c.label, r.body)
		} else {
			require.Equal(t, http.StatusBadRequest, r.code, "%+q", c.label)
			require.JSONEq(t, errBody("invalid_label"), string(r.body))
		}
	}
}

func TestErrorsMatchSwift(t *testing.T) {
	v := setup(t)
	long := strings.Repeat("p", 8193)
	cases := []struct {
		name, method, path, body string
		user                     uuid.UUID
		status                   int
		message                  string
	}{
		{"no token", http.MethodGet, "/v1/profiles", "", uuid.Nil, 401, "Authenticated identity is unavailable"},
		{"no token activate", http.MethodPost, "/v1/profiles/assistant/activate", "", uuid.Nil, 401, "Authenticated identity is unavailable"},
		{"empty body", http.MethodPost, "/v1/profiles", "", alice, 400, "The given data was not valid input: The given data was not valid JSON."},
		{"array body", http.MethodPost, "/v1/profiles", "[]", alice, 400, "Type mismatch for `` key, expected `Dictionary<String, Any>` type."},
		{"missing slug first", http.MethodPost, "/v1/profiles", `{"label":42}`, alice, 400, "Coding key `slug` not found."},
		{"missing label", http.MethodPost, "/v1/profiles", `{"slug":"x"}`, alice, 400, "Coding key `label` not found."},
		{"null label", http.MethodPost, "/v1/profiles", `{"slug":"x","label":null}`, alice, 400, "Value not found for `label` key."},
		{"slug type first", http.MethodPost, "/v1/profiles", `{"slug":42,"label":42}`, alice, 400, "Type mismatch for `slug` key, expected `String` type."},
		{"skills not array", http.MethodPost, "/v1/profiles", `{"slug":"x","label":"x","skillsEnabled":"a"}`, alice, 400, "Type mismatch for `skillsEnabled` key, expected `Array<Any>` type."},
		{"skills element type", http.MethodPost, "/v1/profiles", `{"slug":"x","label":"x","skillsEnabled":["a",1]}`, alice, 400, "Type mismatch for `skillsEnabled.1` key, expected `String` type."},
		{"skills element null", http.MethodPost, "/v1/profiles", `{"slug":"x","label":"x","skillsEnabled":[null]}`, alice, 400, "Value not found for `skillsEnabled.0` key."},
		{"decode before slug check", http.MethodPost, "/v1/profiles", `{"slug":"BAD","label":"x","skillsEnabled":[1]}`, alice, 400, "Type mismatch for `skillsEnabled.0` key, expected `String` type."},
		{"slug before label", http.MethodPost, "/v1/profiles", `{"slug":"BAD","label":""}`, alice, 400, "invalid_slug"},
		{"unknown template", http.MethodPost, "/v1/profiles", `{"slug":"t","label":"x","systemPrompt":"ok","templateSlug":"nope"}`, alice, 400, "template_not_found"},
		{"prompt too long", http.MethodPost, "/v1/profiles", `{"slug":"t","label":"x","systemPrompt":"` + long + `"}`, alice, 400, "system_prompt_too_long"},
		{"two-byte prompt too long", http.MethodPost, "/v1/profiles", `{"slug":"t","label":"x","systemPrompt":"` + strings.Repeat("\u00e9", 4097) + `"}`, alice, 400, "system_prompt_too_long"},
		{"dup with bad label is 400", http.MethodPost, "/v1/profiles", `{"slug":"assistant","label":" "}`, alice, 400, "invalid_label"},
		{"dup", http.MethodPost, "/v1/profiles", `{"slug":"assistant","label":"x"}`, alice, 409, "slug_already_exists"},
		{"patch decode before 404", http.MethodPatch, "/v1/profiles/ghost", "{", alice, 400, "The given data was not valid input: The given data was not valid JSON."},
		{"patch unknown", http.MethodPatch, "/v1/profiles/ghost", `{}`, alice, 404, "profile_not_found"},
		{"patch empty label", http.MethodPatch, "/v1/profiles/assistant", `{"label":""}`, alice, 400, "invalid_label"},
		{"patch long prompt", http.MethodPatch, "/v1/profiles/assistant", `{"label":"Changed","systemPrompt":"` + long + `"}`, alice, 400, "system_prompt_too_long"},
		{"patch skills element", http.MethodPatch, "/v1/profiles/assistant", `{"skillsEnabled":[1]}`, alice, 400, "Type mismatch for `skillsEnabled.0` key, expected `String` type."},
		{"get case sensitive", http.MethodGet, "/v1/profiles/ASSISTANT", "", alice, 404, "profile_not_found"},
		{"delete unknown", http.MethodDelete, "/v1/profiles/ghost", "", alice, 404, "profile_not_found"},
		{"delete default", http.MethodDelete, "/v1/profiles/assistant", "", alice, 409, "cannot_delete_default"},
		{"activate unknown", http.MethodPost, "/v1/profiles/ghost/activate", "", alice, 404, "profile_not_found"},
		{"bob cannot see alice", http.MethodGet, "/v1/profiles/assistant", "", bob, 404, "profile_not_found"},
		{"bob cannot activate alice", http.MethodPost, "/v1/profiles/assistant/activate", "", bob, 404, "profile_not_found"},
		{"bob cannot delete alice", http.MethodDelete, "/v1/profiles/research", "", bob, 404, "profile_not_found"},
	}
	for _, tc := range cases {
		r := v.do(tc.user, tc.method, tc.path, tc.body)
		require.Equal(t, tc.status, r.code, tc.name)
		require.Equal(t, "application/json; charset=utf-8", r.ct, tc.name)
		require.JSONEq(t, errBody(tc.message), string(r.body), tc.name)
	}
	a := v.mustDTO(alice, http.MethodGet, "/v1/profiles/assistant", "")
	require.Equal(t, "Assistant", a.Label, "a failed patch saves nothing")
	l, _ := v.list(alice)
	require.Len(t, l.Items, 3, "no failed create wrote a row")
}

func TestPatchSemantics(t *testing.T) {
	v := setup(t)
	before := v.mustDTO(alice, http.MethodGet, "/v1/profiles/assistant", "")
	same := v.mustDTO(alice, http.MethodPatch, "/v1/profiles/assistant", `{}`)
	require.Equal(t, before, same, "no field set: nothing saved, updated_at kept")
	same = v.mustDTO(alice, http.MethodPatch, "/v1/profiles/assistant", `{"label":null,"systemPrompt":null,"skillsEnabled":null}`)
	require.Equal(t, before, same, "null is unset")

	d := v.mustDTO(alice, http.MethodPatch, "/v1/profiles/assistant", `{"label":"Assistant"}`)
	require.NotEqual(t, before.UpdatedAt, d.UpdatedAt, "assigning the same value still saves")
	d = v.mustDTO(alice, http.MethodPatch, "/v1/profiles/assistant", `{"label":"  New  ","systemPrompt":"","skillsEnabled":[]}`)
	require.Equal(t, "  New  ", d.Label)
	require.Empty(t, d.SystemPrompt)
	require.Equal(t, []string{}, d.SkillsEnabled)
	require.True(t, d.IsDefault)
	got := v.mustDTO(alice, http.MethodGet, "/v1/profiles/assistant", "")
	require.Equal(t, d.Label, got.Label)
	require.Equal(t, []string{}, got.SkillsEnabled)
}

func TestActivateIdempotentAndStamps(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	r := v.do(alice, http.MethodPost, "/v1/profiles/assistant/activate", "")
	require.Equal(t, http.StatusOK, r.code)
	require.JSONEq(t, `{"slug":"assistant","activatedAt":"2026-01-05T10:00:00Z"}`, string(r.body),
		"already default: no write, answers the stored updated_at")

	r = v.do(alice, http.MethodPost, "/v1/profiles/legacy/activate", "")
	require.Equal(t, http.StatusOK, r.code)
	var stamps []struct {
		slug    string
		def     bool
		updated *time.Time
	}
	rows, err := v.pool.Query(ctx, `SELECT slug, is_default, updated_at FROM user_hermes_profiles WHERE tenant_id = $1 ORDER BY slug`, alice)
	require.NoError(t, err)
	for rows.Next() {
		var s struct {
			slug    string
			def     bool
			updated *time.Time
		}
		require.NoError(t, rows.Scan(&s.slug, &s.def, &s.updated))
		stamps = append(stamps, s)
	}
	require.Len(t, stamps, 3)
	require.Equal(t, "assistant", stamps[0].slug)
	require.False(t, stamps[0].def)
	require.True(t, stamps[0].updated.After(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)), "the cleared default is stamped too (Fluent query update)")
	require.True(t, stamps[1].def, "legacy")
	require.NotNil(t, stamps[1].updated)
	require.False(t, stamps[2].def, "research untouched")
	require.True(t, stamps[2].updated.Equal(time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)))
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
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/profiles", ""},
		{http.MethodPost, "/v1/profiles", `{"slug":"spec","label":"Spec","skillsEnabled":["kb-ask"]}`},
		{http.MethodGet, "/v1/profiles/spec", ""},
		{http.MethodPatch, "/v1/profiles/spec", `{"label":"Spec 2"}`},
		{http.MethodPost, "/v1/profiles/spec/activate", ""},
		{http.MethodDelete, "/v1/profiles/research", ""},
		{http.MethodGet, "/v1/profiles/ghost", ""},
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
		if r.code >= 400 {
			continue // the spec's ErrorEnvelope is known-wrong (decisions.md D-err)
		}
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: r.code,
			Header: http.Header{"Content-Type": {r.ct}},
		}
		out.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, r.body)
	}
}
