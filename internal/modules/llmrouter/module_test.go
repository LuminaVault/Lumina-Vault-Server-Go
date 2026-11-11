package llmrouter_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/migrations"
	"lumina/internal/modules/llmrouter"
	"lumina/internal/modules/meproviders"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

// Parity seed users and rows (parity/seed/llm-router.sql).
var (
	lrPro   = uuid.MustParse("1a7e0000-0000-4000-8000-0000000000a2")
	lrUlt   = uuid.MustParse("1a7e0000-0000-4000-8000-0000000000a3")
	lrDash  = uuid.MustParse("1a7e0000-0000-4000-8000-0000000000a6")
	lrBind  = uuid.MustParse("1a7e0000-0000-4000-8000-0000000000a7")
	bindSp  = "1a7e2000-0000-4000-8000-000000000001"
	otherSp = "1a7e2000-0000-4000-8000-000000000002"
	bindWf  = "1a7e3000-0000-4000-8000-000000000001"
	otherWf = "1a7e3000-0000-4000-8000-000000000002"
	ultExec = "1a7e4000-0000-4000-8000-000000000010"
)

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
}

func setup(t *testing.T, parallel bool) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "llm-router.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	box, err := secretbox.New(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	require.NoError(t, err)
	vars := map[string]string{"FREELANE_ENABLED": "false"}
	stack := llm.NewStack(llm.StackConfig{Getenv: func(k string) string { return vars[k] }, Pool: pool, Box: box})
	stack.ParallelEnabled = parallel
	identify := auth.Identify(keys, auth.PGUsers(pool))
	limits := ratelimit.NewStore()
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth: auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{
			llmrouter.New(pool, llmrouter.Options{Identify: identify, Limits: limits, Stack: stack, FixDashboard: true}),
			meproviders.New(pool, meproviders.Options{Identify: identify, Limits: limits, Stack: stack, Box: box}),
		},
	})
	return env{t: t, e: e, pool: pool}
}

func (v env) user(tier string) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	_, err := v.pool.Exec(context.Background(), `
		INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
		VALUES ($1, $2, 'x', true, $3, $4, now(), now())`, id, id.String()+"@test", "u"+id.String()[:8], tier)
	require.NoError(v.t, err)
	_, err = v.pool.Exec(context.Background(), `
		INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'Vault', now(), now())`, id)
	require.NoError(v.t, err)
	return id
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

func (v env) ok(user uuid.UUID, method, path, body string) map[string]any {
	v.t.Helper()
	r := v.do(user, method, path, body)
	require.Equal(v.t, http.StatusOK, r.code, "%s %s: %s", method, path, r.body)
	var m map[string]any
	require.NoError(v.t, json.Unmarshal(r.body, &m), string(r.body))
	return m
}

func (v env) errIs(r resp, status int, msg string) {
	v.t.Helper()
	require.Equal(v.t, status, r.code, string(r.body))
	b, _ := json.Marshal(msg)
	require.JSONEq(v.t, `{"error":{"message":`+string(b)+`}}`, string(r.body))
}

func (v env) defaultProfile(user uuid.UUID) map[string]any {
	v.t.Helper()
	out := v.ok(user, http.MethodGet, "/v1/router", "")
	for _, p := range out["profiles"].([]any) {
		if p.(map[string]any)["id"] == out["defaultProfileID"] {
			return p.(map[string]any)
		}
	}
	v.t.Fatal("no default profile")
	return nil
}

// storedRoutes reads the document the router executes, unscrubbed.
func (v env) storedRoutes(profileID string) []llm.WireRoute {
	v.t.Helper()
	var doc llm.RouterProfileDocument
	require.NoError(v.t, v.pool.QueryRow(context.Background(), `SELECT document FROM router_profiles WHERE id = $1`, profileID).Scan(&doc))
	return doc.DefaultAction.Routes
}

const placeholderRoute = `{"provider":"openRouter","model":"auto"}`

func byokWrite(name string, revision any) string {
	b, _ := json.Marshal(revision)
	rev := ""
	if revision != nil {
		rev = `,"expectedRevision":` + string(b)
	}
	return `{"name":"` + name + `","mode":"byok","objective":{"quality":50,"cost":25,"latency":25},"budget":{},` +
		`"defaultAction":{"kind":"sequential","routes":[` + placeholderRoute + `],"retryPolicy":"fast"},"routingPolicy":"balanced"` + rev + `}`
}

func (v env) saveByokPreference(user uuid.UUID) {
	v.t.Helper()
	r := v.do(user, http.MethodPut, "/v1/me/preferences/llm",
		`{"mode":"byok","primaryProvider":"anthropic","primaryModel":"claude-opus-4-7","fallbackChain":[{"provider":"openai","model":"gpt-4o-mini"}]}`)
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
}

// MARK: - RouterPlaceholderSaveTests

func TestPlaceholderSaveStoresTheSavedByokChain(t *testing.T) {
	v := setup(t, false)
	u := v.user("trial")
	scrubbed := v.defaultProfile(u)
	assert.Equal(t, []any{map[string]any{"provider": "openRouter", "model": "auto"}},
		scrubbed["defaultAction"].(map[string]any)["routes"], "precondition: a managed profile is scrubbed")

	v.saveByokPreference(u)
	current := v.defaultProfile(u)
	id := scrubbed["id"].(string)
	r := v.do(u, http.MethodPut, "/v1/router/"+id, byokWrite(scrubbed["name"].(string), current["revision"]))
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	assert.Equal(t, []llm.WireRoute{{Provider: "anthropic", Model: "claude-opus-4-7"}, {Provider: "openai", Model: "gpt-4o-mini"}}, v.storedRoutes(id))
}

func TestPlaceholderSaveWithNoSavedByokPreferenceIsRefused(t *testing.T) {
	v := setup(t, false)
	u := v.user("trial")
	scrubbed := v.defaultProfile(u)
	id := scrubbed["id"].(string)
	v.errIs(v.do(u, http.MethodPut, "/v1/router/"+id, byokWrite(scrubbed["name"].(string), scrubbed["revision"])), http.StatusBadRequest, "router_placeholder_route")
	assert.NotEqual(t, []llm.WireRoute{{Provider: "openRouter", Model: "auto"}}, v.storedRoutes(id))
}

func TestPlaceholderSaveCreatingAByokProfileStoresTheSavedChain(t *testing.T) {
	v := setup(t, false)
	u := v.user("pro")
	v.defaultProfile(u)
	v.saveByokPreference(u)
	created := v.ok(u, http.MethodPost, "/v1/router", byokWrite("Copy", nil))
	routes := v.storedRoutes(created["id"].(string))
	require.NotEmpty(t, routes)
	assert.Equal(t, llm.WireRoute{Provider: "anthropic", Model: "claude-opus-4-7"}, routes[0])
}

// The preferences PUT bumps the default profile's revision, so a router save
// with the revision read before it conflicts.
func TestPlaceholderSaveWithTheRevisionReadBeforeAPreferencesSaveConflicts(t *testing.T) {
	v := setup(t, false)
	u := v.user("trial")
	before := v.defaultProfile(u)
	v.saveByokPreference(u)
	v.errIs(v.do(u, http.MethodPut, "/v1/router/"+before["id"].(string), byokWrite(before["name"].(string), before["revision"])),
		http.StatusConflict, "router_profile_revision_conflict")
}

// MARK: - Profiles, tiers and validation

func TestProfilesTierGatesAndValidation(t *testing.T) {
	v := setup(t, false)
	free := v.user("free")
	body := `{"name":"X","mode":"managed","objective":{"quality":100,"cost":0,"latency":0},"budget":{},"defaultAction":{"kind":"sequential","routes":[{"provider":"openai","model":"m"}],"retryPolicy":"fast"}}`
	v.errIs(v.do(free, http.MethodPost, "/v1/router", body), http.StatusForbidden, "router_custom_profile_requires_pro")
	def := v.defaultProfile(free)
	v.errIs(v.do(free, http.MethodDelete, "/v1/router/"+def["id"].(string), ""), http.StatusForbidden, "router_custom_profile_requires_pro")

	pro := v.user("pro")
	v.errIs(v.do(pro, http.MethodPost, "/v1/router", strings.Replace(body, `"quality":100`, `"quality":50`, 1)), http.StatusBadRequest, "router_objective_weights_must_total_100")
	v.errIs(v.do(pro, http.MethodPost, "/v1/router", strings.Replace(body, `"routes":[{"provider":"openai","model":"m"}]`, `"routes":[]`, 1)), http.StatusBadRequest, "router_action_requires_route")
	v.errIs(v.do(pro, http.MethodPost, "/v1/router", strings.Replace(body, `"budget":{}`, `"budget":{"softLimitUsdMicros":9,"hardLimitUsdMicros":1}`, 1)), http.StatusBadRequest, "router_soft_budget_exceeds_hard_budget")
	ensemble := strings.Replace(body, `"kind":"sequential"`, `"kind":"ensemble"`, 1)
	v.errIs(v.do(pro, http.MethodPost, "/v1/router", ensemble), http.StatusForbidden, "router_ensemble_requires_ultimate")
	autoByok := strings.Replace(strings.Replace(body, `"mode":"managed"`, `"mode":"byok"`, 1), `"budget":{}`, `"budget":{},"routingPolicy":"autoSmart"`, 1)
	v.errIs(v.do(pro, http.MethodPost, "/v1/router", autoByok), http.StatusBadRequest, "AUTO_REQUIRES_OPENROUTER")
	require.Equal(t, http.StatusOK, v.do(pro, http.MethodPut, "/v1/me/providers/openRouter", `{"kind":"apiKey","apiKey":"sk-or"}`).code)
	assert.Equal(t, http.StatusOK, v.do(pro, http.MethodPost, "/v1/router", autoByok).code, "an OpenRouter key satisfies Auto")
	assert.Equal(t, http.StatusInternalServerError, v.do(pro, http.MethodPost, "/v1/router", body).code, "a duplicate name hits the unique key")

	created := v.ok(pro, http.MethodPost, "/v1/router", strings.Replace(body, `"name":"X"`, `"name":"Y"`, 1))
	assert.Equal(t, []any{map[string]any{"provider": "openRouter", "model": "auto"}}, created["defaultAction"].(map[string]any)["routes"], "managed routes are scrubbed")
	id := created["id"].(string)
	v.errIs(v.do(pro, http.MethodDelete, "/v1/router/"+v.defaultProfile(pro)["id"].(string), ""), http.StatusConflict, "router_default_profile_cannot_be_deleted")
	other := v.user("pro")
	v.errIs(v.do(other, http.MethodDelete, "/v1/router/"+id, ""), http.StatusNotFound, "router_profile_not_found")
	assert.Equal(t, http.StatusNoContent, v.do(pro, http.MethodDelete, "/v1/router/"+id, "").code)
}

// MARK: - Bindings

func TestBindingsOwnershipAndScopes(t *testing.T) {
	v := setup(t, false)
	scoped := v.ok(lrBind, http.MethodPost, "/v1/router", `{"name":"S","mode":"byok","objective":{"quality":100,"cost":0,"latency":0},"budget":{},"defaultAction":{"kind":"sequential","routes":[{"provider":"openai","model":"gpt-4o"}],"retryPolicy":"fast"},"routingPolicy":"balanced"}`)
	pid := `{"profileID":"` + scoped["id"].(string) + `"}`

	b := v.ok(lrBind, http.MethodPut, "/v1/router/bindings/space/"+bindSp, pid)
	assert.Equal(t, "space", b["scope"])
	assert.Equal(t, bindSp, b["scopeID"])
	v.ok(lrBind, http.MethodPut, "/v1/router/bindings/workflow/"+bindWf, pid)
	v.ok(lrBind, http.MethodPut, "/v1/router/bindings/job/morning-brief", pid)
	user := v.ok(lrBind, http.MethodPut, "/v1/router/bindings/user/anyone", pid)
	assert.Equal(t, strings.ToUpper(lrBind.String()), user["scopeID"], "the user scope binds the caller")

	v.errIs(v.do(lrBind, http.MethodPut, "/v1/router/bindings/space/"+otherSp, pid), http.StatusNotFound, "space_not_found")
	v.errIs(v.do(lrBind, http.MethodPut, "/v1/router/bindings/workflow/"+otherWf, pid), http.StatusNotFound, "workflow_not_found")
	v.errIs(v.do(lrBind, http.MethodPut, "/v1/router/bindings/job/nightly", pid), http.StatusNotFound, "job_not_found")
	v.errIs(v.do(lrBind, http.MethodPut, "/v1/router/bindings/team/x", pid), http.StatusBadRequest, "invalid_router_scope")
	v.errIs(v.do(lrBind, http.MethodPut, "/v1/router/bindings/job/morning-brief", `{"profileID":"`+uuid.NewString()+`"}`), http.StatusNotFound, "router_profile_not_found")

	free := v.user("free")
	v.errIs(v.do(free, http.MethodPut, "/v1/router/bindings/space/"+bindSp, pid), http.StatusForbidden, "router_scoped_profile_requires_pro")

	list := v.ok(lrBind, http.MethodGet, "/v1/router/bindings", "")
	assert.Len(t, list["bindings"], 4)
	assert.Equal(t, http.StatusNoContent, v.do(lrBind, http.MethodDelete, "/v1/router/bindings/job/morning-brief", "").code)
	assert.Equal(t, http.StatusInternalServerError, v.do(lrBind, http.MethodDelete, "/v1/router/bindings/user/x", "").code)
	assert.Len(t, v.ok(lrBind, http.MethodGet, "/v1/router/bindings", "")["bindings"], 3)
}

// MARK: - Dashboard

// Swift's dashboard is a 500 on every call; FixDashboard serves it.
func TestDashboardSumsThisMonthAndCarriesTheDefaultBudget(t *testing.T) {
	v := setup(t, false)
	def := v.defaultProfile(lrDash)
	r := v.do(lrDash, http.MethodPut, "/v1/router/"+def["id"].(string),
		`{"name":"D","mode":"managed","objective":{"quality":40,"cost":40,"latency":20},"budget":{"softLimitUsdMicros":500,"hardLimitUsdMicros":2000},"defaultAction":{"kind":"sequential","routes":[{"provider":"openRouter","model":"m"}],"retryPolicy":"fast"}}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	d := v.ok(lrDash, http.MethodGet, "/v1/router/dashboard", "")
	assert.EqualValues(t, 2, d["requests"])
	assert.EqualValues(t, 1, d["successfulRequests"])
	assert.EqualValues(t, 2, d["fallbackCount"])
	assert.EqualValues(t, 200, d["tokensIn"])
	assert.EqualValues(t, 40, d["tokensOut"])
	assert.EqualValues(t, 1000, d["estimatedCostUsdMicros"])
	assert.EqualValues(t, 500, d["monthlySoftLimitUsdMicros"])
	assert.EqualValues(t, 2000, d["monthlyHardLimitUsdMicros"])
}

// MARK: - Parallel (ParallelExecutorTests: the executor is a hook, owner decision 7)

func TestParallelRoutesAre403WhileParallelIsOff(t *testing.T) {
	v := setup(t, false)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/router/parallel/executions/stream", `{"prompt":"p","strategy":"auto"}`},
		{http.MethodGet, "/v1/router/parallel/executions", ""},
		{http.MethodGet, "/v1/router/parallel/executions/" + ultExec, ""},
		{http.MethodDelete, "/v1/router/parallel/executions/" + ultExec, ""},
		{http.MethodGet, "/v1/router/synthesis-presets", ""},
		{http.MethodPost, "/v1/router/synthesis-presets", `{"name":"P","prompt":"Q"}`},
	} {
		v.errIs(v.do(lrUlt, c.method, c.path, c.body), http.StatusForbidden, "router_parallel_requires_ultimate")
	}
}

func TestParallelHistoryAndPresetsForUltimateWhenOn(t *testing.T) {
	v := setup(t, true)
	v.errIs(v.do(lrPro, http.MethodGet, "/v1/router/parallel/executions", ""), http.StatusForbidden, "router_parallel_requires_ultimate")

	list := v.ok(lrUlt, http.MethodGet, "/v1/router/parallel/executions", "")
	require.Len(t, list["executions"], 1)
	detail := v.ok(lrUlt, http.MethodGet, "/v1/router/parallel/executions/"+ultExec, "")
	assert.Equal(t, "Compare", detail["prompt"])
	assert.Equal(t, http.StatusNotFound, v.do(v.user("ultimate"), http.MethodGet, "/v1/router/parallel/executions/"+ultExec, "").code)

	created := v.ok(lrUlt, http.MethodPost, "/v1/router/synthesis-presets", `{"name":"Short","prompt":"Be brief."}`)
	id := created["id"].(string)
	updated := v.ok(lrUlt, http.MethodPut, "/v1/router/synthesis-presets/"+id, `{"name":"Shorter","prompt":"Be briefer."}`)
	assert.Equal(t, "Shorter", updated["name"])
	assert.Len(t, v.ok(lrUlt, http.MethodGet, "/v1/router/synthesis-presets", "")["presets"], 2)
	assert.Equal(t, http.StatusNoContent, v.do(lrUlt, http.MethodDelete, "/v1/router/synthesis-presets/"+id, "").code)
	assert.Equal(t, http.StatusNoContent, v.do(lrUlt, http.MethodDelete, "/v1/router/parallel/executions/"+ultExec, "").code)
	assert.Empty(t, v.ok(lrUlt, http.MethodGet, "/v1/router/parallel/executions", "")["executions"])
}

// MARK: - Spec

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t, true)
	def := v.defaultProfile(lrPro)["id"].(string)
	for _, c := range []struct {
		user               uuid.UUID
		method, path, body string
	}{
		{lrPro, http.MethodGet, "/v1/router", ""},
		{lrPro, http.MethodPost, "/v1/router", `{"name":"Spec","mode":"byok","objective":{"quality":100,"cost":0,"latency":0},"budget":{},"defaultAction":{"kind":"sequential","routes":[{"provider":"openai","model":"gpt-4o"}],"retryPolicy":"fast"},"routingPolicy":"balanced"}`},
		{lrPro, http.MethodPut, "/v1/router/" + def, `{"name":"Default","mode":"managed","objective":{"quality":100,"cost":0,"latency":0},"budget":{},"defaultAction":{"kind":"sequential","routes":[{"provider":"openRouter","model":"m"}],"retryPolicy":"fast"}}`},
		{lrPro, http.MethodGet, "/v1/router/catalog", ""},
		{lrPro, http.MethodGet, "/v1/router/bindings", ""},
		{lrPro, http.MethodPut, "/v1/router/bindings/user/me", `{"profileID":"` + def + `"}`},
		{lrDash, http.MethodGet, "/v1/router/dashboard", ""},
		{lrUlt, http.MethodGet, "/v1/router/parallel/executions", ""},
		{lrUlt, http.MethodGet, "/v1/router/parallel/executions/" + ultExec, ""},
		{lrUlt, http.MethodGet, "/v1/router/synthesis-presets", ""},
		{lrUlt, http.MethodPost, "/v1/router/synthesis-presets", `{"name":"P","prompt":"Q"}`},
	} {
		r := v.do(c.user, c.method, c.path, c.body)
		require.Less(t, r.code, 300, "%s %s: %s", c.method, c.path, r.body)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		outp := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: r.code, Header: http.Header{"Content-Type": {r.ct}}}
		outp.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, outp), "%s %s: %s", c.method, c.path, r.body)
	}
}
