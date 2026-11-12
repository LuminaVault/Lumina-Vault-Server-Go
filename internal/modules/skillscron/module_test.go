package skillscron_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/hermescontainers"
	"lumina/internal/hermesdash"
	"lumina/internal/hermesdash/hermesdashtest"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/migrations"
	"lumina/internal/modules/skillscron"
	"lumina/internal/secretbox"
	"lumina/internal/ssrfguard"
	"lumina/parity/scenario"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Swift server has no route tests for CronBridgeController; its one Swift
// test (HermesDashboardClientTests, `cron bridge keeps its legacy BYO error
// codes`) is ported in internal_test.go. These cover the controller and service
// end to end on stubs: no docker, no network, no real key.

var keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

// containers is a stub HermesContainerManager: a handle per tenant, or an error.
type containers struct {
	mu      sync.Mutex
	handles map[uuid.UUID]*hermescontainers.Handle
	broken  map[uuid.UUID]bool
}

func (c *containers) Handle(_ context.Context, tenant uuid.UUID) (*hermescontainers.Handle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken[tenant] {
		return nil, secretbox.ErrDecryptionFailed
	}
	return c.handles[tenant], nil
}

// docker is a stub DockerExec: canned results keyed by argv, recording calls.
type docker struct {
	mu      sync.Mutex
	calls   [][]string
	results map[string]hermescontainers.Result
	spawn   error
}

func (d *docker) Exec(_ context.Context, container string, command []string, stdin []byte) (hermescontainers.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if stdin != nil {
		return hermescontainers.Result{}, errors.New("unexpected stdin")
	}
	d.calls = append(d.calls, append([]string{container}, command...))
	if d.spawn != nil {
		return hermescontainers.Result{}, &hermescontainers.SpawnError{Err: d.spawn}
	}
	if r, ok := d.results[strings.Join(command, " ")]; ok {
		return r, nil
	}
	return hermescontainers.Result{}, nil
}

func (d *docker) argv() [][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]string(nil), d.calls...)
}

// transport is a stub routed LLM transport answering one completion.
type transport struct {
	content string
	fail    bool
	calls   int
}

func (s *transport) ChatCompletions(context.Context, []byte, string, string) (llm.Metadata, error) {
	s.calls++
	if s.fail {
		return llm.Metadata{}, errors.New("upstream down")
	}
	body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": s.content}}}})
	return llm.Metadata{Data: body}, nil
}

func (s *transport) ChatStream(context.Context, []byte, string, string) llm.Stream { return nil }

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	box   *secretbox.Box
	srv   http.Handler
	ctr   *containers
	dock  *docker
	http  *hermesdashtest.Stub
	llm   *transport
	svc   *skillscron.Service
	guard ssrfguard.Guard
}

func setup(t *testing.T) *env {
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
	v := &env{
		t: t, pool: pool, box: box,
		ctr:  &containers{handles: map[uuid.UUID]*hermescontainers.Handle{}, broken: map[uuid.UUID]bool{}},
		dock: &docker{results: map[string]hermescontainers.Result{}},
		http: hermesdashtest.New(),
		llm:  &transport{content: `{"isJob": false}`},
		// Public answers for every name (IP literals are themselves), so the dashboard URL passes the guard
		// without DNS; HTTPS required, private ranges refused (byoHermes defaults).
		guard: ssrfguard.Guard{RequireHTTPS: true, Resolver: func(_ context.Context, host string) ([]string, error) {
			if net.ParseIP(host) != nil {
				return []string{host}, nil
			}
			return []string{"203.0.113.10"}, nil
		}},
	}
	v.svc = &skillscron.Service{
		Pool: pool, Box: box, Guard: v.guard, Containers: v.ctr, Docker: v.dock, HTTP: v.http,
		Transport: v.llm, Model: "test-model",
	}
	v.srv = httpserver.New(config.Config{}, httpserver.Deps{
		Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: []httpserver.Module{skillscron.New(v.svc)},
	})
	return v
}

func (v *env) user() uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
		VALUES ($1, $2, 'x', $3, now(), now())`, id, id.String()+"@t", "u"+strings.ReplaceAll(id.String(), "-", "")[:12])
	require.NoError(v.t, err)
	return id
}

func (v *env) managed(tenant uuid.UUID) string {
	name := "hermes-tenant-" + strings.ToLower(tenant.String())
	v.ctr.mu.Lock()
	defer v.ctr.mu.Unlock()
	v.ctr.handles[tenant] = &hermescontainers.Handle{TenantID: tenant, ContainerName: name, Port: 8642}
	return name
}

// byo stores dashboard credentials as SetBYOConfig would.
func (v *env) byo(tenant uuid.UUID, rawURL, token string) {
	v.t.Helper()
	require.NoError(v.t, v.svc.SetBYOConfig(context.Background(), tenant, rawURL, token))
}

type resp struct {
	code int
	body string
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.body), &m), r.body)
	return m
}

func (v *env) do(user uuid.UUID, method, path, body string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		token, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	v.srv.ServeHTTP(rec, req)
	return resp{code: rec.Code, body: rec.Body.String()}
}

func errorBody(msg string) string { return `{"error":{"message":"` + msg + `"}}` }

const jobsJSON = `{"jobs":[{"id":"a1","name":"Morning digest","schedule":"0 8 * * *","deliver":"telegram","last_run":"2026-11-01T08:00:00Z","enabled":true},
	{"id":"b2","schedule":{"kind":"cron","expr":"*/5 * * * *"},"enabled":false,"no_agent":true}]}`

func TestCronBridgeRoutes(t *testing.T) {
	v := setup(t)

	t.Run("every route needs a token", func(t *testing.T) {
		for _, r := range [][2]string{
			{http.MethodGet, "/v1/me/hermes/cron"},
			{http.MethodPost, "/v1/me/hermes/cron"},
			{http.MethodPost, "/v1/me/hermes/cron/preview"},
			{http.MethodPut, "/v1/me/hermes/cron/config"},
			{http.MethodPost, "/v1/me/hermes/cron/x/pause"},
			{http.MethodPost, "/v1/me/hermes/cron/x/resume"},
			{http.MethodDelete, "/v1/me/hermes/cron/x"},
		} {
			assert.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, r[0], r[1], "").code, r[1])
		}
	})

	t.Run("no container and no BYO config is 404 no_hermes_cron_source", func(t *testing.T) {
		u := v.user()
		r := v.do(u, http.MethodGet, "/v1/me/hermes/cron", "")
		assert.Equal(t, http.StatusNotFound, r.code)
		assert.JSONEq(t, errorBody("no_hermes_cron_source"), r.body)
		r = v.do(u, http.MethodPost, "/v1/me/hermes/cron", `{"schedule":"0 9 * * *"}`)
		assert.Equal(t, http.StatusNotFound, r.code)
		assert.JSONEq(t, errorBody("no_hermes_cron_source"), r.body)
	})

	t.Run("a handle that fails to load counts as no container for list", func(t *testing.T) {
		u := v.user()
		v.ctr.mu.Lock()
		v.ctr.broken[u] = true
		v.ctr.mu.Unlock()
		r := v.do(u, http.MethodGet, "/v1/me/hermes/cron", "")
		assert.Equal(t, http.StatusNotFound, r.code)
		assert.JSONEq(t, errorBody("no_hermes_cron_source"), r.body)
		// ...but the managed-only mutations surface the error (try, not try?).
		r = v.do(u, http.MethodPost, "/v1/me/hermes/cron/a1/pause", "")
		assert.Equal(t, http.StatusInternalServerError, r.code)
		assert.Empty(t, r.body)
	})

	t.Run("pause, resume and delete are managed-only", func(t *testing.T) {
		u := v.user()
		v.byo(u, "https://hermes.example.com", "tok")
		for _, r := range []resp{
			v.do(u, http.MethodPost, "/v1/me/hermes/cron/a1/pause", ""),
			v.do(u, http.MethodPost, "/v1/me/hermes/cron/a1/resume", ""),
			v.do(u, http.MethodDelete, "/v1/me/hermes/cron/a1", ""),
		} {
			assert.Equal(t, http.StatusNotFound, r.code)
			assert.JSONEq(t, errorBody("no_hermes_container"), r.body)
		}
	})

	t.Run("managed list reads jobs.json in the container", func(t *testing.T) {
		u := v.user()
		name := v.managed(u)
		v.dock.results["cat /opt/data/cron/jobs.json"] = hermescontainers.Result{Stdout: jobsJSON}
		defer delete(v.dock.results, "cat /opt/data/cron/jobs.json")
		before := len(v.dock.argv())
		r := v.do(u, http.MethodGet, "/v1/me/hermes/cron", "")
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.JSONEq(t, `{"source":"managed","jobs":[
			{"id":"a1","name":"Morning digest","schedule":"0 8 * * *","deliver":"telegram","lastRun":"2026-11-01T08:00:00Z","status":"active","mode":"agent"},
			{"id":"b2","schedule":"*/5 * * * *","status":"paused","mode":"script"}]}`, r.body)
		assert.Equal(t, [][]string{{name, "cat", "/opt/data/cron/jobs.json"}}, v.dock.argv()[before:])
	})

	t.Run("managed wins over a BYO config, and a missing jobs.json is no jobs", func(t *testing.T) {
		u := v.user()
		v.byo(u, "https://hermes.example.com", "tok")
		v.managed(u)
		v.dock.results["cat /opt/data/cron/jobs.json"] = hermescontainers.Result{ExitCode: 1, Stderr: "No such file"}
		defer delete(v.dock.results, "cat /opt/data/cron/jobs.json")
		r := v.do(u, http.MethodGet, "/v1/me/hermes/cron", "")
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.JSONEq(t, `{"source":"managed","jobs":[]}`, r.body)
	})

	t.Run("managed create runs the hermes CLI with argv, then lists", func(t *testing.T) {
		u := v.user()
		name := v.managed(u)
		before := len(v.dock.argv())
		r := v.do(u, http.MethodPost, "/v1/me/hermes/cron",
			`{"schedule":"0 9 * * *","prompt":"Summarize; rm -rf /","name":"Digest","deliver":"telegram","skills":["web","","rss"]}`)
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.JSONEq(t, `{"source":"managed","jobs":[]}`, r.body)
		assert.Equal(t, [][]string{
			{name, "hermes", "cron", "create", "0 9 * * *", "Summarize; rm -rf /", "--name", "Digest", "--deliver", "telegram", "--skill", "web", "--skill", "rss"},
			{name, "cat", "/opt/data/cron/jobs.json"},
		}, v.dock.argv()[before:])

		// Empty optionals are left off the argv.
		before = len(v.dock.argv())
		r = v.do(u, http.MethodPost, "/v1/me/hermes/cron", `{"schedule":"*/5 * * * *","prompt":"","name":"","deliver":""}`)
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.Equal(t, []string{name, "hermes", "cron", "create", "*/5 * * * *"}, v.dock.argv()[before])
	})

	t.Run("managed create failure is 502 cron_create_failed", func(t *testing.T) {
		u := v.user()
		v.managed(u)
		v.dock.results["hermes cron create bad"] = hermescontainers.Result{ExitCode: 2, Stderr: "invalid schedule"}
		r := v.do(u, http.MethodPost, "/v1/me/hermes/cron", `{"schedule":"bad"}`)
		assert.Equal(t, http.StatusBadGateway, r.code)
		assert.JSONEq(t, errorBody("cron_create_failed"), r.body)
	})

	t.Run("managed mutations run hermes cron <action> <id>", func(t *testing.T) {
		u := v.user()
		name := v.managed(u)
		for _, c := range []struct{ method, path, action string }{
			{http.MethodPost, "/v1/me/hermes/cron/a1/pause", "pause"},
			{http.MethodPost, "/v1/me/hermes/cron/a1/resume", "resume"},
			{http.MethodDelete, "/v1/me/hermes/cron/a1", "remove"},
		} {
			before := len(v.dock.argv())
			r := v.do(u, c.method, c.path, "")
			require.Equal(t, http.StatusOK, r.code, r.body)
			assert.JSONEq(t, `{"source":"managed","jobs":[]}`, r.body)
			assert.Equal(t, []string{name, "hermes", "cron", c.action, "a1"}, v.dock.argv()[before])

			v.dock.results["hermes cron "+c.action+" a1"] = hermescontainers.Result{ExitCode: 1}
			r = v.do(u, c.method, c.path, "")
			assert.Equal(t, http.StatusBadGateway, r.code)
			assert.JSONEq(t, errorBody("cron_"+c.action+"_failed"), r.body)
			delete(v.dock.results, "hermes cron "+c.action+" a1")
		}
	})

	t.Run("a docker that cannot spawn is an empty 500", func(t *testing.T) {
		u := v.user()
		v.managed(u)
		v.dock.spawn = errors.New("no such file or directory")
		defer func() { v.dock.spawn = nil }()
		r := v.do(u, http.MethodGet, "/v1/me/hermes/cron", "")
		assert.Equal(t, http.StatusInternalServerError, r.code)
		assert.Empty(t, r.body)
	})

	t.Run("create needs a schedule", func(t *testing.T) {
		u := v.user()
		for _, body := range []string{`{"schedule":""}`, `{"schedule":"  \t "}`} {
			r := v.do(u, http.MethodPost, "/v1/me/hermes/cron", body)
			assert.Equal(t, http.StatusBadRequest, r.code, body)
			assert.JSONEq(t, errorBody("schedule_required"), r.body)
		}
		// trimmingCharacters(in: .whitespaces) keeps newlines: not blank.
		r := v.do(u, http.MethodPost, "/v1/me/hermes/cron", `{"schedule":"\n"}`)
		assert.JSONEq(t, errorBody("no_hermes_cron_source"), r.body)
		// A missing key is the decoder's 400.
		r = v.do(u, http.MethodPost, "/v1/me/hermes/cron", `{"prompt":"x"}`)
		assert.Equal(t, http.StatusBadRequest, r.code)
		assert.Contains(t, r.body, "schedule")
	})
}

func TestCronBridgeBYO(t *testing.T) {
	v := setup(t)

	t.Run("config seals the token on a new row, then lists from the dashboard", func(t *testing.T) {
		u := v.user()
		v.http.Respond(http.MethodGet, "/api/cron/jobs", 200,
			`[{"id":"d1","name":"Digest","schedule":{"kind":"cron","expr":"0 9 * * 1-5","display":"0 9 * * 1-5"},"schedule_display":"0 9 * * 1-5","last_run_at":"2026-09-01T03:00:00Z","last_status":"ok","enabled":true}]`)
		r := v.do(u, http.MethodPut, "/v1/me/hermes/cron/config", `{"dashboardUrl":"https://hermes.example.com/","dashboardToken":"dash-token"}`)
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.JSONEq(t, `{"source":"byo","jobs":[{"id":"d1","name":"Digest","schedule":"0 9 * * 1-5","lastRun":"2026-09-01T03:00:00Z (ok)","status":"active","mode":"agent"}]}`, r.body)

		var baseURL, dashURL string
		var ct, nonce []byte
		require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT base_url, cron_dashboard_url, cron_dashboard_token_ciphertext, cron_dashboard_token_nonce
			FROM user_hermes_config WHERE tenant_id = $1`, u).Scan(&baseURL, &dashURL, &ct, &nonce))
		assert.Empty(t, baseURL, "an empty api_server base_url is no override")
		assert.Equal(t, "https://hermes.example.com/", dashURL)
		token, err := v.box.Open(secretbox.Sealed{Ciphertext: ct, Nonce: nonce}, u)
		require.NoError(t, err)
		assert.Equal(t, "dash-token", token)

		last := v.http.Requests()[len(v.http.Requests())-1]
		assert.Equal(t, "Bearer dash-token", last.Header.Get("Authorization"))

		r = v.do(u, http.MethodGet, "/v1/me/hermes/cron", "")
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.Equal(t, "byo", r.json(t)["source"])
	})

	t.Run("config updates an existing row and keeps its gateway settings", func(t *testing.T) {
		u := v.user()
		_, err := v.pool.Exec(context.Background(), `INSERT INTO user_hermes_config (id, tenant_id, base_url, name, created_at, updated_at)
			VALUES ($1, $2, 'https://gw.example.com', 'Mine', now(), now())`, uuid.New(), u)
		require.NoError(t, err)
		v.byo(u, "https://dash.example.com", "t2")
		var n int
		var baseURL, dashURL string
		require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT count(*), max(base_url), max(cron_dashboard_url) FROM user_hermes_config WHERE tenant_id = $1`, u).
			Scan(&n, &baseURL, &dashURL))
		assert.Equal(t, 1, n)
		assert.Equal(t, "https://gw.example.com", baseURL)
		assert.Equal(t, "https://dash.example.com", dashURL)
	})

	t.Run("config refuses a URL the SSRF guard rejects, as an empty 500", func(t *testing.T) {
		u := v.user()
		for _, raw := range []string{"http://hermes.example.com", "https://10.0.0.1", "https://127.0.0.1", "not a url"} {
			r := v.do(u, http.MethodPut, "/v1/me/hermes/cron/config", `{"dashboardUrl":"`+raw+`","dashboardToken":"x"}`)
			assert.Equal(t, http.StatusInternalServerError, r.code, raw)
			assert.Empty(t, r.body, raw)
		}
		var n int
		require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM user_hermes_config WHERE tenant_id = $1`, u).Scan(&n))
		assert.Zero(t, n)
	})

	t.Run("config needs both camelCase keys", func(t *testing.T) {
		u := v.user()
		for _, body := range []string{`{"dashboardUrl":"https://x.example.com"}`, `{"dashboard_url":"https://x.example.com","dashboard_token":"t"}`, ``} {
			r := v.do(u, http.MethodPut, "/v1/me/hermes/cron/config", body)
			assert.Equal(t, http.StatusBadRequest, r.code, body)
		}
	})

	t.Run("BYO create posts the spec, then re-lists", func(t *testing.T) {
		u := v.user()
		v.byo(u, "https://create.example.com", "tok")
		stub := hermesdashtest.New()
		stub.Respond(http.MethodPost, "/api/cron/jobs", 200, `{"id":"new1","name":"Digest","schedule":"0 3 * * *","enabled":true}`)
		stub.Respond(http.MethodGet, "/api/cron/jobs", 200, `[{"id":"new1","name":"Digest","schedule":"0 3 * * *","enabled":true}]`)
		v.svc.HTTP = stub
		defer func() { v.svc.HTTP = v.http }()
		r := v.do(u, http.MethodPost, "/v1/me/hermes/cron", `{"schedule":"0 3 * * *","prompt":"","name":"Digest","skills":["ignored"]}`)
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.JSONEq(t, `{"source":"byo","jobs":[{"id":"new1","name":"Digest","schedule":"0 3 * * *","status":"active","mode":"agent"}]}`, r.body)
		reqs := stub.Requests()
		require.Len(t, reqs, 2)
		assert.Equal(t, http.MethodPost, reqs[0].Method)
		// deliver defaults to origin; an empty prompt and the skills are not sent.
		assert.JSONEq(t, `{"schedule":"0 3 * * *","deliver":"origin","name":"Digest"}`, reqs[0].Body)
	})

	t.Run("BYO errors keep the legacy codes, all 502", func(t *testing.T) {
		u := v.user()
		v.byo(u, "https://errors.example.com", "tok")
		stub := hermesdashtest.New()
		v.svc.HTTP = stub
		defer func() { v.svc.HTTP = v.http }()

		stub.Respond(http.MethodGet, "/api/cron/jobs", 500, `{"detail":"boom"}`)
		r := v.do(u, http.MethodGet, "/v1/me/hermes/cron", "")
		assert.Equal(t, http.StatusBadGateway, r.code)
		assert.JSONEq(t, errorBody("byo_cron_http_500"), r.body)

		stub.Respond(http.MethodPost, "/api/cron/jobs", 422, `{"detail":"bad"}`)
		r = v.do(u, http.MethodPost, "/v1/me/hermes/cron", `{"schedule":"x"}`)
		assert.Equal(t, http.StatusBadGateway, r.code)
		assert.JSONEq(t, errorBody("byo_cron_create_422"), r.body)

		for _, c := range []struct {
			status int
			want   string
		}{{401, "byo_cron_http_401"}, {403, "byo_cron_http_401"}, {303, "hermes_dashboard_auth_mode_unsupported"}} {
			s := hermesdashtest.New()
			s.Respond(http.MethodGet, "/api/cron/jobs", c.status, `{}`, "Location", "/login")
			v.svc.HTTP = s
			r = v.do(u, http.MethodGet, "/v1/me/hermes/cron", "")
			assert.Equal(t, http.StatusBadGateway, r.code)
			assert.JSONEq(t, errorBody(c.want), r.body)
		}

		down := hermesdashtest.New()
		down.Failure = errors.New("connection refused")
		v.svc.HTTP = down
		r = v.do(u, http.MethodGet, "/v1/me/hermes/cron", "")
		assert.Equal(t, http.StatusBadGateway, r.code)
		assert.JSONEq(t, errorBody("hermes_dashboard_unreachable"), r.body)
	})

	t.Run("a token sealed for another tenant is an error, not no source", func(t *testing.T) {
		u, other := v.user(), v.user()
		sealed, err := v.box.Seal("tok", other)
		require.NoError(t, err)
		_, err = v.pool.Exec(context.Background(), `INSERT INTO user_hermes_config (id, tenant_id, base_url, cron_dashboard_url, cron_dashboard_token_ciphertext, cron_dashboard_token_nonce, created_at, updated_at)
			VALUES ($1, $2, '', 'https://x.example.com', $3, $4, now(), now())`, uuid.New(), u, sealed.Ciphertext, sealed.Nonce)
		require.NoError(t, err)
		r := v.do(u, http.MethodGet, "/v1/me/hermes/cron", "")
		assert.Equal(t, http.StatusInternalServerError, r.code)
	})

	t.Run("the dashboard preview is the first five jobs, and none on error", func(t *testing.T) {
		u := v.user()
		v.managed(u)
		var b strings.Builder
		b.WriteString(`[`)
		for i := range 7 {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"id":"p` + string(rune('0'+i)) + `"}`)
		}
		b.WriteString(`]`)
		v.dock.results["cat /opt/data/cron/jobs.json"] = hermescontainers.Result{Stdout: b.String()}
		defer delete(v.dock.results, "cat /opt/data/cron/jobs.json")
		jobs := v.svc.CronPreview(context.Background(), u)
		require.Len(t, jobs, 5)
		assert.Equal(t, "p4", jobs[4].ID)

		assert.Equal(t, []skillscron.Job{}, v.svc.CronPreview(context.Background(), v.user()))
	})
}

func TestCronPreviewRoute(t *testing.T) {
	v := setup(t)
	u := v.user()

	t.Run("deterministic parse, no LLM", func(t *testing.T) {
		calls := v.llm.calls
		r := v.do(u, http.MethodPost, "/v1/me/hermes/cron/preview", `{"text":"Every weekday at 7:30am send me AI news on Telegram"}`)
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.JSONEq(t, `{"schedule":"30 7 * * 1-5","prompt":"Every weekday at 7:30am send me AI news on Telegram","name":"Send me ai news on telegram","deliver":"telegram"}`, r.body)
		assert.Equal(t, calls, v.llm.calls)
	})

	t.Run("LLM fallback fills the spec", func(t *testing.T) {
		v.llm.content = "```json\n{\"isJob\": true, \"title\": \"Moon watch\", \"cron\": \"0 21 1 * *\", \"spec\": \"Report the moon phase\"}\n```"
		r := v.do(u, http.MethodPost, "/v1/me/hermes/cron/preview", `{"text":"on the first of the month tell me the moon phase via slack"}`)
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.JSONEq(t, `{"schedule":"0 21 1 * *","prompt":"Report the moon phase","name":"Moon watch","deliver":"slack"}`, r.body)

		// No title or spec: the text and deriveName stand in.
		v.llm.content = `{"isJob": true, "cron": "0 6 1 1 *"}`
		r = v.do(u, http.MethodPost, "/v1/me/hermes/cron/preview", `{"text":"new year greeting 2027"}`)
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.JSONEq(t, `{"schedule":"0 6 1 1 *","prompt":"new year greeting 2027","name":"New year greeting","deliver":"origin"}`, r.body)
	})

	t.Run("not a job, an empty cron or a failed LLM is 422", func(t *testing.T) {
		for _, c := range []struct {
			content string
			fail    bool
		}{{`{"isJob": false}`, false}, {`{"isJob": true, "cron": ""}`, false}, {`{"isJob": true}`, false}, {"", true}} {
			v.llm.content, v.llm.fail = c.content, c.fail
			r := v.do(u, http.MethodPost, "/v1/me/hermes/cron/preview", `{"text":"what is the capital of France"}`)
			assert.Equal(t, http.StatusUnprocessableEntity, r.code, c.content)
			assert.JSONEq(t, errorBody("not_a_schedulable_job"), r.body)
		}
		v.llm.fail = false
	})

	t.Run("text is required", func(t *testing.T) {
		r := v.do(u, http.MethodPost, "/v1/me/hermes/cron/preview", `{}`)
		assert.Equal(t, http.StatusBadRequest, r.code)
	})

	t.Run("a nil transport has no fallback", func(t *testing.T) {
		svc := *v.svc
		svc.Transport = nil
		_, ok := svc.Preview(context.Background(), u, "tell me something")
		assert.False(t, ok)
		spec, ok := svc.Preview(context.Background(), u, "hourly ping")
		require.True(t, ok)
		assert.Equal(t, "0 * * * *", spec.Schedule)
	})
}

var _ hermesdash.Doer = (*hermesdashtest.Stub)(nil)
