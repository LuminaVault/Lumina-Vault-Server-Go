package quickassistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/llm"
	"lumina/internal/modules/memoryagent"
	"lumina/internal/wire"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The tool loop against a scripted transport and a recording tool runner. No
// database and no real provider: what is under test is which calls happen, in
// which order, and what the money-bearing decision looks like on each.

// MARK: - Fakes

type scriptedCall struct{ id, name, arguments string }

type step struct {
	text  *string
	calls []scriptedCall
	fail  error
	raw   *string
}

func textStep(s string) step                       { return step{text: &s} }
func toolsStep(calls ...scriptedCall) step         { return step{calls: calls} }
func failStep() step                               { return step{fail: errScripted} }
func rawStep(s string) step                        { return step{raw: &s} }
func call(id, name, arguments string) scriptedCall { return scriptedCall{id, name, arguments} }

var errScripted = errors.New("scripted failure")

// scriptedTransport plays steps in order; once they run out it repeats the last.
type scriptedTransport struct {
	mu        sync.Mutex
	steps     []step
	picked    llm.Decision
	pickDelay time.Duration
	payloads  [][]byte
	decisions []llm.Decision
	picks     int
	// pickPayload is what PickDecision was given.
	pickPayload []byte
}

func newTransport(picked llm.Decision, steps ...step) *scriptedTransport {
	return &scriptedTransport{steps: steps, picked: picked}
}

func (s *scriptedTransport) PickDecision(_ context.Context, payload []byte) llm.Decision {
	s.mu.Lock()
	s.picks++
	s.pickPayload = payload
	s.mu.Unlock()
	time.Sleep(s.pickDelay)
	return s.picked
}

func (s *scriptedTransport) ChatCompletionsDecided(_ context.Context, payload []byte, sessionKey, sessionID string, decision llm.Decision) (llm.Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionKey != "key" || sessionID != "session" {
		return llm.Metadata{}, fmt.Errorf("session %q %q", sessionKey, sessionID)
	}
	s.payloads = append(s.payloads, payload)
	s.decisions = append(s.decisions, decision)
	st := s.steps[0]
	if len(s.steps) > 1 {
		s.steps = s.steps[1:]
	}
	switch {
	case st.fail != nil:
		return llm.Metadata{}, st.fail
	case st.raw != nil:
		return llm.Metadata{Data: []byte(*st.raw)}, nil
	case st.text != nil:
		return llm.Metadata{Data: completion(st.text, nil)}, nil
	default:
		return llm.Metadata{Data: completion(nil, st.calls)}, nil
	}
}

func (s *scriptedTransport) sent() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.payloads...)
}

func (s *scriptedTransport) seen() []llm.Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]llm.Decision(nil), s.decisions...)
}

func completion(content *string, calls []scriptedCall) []byte {
	message := map[string]any{"role": "assistant"}
	if content != nil {
		message["content"] = *content
	}
	if len(calls) > 0 {
		var out []map[string]any
		for _, c := range calls {
			out = append(out, map[string]any{
				"id": c.id, "type": "function",
				"function": map[string]any{"name": c.name, "arguments": c.arguments},
			})
		}
		message["tool_calls"] = out
	}
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": message}}})
	return b
}

type ranCall struct {
	name string
	args map[string]any
}

// recordingRunner records each call. after runs once the call returned, which
// is the one moment reliably "between rounds": the tool has finished, the next
// model call has not started.
type recordingRunner struct {
	mu      sync.Mutex
	calls   []ranCall
	results map[string]ToolResult
	after   func()
	// during runs inside Run, before the tool's work is "done"; ctxDone then
	// records whether the ctx the tool was given had been cancelled.
	during  func()
	ctxDone []bool
}

func (r *recordingRunner) Run(ctx context.Context, name string, args map[string]any) ToolResult {
	if r.during != nil {
		r.during()
	}
	r.mu.Lock()
	r.ctxDone = append(r.ctxDone, ctx.Err() != nil)
	r.calls = append(r.calls, ranCall{name, args})
	result, ok := r.results[name]
	r.mu.Unlock()
	if !ok {
		result = ToolResult{ModelContent: "result of " + name}
	}
	if r.after != nil {
		r.after()
	}
	return result
}

func (r *recordingRunner) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []string{}
	for _, c := range r.calls {
		out = append(out, c.name)
	}
	return out
}

type eventLog struct {
	mu     sync.Mutex
	events []memoryagent.StreamEvent
}

func (l *eventLog) emit(e memoryagent.StreamEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) all() []memoryagent.StreamEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]memoryagent.StreamEvent(nil), l.events...)
}

func (l *eventLog) text() string {
	var b strings.Builder
	for _, e := range l.all() {
		if e.Type == "token" {
			b.WriteString(e.Payload.(string))
		}
	}
	return b.String()
}

func (l *eventLog) actions() []Action {
	var out []Action
	for _, e := range l.all() {
		if e.Type == "assistant_action" {
			out = append(out, e.Payload.(Action))
		}
	}
	return out
}

type release struct {
	tenant  uuid.UUID
	micros  int64
	ctxDone bool
}

type releaseLog struct {
	mu       sync.Mutex
	released []release
}

func (r *releaseLog) record(ctx context.Context, tenant uuid.UUID, micros int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = append(r.released, release{tenant, micros, ctx.Err() != nil})
}

func (r *releaseLog) all() []release {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]release(nil), r.released...)
}

// logRecords captures slog records with their attributes.
type logRecords struct {
	mu      sync.Mutex
	records []logRecord
}

type logRecord struct {
	level   slog.Level
	message string
	attrs   map[string]slog.Value
}

type captureHandler struct{ into *logRecords }

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h captureHandler) WithGroup(string) slog.Handler            { return h }
func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, message: r.Message, attrs: map[string]slog.Value{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value
		return true
	})
	h.into.mu.Lock()
	defer h.into.mu.Unlock()
	h.into.records = append(h.into.records, rec)
	return nil
}

func (l *logRecords) named(message string) []logRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []logRecord
	for _, r := range l.records {
		if r.message == message {
			out = append(out, r)
		}
	}
	return out
}

func (l *logRecords) outcomes() []string {
	out := []string{}
	for _, r := range l.named("quick assistant turn") {
		out = append(out, r.attrs["outcome"].String())
	}
	return out
}

// MARK: - Fixtures

var testTenant = uuid.MustParse("7e000000-0000-0000-0000-000000000001")

func metadata(reservation int64, freeLane bool) *llm.CerberusMetadata {
	return &llm.CerberusMetadata{
		TenantID:                   testTenant,
		VaultID:                    testTenant,
		ActorUserID:                testTenant,
		PredictedCostUsdMicros:     reservation,
		BudgetReservationUsdMicros: reservation,
		Mode:                       llm.ModeManaged,
		IsFreeLane:                 freeLane,
	}
}

func paidDecision(reservation int64) llm.Decision {
	return llm.Decision{
		Primary:        llm.Route{Provider: llm.Anthropic, ModelID: "claude-haiku-5-5"},
		Cerberus:       metadata(reservation, false),
		CredentialMode: llm.ModeManaged,
	}
}

func gatewayOnlyDecision(reservation int64) llm.Decision {
	return llm.Decision{
		Primary:        llm.Route{Provider: llm.HermesGateway, ModelID: "hermes-3"},
		Cerberus:       metadata(reservation, false),
		CredentialMode: llm.ModeManaged,
	}
}

func freeLaneDecision() llm.Decision {
	return llm.Decision{
		Primary:        llm.Route{Provider: llm.OpenRouter, ModelID: "free-model"},
		Cerberus:       metadata(0, true),
		CredentialMode: llm.ModeManaged,
	}
}

type harness struct {
	transport *scriptedTransport
	releases  *releaseLog
	logs      *logRecords
	service   *Service
}

func newHarness(transport *scriptedTransport) *harness {
	h := &harness{transport: transport, releases: &releaseLog{}, logs: &logRecords{}}
	h.service = &Service{
		Transport: transport,
		Model:     "router-auto",
		Release:   h.releases.record,
		Tools:     Definitions(DefinitionOptions{}),
		Log:       slog.New(captureHandler{h.logs}),
	}
	return h
}

var testMessages = []llm.ChatMessage{
	{Role: "system", Content: "be helpful"},
	{Role: "user", Content: "what did I save?"},
}

func (h *harness) answer(ctx context.Context, runner ToolRunner, log *eventLog) Outcome {
	if log == nil {
		log = &eventLog{}
	}
	return h.service.Answer(ctx, testMessages, "key", "session", runner, log.emit)
}

// cancelling is a runner that cancels the turn after its first call returns.
func cancelling(results map[string]ToolResult) (context.Context, *recordingRunner) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &recordingRunner{results: results, after: cancel}
	return ctx, runner
}

func doneAction(summary string) *Action {
	return &Action{ID: wire.UUID{UUID: uuid.New()}, Kind: KindDone, Summary: summary}
}

func mustReply(t *testing.T, o Outcome) Reply {
	t.Helper()
	if o.Reply == nil {
		t.Fatalf("expected a reply, got classic (preDecided %v)", o.PreDecided)
	}
	return *o.Reply
}

func decode(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	return body
}

func messagesIn(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range decode(t, payload)["messages"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

func toolMessages(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range messagesIn(t, payload) {
		if m["role"] == "tool" {
			out = append(out, m)
		}
	}
	return out
}

const (
	reminderArguments          = `{"title":"Call mum","fireAt":"2030-01-01T09:00:00Z"}`
	reminderArgumentsReordered = `{"fireAt":"2030-01-01T09:00:00Z","title":"Call mum"}`
)

func writeRunner() *recordingRunner {
	return &recordingRunner{results: map[string]ToolResult{
		"app_reminder_create": {ModelContent: "FIRST-RESULT", Action: doneAction("Reminder set")},
	}}
}

// MARK: - The loop

func TestToolCallThenReply(t *testing.T) {
	tr := newTransport(paidDecision(1000),
		toolsStep(call("call-1", "search", `{"query":"notes"}`)),
		textStep("You saved three notes."),
	)
	h := newHarness(tr)
	runner := &recordingRunner{results: map[string]ToolResult{"search": {ModelContent: "SEARCH-RESULT-TEXT"}}}
	log := &eventLog{}

	reply := mustReply(t, h.answer(context.Background(), runner, log))

	if reply.Text != "You saved three notes." || !reply.IsAnswer || reply.ToolCallCount != 1 {
		t.Errorf("reply = %+v", reply)
	}
	if got := runner.names(); !reflect.DeepEqual(got, []string{"search"}) {
		t.Errorf("runner ran %v", got)
	}
	if got := runner.calls[0].args["query"]; got != "notes" {
		t.Errorf("query = %v", got)
	}
	if log.text() != "You saved three notes." {
		t.Errorf("tokens = %q", log.text())
	}
	payloads := tr.sent()
	if len(payloads) != 2 {
		t.Fatalf("%d rounds, want 2", len(payloads))
	}
	second := messagesIn(t, payloads[1])
	tool := second[len(second)-1]
	if tool["role"] != "tool" || tool["tool_call_id"] != "call-1" || tool["name"] != "search" || tool["content"] != "SEARCH-RESULT-TEXT" {
		t.Errorf("tool message = %v", tool)
	}
	echoed := second[len(second)-2]
	calls, _ := echoed["tool_calls"].([]any)
	if echoed["role"] != "assistant" || len(calls) != 1 || calls[0].(map[string]any)["id"] != "call-1" {
		t.Errorf("echoed assistant message = %v", echoed)
	}
	if _, ok := echoed["content"]; ok {
		t.Errorf("a message with no content echoes content: %v", echoed)
	}
	first := messagesIn(t, payloads[0])
	if len(first) != 2 || first[0]["role"] != "system" || first[0]["content"] != "be helpful" || first[1]["content"] != "what did I save?" {
		t.Errorf("round 1 messages = %v", first)
	}
}

func TestPayloadHasNoTemperature(t *testing.T) {
	tr := newTransport(paidDecision(1000), toolsStep(call("c", "search", "{}")), textStep("ok"))
	h := newHarness(tr)

	mustReply(t, h.answer(context.Background(), &recordingRunner{}, nil))

	for i, payload := range tr.sent() {
		body := decode(t, payload)
		for _, key := range []string{"temperature", "top_p", "top_k"} {
			if _, ok := body[key]; ok {
				t.Errorf("round %d sends %s", i+1, key)
			}
		}
		if body["tool_choice"] != "auto" || body["stream"] != false || body["model"] != "router-auto" || body["tools"] == nil {
			t.Errorf("round %d body = %v", i+1, body)
		}
		if len(body) != 5 {
			t.Errorf("round %d has keys beyond model, messages, tools, tool_choice, stream: %v", i+1, body)
		}
		msgs := messagesIn(t, payload)
		if msgs[len(msgs)-1]["role"] == "assistant" {
			t.Errorf("round %d ends on an assistant message (prefill)", i+1)
		}
	}
}

func TestOffersExactlyTheServiceTools(t *testing.T) {
	tr := newTransport(paidDecision(1000), textStep("ok"))
	h := newHarness(tr)
	h.service.Tools = Definitions(DefinitionOptions{})

	mustReply(t, h.answer(context.Background(), &recordingRunner{}, nil))

	body := decode(t, tr.sent()[0])
	var offered []string
	for _, tool := range body["tools"].([]any) {
		offered = append(offered, tool.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	if want := names(h.service.Tools); !reflect.DeepEqual(offered, want) {
		t.Errorf("offered %v, want %v", offered, want)
	}
	for _, name := range offered {
		if name == "memory_update" || name == "calendar_create" {
			t.Errorf("offered %s", name)
		}
	}
	wantJSON, _ := json.Marshal(OpenAITools(h.service.Tools))
	gotJSON, _ := json.Marshal(body["tools"])
	var a, b any
	_ = json.Unmarshal(wantJSON, &a)
	_ = json.Unmarshal(gotJSON, &b)
	if !reflect.DeepEqual(a, b) {
		t.Error("the tools array is not OpenAITools(Service.Tools)")
	}
}

func TestStopsAtFourRounds(t *testing.T) {
	tr := newTransport(paidDecision(1000), toolsStep(call("c", "search", "{}")))
	h := newHarness(tr)
	runner := &recordingRunner{}

	reply := mustReply(t, h.answer(context.Background(), runner, nil))

	if tr.picks != 1 {
		t.Errorf("%d picks", tr.picks)
	}
	if n := len(tr.sent()); n != 4 {
		t.Errorf("%d rounds, want 4", n)
	}
	if reply.Text != "I ran out of steps before I could finish." {
		t.Errorf("text = %q", reply.Text)
	}
	// The fourth round's calls never ran: the model could not have read
	// their results, and nothing is left to report them to it.
	if n := len(runner.names()); n != 3 {
		t.Errorf("runner ran %d calls, want 3", n)
	}
}

func TestChipsSurviveRoundCap(t *testing.T) {
	tr := newTransport(paidDecision(1000),
		toolsStep(call("c1", "memory_save", `{"content":"x"}`)),
		toolsStep(call("c2", "app_reminder_create", `{"title":"y"}`)),
		toolsStep(call("c3", "search", "{}")),
	)
	h := newHarness(tr)
	saved, reminded := doneAction("Saved a memory"), doneAction("Reminder set")
	runner := &recordingRunner{results: map[string]ToolResult{
		"memory_save":         {ModelContent: "saved", Action: saved},
		"app_reminder_create": {ModelContent: "set", Action: reminded},
	}}
	log := &eventLog{}

	reply := mustReply(t, h.answer(context.Background(), runner, log))

	if got := log.actions(); !reflect.DeepEqual(got, []Action{*saved, *reminded}) {
		t.Errorf("actions = %v", got)
	}
	want := "I ran out of steps before I could finish. Already done: Saved a memory; Reminder set."
	if reply.Text != want || reply.IsAnswer || !reply.ShouldPersist {
		t.Errorf("reply = %+v", reply)
	}
}

func TestFirstRoundFailureFallsBack(t *testing.T) {
	tr := newTransport(paidDecision(1000), failStep())
	h := newHarness(tr)
	runner := &recordingRunner{}
	log := &eventLog{}

	out := h.answer(context.Background(), runner, log)

	if out.Reply != nil || out.PreDecided != nil {
		t.Errorf("outcome = %+v", out)
	}
	if len(log.all()) != 0 || len(runner.names()) != 0 {
		t.Errorf("events %v, calls %v", log.all(), runner.names())
	}
}

func TestEmptyFirstReplyFallsBack(t *testing.T) {
	for _, st := range []step{textStep("   "), textStep(""), rawStep(`{"choices":[]}`), rawStep(`{"choices":[{"message":{"role":"assistant","content":null}}]}`)} {
		tr := newTransport(paidDecision(1000), st)
		log := &eventLog{}
		if out := newHarness(tr).answer(context.Background(), &recordingRunner{}, log); out.Reply != nil || len(log.all()) != 0 {
			t.Errorf("outcome = %+v, events %v", out, log.all())
		}
	}
}

func TestUnreadableFirstReplyFallsBack(t *testing.T) {
	for _, raw := range []string{
		"not a completion",
		`{}`,
		`{"choices":null}`,
		`{"choices":[{}]}`,
		`{"choices":[{"message":{"content":"no role"}}]}`,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"function":{"name":"search","arguments":"{}"}}]}}]}`,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c","function":{"name":"search","arguments":{}}}]}}]}`,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c","function":{"arguments":"{}"}}]}}]}`,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c"}]}}]}`,
	} {
		tr := newTransport(paidDecision(1000), rawStep(raw))
		runner := &recordingRunner{}
		log := &eventLog{}
		out := newHarness(tr).answer(context.Background(), runner, log)
		if out.Reply != nil || len(log.all()) != 0 || len(runner.names()) != 0 {
			t.Errorf("%s: outcome = %+v, events %v, calls %v", raw, out, log.all(), runner.names())
		}
	}
}

func TestLaterRoundFailureNeverRerunsWrites(t *testing.T) {
	tr := newTransport(paidDecision(1000), toolsStep(call("c1", "memory_save", `{"content":"x"}`)), failStep())
	h := newHarness(tr)
	runner := &recordingRunner{results: map[string]ToolResult{
		"memory_save": {ModelContent: "saved", Action: doneAction("Saved a memory")},
	}}

	reply := mustReply(t, h.answer(context.Background(), runner, nil))

	if n := len(tr.sent()); n != 2 {
		t.Errorf("%d rounds", n)
	}
	if got := runner.names(); !reflect.DeepEqual(got, []string{"memory_save"}) {
		t.Errorf("runner ran %v", got)
	}
	if reply.Text != "Something went wrong before I could finish. Already done: Saved a memory." {
		t.Errorf("text = %q", reply.Text)
	}
}

func TestLaterRoundFailureWithNothingDone(t *testing.T) {
	for _, second := range []step{failStep(), rawStep("garbage"), textStep("  ")} {
		tr := newTransport(paidDecision(1000), toolsStep(call("c1", "search", "{}")), second)
		reply := mustReply(t, newHarness(tr).answer(context.Background(), &recordingRunner{}, nil))
		if reply.Text != "Something went wrong before I could finish." {
			t.Errorf("text = %q", reply.Text)
		}
	}
}

func TestCancellationKeepsCompletedWrite(t *testing.T) {
	tr := newTransport(paidDecision(1000), toolsStep(call("c1", "memory_save", `{"content":"x"}`)), textStep("never reached"))
	h := newHarness(tr)
	ctx, runner := cancelling(map[string]ToolResult{
		"memory_save": {ModelContent: "saved", Action: doneAction("Saved a memory")},
	})
	log := &eventLog{}

	reply := mustReply(t, h.answer(ctx, runner, log))

	if n := len(tr.sent()); n != 1 {
		t.Errorf("%d rounds, want no model round after the cancel", n)
	}
	if got := runner.names(); !reflect.DeepEqual(got, []string{"memory_save"}) {
		t.Errorf("runner ran %v", got)
	}
	if len(log.actions()) != 1 {
		t.Errorf("actions = %v", log.actions())
	}
	if !strings.Contains(reply.Text, "Saved a memory") {
		t.Errorf("text = %q", reply.Text)
	}
}

func TestCancellationStopsTheRemainingToolsOfARound(t *testing.T) {
	tr := newTransport(paidDecision(1000), toolsStep(
		call("c1", "memory_save", `{"content":"x"}`),
		call("c2", "app_reminder_create", `{"title":"y"}`),
	))
	ctx, runner := cancelling(nil)

	newHarness(tr).answer(ctx, runner, nil)

	if got := runner.names(); !reflect.DeepEqual(got, []string{"memory_save"}) {
		t.Errorf("runner ran %v", got)
	}
}

func TestStoppedWithNothingDoneLeavesNothing(t *testing.T) {
	tr := newTransport(paidDecision(1000), toolsStep(call("c1", "search", "{}")), textStep("never"))
	ctx, runner := cancelling(nil)
	log := &eventLog{}

	reply := mustReply(t, newHarness(tr).answer(ctx, runner, log))

	if reply.Text != "" || reply.ShouldPersist || reply.IsAnswer {
		t.Errorf("reply = %+v", reply)
	}
	if len(log.all()) != 0 {
		t.Errorf("events = %v", log.all())
	}
}

func TestStoppedAfterAnActionKeepsItsRecord(t *testing.T) {
	tr := newTransport(paidDecision(1000), toolsStep(call("c1", "memory_save", "{}")), textStep("never"))
	ctx, runner := cancelling(map[string]ToolResult{
		"memory_save": {ModelContent: "saved", Action: doneAction("Saved a memory")},
	})

	reply := mustReply(t, newHarness(tr).answer(ctx, runner, nil))

	if !reply.ShouldPersist || reply.IsAnswer || reply.Text != "This request was stopped before it finished. Already done: Saved a memory." {
		t.Errorf("reply = %+v", reply)
	}
}

// A tool already running finishes even if the turn is cancelled under it, as
// Swift's cooperative cancellation lets it: a write must not commit half-way
// or commit with no chip. The loop then stops before the next call and round.
func TestATurnCancelledDuringAToolLetsThatToolFinish(t *testing.T) {
	tr := newTransport(paidDecision(1000), toolsStep(
		call("c1", "app_reminder_create", reminderArguments),
		call("c2", "memory_save", `{"content":"x"}`),
	), textStep("never"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := &recordingRunner{
		results: map[string]ToolResult{"app_reminder_create": {ModelContent: "set", Action: doneAction("Reminder set")}},
		during:  cancel,
	}
	log := &eventLog{}

	reply := mustReply(t, newHarness(tr).answer(ctx, runner, log))

	if got := runner.names(); !reflect.DeepEqual(got, []string{"app_reminder_create"}) {
		t.Errorf("runner ran %v", got)
	}
	if !reflect.DeepEqual(runner.ctxDone, []bool{false}) {
		t.Errorf("the running tool's ctx was cancelled under it: %v", runner.ctxDone)
	}
	if n := len(tr.sent()); n != 1 {
		t.Errorf("%d rounds, want no round after the cancel", n)
	}
	if len(log.actions()) != 1 {
		t.Errorf("actions = %v", log.actions())
	}
	if reply.Text != "This request was stopped before it finished. Already done: Reminder set." || !reply.ShouldPersist || reply.IsAnswer {
		t.Errorf("reply = %+v", reply)
	}
}

func TestStoppedDuringALaterRoundIsStoppedNotFailed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := newTransport(paidDecision(1000), toolsStep(call("c1", "memory_save", "{}")), failStep())
	h := newHarness(tr)
	// The transport fails because the turn was cancelled during round 2.
	h.service.Transport = cancelOnRound{scriptedTransport: tr, round: 2, cancel: cancel}
	runner := &recordingRunner{results: map[string]ToolResult{
		"memory_save": {ModelContent: "saved", Action: doneAction("Saved a memory")},
	}}

	reply := mustReply(t, h.answer(ctx, runner, nil))

	if reply.Text != "This request was stopped before it finished. Already done: Saved a memory." {
		t.Errorf("text = %q", reply.Text)
	}
	if got := h.logs.outcomes(); !reflect.DeepEqual(got, []string{"stopped"}) {
		t.Errorf("outcomes = %v", got)
	}
}

type cancelOnRound struct {
	*scriptedTransport
	round  int
	cancel context.CancelFunc
}

func (c cancelOnRound) ChatCompletionsDecided(ctx context.Context, payload []byte, sessionKey, sessionID string, decision llm.Decision) (llm.Metadata, error) {
	if len(c.sent())+1 == c.round {
		c.cancel()
	}
	return c.scriptedTransport.ChatCompletionsDecided(ctx, payload, sessionKey, sessionID, decision)
}

func TestRefusedToolIsNotRun(t *testing.T) {
	tr := newTransport(paidDecision(1000), toolsStep(call("c1", "memory_update", `{"id":"x"}`)), textStep("I can't change that."))
	runner := &recordingRunner{}

	reply := mustReply(t, newHarness(tr).answer(context.Background(), runner, nil))

	if reply.Text != "I can't change that." || reply.ToolCallCount != 0 {
		t.Errorf("reply = %+v", reply)
	}
	if len(runner.names()) != 0 {
		t.Errorf("runner ran %v", runner.names())
	}
	tool := messagesIn(t, tr.sent()[1])
	last := tool[len(tool)-1]
	if last["role"] != "tool" || last["tool_call_id"] != "c1" || last["name"] != "memory_update" || last["content"] != "That tool is not available. Do not try it again." {
		t.Errorf("tool message = %v", last)
	}
}

func TestToolTheModelInventedIsNotRun(t *testing.T) {
	tr := newTransport(paidDecision(1000), toolsStep(call("c1", "delete_everything", "{}")), textStep("no"))
	runner := &recordingRunner{}

	newHarness(tr).answer(context.Background(), runner, nil)

	if len(runner.names()) != 0 {
		t.Errorf("runner ran %v", runner.names())
	}
}

func TestMalformedArgumentsAreToldToTheModelAndNotRun(t *testing.T) {
	for _, args := range []string{"{not json", "[1]", `"text"`, "null", "42", `{"a":1} trailing`} {
		tr := newTransport(paidDecision(1000), toolsStep(call("c1", "search", args)), textStep("sorry"))
		runner := &recordingRunner{}

		newHarness(tr).answer(context.Background(), runner, nil)

		if len(runner.names()) != 0 {
			t.Errorf("%s: runner ran %v", args, runner.names())
		}
		tool := toolMessages(t, tr.sent()[1])
		if len(tool) != 1 || tool[0]["tool_call_id"] != "c1" || tool[0]["content"] != "The tool did not run: its arguments were not valid JSON." {
			t.Errorf("%s: tool messages = %v", args, tool)
		}
	}
}

func TestEmptyArgumentsMeanNoArguments(t *testing.T) {
	for _, args := range []string{"", "  \n"} {
		tr := newTransport(paidDecision(1000), toolsStep(call("c1", "recent", args)), textStep("ok"))
		runner := &recordingRunner{}

		newHarness(tr).answer(context.Background(), runner, nil)

		if got := runner.names(); !reflect.DeepEqual(got, []string{"recent"}) {
			t.Fatalf("runner ran %v", got)
		}
		if a := runner.calls[0].args; a == nil || len(a) != 0 {
			t.Errorf("args = %#v, want an empty map", a)
		}
	}
}

func TestToolCallsPastTheCapGetARefusalAndDoNotRun(t *testing.T) {
	var calls []scriptedCall
	for i := 1; i <= 9; i++ {
		calls = append(calls, call(fmt.Sprintf("c%d", i), "search", "{}"))
	}
	tr := newTransport(paidDecision(1000), toolsStep(calls...), textStep("ok"))
	runner := &recordingRunner{}

	reply := mustReply(t, newHarness(tr).answer(context.Background(), runner, nil))

	if n := len(runner.names()); n != 8 || reply.ToolCallCount != 8 {
		t.Errorf("runner ran %d, count %d; want 8", n, reply.ToolCallCount)
	}
	tool := toolMessages(t, tr.sent()[1])
	if len(tool) != 9 {
		t.Fatalf("%d tool messages, want 9", len(tool))
	}
	for i, m := range tool {
		if m["tool_call_id"] != calls[i].id {
			t.Errorf("tool message %d answers %v", i, m["tool_call_id"])
		}
	}
	if tool[8]["content"] != "The tool did not run: too many tools were called in one step." {
		t.Errorf("9th content = %v", tool[8]["content"])
	}
	if tool[7]["content"] != "result of search" {
		t.Errorf("8th content = %v", tool[7]["content"])
	}
}

func TestReplyIsChunkedByWordAndLosesNothing(t *testing.T) {
	if got := WordChunks("a  b\nc "); !reflect.DeepEqual(got, []string{"a  ", "b\n", "c "}) {
		t.Errorf("WordChunks = %q", got)
	}
	text := "One  two\nthree   four "
	chunks := WordChunks(text)
	if strings.Join(chunks, "") != text || len(chunks) != 4 || chunks[0] != "One  " {
		t.Errorf("WordChunks(%q) = %q", text, chunks)
	}
	if got := WordChunks("  lead"); !reflect.DeepEqual(got, []string{"  ", "lead"}) {
		t.Errorf("leading space = %q", got)
	}
	if got := WordChunks("x\r\ny\u00a0z"); !reflect.DeepEqual(got, []string{"x\r\n", "y\u00a0", "z"}) {
		t.Errorf("CRLF and no-break space = %q", got)
	}
	if got := WordChunks(""); len(got) != 0 {
		t.Errorf("empty = %q", got)
	}
}

// MARK: - Settling the reservation

func TestOnlyRoundOneCarriesTheReservation(t *testing.T) {
	picked := paidDecision(1000)
	tr := newTransport(picked, toolsStep(call("c1", "search", "{}")), toolsStep(call("c2", "search", "{}")), textStep("done"))
	h := newHarness(tr)

	mustReply(t, h.answer(context.Background(), &recordingRunner{}, nil))

	seen := tr.seen()
	var reservations []int64
	for _, d := range seen {
		reservations = append(reservations, d.Cerberus.BudgetReservationUsdMicros)
		if !reflect.DeepEqual(d.Candidates(), picked.Candidates()) || d.CredentialMode != picked.CredentialMode {
			t.Errorf("decision routes changed: %+v", d)
		}
	}
	if !reflect.DeepEqual(reservations, []int64{1000, 0, 0}) {
		t.Errorf("reservations = %v", reservations)
	}
	if picked.Cerberus.BudgetReservationUsdMicros != 1000 {
		t.Error("stripping the reservation changed the picked decision")
	}
	if len(h.releases.all()) != 0 {
		t.Errorf("released %v", h.releases.all())
	}
}

func TestRoundOneFailureDoesNotReleaseAgain(t *testing.T) {
	h := newHarness(newTransport(paidDecision(1000), failStep()))

	h.answer(context.Background(), &recordingRunner{}, nil)

	if len(h.releases.all()) != 0 {
		t.Errorf("released %v; the transport already settled it", h.releases.all())
	}
}

func TestTurnCancelledBeforeRoundOneReleasesOnce(t *testing.T) {
	tr := newTransport(paidDecision(700), textStep("never"))
	h := newHarness(tr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := h.answer(ctx, &recordingRunner{}, nil)

	if out.Reply != nil || out.PreDecided != nil {
		t.Errorf("outcome = %+v", out)
	}
	if n := len(tr.sent()); n != 0 {
		t.Errorf("%d rounds reached the transport", n)
	}
	got := h.releases.all()
	if len(got) != 1 || got[0].micros != 700 || got[0].tenant != testTenant {
		t.Fatalf("releases = %+v", got)
	}
	if got[0].ctxDone {
		t.Error("the release ran on a cancelled context and could not reach the database")
	}
	if outcomes := h.logs.outcomes(); !reflect.DeepEqual(outcomes, []string{"fallback"}) {
		t.Errorf("outcomes = %v", outcomes)
	}
}

func TestReleaseOfZeroIsANoOp(t *testing.T) {
	for _, picked := range []llm.Decision{
		gatewayOnlyDecision(0),
		{Primary: llm.Route{Provider: llm.HermesGateway, ModelID: "hermes-3"}},
	} {
		h := newHarness(newTransport(picked, textStep("never")))
		h.answer(context.Background(), &recordingRunner{}, nil)
		if len(h.releases.all()) != 0 {
			t.Errorf("released %v", h.releases.all())
		}
	}
	// A nil Release is allowed.
	h := newHarness(newTransport(gatewayOnlyDecision(500), textStep("never")))
	h.service.Release = nil
	h.answer(context.Background(), &recordingRunner{}, nil)
}

// MARK: - The whole turn

func assertNoToolRoute(t *testing.T, picked llm.Decision, micros int64) {
	t.Helper()
	tr := newTransport(picked, textStep("never"))
	h := newHarness(tr)

	out := h.answer(context.Background(), &recordingRunner{}, nil)

	if out.Reply != nil || out.PreDecided != nil {
		t.Errorf("outcome = %+v; a paid turn lets the classic path pick its own route", out)
	}
	if n := len(tr.sent()); n != 0 {
		t.Errorf("%d rounds reached the transport", n)
	}
	got := h.releases.all()
	if len(got) != 1 || got[0].micros != micros || got[0].tenant != testTenant {
		t.Errorf("releases = %+v", got)
	}
	if outcomes := h.logs.outcomes(); !reflect.DeepEqual(outcomes, []string{"no_tool_route"}) {
		t.Errorf("outcomes = %v", outcomes)
	}
}

func TestGatewayOnlyPaidDecisionReleasesOnceAndGoesClassic(t *testing.T) {
	assertNoToolRoute(t, gatewayOnlyDecision(900), 900)
}

func TestOllamaOnlyPaidDecisionReleasesOnceAndGoesClassic(t *testing.T) {
	assertNoToolRoute(t, llm.Decision{
		Primary:        llm.Route{Provider: llm.Ollama, ModelID: "llama"},
		Fallbacks:      []llm.Route{{Provider: llm.HermesGateway, ModelID: "hermes-3"}},
		Cerberus:       metadata(900, false),
		CredentialMode: llm.ModeBYOK,
	}, 900)
}

func TestGeminiOnlyPaidDecisionReleasesOnceAndGoesClassic(t *testing.T) {
	assertNoToolRoute(t, llm.Decision{
		Primary:        llm.Route{Provider: llm.Gemini, ModelID: "gemini-2.5-flash"},
		Fallbacks:      []llm.Route{{Provider: llm.Ollama, ModelID: "llama"}, {Provider: llm.HermesGateway, ModelID: "hermes-3"}},
		Cerberus:       metadata(900, false),
		CredentialMode: llm.ModeManaged,
	}, 900)
}

func TestAMixedDecisionLoopsOnItsOtherRoutes(t *testing.T) {
	anthropic := llm.Route{Provider: llm.Anthropic, ModelID: "a"}
	openAI := llm.Route{Provider: llm.OpenAI, ModelID: "b"}
	tr := newTransport(llm.Decision{
		Primary:        llm.Route{Provider: llm.Ollama, ModelID: "llama"},
		Fallbacks:      []llm.Route{anthropic, {Provider: llm.Gemini, ModelID: "g"}, openAI, {Provider: llm.HermesGateway, ModelID: "h"}},
		CredentialMode: llm.ModeBYOK,
	}, textStep("From the rest."))

	reply := mustReply(t, newHarness(tr).answer(context.Background(), &recordingRunner{}, nil))

	if reply.Text != "From the rest." {
		t.Errorf("text = %q", reply.Text)
	}
	if got := tr.seen()[0].Candidates(); !reflect.DeepEqual(got, []llm.Route{anthropic, openAI}) {
		t.Errorf("candidates = %v", got)
	}
}

func TestFreeLaneFailureReusesThePickedDecision(t *testing.T) {
	picked := freeLaneDecision()
	tr := newTransport(picked, failStep())
	h := newHarness(tr)

	out := h.answer(context.Background(), &recordingRunner{}, nil)

	if out.Reply != nil || out.PreDecided == nil || !reflect.DeepEqual(*out.PreDecided, picked) {
		t.Errorf("outcome = %+v", out)
	}
	if tr.picks != 1 || len(h.releases.all()) != 0 {
		t.Errorf("picks %d, releases %v", tr.picks, h.releases.all())
	}

	// A paid pick that fails the same way leaves the classic path to pick.
	paid := newHarness(newTransport(paidDecision(1000), failStep()))
	if out := paid.answer(context.Background(), &recordingRunner{}, nil); out.Reply != nil || out.PreDecided != nil {
		t.Errorf("paid outcome = %+v", out)
	}
}

func TestFreeLaneDecisionWithNoToolRouteIsStillReused(t *testing.T) {
	// An exhausted or unavailable lane carries a placeholder primary, which
	// can be the gateway. Its error has to surface without a second claim.
	picked := llm.Decision{
		Primary:        llm.Route{Provider: llm.HermesGateway, ModelID: "hermes-3"},
		Cerberus:       metadata(0, true),
		CredentialMode: llm.ModeManaged,
	}
	tr := newTransport(picked, textStep("never"))
	h := newHarness(tr)

	out := h.answer(context.Background(), &recordingRunner{}, nil)

	if out.Reply != nil || out.PreDecided == nil || !reflect.DeepEqual(*out.PreDecided, picked) {
		t.Errorf("outcome = %+v", out)
	}
	if tr.picks != 1 || len(h.releases.all()) != 0 || len(tr.sent()) != 0 {
		t.Errorf("picks %d, releases %v, rounds %d", tr.picks, h.releases.all(), len(tr.sent()))
	}
}

func TestAReplyPicksOnce(t *testing.T) {
	tr := newTransport(freeLaneDecision(), toolsStep(call("c", "search", "{}")), textStep("answer"))

	out := newHarness(tr).answer(context.Background(), &recordingRunner{}, nil)

	if reply := mustReply(t, out); reply.Text != "answer" {
		t.Errorf("text = %q", reply.Text)
	}
	if out.PreDecided != nil {
		t.Errorf("a reply carries a pre-decided route: %+v", out.PreDecided)
	}
	if tr.picks != 1 {
		t.Errorf("%d picks", tr.picks)
	}
	// The pick sees the opening payload, tools offered.
	if body := decode(t, tr.pickPayload); body["tools"] == nil || len(body["messages"].([]any)) != len(testMessages) {
		t.Errorf("pick payload = %v", body)
	}
}

// MARK: - What a reply is marked as

func TestAModelAnswerIsAnAnswer(t *testing.T) {
	tr := newTransport(paidDecision(1000), toolsStep(call("c1", "search", "{}")), textStep("  The answer.\n"))

	reply := mustReply(t, newHarness(tr).answer(context.Background(), &recordingRunner{}, nil))

	if reply.Text != "The answer." || !reply.IsAnswer || !reply.Indexable() || !reply.ShouldPersist || reply.ToolCallCount != 1 || reply.RanPersonalTool {
		t.Errorf("reply = %+v", reply)
	}
}

func TestCannedLinesAreNotAnswers(t *testing.T) {
	for name, tr := range map[string]*scriptedTransport{
		"failed":  newTransport(paidDecision(1000), toolsStep(call("c1", "search", "{}")), failStep()),
		"ran out": newTransport(paidDecision(1000), toolsStep(call("c1", "search", "{}"))),
		"empty":   newTransport(paidDecision(1000), toolsStep(call("c1", "search", "{}")), textStep("  ")),
	} {
		log := &eventLog{}
		reply := mustReply(t, newHarness(tr).answer(context.Background(), &recordingRunner{}, log))
		if reply.IsAnswer || reply.Indexable() || !reply.ShouldPersist || reply.Text == "" || log.text() != reply.Text {
			t.Errorf("%s: reply = %+v, tokens %q", name, reply, log.text())
		}
	}
}

func TestAPersonalToolMakesTheReplyUnindexable(t *testing.T) {
	for _, tool := range []string{"health_query", "calendar_query", "reminders_list", "app_reminders_list", "app_reminder_create"} {
		tr := newTransport(paidDecision(1000), toolsStep(call("c1", tool, "{}")), textStep("Your data."))
		reply := mustReply(t, newHarness(tr).answer(context.Background(), &recordingRunner{}, nil))
		if !reply.RanPersonalTool || !reply.IsAnswer || reply.Indexable() {
			t.Errorf("%s: reply = %+v", tool, reply)
		}
	}
	tr := newTransport(paidDecision(1000), toolsStep(call("c1", "open_screen", `{"screen":"home"}`)), textStep("Done."))
	if reply := mustReply(t, newHarness(tr).answer(context.Background(), &recordingRunner{}, nil)); reply.RanPersonalTool || !reply.Indexable() {
		t.Errorf("open_screen: reply = %+v", reply)
	}
}

func TestARefusedPersonalToolDidNotRun(t *testing.T) {
	tr := newTransport(paidDecision(1000), toolsStep(call("c1", "reminder_create", "{}")), textStep("No."))

	reply := mustReply(t, newHarness(tr).answer(context.Background(), &recordingRunner{}, nil))

	if reply.RanPersonalTool || reply.ToolCallCount != 0 {
		t.Errorf("reply = %+v", reply)
	}
}

// MARK: - Writes run once

func TestAnIdenticalWriteInALaterRoundRunsOnce(t *testing.T) {
	tr := newTransport(paidDecision(1000),
		toolsStep(call("c1", "app_reminder_create", reminderArguments)),
		toolsStep(call("c2", "app_reminder_create", reminderArgumentsReordered)),
		textStep("Done."),
	)
	runner := writeRunner()
	log := &eventLog{}

	reply := mustReply(t, newHarness(tr).answer(context.Background(), runner, log))

	if got := runner.names(); !reflect.DeepEqual(got, []string{"app_reminder_create"}) {
		t.Errorf("runner ran %v", got)
	}
	if len(log.actions()) != 1 || reply.ToolCallCount != 1 {
		t.Errorf("actions %v, count %d", log.actions(), reply.ToolCallCount)
	}
	third := messagesIn(t, tr.sent()[2])
	last := third[len(third)-1]
	if last["content"] != "FIRST-RESULT" || last["tool_call_id"] != "c2" {
		t.Errorf("replayed tool message = %v", last)
	}
}

func TestKeyOrderDoesNotMakeAWriteDifferent(t *testing.T) {
	// Many keys, nested too, written in opposite orders.
	var forwardKeys, backwardKeys []string
	for i := range 24 {
		forwardKeys = append(forwardKeys, fmt.Sprintf(`"k%d":{"b":1,"a":[2,{"y":1,"x":2}]}`, i))
		backwardKeys = append([]string{fmt.Sprintf(`"k%d":{"a":[2,{"x":2,"y":1}],"b":1}`, i)}, backwardKeys...)
	}
	forward := "{" + strings.Join(forwardKeys, ",") + "}"
	backward := "{" + strings.Join(backwardKeys, ",") + "}"
	tr := newTransport(paidDecision(1000),
		toolsStep(call("c1", "app_reminder_create", forward)),
		toolsStep(call("c2", "app_reminder_create", backward)),
		textStep("Done."),
	)
	runner := writeRunner()

	newHarness(tr).answer(context.Background(), runner, nil)

	if got := runner.names(); !reflect.DeepEqual(got, []string{"app_reminder_create"}) {
		t.Errorf("runner ran %v", got)
	}
}

func TestAnIdenticalWriteWithinOneRoundRunsOnce(t *testing.T) {
	tr := newTransport(paidDecision(1000), toolsStep(
		call("c1", "app_reminder_create", reminderArguments),
		call("c2", "app_reminder_create", reminderArguments),
	), textStep("Done."))
	runner := writeRunner()
	log := &eventLog{}

	newHarness(tr).answer(context.Background(), runner, log)

	if got := runner.names(); !reflect.DeepEqual(got, []string{"app_reminder_create"}) {
		t.Errorf("runner ran %v", got)
	}
	if len(log.actions()) != 1 {
		t.Errorf("actions = %v", log.actions())
	}
	var contents []any
	for _, m := range toolMessages(t, tr.sent()[1]) {
		contents = append(contents, m["content"])
	}
	if !reflect.DeepEqual(contents, []any{"FIRST-RESULT", "FIRST-RESULT"}) {
		t.Errorf("tool contents = %v", contents)
	}
}

func TestDifferentArgumentsAndReadsStillRunEachTime(t *testing.T) {
	tr := newTransport(paidDecision(1000), toolsStep(
		call("c1", "app_reminder_create", reminderArguments),
		call("c2", "app_reminder_create", `{"title":"Call dad","fireAt":"2030-01-01T09:00:00Z"}`),
		call("c3", "search", `{"query":"a"}`),
		call("c4", "search", `{"query":"a"}`),
		call("c5", "memory_save", `{"content":"Call mum"}`),
	), textStep("Done."))
	runner := writeRunner()

	newHarness(tr).answer(context.Background(), runner, nil)

	want := []string{"app_reminder_create", "app_reminder_create", "search", "search", "memory_save"}
	if got := runner.names(); !reflect.DeepEqual(got, want) {
		t.Errorf("runner ran %v", got)
	}
}

func TestAWriteWithoutAChipMayBeRetried(t *testing.T) {
	tr := newTransport(paidDecision(1000),
		toolsStep(call("c1", "app_reminder_create", reminderArguments)),
		toolsStep(call("c2", "app_reminder_create", reminderArguments)),
		textStep("Done."),
	)
	runner := &recordingRunner{}

	reply := mustReply(t, newHarness(tr).answer(context.Background(), runner, nil))

	if got := runner.names(); !reflect.DeepEqual(got, []string{"app_reminder_create", "app_reminder_create"}) {
		t.Errorf("runner ran %v", got)
	}
	if reply.ToolCallCount != 2 {
		t.Errorf("count = %d", reply.ToolCallCount)
	}
}

func TestANeedsConfirmCardIsNotADoneChipButStopsARepeat(t *testing.T) {
	card := &Action{ID: wire.UUID{UUID: uuid.New()}, Kind: KindNeedsConfirm, Summary: "Forget X?"}
	tr := newTransport(paidDecision(1000),
		toolsStep(call("c1", "memory_forget", `{"id":"m"}`)),
		toolsStep(call("c2", "memory_forget", `{"id":"m"}`)),
	)
	runner := &recordingRunner{results: map[string]ToolResult{"memory_forget": {ModelContent: "waiting", Action: card}}}
	log := &eventLog{}

	reply := mustReply(t, newHarness(tr).answer(context.Background(), runner, log))

	if got := runner.names(); !reflect.DeepEqual(got, []string{"memory_forget"}) {
		t.Errorf("runner ran %v", got)
	}
	if !reflect.DeepEqual(log.actions(), []Action{*card}) {
		t.Errorf("actions = %v", log.actions())
	}
	// Only a done chip is listed as done.
	if reply.Text != "I ran out of steps before I could finish." {
		t.Errorf("text = %q", reply.Text)
	}
}

// MARK: - Turn logs

func TestEachEndingLogsItsOwnOutcome(t *testing.T) {
	outcome := func(tr *scriptedTransport, cancelAfterTool bool) []string {
		h := newHarness(tr)
		ctx, runner := context.Background(), &recordingRunner{}
		if cancelAfterTool {
			ctx, runner = cancelling(nil)
		}
		h.answer(ctx, runner, nil)
		return h.logs.outcomes()
	}
	search := toolsStep(call("c", "search", "{}"))
	cases := []struct {
		want string
		got  []string
	}{
		{"answer", outcome(newTransport(paidDecision(1000), textStep("hi")), false)},
		{"fallback", outcome(newTransport(paidDecision(1000), failStep()), false)},
		{"failed", outcome(newTransport(paidDecision(1000), search, failStep()), false)},
		{"ran_out", outcome(newTransport(paidDecision(1000), search), false)},
		{"stopped", outcome(newTransport(paidDecision(1000), search, textStep("never")), true)},
		{"no_tool_route", outcome(newTransport(gatewayOnlyDecision(1000), textStep("never")), false)},
	}
	for _, c := range cases {
		if !reflect.DeepEqual(c.got, []string{c.want}) {
			t.Errorf("outcomes = %v, want [%s]", c.got, c.want)
		}
	}

	h := newHarness(newTransport(paidDecision(1000), search, search, textStep("hi")))
	h.answer(context.Background(), &recordingRunner{}, nil)
	turns := h.logs.named("quick assistant turn")
	if len(turns) != 1 || turns[0].attrs["rounds"].Int64() != 3 {
		t.Errorf("turn logs = %+v", turns)
	}
	rounds := h.logs.named("quick assistant round")
	if len(rounds) != 3 {
		t.Fatalf("%d round logs", len(rounds))
	}
	for i, r := range rounds {
		_, hasDuration := r.attrs["duration_ms"]
		wantCalls := int64(1)
		if i == 2 {
			wantCalls = 0
		}
		if r.attrs["round"].Int64() != int64(i+1) || r.attrs["tool_calls"].Int64() != wantCalls || !hasDuration {
			t.Errorf("round log %d = %+v", i+1, r.attrs)
		}
	}
}

func TestARoundErrorLogsItsTypeOnly(t *testing.T) {
	tr := newTransport(paidDecision(1000), step{fail: &leakyError{"SECRET PROMPT TEXT"}})
	h := newHarness(tr)

	h.answer(context.Background(), &recordingRunner{}, nil)

	failed := h.logs.named("quick assistant round failed")
	if len(failed) != 1 || failed[0].level != slog.LevelWarn {
		t.Fatalf("failure logs = %+v", failed)
	}
	if got := failed[0].attrs["error_type"].String(); got != "*quickassistant.leakyError" {
		t.Errorf("error_type = %q", got)
	}
	h.logs.mu.Lock()
	defer h.logs.mu.Unlock()
	for _, r := range h.logs.records {
		for _, v := range r.attrs {
			if strings.Contains(v.String(), "SECRET") {
				t.Errorf("%q logs the error text", r.message)
			}
		}
	}
}

type leakyError struct{ text string }

func (e *leakyError) Error() string { return e.text }

func TestTheTurnTimerIncludesThePick(t *testing.T) {
	tr := newTransport(gatewayOnlyDecision(1000), textStep("never"))
	tr.pickDelay = 30 * time.Millisecond
	h := newHarness(tr)

	h.answer(context.Background(), &recordingRunner{}, nil)

	turns := h.logs.named("quick assistant turn")
	if len(turns) != 1 || turns[0].attrs["duration_ms"].Int64() < 30 {
		t.Errorf("turn logs = %+v", turns)
	}
}

func TestActionFramesAreEmittedAsToolsReturnThem(t *testing.T) {
	saved := doneAction("Saved a memory")
	tr := newTransport(paidDecision(1000), toolsStep(call("c1", "memory_save", `{"content":"x"}`)), textStep("Saved it."))
	runner := &recordingRunner{results: map[string]ToolResult{"memory_save": {ModelContent: "saved", Action: saved}}}
	log := &eventLog{}

	mustReply(t, newHarness(tr).answer(context.Background(), runner, log))

	events := log.all()
	if len(events) != 3 {
		t.Fatalf("events = %+v", events)
	}
	if !reflect.DeepEqual(events[0], Frame(*saved)) {
		t.Errorf("first event = %+v", events[0])
	}
	for _, e := range events[1:] {
		if e.Type != "token" {
			t.Errorf("event after the frame = %+v", e)
		}
	}
	if log.text() != "Saved it." {
		t.Errorf("tokens = %q", log.text())
	}
}
