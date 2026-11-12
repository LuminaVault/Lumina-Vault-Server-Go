package chat

import (
	"context"
	"encoding/json"
	"io"
	"lumina/internal/auth"
	"lumina/internal/chunkindex"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/devicerpc"
	"lumina/internal/devicetools"
	"lumina/internal/embedding"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/modules/apple"
	"lumina/internal/modules/mcp"
	"lumina/internal/modules/memoryagent"
	"lumina/internal/modules/quickassistant"
	"lumina/internal/modules/reminders"
	"lumina/internal/onboardlatch"
	"lumina/internal/posthog"
	"lumina/internal/ratelimit"
	"lumina/internal/selfimprove"
	"lumina/internal/soul"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ports Tests/AppTests/Chat/QuickAssistantE2ETests.swift and
// Tests/AppTests/LLM/Routing/QuickAssistantBYOKTests.swift: a Quick Assistant
// turn through the real chat route, transport, router, budget and free-lane
// gate, over a real database. Only the providers are stubs. The TheBuiltApp
// tests use the real llm.NewStack, as cmd/App wires it.

// MARK: - Fakes

// qaStep is one scripted completion.
type qaStep struct {
	text string
	tool string
	args string
	fail bool
	// tools is several calls to tool in one round, each with "{}".
	tools int
	// cancel drops the request, then asks for tool.
	cancel bool
	// hang waits for the request to be dropped, then fails with it.
	hang bool
}

func say(text string) qaStep             { return qaStep{text: text} }
func call(name, args string) qaStep      { return qaStep{tool: name, args: args} }
func fail() qaStep                       { return qaStep{fail: true} }
func calls(name string, n int) qaStep    { return qaStep{tool: name, tools: n} }
func cancelThen(name string) qaStep      { return qaStep{tool: name, cancel: true} }
func recentThen(text string) []qaStep    { return []qaStep{call("recent", "{}"), say(text)} }
func pinnedPayloadModel(p []byte) string { return decodeBody(p)["model"].(string) }

// scriptedAdapter plays its steps on buffered completions, repeating the last,
// and answers streams with "lane answer".
type scriptedAdapter struct {
	kind        llm.ProviderKind
	mu          sync.Mutex
	steps       []qaStep
	completions [][]byte
	streams     [][]byte
	cancel      func()
}

func newAdapter(kind llm.ProviderKind, steps ...qaStep) *scriptedAdapter {
	return &scriptedAdapter{kind: kind, steps: steps}
}

func (a *scriptedAdapter) Kind() llm.ProviderKind       { return a.kind }
func (a *scriptedAdapter) AcceptsUserCredentials() bool { return true }

func toolCall(id, name, args string) map[string]any {
	return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
}

func (a *scriptedAdapter) ChatCompletions(ctx context.Context, payload []byte, _, _ string) (llm.Metadata, error) {
	a.mu.Lock()
	a.completions = append(a.completions, payload)
	n := len(a.completions)
	step := a.steps[0]
	if len(a.steps) > 1 {
		a.steps = a.steps[1:]
	}
	cancel := a.cancel
	a.mu.Unlock()
	var message map[string]any
	switch {
	case step.hang:
		select {
		case <-ctx.Done():
			return llm.Metadata{}, ctx.Err()
		case <-time.After(20 * time.Second):
			return llm.Metadata{}, io.ErrUnexpectedEOF
		}
	case step.fail:
		body := "stub"
		return llm.Metadata{}, llm.Transient(a.kind, 503, &body)
	case step.cancel:
		cancel()
		message = map[string]any{"role": "assistant", "tool_calls": []any{toolCall("call-1", step.tool, "{}")}}
	case step.tools > 0:
		list := make([]any, step.tools)
		for i := range list {
			list[i] = toolCall("call-"+strconv.Itoa(i+1), step.tool, "{}")
		}
		message = map[string]any{"role": "assistant", "tool_calls": list}
	case step.tool != "":
		message = map[string]any{"role": "assistant", "tool_calls": []any{toolCall("call-"+strconv.Itoa(n), step.tool, step.args)}}
	default:
		message = map[string]any{"role": "assistant", "content": step.text}
	}
	data, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": message}}})
	return llm.Metadata{Data: data}, err
}

func (a *scriptedAdapter) ChatStream(_ context.Context, payload []byte, _, _ string) llm.Stream {
	a.mu.Lock()
	a.streams = append(a.streams, payload)
	a.mu.Unlock()
	return func(yield func(llm.Chunk, error) bool) {
		if yield(llm.Chunk{Delta: "lane "}, nil) {
			yield(llm.Chunk{Delta: "answer", FinishReason: "stop"}, nil)
		}
	}
}

func (a *scriptedAdapter) completionPayloads() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][]byte(nil), a.completions...)
}

func (a *scriptedAdapter) streamPayloads() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][]byte(nil), a.streams...)
}

// fixedRouter answers every pick with one decision, counting picks. reserve,
// when set, runs on every pick, as Cerberus reserves.
type fixedRouter struct {
	decision llm.Decision
	picks    atomic.Int32
	reserve  func()
}

func (r *fixedRouter) Pick(context.Context, string, llm.Capability, *llm.User) llm.Decision {
	r.picks.Add(1)
	if r.reserve != nil {
		r.reserve()
	}
	return r.decision
}

func decision(primary llm.Route, fallbacks ...llm.Route) llm.Decision {
	return llm.Decision{Primary: primary, Fallbacks: fallbacks}
}

var (
	gatewayRoute = llm.Route{Provider: llm.HermesGateway, ModelID: "hermes-3"}
	haikuRoute   = llm.Route{Provider: llm.Anthropic, ModelID: "claude-haiku-5-5"}
)

// cannedGateway is the managed gateway: classic tokens and a record of requests.
type cannedGateway struct {
	mu       sync.Mutex
	requests []llm.ChatRequest
}

func (g *cannedGateway) ChatStream(_ context.Context, _, _ string, req llm.ChatRequest) llm.Stream {
	g.mu.Lock()
	g.requests = append(g.requests, req)
	g.mu.Unlock()
	return func(yield func(llm.Chunk, error) bool) {
		if yield(llm.Chunk{Delta: "classic "}, nil) {
			yield(llm.Chunk{Delta: "answer", FinishReason: "stop"}, nil)
		}
	}
}

func (g *cannedGateway) all() []llm.ChatRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]llm.ChatRequest(nil), g.requests...)
}

// countingTransport fails every call and counts them: follow-ups that never ran.
type countingTransport struct{ calls atomic.Int32 }

func (c *countingTransport) ChatCompletions(context.Context, []byte, string, string) (llm.Metadata, error) {
	c.calls.Add(1)
	return llm.Metadata{}, io.ErrUnexpectedEOF
}

func (c *countingTransport) ChatStream(context.Context, []byte, string, string) llm.Stream {
	c.calls.Add(1)
	return func(yield func(llm.Chunk, error) bool) { yield(llm.Chunk{}, io.ErrUnexpectedEOF) }
}

// okPhone is a phone that answers every command.
type okPhone struct{}

func (okPhone) Request(_ context.Context, _ uuid.UUID, cmd devicerpc.Command) (devicerpc.Result, error) {
	return devicerpc.Result{ID: cmd.ID, OK: true, Payload: map[string]string{}}, nil
}

// analyticsSink is a PostHog endpoint that keeps chat_grounded_answer events.
type analyticsSink struct {
	mu     sync.Mutex
	used   []bool
	client *posthog.Client
}

func newAnalytics(t *testing.T) *analyticsSink {
	s := &analyticsSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Batch []struct {
				Event      string         `json:"event"`
				Properties map[string]any `json:"properties"`
			} `json:"batch"`
		}
		if json.Unmarshal(b, &body) == nil {
			s.mu.Lock()
			for _, e := range body.Batch {
				if e.Event == "chat_grounded_answer" {
					used, _ := e.Properties["used_tools"].(bool)
					s.used = append(s.used, used)
				}
			}
			s.mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	s.client = posthog.New("phc_stub", srv.URL, posthog.Options{})
	return s
}

func (s *analyticsSink) groundedAnswers() []bool {
	s.client.Flush(context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool{}, s.used...)
}

// MARK: - Harness

type qaOptions struct {
	router   llm.Router
	adapters []llm.Adapter
	devices  quickassistant.DeviceRequester
	activity *selfimprove.Activity
	runs     HermesRuns
	// hermes is the BYO Hermes middleware slot.
	hermes echo.MiddlewareFunc
	// off builds the module without Quick Assistant.
	off bool
	// stream replaces the routed stream service (the TheBuiltApp tests).
	stream llm.StreamService
	// service replaces the loop's service (the TheBuiltApp tests).
	service *quickassistant.Service
}

type qa struct {
	env
	module    *Module
	gateway   *cannedGateway
	telemetry *llm.RouterTelemetryService
	analytics *analyticsSink
	followUps *countingTransport
	turns     chan struct{}
}

func qaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	return pool
}

func qaDeps(t *testing.T, pool *pgxpool.Pool, devices quickassistant.DeviceRequester) quickassistant.Deps {
	paths := vaultpaths.Paths{Root: t.TempDir()}
	memories := memorystore.New(pool, nil)
	chunks := chunkindex.NewChunkRepository(pool)
	links := chunkindex.NewLinkRepository(pool)
	embeddings := embedding.Deterministic{}
	return quickassistant.Deps{
		Pool:   pool,
		Access: vaultaccess.New(pool),
		MCP: mcp.NewService(mcp.ServiceDeps{
			Pool: pool, Paths: paths, Links: links,
			Hybrid:     chunkindex.NewHybrid(memories, chunks),
			Embeddings: embeddings,
			Indexer:    chunkindex.NewIndexer(chunks, embeddings, links),
			Memories:   memories,
			Personal:   devicetools.New(pool, nil, nil),
			Soul:       soul.New(paths, t.TempDir()),
		}),
		Store:     quickassistant.NewStore(pool),
		Reminders: reminders.NewStore(pool),
		Consent:   apple.NewConsentReader(pool),
		Devices:   devices,
	}
}

func newQA(t *testing.T, pool *pgxpool.Pool, o qaOptions) *qa {
	t.Helper()
	gateway := &cannedGateway{}
	telemetry := llm.NewRouterTelemetryService(pool)
	preferences := llm.NewPreferenceRepository(pool)
	transport := &llm.Transport{Registry: llm.NewRegistry(nil, o.adapters...), Router: o.router, RouterTelemetry: telemetry}
	stream := o.stream
	if stream == nil {
		stream = &llm.RoutedStreamService{Fallback: gateway, Transport: transport, Preferences: preferences, Router: o.router, Telemetry: telemetry}
	}
	analytics := newAnalytics(t)
	followUps := &countingTransport{}
	paths := vaultpaths.Paths{Root: t.TempDir()}
	opts := Options{
		Identify: auth.Identify(testKeys, auth.PGUsers(pool)), Hermes: o.hermes, Limits: ratelimit.NewStore(),
		Stream:      stream,
		FollowUps:   &memoryagent.FollowUpGenerator{Transport: followUps, DefaultModel: "test"},
		Memories:    memorystore.New(pool, nil),
		Embeddings:  embedding.Deterministic{},
		Preferences: preferences,
		Traces:      llm.PGMeters{Pool: pool}.TraceRecorder(),
		Latches:     onboardlatch.New(pool),
		Analytics:   analytics.client,
		Activity:    o.activity,
		Soul:        soul.New(paths, t.TempDir()),
		Runs:        o.runs,
	}
	if !o.off {
		service := o.service
		if service == nil {
			service = &quickassistant.Service{
				Transport: transport, Release: telemetry.Release,
				Tools: quickassistant.Definitions(quickassistant.DefinitionOptions{DeviceWrites: o.devices != nil}),
			}
		}
		opts.QuickAssistant = &QuickAssistant{Service: service, Deps: qaDeps(t, pool, o.devices)}
	}
	module := New(pool, opts)
	turns := make(chan struct{}, 16)
	module.afterTurn = func() { turns <- struct{}{} }
	users := auth.PGUsers(pool)
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(testKeys, users), Modules: []httpserver.Module{module}})
	return &qa{
		env: env{t: t, e: e, pool: pool}, module: module, gateway: gateway, telemetry: telemetry,
		analytics: analytics, followUps: followUps, turns: turns,
	}
}

// seed is a user (tier) with their personal vault and one conversation.
func (q *qa) seed(tier string) (user, conversation uuid.UUID) {
	q.t.Helper()
	user = uuid.New()
	short := strings.ToLower(user.String()[:8])
	q.exec(`INSERT INTO users (id, email, username, password_hash, is_verified, tier, created_at, updated_at)
		VALUES ($1, $2, $3, 'stub', true, $4, now(), now())`, user, "qa-e2e-"+short+"@test.luminavault", "qa-e2e-"+short, tier)
	q.exec(`INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'personal', now(), now())`, user)
	conversation = uuid.New()
	q.exec(`INSERT INTO conversations (id, tenant_id, title, pinned_memory_ids, created_at, updated_at) VALUES ($1, $2, 'qa', '{}', now(), now())`, conversation, user)
	return user, conversation
}

func (q *qa) exec(sql string, args ...any) {
	q.t.Helper()
	_, err := q.pool.Exec(context.Background(), sql, args...)
	require.NoError(q.t, err)
}

func (q *qa) count(sql string, args ...any) int {
	q.t.Helper()
	var n int
	require.NoError(q.t, q.pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

type qaBody struct {
	content, tab string
	space        *uuid.UUID
	zone         *string
}

func body(b qaBody) string {
	if b.content == "" {
		b.content = "hello"
	}
	if b.tab == "" {
		b.tab = "home"
	}
	extra := ""
	if b.space != nil {
		extra += `,"activeSpaceID":"` + strings.ToUpper(b.space.String()) + `"`
	}
	if b.zone != nil {
		extra += `,"timeZone":"` + *b.zone + `"`
	}
	return `{"content":"` + b.content + `","quickAssistant":{"currentTab":"` + b.tab + `"` + extra + `}}`
}

func zone(z string) *string { return &z }

// send posts one message and waits for the turn to finish.
func (q *qa) send(user, conversation uuid.UUID, raw string, headers ...string) []map[string]any {
	q.t.Helper()
	rec := q.do(user, http.MethodPost, "/v1/conversations/"+conversation.String()+"/messages/stream", raw, headers...)
	require.Equal(q.t, http.StatusOK, rec.Code, rec.Body.String())
	q.waitTurn()
	return frames(q.t, rec.Body.String())
}

func (q *qa) waitTurn() {
	q.t.Helper()
	select {
	case <-q.turns:
	case <-time.After(30 * time.Second):
		q.t.Fatal("the turn did not finish")
	}
}

// sendAndDrop posts one message and drops the request when drop says so,
// reading what was written so far. setup, when set, gets the drop itself.
func (q *qa) sendAndDrop(user, conversation uuid.UUID, raw string, setup func(cancel func()), drop func(written string) bool) string {
	q.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if setup != nil {
		setup(cancel)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/conversations/"+conversation.String()+"/messages/stream", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	token, err := scenario.Mint(testKeys, user)
	require.NoError(q.t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	w := &droppingWriter{ResponseRecorder: httptest.NewRecorder(), drop: drop, cancel: cancel}
	q.e.ServeHTTP(w, req)
	q.waitTurn()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen.String()
}

type droppingWriter struct {
	*httptest.ResponseRecorder
	mu     sync.Mutex
	seen   strings.Builder
	drop   func(string) bool
	cancel func()
}

func (w *droppingWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	w.seen.Write(b)
	drop := w.drop != nil && w.drop(w.seen.String())
	w.mu.Unlock()
	if drop {
		w.cancel()
	}
	return w.ResponseRecorder.Write(b)
}

func tokens(fs []map[string]any) string {
	var b strings.Builder
	for _, f := range fs {
		if f["type"] == "token" {
			s, _ := f["payload"].(string)
			b.WriteString(s)
		}
	}
	return b.String()
}

func hasFrame(fs []map[string]any, kind string) bool {
	for _, f := range fs {
		if f["type"] == kind {
			return true
		}
	}
	return false
}

func chips(fs []map[string]any) []string {
	var out []string
	for _, f := range fs {
		if f["type"] == "assistant_action" {
			p, _ := f["payload"].(map[string]any)
			s, _ := p["summary"].(string)
			out = append(out, s)
		}
	}
	return out
}

type storedRow struct {
	Role, Content string
	ToolCalls     int
	ID            uuid.UUID
}

func (q *qa) stored(conversation uuid.UUID) []storedRow {
	q.t.Helper()
	rows, err := q.pool.Query(context.Background(),
		`SELECT id, role, content, tool_call_count FROM conversation_messages WHERE conversation_id = $1 ORDER BY created_at, role DESC`, conversation)
	require.NoError(q.t, err)
	defer rows.Close()
	var out []storedRow
	for rows.Next() {
		var r storedRow
		require.NoError(q.t, rows.Scan(&r.ID, &r.Role, &r.Content, &r.ToolCalls))
		out = append(out, r)
	}
	require.NoError(q.t, rows.Err())
	return out
}

func contents(rows []storedRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Content
	}
	return out
}

func (q *qa) indexJobs(tenant uuid.UUID) int {
	return q.count(`SELECT count(*) FROM memory_index_jobs WHERE tenant_id = $1`, tenant)
}

func (q *qa) firstQueryLatched(tenant uuid.UUID) bool {
	return q.count(`SELECT count(*) FROM onboarding_state WHERE tenant_id = $1 AND first_query_completed`, tenant) == 1
}

func (q *qa) seedReserved(tenant uuid.UUID, micros int64) {
	q.exec(`INSERT INTO router_monthly_usage (tenant_id, month, reserved_usd_micros)
		VALUES ($1, date_trunc('month', CURRENT_DATE)::date, $2)`, tenant, micros)
}

func (q *qa) reserved(tenant uuid.UUID) int64 {
	q.t.Helper()
	var n int64
	require.NoError(q.t, q.pool.QueryRow(context.Background(), `SELECT reserved_usd_micros FROM router_monthly_usage
		WHERE tenant_id = $1 AND month = date_trunc('month', CURRENT_DATE)::date`, tenant).Scan(&n))
	return n
}

func (q *qa) pin(tenant uuid.UUID, provider, model string) {
	q.exec(`INSERT INTO user_llm_preferences (id, tenant_id, primary_provider, primary_model, fallback_chain, mode, created_at, updated_at)
		VALUES ($1, $2, $3, $4, '{"steps":[]}'::jsonb, 'byok', now(), now())`, uuid.New(), tenant, provider, model)
}

// paidDecision really holds reservation. "locked" keeps the classic path on
// its plain gateway branch, which settles nothing, so the reservation row
// shows exactly what the Quick Assistant did to it.
func paidDecision(tenant uuid.UUID, primary llm.Route, reservation int64) llm.Decision {
	return llm.Decision{Primary: primary, CredentialMode: llm.ModeManaged, Cerberus: &llm.CerberusMetadata{
		ExecutionID: uuid.New(), TenantID: tenant, VaultID: tenant, ActorUserID: tenant, ProfileID: uuid.New(),
		ProfileName: "QA", TaskType: "general", Surface: llm.SurfaceChat, Strategy: "sequential",
		MinimumSuccessfulResults: 1, PredictedCostUsdMicros: reservation, BudgetReservationUsdMicros: reservation,
		Mode: llm.ModeManaged, RoutingPolicy: "locked",
	}}
}

func decodeBody(payload []byte) map[string]any {
	var out map[string]any
	_ = json.Unmarshal(payload, &out)
	return out
}

// parts is (system text, content of the last tool message) of a model request.
func parts(t *testing.T, payload []byte) (system string, lastTool *string) {
	t.Helper()
	b := decodeBody(payload)
	messages, _ := b["messages"].([]any)
	require.NotEmpty(t, messages)
	for _, m := range messages {
		msg := m.(map[string]any)
		if msg["role"] == "system" && system == "" {
			system, _ = msg["content"].(string)
		}
		if msg["role"] == "tool" {
			c, _ := msg["content"].(string)
			lastTool = &c
		}
	}
	return system, lastTool
}

// runTurn is one turn on a scripted Anthropic route for a fresh user.
func runTurn(t *testing.T, steps ...qaStep) (*qa, uuid.UUID, uuid.UUID, []map[string]any) {
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: decision(haikuRoute)}, adapters: []llm.Adapter{newAdapter(llm.Anthropic, steps...)}})
	user, conversation := q.seed("pro")
	return q, user, conversation, q.send(user, conversation, body(qaBody{}))
}

// MARK: - Flag off and ordinary turns (Review Focus 1)

func TestFlagOffIgnoresQuickAssistantField(t *testing.T) {
	v := newEnv(t, nil)
	turn := func(raw string) ([]map[string]any, []string, string) {
		user := v.tenant()
		conv := v.create(user, `{"title":"qa"}`)
		rec := v.do(user, http.MethodPost, "/v1/conversations/"+conv.ID.String()+"/messages/stream", raw)
		require.Equal(t, http.StatusOK, rec.Code, raw+": "+rec.Body.String())
		rows, err := v.pool.Query(context.Background(), `SELECT role || ':' || content FROM conversation_messages WHERE conversation_id = $1 ORDER BY created_at, role DESC`, conv.ID.UUID)
		require.NoError(t, err)
		var stored []string
		for rows.Next() {
			var s string
			require.NoError(t, rows.Scan(&s))
			stored = append(stored, s)
		}
		rows.Close()
		request, err := json.Marshal(v.stream.last)
		require.NoError(t, err)
		return frames(t, rec.Body.String()), stored, string(request)
	}
	wantFrames, wantRows, wantRequest := turn(`{"content":"hello"}`)
	require.Contains(t, wantRequest, `"Temperature":0.4`)
	for _, field := range []string{`{"currentTab":"home","timeZone":"America/Santiago"}`, `null`, `"x"`, `{"currentTab":1}`} {
		gotFrames, gotRows, gotRequest := turn(`{"content":"hello","quickAssistant":` + field + `}`)
		assert.Equal(t, wantFrames, gotFrames, field)
		assert.Equal(t, wantRows, gotRows, field)
		assert.Equal(t, wantRequest, gotRequest, field)
	}
}

func TestNoQuickAssistantFieldIsUnchanged(t *testing.T) {
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: decision(gatewayRoute)}})
	user, conversation := q.seed("pro")
	// An alternating history, which the merge must pass through as it is.
	q.exec(`INSERT INTO conversation_messages (id, conversation_id, role, content, source_memory_ids, tool_call_count, created_at)
		VALUES ($1, $3, 'user', 'earlier question', '{}', 0, now() - interval '2 minutes'),
		       ($2, $3, 'assistant', 'earlier answer', '{}', 0, now() - interval '1 minute')`, uuid.New(), uuid.New(), conversation)

	fs := q.send(user, conversation, `{"content":"hello"}`)

	assert.Equal(t, []string{"token", "token", "follow_ups", "done"}, frameTypes(fs))
	assert.Equal(t, "classic answer", tokens(fs))
	requests := q.gateway.all()
	require.Len(t, requests, 1)
	require.NotNil(t, requests[0].Temperature)
	assert.InDelta(t, 0.4, *requests[0].Temperature, 1e-9)
	system := requests[0].Messages[0].Content
	assert.NotContains(t, system, "Quick Assistant")
	assert.NotContains(t, system, quickassistant.ActionsUnavailable)
	var roles []string
	for _, m := range requests[0].Messages[1:] {
		roles = append(roles, m.Role+":"+m.Content)
	}
	assert.Equal(t, []string{"user:earlier question", "assistant:earlier answer", "user:hello"}, roles)
	assert.Positive(t, q.followUps.calls.Load(), "ordinary chat still asks for follow-ups")
}

// Ruling 55: two assistant messages in a row reach the model as one, only
// when Quick Assistant is on.
func TestAdjacentAssistantMessagesMergeOnlyWhenOn(t *testing.T) {
	pool := qaPool(t)
	for _, on := range []bool{false, true} {
		q := newQA(t, pool, qaOptions{router: &fixedRouter{decision: decision(gatewayRoute)}, off: !on})
		user, conversation := q.seed("pro")
		q.exec(`INSERT INTO conversation_messages (id, conversation_id, role, content, source_memory_ids, tool_call_count, created_at)
			VALUES ($1, $4, 'user', 'add a reminder', '{}', 0, now() - interval '3 minutes'),
			       ($2, $4, 'assistant', 'Confirm below.', '{}', 0, now() - interval '2 minutes'),
			       ($3, $4, 'assistant', 'Cancelled. Nothing was changed.', '{}', 0, now() - interval '1 minute')`,
			uuid.New(), uuid.New(), uuid.New(), conversation)

		q.send(user, conversation, `{"content":"hello"}`)

		var history []string
		for _, m := range q.gateway.all()[0].Messages[1:] {
			history = append(history, m.Role+":"+m.Content)
		}
		if on {
			assert.Equal(t, []string{"user:add a reminder", "assistant:Confirm below.\n\nCancelled. Nothing was changed.", "user:hello"}, history)
		} else {
			assert.Equal(t, []string{"user:add a reminder", "assistant:Confirm below.", "assistant:Cancelled. Nothing was changed.", "user:hello"}, history)
		}
	}
}

func TestMalformedQuickAssistantIs400WhenOn(t *testing.T) {
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: decision(gatewayRoute)}})
	user, conversation := q.seed("pro")
	for _, field := range []string{`"x"`, `{"currentTab":1}`, `{}`, `[]`} {
		rec := q.do(user, http.MethodPost, "/v1/conversations/"+conversation.String()+"/messages/stream",
			`{"content":"hello","quickAssistant":`+field+`}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code, field)
	}
	assert.Empty(t, q.stored(conversation), "rejected before the user message is written")
	assert.Empty(t, q.gateway.all())
}

func TestContextIsNotInPersistedUserMessage(t *testing.T) {
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: decision(haikuRoute)}, adapters: []llm.Adapter{newAdapter(llm.Anthropic, say("ok"))}})
	user, conversation := q.seed("pro")

	q.send(user, conversation, body(qaBody{tab: "qa-sentinel-tab"}))

	rec := q.do(user, http.MethodGet, "/v1/conversations/"+conversation.String(), "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "qa-sentinel-tab")
	assert.Equal(t, []string{"hello", "ok"}, contents(q.stored(conversation)))
}

// MARK: - Through the built stack

// anthropicStub answers /v1/messages: a tool_use for app_reminder_create,
// then text. It records every request body.
type anthropicStub struct {
	mu     sync.Mutex
	bodies [][]byte
	server *httptest.Server
}

func newAnthropicStub(t *testing.T) *anthropicStub {
	s := &anthropicStub{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, b)
		n := len(s.bodies)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5",`+
				`"content":[{"type":"tool_use","id":"toolu_1","name":"app_reminder_create","input":{"title":"Call mum","fireAt":"2030-01-01T09:00:00"}}],`+
				`"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"msg_2","type":"message","role":"assistant","model":"claude-haiku-5-5",`+
			`"content":[{"type":"text","text":"Answer from the stub."}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *anthropicStub) all() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.bodies...)
}

// gatewayStub is the Hermes gateway: an OpenAI stream of "classic answer".
type gatewayStub struct {
	mu     sync.Mutex
	bodies [][]byte
	server *httptest.Server
}

func newGatewayStub(t *testing.T) *gatewayStub {
	s := &gatewayStub{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, b)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, delta := range []string{"classic ", "answer"} {
			_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"`+delta+`"}}]}`+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *gatewayStub) all() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.bodies...)
}

// builtStack is llm.NewStack as cmd/App builds it, over env, with the chat
// stream and the loop's service wired the way cmd/App/chat.go wires them.
func builtStack(t *testing.T, pool *pgxpool.Pool, gateway *gatewayStub, env map[string]string) qaOptions {
	t.Helper()
	gatewayURL, err := url.Parse(gateway.server.URL)
	require.NoError(t, err)
	base := map[string]string{"FREELANE_ENABLED": "false"}
	for k, v := range env {
		base[k] = v
	}
	stack := llm.NewStack(llm.StackConfig{
		Getenv: func(k string) string { return base[k] }, Pool: pool, HermesURL: gatewayURL, HermesDefaultModel: "hermes-3",
	})
	return qaOptions{
		stream: stack.StreamService(llm.GatewayConfig{BaseURL: gatewayURL, DefaultModel: "hermes-3", IdleTimeout: 10 * time.Second}),
		service: &quickassistant.Service{
			Transport: stack.Transport, Model: "hermes-3", Release: stack.Telemetry.Release,
			Tools: quickassistant.Definitions(quickassistant.DefinitionOptions{DeviceWrites: false}),
		},
	}
}

func TestTheBuiltAppRunsTheLoopOnTheTrialRoute(t *testing.T) {
	pool := qaPool(t)
	anthropic := newAnthropicStub(t)
	gateway := newGatewayStub(t)
	q := newQA(t, pool, builtStack(t, pool, gateway, map[string]string{
		"ANTHROPIC_FIRST": "true", "ANTHROPIC_API_KEY": "fake-key-not-real",
		"LLM_PROVIDER_ANTHROPIC_API_KEY":  "fake-key-not-real",
		"LLM_PROVIDER_ANTHROPIC_BASE_URL": anthropic.server.URL,
	}))
	user, conversation := q.seed("trial")

	fs := q.send(user, conversation, body(qaBody{}))

	// Exactly what the client receives. Each round is one decided call, and
	// the transport publishes its routing frame before the call and its usage
	// frame after it, as Swift's RoutedLLMTransport.completeDecided does
	// (publishSelectedRoute, then publishUsage). The chip comes between the
	// rounds; the reply is streamed as tokens after the last one.
	require.Equal(t, []string{
		"routing", "usage", "assistant_action",
		"routing", "usage",
		"token", "token", "token", "token",
		"done",
	}, frameTypes(fs))
	assert.Equal(t, "done", fs[2]["payload"].(map[string]any)["kind"])
	assert.Equal(t, "selected", fs[0]["payload"].(map[string]any)["phase"])
	assert.Equal(t, fs[0]["payload"].(map[string]any)["executionID"], fs[3]["payload"].(map[string]any)["executionID"],
		"both rounds report the one decision picked for the question")
	assert.Equal(t, "Answer from the stub.", tokens(fs))
	assert.Equal(t, "done", fs[len(fs)-1]["type"])
	assert.False(t, hasFrame(fs, "follow_ups"))
	assert.False(t, hasFrame(fs, "error"))
	assert.Empty(t, gateway.all())
	assert.Equal(t, 1, q.count(`SELECT count(*) FROM reminders WHERE tenant_id = $1`, user))

	bodies := anthropic.all()
	require.Len(t, bodies, 2)
	for _, b := range bodies {
		sent := decodeBody(b)
		assert.NotEmpty(t, sent["tools"])
		for _, key := range []string{"temperature", "top_p", "top_k"} {
			assert.NotContains(t, sent, key)
		}
	}
}

func TestTheBuiltAppSendsNoActionFrameWithNoToolRoute(t *testing.T) {
	pool := qaPool(t)
	gateway := newGatewayStub(t)
	q := newQA(t, pool, builtStack(t, pool, gateway, nil))
	user, conversation := q.seed("trial")

	fs := q.send(user, conversation, body(qaBody{}))

	assert.Equal(t, "classic answer", tokens(fs))
	assert.False(t, hasFrame(fs, "assistant_action"))
	assert.Equal(t, "done", fs[len(fs)-1]["type"])
	bodies := gateway.all()
	require.Len(t, bodies, 1)
	sent := decodeBody(bodies[0])
	assert.NotContains(t, sent, "temperature")
	system, _ := parts(t, bodies[0])
	assert.Contains(t, system, quickassistant.ActionsUnavailable)
}

func TestNoToolRouteFallsBackToClassic(t *testing.T) {
	q := newQA(t, qaPool(t), qaOptions{})
	user, conversation := q.seed("pro")
	// Ours plus another in-flight request's.
	q.seedReserved(user, 6000)
	router := &fixedRouter{decision: paidDecision(user, gatewayRoute, 1000)}
	q = newQA(t, q.pool, qaOptions{router: router})

	fs := q.send(user, conversation, body(qaBody{}))

	assert.Equal(t, "classic answer", tokens(fs))
	assert.False(t, hasFrame(fs, "assistant_action"))
	assert.Equal(t, "done", fs[len(fs)-1]["type"])
	// Released before the classic path picks its own route, exactly once.
	assert.Equal(t, int64(5000), q.reserved(user))
	assert.Equal(t, int32(2), router.picks.Load())
	requests := q.gateway.all()
	require.Len(t, requests, 1)
	assert.Nil(t, requests[0].Temperature)
	assert.Contains(t, requests[0].Messages[0].Content, quickassistant.ActionsUnavailable)
	assert.Equal(t, []string{"hello", "classic answer"}, contents(q.stored(conversation)))
}

func TestPaidFirstRoundFailureReleasesOnceThenAnswersClassic(t *testing.T) {
	pool := qaPool(t)
	adapter := newAdapter(llm.Anthropic, fail())
	q := newQA(t, pool, qaOptions{})
	user, conversation := q.seed("pro")
	q.seedReserved(user, 6000)
	q = newQA(t, pool, qaOptions{router: &fixedRouter{decision: paidDecision(user, haikuRoute, 1000)}, adapters: []llm.Adapter{adapter}})

	fs := q.send(user, conversation, body(qaBody{}))

	assert.Equal(t, "classic answer", tokens(fs))
	assert.Len(t, adapter.completionPayloads(), 1)
	// The transport released it; a second release would leave 4000.
	assert.Equal(t, int64(5000), q.reserved(user))
}

// MARK: - A real tool turn

func TestToolTurnAnswersPersistsAndKeepsContextOutOfTheRow(t *testing.T) {
	adapter := newAdapter(llm.Anthropic, recentThen("You saved nothing recently.")...)
	router := &fixedRouter{decision: decision(haikuRoute)}
	q := newQA(t, qaPool(t), qaOptions{router: router, adapters: []llm.Adapter{adapter}})
	user, conversation := q.seed("pro")
	space := uuid.New()
	q.exec(`INSERT INTO spaces (id, tenant_id, name, slug) VALUES ($1, $2, 'Dentist Plans', $3)`, space, user, "dentist-"+space.String()[:8])

	fs := q.send(user, conversation, body(qaBody{tab: "brain", space: &space}))

	assert.Equal(t, "You saved nothing recently.", tokens(fs))
	assert.Equal(t, "done", fs[len(fs)-1]["type"])
	assert.False(t, hasFrame(fs, "error"))
	assert.False(t, hasFrame(fs, "assistant_action"), "a read shows no chip")
	assert.False(t, hasFrame(fs, "follow_ups"))
	assert.Empty(t, q.gateway.all())
	assert.Equal(t, int32(1), router.picks.Load())

	payloads := adapter.completionPayloads()
	require.Len(t, payloads, 2)
	first, _ := parts(t, payloads[0])
	assert.Contains(t, first, "The user is on the Brain tab")
	assert.Contains(t, first, "Dentist Plans")
	assert.Contains(t, first, "You are Lumina")
	assert.NotContains(t, decodeBody(payloads[0]), "temperature")
	_, lastTool := parts(t, payloads[1])
	require.NotNil(t, lastTool)
	assert.NotEmpty(t, *lastTool)

	rows := q.stored(conversation)
	assert.Equal(t, []string{"hello", "You saved nothing recently."}, contents(rows))
	assert.Equal(t, 1, rows[1].ToolCalls)
}

func TestActiveSpaceFromAnotherVaultIsIgnored(t *testing.T) {
	adapter := newAdapter(llm.Anthropic, say("ok"))
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: decision(haikuRoute)}, adapters: []llm.Adapter{adapter}})
	user, conversation := q.seed("pro")
	other, _ := q.seed("pro")
	space := uuid.New()
	q.exec(`INSERT INTO spaces (id, tenant_id, name, slug) VALUES ($1, $2, 'Someone Elses Space', $3)`, space, other, "other-"+space.String()[:8])

	q.send(user, conversation, body(qaBody{space: &space}))

	system, _ := parts(t, adapter.completionPayloads()[0])
	assert.NotContains(t, system, "Someone Elses Space")
	assert.NotContains(t, system, "active space")
}

// MARK: - One question, one grant (Review Focus 5)

// freeLaneRouter is the real Cerberus router and gate on a gateway table: a
// lapsed user's every pick claims.
func freeLaneRouter(pool *pgxpool.Pool) (*llm.CerberusRouter, *llm.FreeLaneGate) {
	gate := llm.NewFreeLaneGate(pool, llm.FreeLaneLimits{PerUserDaily: 20, PerLegDaily: map[llm.FreeLaneLeg]int64{
		llm.LegOpenRouterFree: 1000, llm.LegNvidiaDirect: 1000,
	}})
	preferences := llm.NewPreferenceRepository(pool)
	return &llm.CerberusRouter{
		Profiles: llm.NewRouterProfileRepository(pool, preferences, "deepseek/deepseek-v4-flash"),
		Fallback: &fixedRouter{decision: decision(gatewayRoute)},
		Budget:   llm.NewRouterTelemetryService(pool),
		Registry: llm.NewRegistry([]llm.ProviderConfig{{Kind: llm.OpenRouter, APIKey: "or-key"}, {Kind: llm.Nvidia, APIKey: "nvapi-key"}}),
		FreeLane: &llm.FreeLaneRuntime{
			Gate: gate, OpenRouterModel: llm.FreeLaneOpenRouterModel,
			OpenRouterSecondaryModel: llm.FreeLaneOpenRouterSecondaryModel, NvidiaModel: llm.FreeLaneNvidiaModel,
		},
	}, gate
}

func resetLegBuckets(t *testing.T, pool *pgxpool.Pool) {
	_, err := pool.Exec(context.Background(), `DELETE FROM workflow_spend_buckets WHERE scope_key LIKE 'freelane:leg:%'`)
	require.NoError(t, err)
}

func TestFourRoundFreeLaneTurnClaimsOneGrant(t *testing.T) {
	pool := qaPool(t)
	resetLegBuckets(t, pool)
	router, gate := freeLaneRouter(pool)
	adapter := newAdapter(llm.OpenRouter, call("recent", "{}"), call("recent", "{}"), call("recent", "{}"), say("Four rounds later."))
	q := newQA(t, pool, qaOptions{router: router, adapters: []llm.Adapter{adapter}})
	user, conversation := q.seed("lapsed")
	before := gate.RemainingToday(context.Background(), user)

	fs := q.send(user, conversation, body(qaBody{}))

	assert.Equal(t, "Four rounds later.", tokens(fs))
	assert.Len(t, adapter.completionPayloads(), 4)
	assert.Equal(t, before-1, gate.RemainingToday(context.Background(), user))
}

func TestFallbackFreeLaneTurnClaimsOneGrant(t *testing.T) {
	pool := qaPool(t)
	resetLegBuckets(t, pool)
	router, gate := freeLaneRouter(pool)
	openRouter := newAdapter(llm.OpenRouter, fail())
	nvidia := newAdapter(llm.Nvidia, fail())
	q := newQA(t, pool, qaOptions{router: router, adapters: []llm.Adapter{openRouter, nvidia}})
	user, conversation := q.seed("lapsed")
	before := gate.RemainingToday(context.Background(), user)

	fs := q.send(user, conversation, body(qaBody{}))

	assert.Equal(t, "lane answer", tokens(fs))
	assert.False(t, hasFrame(fs, "assistant_action"))
	assert.Len(t, openRouter.streamPayloads(), 1, "classic reused the lane's decision")
	assert.Empty(t, q.gateway.all(), "never the paid gateway")
	assert.Equal(t, before-1, gate.RemainingToday(context.Background(), user))
}

// MARK: - BYO Hermes and BYOK

// A BYO Hermes user's trial is never prepended, so the decision is
// gateway-only and they answer through classic.
func TestBYOHermesUserGetsAnswersOnly(t *testing.T) {
	adapter := newAdapter(llm.Anthropic, say("never"))
	trial := llm.TrialConfig{APIKey: "fake-key-not-real", Model: "claude-haiku-5-5", Scope: llm.ScopeAll}
	byo := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Set("lv.hermes.resolution", llm.Resolution{IsUserOverride: true})
			return next(c)
		}
	}
	router := &llm.TrialRouter{Inner: &fixedRouter{decision: decision(gatewayRoute)}, Config: trial}
	q := newQA(t, qaPool(t), qaOptions{router: router, adapters: []llm.Adapter{adapter}, hermes: byo})
	user, conversation := q.seed("pro")

	fs := q.send(user, conversation, body(qaBody{}))

	assert.Equal(t, "classic answer", tokens(fs))
	assert.False(t, hasFrame(fs, "assistant_action"))
	assert.Empty(t, adapter.completionPayloads())
	assert.Empty(t, adapter.streamPayloads())

	// Control: without the override the trial goes in front and the loop runs.
	control := newQA(t, q.pool, qaOptions{router: router, adapters: []llm.Adapter{adapter}})
	user2, conversation2 := control.seed("pro")
	assert.Equal(t, "never", tokens(control.send(user2, conversation2, body(qaBody{}))))
}

// Cerberus would rank another provider first, with a fallback behind it.
var byokPool = llm.Decision{
	Primary:        llm.Route{Provider: llm.Anthropic, ModelID: "pool-model"},
	Fallbacks:      []llm.Route{{Provider: llm.OpenAI, ModelID: "pool-fallback"}},
	CredentialMode: llm.ModeBYOK,
}

func TestPinnedModelServesEveryRoundAndNothingElseIsCalled(t *testing.T) {
	pinned := newAdapter(llm.OpenAI, recentThen("From your model.")...)
	other := newAdapter(llm.Anthropic, say("wrong provider"))
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: byokPool}, adapters: []llm.Adapter{pinned, other}})
	user, conversation := q.seed("pro")
	q.pin(user, "openai", "pinned-model")

	fs := q.send(user, conversation, body(qaBody{}))

	assert.Equal(t, "From your model.", tokens(fs))
	assert.Empty(t, other.completionPayloads())
	payloads := pinned.completionPayloads()
	require.Len(t, payloads, 2)
	for _, p := range payloads {
		assert.Equal(t, "pinned-model", pinnedPayloadModel(p))
	}
}

func TestAPinnedModelThatFailsLandsOnClassicWithoutWalkingToOthers(t *testing.T) {
	pinned := newAdapter(llm.OpenAI, fail())
	other := newAdapter(llm.Anthropic, say("wrong provider"))
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: byokPool}, adapters: []llm.Adapter{pinned, other}})
	user, conversation := q.seed("pro")
	q.pin(user, "openai", "pinned-model")

	fs := q.send(user, conversation, body(qaBody{}))

	assert.Len(t, pinned.completionPayloads(), 1)
	assert.Len(t, pinned.streamPayloads(), 1, "classic answered on the pinned model too")
	assert.Empty(t, other.completionPayloads())
	assert.Empty(t, other.streamPayloads())
	assert.Equal(t, "lane answer", tokens(fs))
	assert.False(t, hasFrame(fs, "assistant_action"))
}

func TestAPinnedOllamaModelLandsOnClassic(t *testing.T) {
	ollama := newAdapter(llm.Ollama, say("never"))
	anthropic := newAdapter(llm.Anthropic, say("wrong provider"))
	openAI := newAdapter(llm.OpenAI, say("wrong provider"))
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: byokPool}, adapters: []llm.Adapter{ollama, anthropic, openAI}})
	user, conversation := q.seed("pro")
	q.pin(user, "ollama", "local-llama")

	fs := q.send(user, conversation, body(qaBody{}))

	assert.Equal(t, "lane answer", tokens(fs))
	assert.Empty(t, ollama.completionPayloads())
	assert.Len(t, ollama.streamPayloads(), 1)
	for _, a := range []*scriptedAdapter{anthropic, openAI} {
		assert.Empty(t, a.completionPayloads())
		assert.Empty(t, a.streamPayloads())
	}
	system, _ := parts(t, ollama.streamPayloads()[0])
	assert.Contains(t, system, quickassistant.ActionsUnavailable)
}

// Ruling 51: Gemini cannot replay a tool round, so a pinned Gemini model
// answers through the classic path.
func TestAPinnedGeminiModelAnswersOnClassicWithoutATurnOfTools(t *testing.T) {
	gemini := newAdapter(llm.Gemini, call("recent", "{}"), say("never"))
	anthropic := newAdapter(llm.Anthropic, say("wrong provider"))
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: byokPool}, adapters: []llm.Adapter{gemini, anthropic}})
	user, conversation := q.seed("pro")
	q.pin(user, "gemini", "gemini-2.5-flash")

	fs := q.send(user, conversation, body(qaBody{}))

	assert.Equal(t, "lane answer", tokens(fs))
	assert.False(t, hasFrame(fs, "assistant_action"))
	assert.Empty(t, gemini.completionPayloads(), "no tool loop")
	assert.Len(t, gemini.streamPayloads(), 1)
	assert.Empty(t, anthropic.completionPayloads())
	assert.Empty(t, anthropic.streamPayloads())
	system, _ := parts(t, gemini.streamPayloads()[0])
	assert.Contains(t, system, quickassistant.ActionsUnavailable)
}

func TestAManagedUserIsNotPinned(t *testing.T) {
	ranked := newAdapter(llm.Anthropic, say("Ranked first."))
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: byokPool}, adapters: []llm.Adapter{ranked}})
	user, conversation := q.seed("pro")

	assert.Equal(t, "Ranked first.", tokens(q.send(user, conversation, body(qaBody{}))))
}

// MARK: - The turn's time zone

func (q *qa) setAccountZone(user uuid.UUID, z string) {
	q.exec(`UPDATE users SET timezone = $2 WHERE id = $1`, user, z)
}

// seedEvent is one event two days out, so the schedule names it on its Next line.
func (q *qa) seedEvent(tenant uuid.UUID) time.Time {
	start := time.Now().Truncate(time.Minute).Add(48 * time.Hour)
	q.exec(`INSERT INTO calendar_events (tenant_id, source, external_id, title, starts_at, ends_at, status)
		VALUES ($1, 'apple_eventkit', $2, 'Standup', $3, $4, 'confirmed')`, tenant, "qa-"+uuid.NewString(), start, start.Add(30*time.Minute))
	return start
}

func nextLine(start time.Time, z string) string {
	loc, _ := time.LoadLocation(z)
	return `- Next: "Standup" ` + start.In(loc).Format("Mon 2 Jan, 15:04")
}

func santiagoTurn(t *testing.T, q *qa, raw string) ([]map[string]any, *scriptedAdapter, uuid.UUID) {
	t.Helper()
	user, conversation := q.seed("pro")
	q.setAccountZone(user, "Asia/Tokyo")
	q.exec(`INSERT INTO apple_consent (tenant_id, domain, allowed, allow_writes, updated_at) VALUES ($1, $2, true, true, now())`,
		user, devicetools.DomainCalendar)
	adapter := newAdapter(llm.Anthropic,
		call("app_reminder_create", `{"title":"Call mum","fireAt":"2030-01-01T09:00:00"}`),
		call("calendar_create", `{"title":"Dentist","start":"2030-01-01T15:00:00"}`),
		say("Both are set up."))
	q.module.opts.QuickAssistant.Service.Transport = &llm.Transport{
		Registry: llm.NewRegistry(nil, adapter), Router: &fixedRouter{decision: decision(haikuRoute)}, RouterTelemetry: q.telemetry,
	}
	return q.send(user, conversation, raw), adapter, user
}

func (q *qa) reminderFireAt(tenant uuid.UUID) time.Time {
	q.t.Helper()
	var at time.Time
	require.NoError(q.t, q.pool.QueryRow(context.Background(), `SELECT fire_at FROM reminders WHERE tenant_id = $1`, tenant).Scan(&at))
	return at
}

func zoneQA(t *testing.T) *qa {
	return newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: decision(haikuRoute)}, devices: okPhone{}})
}

func TestASantiagoClientGetsItsOwnClockOnChipsCardsAndPrompt(t *testing.T) {
	q := zoneQA(t)
	fs, adapter, user := santiagoTurn(t, q, body(qaBody{zone: zone("America/Santiago")}))

	assert.Equal(t, []string{
		"Reminder set: Call mum, Tue 1 Jan 2030, 09:00",
		"Add to Calendar: Dentist, Tue 1 Jan 2030, 15:00",
	}, chips(fs))
	santiago, err := time.LoadLocation("America/Santiago")
	require.NoError(t, err)
	assert.True(t, q.reminderFireAt(user).Equal(time.Date(2030, 1, 1, 9, 0, 0, 0, santiago)))
	system, _ := parts(t, adapter.completionPayloads()[0])
	assert.Contains(t, system, "(America/Santiago, UTC-0")
	assert.NotContains(t, system, "Asia/Tokyo")
}

func TestAnInvalidOrMissingDeviceZoneFallsBackToTheAccountZone(t *testing.T) {
	q := zoneQA(t)
	for _, z := range []*string{zone("Mars/Olympus"), nil} {
		fs, adapter, user := santiagoTurn(t, q, body(qaBody{zone: z}))

		assert.Equal(t, []string{
			"Reminder set: Call mum, Tue 1 Jan 2030, 09:00",
			"Add to Calendar: Dentist, Tue 1 Jan 2030, 15:00",
		}, chips(fs))
		// 09:00 in Tokyo is 00:00 UTC.
		assert.True(t, q.reminderFireAt(user).Equal(time.Unix(1_893_456_000, 0)))
		system, _ := parts(t, adapter.completionPayloads()[0])
		assert.Contains(t, system, "(Asia/Tokyo, UTC+09:00)")
		assert.NotContains(t, system, "Mars")
	}
}

// With no zone from the device or the account, the prompt asks for an
// explicit offset, the shape the zoneless runner takes, so the first call
// lands instead of being refused.
func TestNoKnownZoneAsksForAnOffsetAndTheFirstCallLands(t *testing.T) {
	q := zoneQA(t)
	user, conversation := q.seed("pro")
	q.setAccountZone(user, "")
	adapter := newAdapter(llm.Anthropic,
		call("app_reminder_create", `{"title":"Call mum","fireAt":"2030-01-01T09:00:00+00:00"}`),
		say("Set."))
	q.module.opts.QuickAssistant.Service.Transport = &llm.Transport{
		Registry: llm.NewRegistry(nil, adapter), Router: &fixedRouter{decision: decision(haikuRoute)}, RouterTelemetry: q.telemetry,
	}
	fs := q.send(user, conversation, body(qaBody{}))

	require.Len(t, chips(fs), 1)
	assert.True(t, q.reminderFireAt(user).Equal(time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC)))
	system, _ := parts(t, adapter.completionPayloads()[0])
	assert.Contains(t, system, "with an explicit UTC offset, like YYYY-MM-DDTHH:MM:SS+00:00.")
	assert.NotContains(t, system, "no offset")
	assert.Contains(t, system, "(UTC, UTC+00:00)")
}

func TestTheClassicFallbackOnAQuickAssistantTurnUsesTheDevicesZone(t *testing.T) {
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: decision(gatewayRoute)}})
	user, conversation := q.seed("pro")
	q.setAccountZone(user, "Asia/Tokyo")
	start := q.seedEvent(user)

	q.send(user, conversation, body(qaBody{zone: zone("America/Santiago")}))

	system := q.gateway.all()[0].Messages[0].Content
	assert.Contains(t, system, quickassistant.ActionsUnavailable)
	assert.Contains(t, system, "(America/Santiago, UTC-0")
	assert.Contains(t, system, nextLine(start, "America/Santiago"))
}

func TestClassicChatKeepsTheAccountZoneAndHasNoClockLine(t *testing.T) {
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: decision(gatewayRoute)}})
	user, conversation := q.seed("pro")
	q.setAccountZone(user, "Asia/Tokyo")
	start := q.seedEvent(user)

	q.send(user, conversation, `{"content":"hello"}`)

	system := q.gateway.all()[0].Messages[0].Content
	assert.Contains(t, system, nextLine(start, "Asia/Tokyo"))
	assert.NotContains(t, system, "(Asia/Tokyo, UTC")
	assert.NotContains(t, system, quickassistant.ActionsUnavailable)
}

// MARK: - Tenancy

func TestVaultToolReadsTheSharedVaultNotTheViewersOwn(t *testing.T) {
	adapter := newAdapter(llm.Anthropic, recentThen("Here.")...)
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: decision(haikuRoute)}, adapters: []llm.Adapter{adapter}})
	owner, _ := q.seed("pro")
	viewer, conversation := q.seed("free")
	team, vault := uuid.New(), uuid.New()
	q.exec(`INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at) VALUES ($1, 'qa-team', $2, $2, now(), now())`, team, owner)
	q.exec(`INSERT INTO vaults (id, team_id, name, created_at, updated_at) VALUES ($1, $2, 'shared', now(), now())`, vault, team)
	q.exec(`INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
		VALUES ($1, $2, $3, 'viewer', true, $4, now(), now())`, uuid.New(), vault, viewer, owner)
	for tenant, path := range map[uuid.UUID]string{vault: "team/shared-plan.md", viewer: "me/private-diary.md"} {
		q.exec(`INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at)
			VALUES ($1, $2, $3, 'text/markdown', 1, $4, now(), now())`, uuid.New(), tenant, path, uuid.NewString())
	}

	q.send(viewer, conversation, body(qaBody{}), "X-Vault-ID", strings.ToUpper(vault.String()))

	payloads := adapter.completionPayloads()
	require.Len(t, payloads, 2)
	_, lastTool := parts(t, payloads[1])
	require.NotNil(t, lastTool)
	assert.Contains(t, *lastTool, "shared-plan")
	assert.NotContains(t, *lastTool, "private-diary")
}

// MARK: - What a finished turn writes down

func TestAnAnswerIsIndexedLatchedAndCountedWithItsRealToolCount(t *testing.T) {
	q, user, conversation, _ := runTurn(t, recentThen("The real answer.")...)

	assert.Equal(t, 1, q.indexJobs(user))
	assert.True(t, q.firstQueryLatched(user))
	assert.Equal(t, []bool{true}, q.analytics.groundedAnswers())
	rows := q.stored(conversation)
	require.Len(t, rows, 2)
	assert.Equal(t, 1, rows[1].ToolCalls)
	traces := q.traces(user, wire.UUID{UUID: conversation})
	require.Len(t, traces, 1)
	require.NotNil(t, traces[0].ConversationMessageID)
	assert.Equal(t, rows[1].ID, traces[0].ConversationMessageID.UUID)
	assert.Equal(t, 1, traces[0].ToolCallCount)
}

func TestAReplyAfterAPersonalToolIsNeverIndexed(t *testing.T) {
	q, user, conversation, _ := runTurn(t, call("health_query", "{}"), say("You walked a lot."))

	assert.Equal(t, 0, q.indexJobs(user))
	assert.Equal(t, []string{"hello", "You walked a lot."}, contents(q.stored(conversation)))
	// Still a real answer: only the indexing is withheld.
	assert.True(t, q.firstQueryLatched(user))
	assert.Equal(t, []bool{true}, q.analytics.groundedAnswers())
}

// expectCannedLineLeftNoTrace: a canned line the user saw is kept, and
// nothing counts it as an answer.
func expectCannedLineLeftNoTrace(t *testing.T, q *qa, user, conversation uuid.UUID, fs []map[string]any, line string) {
	t.Helper()
	assert.Contains(t, tokens(fs), line)
	assert.Equal(t, "done", fs[len(fs)-1]["type"])
	rows := q.stored(conversation)
	require.Len(t, rows, 2)
	assert.Contains(t, rows[1].Content, line)
	assert.Equal(t, 0, q.indexJobs(user))
	assert.False(t, q.firstQueryLatched(user))
	assert.Empty(t, q.analytics.groundedAnswers())
}

func TestAFailedTurnIsNotAnAnswer(t *testing.T) {
	q, user, conversation, fs := runTurn(t, call("recent", "{}"), fail())
	expectCannedLineLeftNoTrace(t, q, user, conversation, fs, "Something went wrong")
}

func TestATurnThatRanOutOfStepsIsNotAnAnswer(t *testing.T) {
	q, user, conversation, fs := runTurn(t, call("recent", "{}"))
	expectCannedLineLeftNoTrace(t, q, user, conversation, fs, "ran out of steps")
}

func TestAStoppedTurnWithNothingDoneWritesNoRow(t *testing.T) {
	adapter := newAdapter(llm.Anthropic, cancelThen("recent"))
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: decision(haikuRoute)}, adapters: []llm.Adapter{adapter}})
	user, conversation := q.seed("pro")

	written := q.sendAndDrop(user, conversation, body(qaBody{}), func(cancel func()) { adapter.cancel = cancel }, nil)

	assert.Equal(t, []string{"hello"}, contents(q.stored(conversation)))
	assert.NotContains(t, written, `"token"`)
	assert.Equal(t, 0, q.indexJobs(user))
	assert.False(t, q.firstQueryLatched(user))
	assert.Empty(t, q.analytics.groundedAnswers())
}

func TestAQuickAssistantTurnNeverStartsAComplexSessionReview(t *testing.T) {
	pool := qaPool(t)
	activity := selfimprove.NewActivity(pool, true, nil)
	adapter := newAdapter(llm.Anthropic, calls("recent", 5), say("Done."))
	q := newQA(t, pool, qaOptions{router: &fixedRouter{decision: decision(haikuRoute)}, adapters: []llm.Adapter{adapter}, activity: activity})
	user, conversation := q.seed("pro")

	q.send(user, conversation, body(qaBody{}))

	rows := q.stored(conversation)
	assert.Equal(t, 5, rows[len(rows)-1].ToolCalls, "enough tools to trigger a review")
	runs := `SELECT count(*) FROM self_improvement_runs WHERE tenant_id = $1`
	assert.Equal(t, 0, q.count(runs, user))
	// Control: the same count from anywhere else does start one.
	activity.TriggerComplexSession(context.Background(), user, 5, false)
	assert.Equal(t, 1, q.count(runs, user))
}

func TestAQuickAssistantTurnNeverEscalates(t *testing.T) {
	runs := &fakeRuns{}
	adapter := newAdapter(llm.Anthropic, say("Here in the card."))
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: decision(haikuRoute)}, adapters: []llm.Adapter{adapter}, runs: runs})
	user, conversation := q.seed("pro")

	fs := q.send(user, conversation, `{"content":"deploy the api","agentMode":"force","quickAssistant":{"currentTab":"home"}}`,
		"x-lv-client-caps", "chat.hermes_run")

	assert.Empty(t, runs.starts)
	assert.False(t, hasFrame(fs, "hermes_run"))
	assert.Equal(t, "Here in the card.", tokens(fs))
}

func TestNoFollowUpsOnAQuickAssistantTurn(t *testing.T) {
	// A loop reply: no follow_ups frame at all.
	q, _, _, fs := runTurn(t, say("Short answer."))
	assert.False(t, hasFrame(fs, "follow_ups"))
	assert.Zero(t, q.followUps.calls.Load())

	// The classic fallback: an empty list, and nobody asked for one.
	classic := newQA(t, q.pool, qaOptions{router: &fixedRouter{decision: decision(gatewayRoute)}})
	user, conversation := classic.seed("pro")
	fs = classic.send(user, conversation, body(qaBody{}))
	var followUps []map[string]any
	for _, f := range fs {
		if f["type"] == "follow_ups" {
			followUps = append(followUps, f)
		}
	}
	require.Len(t, followUps, 1)
	assert.Equal(t, []any{}, followUps[0]["payload"])
	assert.Zero(t, classic.followUps.calls.Load())
}

// Review Focus 4: the user dismisses the card right after a reminder was
// set. The reminder and the assistant's record of it survive.
func TestStoppedAfterAReminderKeepsTheRowAndTheMessage(t *testing.T) {
	adapter := newAdapter(llm.Anthropic,
		call("app_reminder_create", `{"title":"Call mum","fireAt":"2030-01-01T09:00:00"}`),
		qaStep{hang: true})
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: decision(haikuRoute)}, adapters: []llm.Adapter{adapter}})
	user, conversation := q.seed("pro")

	q.sendAndDrop(user, conversation, body(qaBody{}), nil, func(written string) bool {
		return strings.Contains(written, `"assistant_action"`)
	})

	assert.Equal(t, 1, q.count(`SELECT count(*) FROM reminders WHERE tenant_id = $1`, user))
	rows := q.stored(conversation)
	require.Len(t, rows, 2)
	assert.Contains(t, rows[1].Content, "This request was stopped before it finished.")
	assert.Contains(t, rows[1].Content, "Call mum")
	assert.Equal(t, 0, q.indexJobs(user))
}

func TestARefusedToolNeverRunsEndToEnd(t *testing.T) {
	pool := qaPool(t)
	adapter := newAdapter(llm.Anthropic)
	q := newQA(t, pool, qaOptions{router: &fixedRouter{decision: decision(haikuRoute)}, adapters: []llm.Adapter{adapter}})
	user, conversation := q.seed("pro")
	saved := quickassistant.NewRunner(qaDeps(t, pool, nil), user, user, nil, nil, nil).
		Run(context.Background(), "memory_save", map[string]any{"content": "original text"})
	require.NotContains(t, saved.ModelContent, "did not run")
	var memoryID uuid.UUID
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT id FROM memories WHERE tenant_id = $1`, user).Scan(&memoryID))
	adapter.steps = []qaStep{
		call("memory_update", `{"memoryID":"`+memoryID.String()+`","content":"rewritten"}`),
		call("reminder_create", `{"title":"x"}`),
		say("Sorry."),
	}

	fs := q.send(user, conversation, body(qaBody{}))

	assert.False(t, hasFrame(fs, "assistant_action"))
	payloads := adapter.completionPayloads()
	require.Len(t, payloads, 3)
	const refusal = "That tool is not available. Do not try it again."
	for _, p := range payloads[1:] {
		_, lastTool := parts(t, p)
		require.NotNil(t, lastTool)
		assert.Equal(t, refusal, *lastTool)
	}
	var content string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT content FROM memories WHERE id = $1`, memoryID).Scan(&content))
	assert.Equal(t, "original text", content)
}
