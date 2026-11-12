package skills

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"lumina/internal/apns"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/devicerpc"
	"lumina/internal/devicetools"
	"lumina/internal/embedding"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/ratelimit"
	"lumina/internal/skills"
	"lumina/internal/vaultpaths"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// Database-backed ports: SkillRunCapGuardTests, SkillRunnerTests, JobOriginTests
// and the WeatherForecastToolTests cases that read the stored fix and the
// Location consent. One container serves them all.

var testKeys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

// scriptedTransport is SkillScriptedTransport: it answers each completion with
// the next scripted body and records every payload it was sent.
type scriptedTransport struct {
	mu      sync.Mutex
	steps   []string
	headers map[string]string
	calls   [][]byte
}

func (s *scriptedTransport) ChatCompletions(_ context.Context, payload []byte, _, _ string) (llm.Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, payload)
	if len(s.steps) == 0 {
		return llm.Metadata{}, errors.New("script exhausted")
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	return llm.Metadata{Data: []byte(step), Headers: s.headers}, nil
}

func (s *scriptedTransport) ChatStream(context.Context, []byte, string, string) llm.Stream {
	return func(func(llm.Chunk, error) bool) {}
}

func (s *scriptedTransport) payloads() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func toolCallStep(name, arguments string) string {
	return `{"id":"chatcmpl-test","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":` +
		mustJSON(name) + `,"arguments":` + mustJSON(arguments) + `}}]},"finish_reason":"tool_calls"}]}`
}

func contentStep(content string) string {
	return `{"id":"chatcmpl-test","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":` +
		mustJSON(content) + `},"finish_reason":"stop"}]}`
}

// pushRecorder is RecordingPushSender.
type pushRecorder struct {
	mu   sync.Mutex
	sent []apns.Delivery
}

func (p *pushRecorder) Send(_ context.Context, d apns.Delivery) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, d)
	return nil
}

func (p *pushRecorder) categories() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []string{}
	for _, d := range p.sent {
		out = append(out, string(d.Message.Category))
	}
	slices.Sort(out)
	return out
}

// phone is the device round-trip: a location item, or no answer.
type phone struct {
	items string
	err   error
	asked int
}

func (p *phone) Request(context.Context, uuid.UUID, devicerpc.Command) (devicerpc.Result, error) {
	p.asked++
	if p.err != nil {
		return devicerpc.Result{}, p.err
	}
	return devicerpc.Result{OK: true, Payload: map[string]string{"items": p.items}}, nil
}

type dbEnv struct {
	t     *testing.T
	pool  *pgxpool.Pool
	paths vaultpaths.Paths
}

func (v dbEnv) user() uuid.UUID {
	v.t.Helper()
	ctx := context.Background()
	id := uuid.New()
	short := strings.ReplaceAll(id.String(), "-", "")[:12]
	_, err := v.pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
		VALUES ($1, $2, 'x', $3, now(), now())`, id, "skill-"+short+"@test.luminavault", "skill-"+short)
	require.NoError(v.t, err)
	_, err = v.pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
		VALUES ($1, $1, 'v', now(), now())`, id)
	require.NoError(v.t, err)
	return id
}

type runnerDeps struct {
	transport llm.ChatTransport
	push      apns.Sender
	device    *phone
	http      *http.Client
}

func (v dbEnv) runner(d runnerDeps) *Runner {
	opt := RunnerOptions{
		Transport: d.transport, DefaultModel: "test-model", Paths: v.paths,
		Memories: memorystore.New(v.pool, nil), Embeddings: embedding.Deterministic{},
		HTTP: d.http, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if d.push != nil {
		opt.APNS = apns.NewWithSender(v.pool, "com.luminavault.test", d.push)
	}
	if d.device != nil {
		opt.Device = devicetools.New(v.pool, d.device, nil)
	}
	return NewRunner(v.pool, opt)
}

func testManifest(name string, allowedTools []string, outputs []skills.Output) skills.Manifest {
	return skills.Manifest{
		Source: skills.SourceBuiltin, Name: name, Description: "test skill", AllowedTools: allowedTools,
		Capability: skills.Capability("low"), Outputs: outputs, Body: "Run the test skill.",
	}
}

func TestSkillsDB(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	v := dbEnv{t: t, pool: pool, paths: vaultpaths.Paths{Root: t.TempDir()}}

	t.Run("SkillRunCapGuard", func(t *testing.T) { testCapGuard(t, v) })
	t.Run("SkillRunner", func(t *testing.T) { testSkillRunner(t, v) })
	t.Run("JobOrigin", func(t *testing.T) { testJobOrigin(t, v) })
	t.Run("WeatherForecastTool", func(t *testing.T) { testWeatherTool(t, v) })
	t.Run("CronScheduler", func(t *testing.T) { testCronScheduler(t, v) })
	t.Run("RunWorkflowSkill", func(t *testing.T) { testRunWorkflowSkill(t, v) })
}

// MARK: - SkillRunCapGuardTests

func testCapGuard(t *testing.T, v dbEnv) {
	ctx := context.Background()
	capped := func(trial, pro, ultimate int) skills.Manifest {
		mf := testManifest("pattern-detector", nil, nil)
		mf.Capability = skills.Capability("high")
		mf.DailyRunCap = &skills.DailyRunCap{Trial: trial, Pro: pro, Ultimate: ultimate}
		return mf
	}

	// SkillRunCapGuardTests: "ultimate tier with cap zero short circuits without DB hit".
	t.Run("ultimate tier with cap zero short circuits without a DB hit", func(t *testing.T) {
		g := capGuard{pool: nil, now: time.Now} // a query would dereference the nil pool
		require.NoError(t, g.checkAndIncrement(ctx, v.user(), "ultimate", capped(3, 3, 0)))
	})

	// SkillRunCapGuardTests: "pro tier allows three then denies fourth with retry after".
	t.Run("pro tier allows three then denies the fourth with a retry-after", func(t *testing.T) {
		g := capGuard{pool: v.pool, now: time.Now}
		tenant, mf := v.user(), capped(3, 3, 0)
		for range 3 {
			require.NoError(t, g.checkAndIncrement(ctx, tenant, "pro", mf))
		}
		err := g.checkAndIncrement(ctx, tenant, "pro", mf)
		var denied *capExceededError
		require.ErrorAs(t, err, &denied, "the 4th invocation must be denied")
		require.Positive(t, denied.retryAfter)
		require.LessOrEqual(t, denied.retryAfter, 86401*time.Second)
	})

	// SkillRunCapGuardTests: "recordFailure refunds a slot so failed runs don't burn cap".
	t.Run("recordFailure refunds a slot", func(t *testing.T) {
		g := capGuard{pool: v.pool, now: time.Now}
		tenant, mf := v.user(), capped(3, 3, 0)
		for range 3 {
			require.NoError(t, g.checkAndIncrement(ctx, tenant, "pro", mf))
		}
		require.NoError(t, g.recordFailure(ctx, tenant, mf))
		require.NoError(t, g.checkAndIncrement(ctx, tenant, "pro", mf), "after a refund one slot is free again")
	})

	// SkillRunCapGuardTests: "stale reset stamp rolls forward and resets counter".
	t.Run("a stale reset stamp rolls forward and resets the counter", func(t *testing.T) {
		g := capGuard{pool: v.pool, now: time.Now}
		tenant, mf := v.user(), capped(1, 1, 0)
		require.NoError(t, g.checkAndIncrement(ctx, tenant, "pro", mf))
		_, err := v.pool.Exec(ctx, `UPDATE skills_state SET daily_run_reset_at = $4
			WHERE tenant_id = $1 AND source = $2 AND name = $3`, tenant, string(mf.Source), mf.Name, time.Now().Add(-24*time.Hour))
		require.NoError(t, err)
		require.NoError(t, g.checkAndIncrement(ctx, tenant, "pro", mf))
	})
}

// MARK: - SkillRunnerTests

func testSkillRunner(t *testing.T, v dbEnv) {
	ctx := context.Background()
	memories := func(t *testing.T, tenant uuid.UUID) []memorystore.Memory {
		t.Helper()
		rows, err := memorystore.New(v.pool, nil).List(ctx, tenant)
		require.NoError(t, err)
		return rows
	}

	// SkillRunnerTests: "disallowed tool returns tool error and does not write memory".
	t.Run("a disallowed tool returns a tool error and writes no memory", func(t *testing.T) {
		tenant := v.user()
		transport := &scriptedTransport{steps: []string{
			toolCallStep("memory_upsert", `{"content":"must not persist"}`),
			contentStep("I could not write that memory."),
		}}
		r := v.runner(runnerDeps{transport: transport})
		res, err := r.Run(ctx, testManifest("deny-memory", []string{"session_search"}, nil), runUser{tenant: tenant, tier: "trial", username: "u"}, nil, nil)
		require.NoError(t, err)
		require.Equal(t, "ok", res.status)
		require.Empty(t, memories(t, tenant))

		calls := transport.payloads()
		require.Len(t, calls, 2)
		var second struct {
			Messages []struct {
				Role    string  `json:"role"`
				Content *string `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.Unmarshal(calls[1], &second))
		last := second.Messages[len(second.Messages)-1]
		require.Equal(t, "tool", last.Role)
		require.NotNil(t, last.Content)
		require.Contains(t, *last.Content, `"status":"error"`)
		require.Contains(t, *last.Content, "not allowed")
	})

	// SkillRunnerTests: "dispatches every output kind". Swift's case runs on an
	// event trigger carrying a source vault file, which is what lets vault_rewrite
	// and memory_emit's lineage work. No production path sends that payload (the
	// routes run manual, the workflow engine sends .event(name: "workflow") with
	// none, cron has none), and Go's runner has only the manual trigger, so every
	// other kind is dispatched here and vault_rewrite is held to the failure it
	// produces on every reachable trigger.
	t.Run("dispatches every output kind", func(t *testing.T) {
		tenant := v.user()
		_, err := v.pool.Exec(ctx, `INSERT INTO device_tokens (id, tenant_id, token, platform, created_at, updated_at)
			VALUES ($1, $2, $3, 'ios', now(), now())`, uuid.New(), tenant, "token-"+uuid.NewString())
		require.NoError(t, err)
		push := &pushRecorder{}
		memo := "memos/skill.md"
		r := v.runner(runnerDeps{transport: &scriptedTransport{steps: []string{contentStep("Enriched skill output.")}}, push: push})
		mf := testManifest("dispatch-all", []string{"memory_upsert"}, []skills.Output{
			{Kind: skills.OutputMemo, Path: &memo},
			{Kind: skills.OutputAPNSDigest},
			{Kind: skills.OutputAPNSNudge},
			{Kind: skills.OutputMemoryEmit},
		})
		res, err := r.Run(ctx, mf, runUser{tenant: tenant, tier: "trial", username: "u"}, nil, nil)
		require.NoError(t, err)
		require.Equal(t, "ok", res.status)

		written, err := os.ReadFile(filepath.Join(v.paths.Raw(tenant), memo))
		require.NoError(t, err)
		require.Equal(t, "Enriched skill output.", string(written))
		var rows int
		require.NoError(t, v.pool.QueryRow(ctx, `SELECT count(*) FROM vault_files WHERE tenant_id = $1 AND path = $2`, tenant, memo).Scan(&rows))
		require.Equal(t, 1, rows)
		require.Equal(t, []string{string(apns.CategoryDigest), string(apns.CategoryNudge)}, push.categories())
		mems := memories(t, tenant)
		require.Len(t, mems, 1)
		require.Equal(t, "Enriched skill output.", mems[0].Content)

		rewrite := testManifest("dispatch-rewrite", nil, []skills.Output{{Kind: skills.OutputVaultRewrite}})
		r = v.runner(runnerDeps{transport: &scriptedTransport{steps: []string{contentStep("Rewritten.")}}})
		_, err = r.Run(ctx, rewrite, runUser{tenant: tenant, tier: "trial", username: "u"}, nil, nil)
		require.ErrorContains(t, err, "vault_rewrite output requires source vault file id")
		var status string
		require.NoError(t, v.pool.QueryRow(ctx, `SELECT status FROM skill_run_log WHERE tenant_id = $1 AND name = $2`, tenant, rewrite.Name).Scan(&status))
		require.Equal(t, "error", status)
	})

	// SkillRunnerTests: "records usage counters from provider metadata".
	t.Run("records usage counters from provider metadata", func(t *testing.T) {
		tenant := v.user()
		transport := &scriptedTransport{steps: []string{contentStep("Done.")}, headers: map[string]string{"x-mtok-in": "12", "x-mtok-out": "34"}}
		res, err := v.runner(runnerDeps{transport: transport}).Run(ctx, testManifest("usage", nil, nil), runUser{tenant: tenant, tier: "trial", username: "u"}, nil, nil)
		require.NoError(t, err)
		require.Equal(t, 12, res.mtokIn)
		require.Equal(t, 34, res.mtokOut)
		var in, out int
		require.NoError(t, v.pool.QueryRow(ctx, `SELECT mtok_in, mtok_out FROM skill_run_log WHERE id = $1`, res.runID).Scan(&in, &out))
		require.Equal(t, 12, in)
		require.Equal(t, 34, out)
	})
}

// MARK: - JobOriginTests

func skillsServer(pool *pgxpool.Pool, paths vaultpaths.Paths) *echo.Echo {
	users := auth.PGUsers(pool)
	module := New(pool, Options{
		Identify: auth.Identify(testKeys, users), Limits: ratelimit.NewStore(),
		Catalog: skills.NewCatalog(paths), Paths: paths,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(testKeys, users), Modules: []httpserver.Module{module}})
}

func call(t *testing.T, e *echo.Echo, user uuid.UUID, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		token, err := testKeys.MintAccess(user, time.Now(), "")
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

type originJSON struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type listedSkill struct {
	Name   string      `json:"name"`
	Origin *originJSON `json:"origin"`
}

func testJobOrigin(t *testing.T, v dbEnv) {
	ctx := context.Background()
	e := skillsServer(v.pool, v.paths)
	conversation := func(t *testing.T, tenant uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		_, err := v.pool.Exec(ctx, `INSERT INTO conversations (id, tenant_id, title, created_at, updated_at)
			VALUES ($1, $2, 'Planning', now(), now())`, id, tenant)
		require.NoError(t, err)
		return id
	}
	createJob := func(t *testing.T, tenant uuid.UUID, title string, chat uuid.UUID) *httptest.ResponseRecorder {
		t.Helper()
		body := mustJSON(map[string]string{"title": title, "cron": "0 9 * * 1", "spec": "Summarize the week", "conversationId": chat.String()})
		return call(t, e, tenant, http.MethodPost, "/v1/jobs", body)
	}

	// JobOriginTests: "a job proposed in a chat links back to it".
	t.Run("a job proposed in a chat links back to it", func(t *testing.T) {
		tenant := v.user()
		chat := conversation(t, tenant)
		rec := createJob(t, tenant, "Weekly review "+uuid.NewString()[:6], chat)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var job listedSkill
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &job))
		require.NotNil(t, job.Origin)
		require.Equal(t, "conversation", job.Origin.Kind)
		require.True(t, strings.EqualFold(chat.String(), job.Origin.ID), job.Origin.ID)

		rec = call(t, e, tenant, http.MethodGet, "/v1/skills", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var listed struct {
			Skills []listedSkill `json:"skills"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
		idx := slices.IndexFunc(listed.Skills, func(s listedSkill) bool { return s.Name == job.Name })
		require.GreaterOrEqual(t, idx, 0, "the job is listed")
		require.Equal(t, job.Origin, listed.Skills[idx].Origin)
	})

	// JobOriginTests: "another account's chat is refused".
	t.Run("another account's chat is refused", func(t *testing.T) {
		chat := conversation(t, v.user())
		rec := createJob(t, v.user(), "Steal", chat)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})
}

// SkillsControllerTests: "unauthenticated request is rejected". No database: the
// request never gets past the missing token.
func TestSkillRunWithoutATokenIsUnauthorized(t *testing.T) {
	e := skillsServer(nil, vaultpaths.Paths{Root: t.TempDir()})
	rec := call(t, e, uuid.Nil, http.MethodPost, "/v1/skills/daily-brief/run", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// MARK: - WeatherForecastToolTests (stored fix and consent)

func testWeatherTool(t *testing.T, v dbEnv) {
	ctx := context.Background()
	place := "Lisbon"
	lisbon := locationFix{lat: 38.72, lng: -9.14, place: &place, capturedAt: time.Unix(1_790_000_000, 0).UTC()}
	liveItems := `[{"lat":38.72,"lng":-9.14,"place":"Lisbon","at":"2026-09-21T10:13:20Z"}]`
	offline := errors.New("device did not respond")
	consent := func(t *testing.T, tenant uuid.UUID, allowed bool) {
		t.Helper()
		_, err := v.pool.Exec(ctx, `INSERT INTO apple_consent (tenant_id, domain, allowed) VALUES ($1, 'location', $2)`, tenant, allowed)
		require.NoError(t, err)
	}
	keep := func(t *testing.T, tenant uuid.UUID) {
		t.Helper()
		require.NoError(t, saveFix(ctx, v.pool, tenant, lisbon))
	}

	// WeatherForecastToolTests: "a live fix is used, kept for later, and the dry streak is counted".
	t.Run("a live fix is used, kept, and the dry streak counted", func(t *testing.T) {
		tenant := v.user()
		consent(t, tenant, true)
		stub := &stubHTTP{replies: []stubReply{okReply(forecastJSON)}}
		out := toolJSON(t, v.runner(runnerDeps{device: &phone{items: liveItems}, http: stub.client()}).weather(ctx, tenant))
		require.Equal(t, "ok", out["status"])
		require.InDelta(t, 5, out["dry_streak_days"], 0)
		location := out["location"].(map[string]any)
		require.Equal(t, "device", location["source"])
		require.Equal(t, "Lisbon", location["place"])
		days := out["days"].([]any)
		require.Len(t, days, 7)
		require.Equal(t, "rain", days[5].(map[string]any)["summary"])
		require.Equal(t, false, days[5].(map[string]any)["dry"])
		kept := loadFix(ctx, v.pool, tenant)
		require.NotNil(t, kept, "the live fix is kept")
		require.InDelta(t, 38.72, kept.lat, 1e-9)
		require.InDelta(t, -9.14, kept.lng, 1e-9)
		require.Equal(t, "Lisbon", *kept.place)
	})

	// WeatherForecastToolTests: "phone offline falls back to the kept fix".
	t.Run("phone offline falls back to the kept fix", func(t *testing.T) {
		tenant := v.user()
		consent(t, tenant, true)
		keep(t, tenant)
		stub := &stubHTTP{replies: []stubReply{okReply(forecastJSON)}}
		out := toolJSON(t, v.runner(runnerDeps{device: &phone{err: offline}, http: stub.client()}).weather(ctx, tenant))
		require.Equal(t, "ok", out["status"])
		require.Equal(t, "cached", out["location"].(map[string]any)["source"])
		require.Len(t, stub.requested(), 1)
	})

	// WeatherForecastToolTests: "the kept fix is not used once Location access is off".
	t.Run("the kept fix is not used once Location access is off", func(t *testing.T) {
		tenant := v.user()
		consent(t, tenant, false)
		keep(t, tenant)
		stub := &stubHTTP{replies: []stubReply{okReply(forecastJSON)}}
		out := toolJSON(t, v.runner(runnerDeps{device: &phone{err: offline}, http: stub.client()}).weather(ctx, tenant))
		require.Equal(t, "error", out["status"])
		require.Empty(t, stub.requested())
	})

	// WeatherForecastToolTests: "no location at all is a tool error, not a crash".
	t.Run("no location at all is a tool error", func(t *testing.T) {
		tenant := v.user()
		consent(t, tenant, true)
		stub := &stubHTTP{}
		out := toolJSON(t, v.runner(runnerDeps{device: &phone{err: offline}, http: stub.client()}).weather(ctx, tenant))
		require.Equal(t, "error", out["status"])
		require.Empty(t, stub.requested())
	})

	// WeatherForecastToolTests: "network trouble becomes a readable tool error",
	// for each of Swift's arguments: a failure, a 503, and a body that isn't JSON.
	for name, reply := range map[string]stubReply{
		"failure": {fail: true},
		"503":     {status: http.StatusServiceUnavailable},
		"html":    okReply("<html>oops</html>"),
	} {
		t.Run("network trouble becomes a readable tool error/"+name, func(t *testing.T) {
			tenant := v.user()
			consent(t, tenant, true)
			keep(t, tenant)
			stub := &stubHTTP{replies: []stubReply{reply}}
			out := toolJSON(t, v.runner(runnerDeps{device: &phone{err: offline}, http: stub.client()}).weather(ctx, tenant))
			require.Equal(t, "error", out["status"])
			require.Contains(t, out["reason"], "weather service unreachable")
		})
	}
}
