package meproviders

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/migrations"
	"lumina/internal/modules/llmrouter"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
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

// Parity seed users (parity/seed/me-providers.sql).
var (
	eve = uuid.MustParse("3e9a0000-0000-4000-8000-0000000000a5")
	fay = uuid.MustParse("3e9a0000-0000-4000-8000-0000000000a6")
	gus = uuid.MustParse("3e9a0000-0000-4000-8000-0000000000a7")
	jon = uuid.MustParse("3e9a0000-0000-4000-8000-0000000000aa")
)

// upstream is a stub provider: every outbound call of the module lands here,
// whatever host it names, and is recorded.
type upstream struct {
	mu    sync.Mutex
	calls []string
	srv   *httptest.Server
}

func (u *upstream) record(r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, r.Method+" "+r.Host+r.URL.Path)
}

func (u *upstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.calls...)
}

type redirect struct{ target *url.URL }

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host = r.target.Scheme, r.target.Host
	out.Host = req.URL.Host
	return http.DefaultTransport.RoundTrip(out)
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		u.record(r)
		if r.Host == "api.anthropic.com" {
			_, _ = io.WriteString(w, `{"data":[{"id":"claude-sonnet-4-5","display_name":"Claude Sonnet 4.5"}]}`)
			return
		}
		if r.Header.Get("Authorization") == "Bearer sk-bad" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini","name":"Mini"}]}`)
	})
	mux.HandleFunc("GET /v1beta/models", func(w http.ResponseWriter, r *http.Request) {
		u.record(r)
		_, _ = io.WriteString(w, `{"models":[{"name":"models/gemini-2.5-flash","supportedGenerationMethods":["generateContent"]}]}`)
	})
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, r *http.Request) {
		u.record(r)
		_, _ = io.WriteString(w, `{"models":[{"name":"llama3:8b"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		u.record(r)
		if r.Header.Get("Authorization") == "Bearer sk-bad" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"tools"`) {
			_, _ = io.WriteString(w, `{"id":"c","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"1","type":"function","function":{"name":"lookup_job_status","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"c","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	u.srv = httptest.NewServer(mux)
	t.Cleanup(u.srv.Close)
	return u
}

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
	m    *Module
	up   *upstream
}

type options struct{ noBox, freeLane bool }

func setup(t *testing.T, o ...options) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "me-providers.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	up := newUpstream(t)
	target, _ := url.Parse(up.srv.URL)
	client := &http.Client{Transport: redirect{target}}
	var box *secretbox.Box
	if len(o) == 0 || !o[0].noBox {
		var err error
		box, err = secretbox.New(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
		require.NoError(t, err)
	}
	// The free lane off, as Swift's dbTestReaderFreeLaneOff: on, it overrides a
	// BYOK choice that has no usable key.
	vars := map[string]string{"FREELANE_ENABLED": "false"}
	if len(o) > 0 && o[0].freeLane {
		vars = map[string]string{}
	}
	stack := llm.NewStack(llm.StackConfig{Getenv: func(k string) string { return vars[k] }, Pool: pool, Box: box, Client: client})
	identify := auth.Identify(keys, auth.PGUsers(pool))
	limits := ratelimit.NewStore()
	m := New(pool, Options{Identify: identify, Limits: limits, Stack: stack, Box: box, Client: client})
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth: auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{
			m,
			llmrouter.New(pool, llmrouter.Options{Identify: identify, Limits: limits, Stack: stack}),
		},
	})
	return env{t: t, e: e, pool: pool, m: m, up: up}
}

// user inserts a fresh account with its personal vault.
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

func (r resp) object(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(r.body, &m), string(r.body))
	return m
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
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	return r.object(v.t)
}

func (v env) errIs(r resp, status int, msg string) {
	v.t.Helper()
	require.Equal(v.t, status, r.code, string(r.body))
	b, _ := json.Marshal(msg)
	require.JSONEq(v.t, `{"error":{"message":`+string(b)+`}}`, string(r.body))
}

// MARK: - LLMPreferencesControllerTests

func (v env) prefs(user uuid.UUID) map[string]any {
	return v.ok(user, http.MethodGet, "/v1/me/preferences/llm", "")
}

func (v env) putPrefs(user uuid.UUID, body string) resp {
	return v.do(user, http.MethodPut, "/v1/me/preferences/llm", body)
}

// activeProfile is the default profile /v1/router reports.
func (v env) activeProfile(user uuid.UUID) map[string]any {
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

func routeIDs(action any) []string {
	var out []string
	for _, r := range action.(map[string]any)["routes"].([]any) {
		m := r.(map[string]any)
		out = append(out, m["provider"].(string)+":"+m["model"].(string))
	}
	return out
}

func TestPreferencesGetReturnsManagedDefaultWhenNoRowExists(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	p := v.prefs(u)
	assert.Equal(t, "managed", p["mode"])
	assert.Empty(t, p["fallbackChain"])
	assert.Equal(t, "openRouter", p["primaryProvider"])
	assert.Equal(t, llm.GenericBrainName, p["primaryModel"])

	// Unauthenticated returns 401.
	assert.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, "/v1/me/preferences/llm", "").code)

	// put without mode defaults to managed.
	r := v.putPrefs(u, `{"primaryProvider":"anthropic","primaryModel":"legacy-client-model","fallbackChain":[]}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	put := r.object(t)
	assert.Equal(t, "managed", put["mode"])
	assert.Equal(t, "openRouter", put["primaryProvider"])
	assert.Equal(t, llm.GenericBrainName, put["primaryModel"])
}

func TestPreferencesPutCanonicalizesManagedModeToTheServerRoute(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	r := v.putPrefs(u, `{"mode":"managed","primaryProvider":"anthropic","primaryModel":"stale-client-model","fallbackChain":[{"provider":"openai","model":"stale-fallback"}],"allowedProviders":["anthropic"],"blockedProviders":["openRouter"]}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	for _, p := range []map[string]any{r.object(t), v.prefs(u)} {
		assert.Equal(t, "managed", p["mode"])
		assert.Equal(t, "openRouter", p["primaryProvider"])
		assert.Equal(t, llm.GenericBrainName, p["primaryModel"])
		assert.Empty(t, p["fallbackChain"])
		assert.Empty(t, p["allowedProviders"])
		assert.Empty(t, p["blockedProviders"])
	}
	// The row keeps the backend's real model, not the client's.
	var stored string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT primary_model FROM user_llm_preferences WHERE tenant_id = $1`, u).Scan(&stored))
	assert.Equal(t, llm.ManagedModel, stored)

	active := v.activeProfile(u)
	assert.Equal(t, "managed", active["mode"])
	assert.Equal(t, []any{"openRouter"}, active["allowedProviders"])
	assert.Empty(t, active["blockedProviders"])
	assert.Equal(t, []string{"openRouter:" + llm.GenericModelID}, routeIDs(active["defaultAction"]))
	assert.NotContains(t, routeIDs(active["defaultAction"]), "openRouter:"+llm.ManagedModel)
}

func TestPreferencesPutPersistsByokWithPrimaryAndFallback(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	r := v.putPrefs(u, `{"mode":"byok","primaryProvider":"anthropic","primaryModel":"claude-opus-4-7","fallbackChain":[{"provider":"openai","model":"gpt-4o"}]}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	put := r.object(t)
	assert.Equal(t, "byok", put["mode"])
	assert.Equal(t, "anthropic", put["primaryProvider"])
	assert.Equal(t, "claude-opus-4-7", put["primaryModel"])
	chain := put["fallbackChain"].([]any)
	require.Len(t, chain, 1)
	assert.Equal(t, "openai", chain[0].(map[string]any)["provider"])

	active := v.activeProfile(u)
	assert.Equal(t, "byok", active["mode"])
	assert.Equal(t, []string{"anthropic:claude-opus-4-7", "openai:gpt-4o"}, routeIDs(active["defaultAction"]))
}

func TestPreferencesByokSaveCarryingTheLabelKeepsTheStoredModel(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	require.Equal(t, http.StatusOK, v.putPrefs(u, `{"mode":"byok","primaryProvider":"anthropic","primaryModel":"claude-opus-4-7","fallbackChain":[]}`).code)
	r := v.putPrefs(u, `{"mode":"byok","primaryProvider":"anthropic","primaryModel":"`+llm.GenericBrainName+`","fallbackChain":[]}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	p := v.prefs(u)
	assert.Equal(t, "byok", p["mode"])
	assert.Equal(t, "claude-opus-4-7", p["primaryModel"])
}

func TestPreferencesByokSaveCarryingTheLabelWithNothingToRestoreIsRefused(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	v.errIs(v.putPrefs(u, `{"mode":"byok","primaryProvider":"openRouter","primaryModel":"`+llm.GenericBrainName+`","fallbackChain":[]}`), http.StatusBadRequest, "primary_model_placeholder")
	assert.Equal(t, "managed", v.prefs(u)["mode"])
}

// The managed row holds the platform's real model; restoring from it would pin
// that model to the user's own key and reveal it.
func TestPreferencesAStoredManagedModelIsNeverUsedAsTheRestore(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	require.Equal(t, http.StatusOK, v.putPrefs(u, `{"mode":"managed","primaryProvider":"openRouter","primaryModel":"stale-client-model","fallbackChain":[]}`).code)
	r := v.putPrefs(u, `{"mode":"byok","primaryProvider":"openRouter","primaryModel":"`+llm.GenericBrainName+`","fallbackChain":[]}`)
	assert.Equal(t, http.StatusBadRequest, r.code, string(r.body))
	assert.Equal(t, "managed", v.prefs(u)["mode"])
}

func TestPreferencesTheRestoreOnlyAppliesOnTheSameProvider(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	require.Equal(t, http.StatusOK, v.putPrefs(u, `{"mode":"byok","primaryProvider":"anthropic","primaryModel":"claude-opus-4-7","fallbackChain":[]}`).code)
	r := v.putPrefs(u, `{"mode":"byok","primaryProvider":"openai","primaryModel":"`+llm.GenericBrainName+`","fallbackChain":[]}`)
	assert.Equal(t, http.StatusBadRequest, r.code, string(r.body))
	assert.Equal(t, "claude-opus-4-7", v.prefs(u)["primaryModel"])
}

func TestPreferencesTheHiddenRouteIDIsRefusedAsAByokModel(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	v.errIs(v.putPrefs(u, `{"mode":"byok","primaryProvider":"openRouter","primaryModel":"`+llm.GenericModelID+`","fallbackChain":[]}`), http.StatusBadRequest, "primary_model_placeholder")
}

func TestPreferencesAFallbackStepCarryingTheLabelIsRefused(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	v.errIs(v.putPrefs(u, `{"mode":"byok","primaryProvider":"anthropic","primaryModel":"claude-opus-4-7","fallbackChain":[{"provider":"openRouter","model":"`+llm.GenericBrainName+`"}]}`), http.StatusBadRequest, "fallback_model_placeholder")
}

// With the free lane on, a BYOK choice with no usable key is reported as the
// managed route the router will really take. A usable key restores the choice.
func TestPreferencesFreeLaneOverridesAnUnusableByokChoice(t *testing.T) {
	v := setup(t, options{freeLane: true})
	for _, u := range []uuid.UUID{gus, jon} {
		p := v.prefs(u)
		assert.Equal(t, "managed", p["mode"], u)
		assert.Equal(t, llm.GenericBrainName, p["primaryModel"], u)
	}
	require.Equal(t, http.StatusOK, v.do(gus, http.MethodPut, "/v1/me/providers/openai", `{"kind":"apiKey","apiKey":"sk-good"}`).code)
	assert.Equal(t, "byok", v.prefs(gus)["mode"])
}

// MARK: - Credentials, test and models

func TestProvidersPutSealsTheKeyAndNeverEchoesIt(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	r := v.do(u, http.MethodPut, "/v1/me/providers/openai", `{"kind":"apiKey","apiKey":"sk-secret-1","label":"Main"}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	assert.NotContains(t, string(r.body), "sk-secret-1")

	var ct []byte
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT ciphertext FROM user_provider_credentials WHERE tenant_id = $1 AND provider = 'openai'`, u).Scan(&ct))
	assert.NotContains(t, string(ct), "sk-secret-1")

	cred, err := v.m.opts.Stack.Credentials.Credential(context.Background(), llm.OpenAI, u)
	require.NoError(t, err)
	require.NotNil(t, cred)
	require.NotNil(t, cred.APIKey)
	assert.Equal(t, "sk-secret-1", *cred.APIKey, "the router's cache sees the write")

	list := v.ok(u, http.MethodGet, "/v1/me/providers", "")
	assert.NotContains(t, mustJSON(t, list), "sk-secret-1")

	v.errIs(v.do(u, http.MethodPut, "/v1/me/providers/xai", `{"kind":"apiKey"}`), http.StatusBadRequest, "api_key_required")
	v.errIs(v.do(u, http.MethodPut, "/v1/me/providers/custom", `{"kind":"hostURL"}`), http.StatusBadRequest, "base_url_required")

	require.Equal(t, http.StatusNoContent, v.do(u, http.MethodDelete, "/v1/me/providers/openai", "").code)
	cred, err = v.m.opts.Stack.Credentials.Credential(context.Background(), llm.OpenAI, u)
	require.NoError(t, err)
	assert.Nil(t, cred, "delete invalidates the cache")
}

func TestProvidersTestStampsVerifiedAndRecordsTheToolVerdict(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	require.Equal(t, http.StatusOK, v.do(u, http.MethodPut, "/v1/me/providers/openai", `{"kind":"apiKey","apiKey":"sk-good"}`).code)
	out := v.ok(u, http.MethodPost, "/v1/me/providers/openai/test", "")
	assert.NotEmpty(t, out["verifiedAt"])
	v.m.Wait()

	var verified, probed bool
	var supports *bool
	require.NoError(t, v.pool.QueryRow(context.Background(), `
		SELECT verified_at IS NOT NULL, supports_tools, tools_probed_at IS NOT NULL
		FROM user_provider_credentials WHERE tenant_id = $1 AND provider = 'openai'`, u).Scan(&verified, &supports, &probed))
	assert.True(t, verified)
	assert.True(t, probed)
	require.NotNil(t, supports)
	assert.True(t, *supports)
	assert.Contains(t, v.up.seen(), "POST api.openai.com/v1/chat/completions")
}

func TestProvidersTestRecordsARejectedKey(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	require.Equal(t, http.StatusOK, v.do(u, http.MethodPut, "/v1/me/providers/openai", `{"kind":"apiKey","apiKey":"sk-bad"}`).code)
	r := v.do(u, http.MethodPost, "/v1/me/providers/openai/test", "")
	assert.GreaterOrEqual(t, r.code, 400, string(r.body))
	v.m.Wait()
	var code *string
	require.NoError(t, v.pool.QueryRow(context.Background(), `
		SELECT last_failure_code FROM user_provider_credentials WHERE tenant_id = $1 AND provider = 'openai'`, u).Scan(&code))
	require.NotNil(t, code)
}

func TestProvidersModelsListsLiveModelsPerProviderShape(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	require.Equal(t, http.StatusOK, v.do(u, http.MethodPut, "/v1/me/providers/anthropic", `{"kind":"apiKey","apiKey":"sk-ant"}`).code)
	require.Equal(t, http.StatusOK, v.do(u, http.MethodPut, "/v1/me/providers/gemini", `{"kind":"apiKey","apiKey":"AIza-x"}`).code)
	require.Equal(t, http.StatusOK, v.do(u, http.MethodPut, "/v1/me/providers/ollama", `{"kind":"hostURL","baseUrl":"http://ollama.local:11434"}`).code)

	ids := func(provider string) []string {
		out := v.ok(u, http.MethodGet, "/v1/me/providers/"+provider+"/models", "")
		var got []string
		for _, m := range out["models"].([]any) {
			got = append(got, m.(map[string]any)["id"].(string))
		}
		return got
	}
	assert.Equal(t, []string{"claude-sonnet-4-5"}, ids("anthropic"))
	assert.Equal(t, []string{"gemini-2.5-flash"}, ids("gemini"))
	assert.Equal(t, []string{"llama3:8b"}, ids("ollama"))
	assert.Contains(t, v.up.seen(), "GET ollama.local:11434/api/tags")
}

func TestProvidersRoutesAreUnmountedWithoutASecretBox(t *testing.T) {
	v := setup(t, options{noBox: true})
	u := v.user("trial")
	assert.Equal(t, http.StatusNotFound, v.do(u, http.MethodGet, "/v1/me/providers", "").code)
	assert.Equal(t, http.StatusOK, v.do(u, http.MethodGet, "/v1/me/preferences/llm", "").code)
	assert.Equal(t, http.StatusOK, v.do(u, http.MethodGet, "/v1/me/connections", "").code)
}

// MARK: - Connections

func TestConnectionsSummaryOrderAndHealth(t *testing.T) {
	v := setup(t)
	out := v.ok(eve, http.MethodGet, "/v1/me/connections", "")
	byID := map[string]map[string]any{}
	var order []string
	for _, c := range out["connections"].([]any) {
		m := c.(map[string]any)
		byID[m["id"].(string)] = m
		order = append(order, m["id"].(string))
	}
	assert.Equal(t, "server:api", order[0])
	assert.Equal(t, "error", byID["calendar:google"]["health"], "a grant that needs re-auth")
	assert.Equal(t, "error", byID["provider:anthropic"]["health"], "a failure newer than the last success")
	assert.Equal(t, "connected", byID["provider:ollama"]["health"])

	fayOut := v.ok(fay, http.MethodGet, "/v1/me/connections", "")
	for _, c := range fayOut["connections"].([]any) {
		m := c.(map[string]any)
		if m["id"] == "calendar:google" {
			assert.Equal(t, "needs_setup", m["health"], "a Gmail-only grant is not a calendar")
		}
	}
}

func TestConnectionsEventsDropUnknownRowsAndClampTheLimit(t *testing.T) {
	v := setup(t)
	events := func(q string) []any {
		return v.ok(fay, http.MethodGet, "/v1/me/connections/events"+q, "")["events"].([]any)
	}
	assert.Len(t, events(""), 2, "the unknown kind and the unknown severity are dropped")
	assert.Len(t, events("?limit=0"), 1, "clamped to 1, which is the newest row")
	assert.Len(t, events("?limit=1000"), 2)

	out := v.ok(fay, http.MethodPost, "/v1/me/connections/test-all", "")
	results := out["results"].([]any)
	require.NotEmpty(t, results)
	var n int
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM connection_diagnostic_events WHERE tenant_id = $1`, fay).Scan(&n))
	assert.Equal(t, 4+len(results), n, "one event per result")
}

// MARK: - Spec

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	u := v.user("trial")
	for _, c := range []struct {
		user               uuid.UUID
		method, path, body string
	}{
		{u, http.MethodGet, "/v1/me/providers/catalog", ""},
		{u, http.MethodPut, "/v1/me/providers/openai", `{"kind":"apiKey","apiKey":"sk-good","label":"Main"}`},
		{u, http.MethodGet, "/v1/me/providers", ""},
		{u, http.MethodPost, "/v1/me/providers/openai/test", ""},
		{u, http.MethodGet, "/v1/me/providers/openai/models", ""},
		{u, http.MethodPost, "/v1/me/providers/openRouter/pool", `{"apiKey":"sk-or","label":"One"}`},
		{u, http.MethodGet, "/v1/me/providers/openRouter/pool", ""},
		{u, http.MethodDelete, "/v1/me/providers/openai", ""},
		{u, http.MethodGet, "/v1/me/preferences/llm", ""},
		{u, http.MethodPut, "/v1/me/preferences/llm", `{"mode":"byok","primaryProvider":"openai","primaryModel":"gpt-4o","fallbackChain":[]}`},
		{eve, http.MethodGet, "/v1/me/connections", ""},
		{eve, http.MethodPost, "/v1/me/connections/test-all", ""},
		{fay, http.MethodGet, "/v1/me/connections/events", ""},
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
		outp.SetBodyBytes(withoutPhoton(t, r.body))
		require.NoError(t, openapi3filter.ValidateResponse(ctx, outp), "%s %s: %s", c.method, c.path, r.body)
	}
	v.m.Wait()
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// withoutPhoton drops the photon gateway row before validation: Swift's
// HermesGatewayID (LuminaVaultShared) has photon, and the summary lists it, but
// the spec's HermesGatewayID enum does not. Spec drift, named in the PR.
func withoutPhoton(t *testing.T, body []byte) []byte {
	t.Helper()
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	list, ok := m["connections"].([]any)
	if !ok {
		return body
	}
	kept := []any{}
	for _, c := range list {
		if c.(map[string]any)["gatewayID"] != "photon" {
			kept = append(kept, c)
		}
	}
	m["connections"] = kept
	return []byte(mustJSON(t, m))
}
