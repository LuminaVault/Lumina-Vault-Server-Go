package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// stubTransport answers each ChatCompletions from a script, recording the routing
// values every call was made with.
type stubTransport struct {
	mu      sync.Mutex
	replies []func(llm.Values) (llm.Metadata, error)
	calls   []llm.Values
}

func (s *stubTransport) ChatCompletions(ctx context.Context, _ []byte, _, _ string) (llm.Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := llm.RoutingValues(ctx)
	s.calls = append(s.calls, values)
	if len(s.replies) == 0 {
		return llm.Metadata{}, errors.New("script exhausted")
	}
	next := s.replies[0]
	s.replies = s.replies[1:]
	return next(values)
}

func completion(body string) func(llm.Values) (llm.Metadata, error) {
	return func(llm.Values) (llm.Metadata, error) { return llm.Metadata{Data: []byte(body)}, nil }
}

func failing(llm.Values) (llm.Metadata, error) { return llm.Metadata{}, errors.New("upstream 503") }

type stubSkills struct {
	run func(ctx context.Context, tenant uuid.UUID, name, input string) (string, *string, bool, error)
}

func (s stubSkills) RunWorkflowSkill(ctx context.Context, tenant uuid.UUID, _, _, name, input string) (string, *string, bool, error) {
	return s.run(ctx, tenant, name, input)
}

type pushRecord struct{ title, body, state string }

type stubPush struct {
	mu   sync.Mutex
	sent []pushRecord
}

func (p *stubPush) NotifyWorkflow(_ context.Context, _, _, _ uuid.UUID, title, body, state string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, pushRecord{title, body, state})
	return nil
}

func (p *stubPush) states() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, s := range p.sent {
		out = append(out, s.state)
	}
	return out
}

func engineEnv(t *testing.T) env {
	t.Helper()
	v := setup(t, func(o *Options) { o.ManagedInferenceAvailable = true })
	// Seeded runs would be claimed too: park them.
	_, err := v.pool.Exec(context.Background(),
		`UPDATE workflow_runs SET status = 'cancelled' WHERE status IN ('queued', 'running', 'waitingForApproval')`)
	require.NoError(t, err)
	return v
}

// runGraph publishes def for wfAnn, enqueues one manual run with input and drains
// the engine once.
func runGraph(t *testing.T, v env, e *Engine, def WorkflowDefinitionDTO, input StringMap) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	created, err := v.m.createWorkflow(ctx, wfAnn, "Engine "+uuid.NewString()[:8], nil, def)
	require.NoError(t, err)
	_, err = v.m.publishWorkflow(ctx, wfAnn, created.Workflow.Id.UUID)
	require.NoError(t, err)
	run, err := v.m.enqueue(ctx, wfAnn, created.Workflow.Id.UUID, Manual, input, nil, nil)
	require.NoError(t, err)
	e.Drain(ctx)
	return run.Id.UUID
}

type runState struct {
	status                    string
	errorMessage, pauseReason *string
	leaseOwner                *string
	spent                     int64
}

func readRun(t *testing.T, v env, id uuid.UUID) runState {
	t.Helper()
	var s runState
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT status, error_message, pause_reason, lease_owner,
		managed_spend_usd_micros FROM workflow_runs WHERE id = $1`, id).
		Scan(&s.status, &s.errorMessage, &s.pauseReason, &s.leaseOwner, &s.spent))
	return s
}

func nodeOutput(t *testing.T, v env, run, node uuid.UUID) (string, StringMap) {
	t.Helper()
	var status string
	var raw []byte
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT status, output_snapshot FROM workflow_node_runs
		WHERE run_id = $1 AND node_id = $2 ORDER BY created_at DESC LIMIT 1`, run, node).Scan(&status, &raw))
	out := StringMap{}
	if raw != nil {
		require.NoError(t, json.Unmarshal(raw, &out))
	}
	return status, out
}

func eventKinds(t *testing.T, v env, run uuid.UUID) []string {
	t.Helper()
	rows, err := v.pool.Query(context.Background(), `SELECT kind FROM workflow_run_events WHERE run_id = $1 ORDER BY sequence`, run)
	require.NoError(t, err)
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		kinds = append(kinds, k)
	}
	return kinds
}

func chain(nodes ...WorkflowNodeDTO) []WorkflowEdgeDTO {
	var edges []WorkflowEdgeDTO
	for i := 1; i < len(nodes); i++ {
		edges = append(edges, testEdge(nodes[i-1], nodes[i]))
	}
	return edges
}

// A template feeds an output through {{nodes.<ID>.text}}; the run succeeds with a
// node run per node, the run input interpolated, and the lifecycle events and push.
func TestEngineRunsTemplateGraphToSuccess(t *testing.T) {
	v := engineEnv(t)
	push := &stubPush{}
	e := v.m.NewEngine(EngineDeps{Push: push})
	trigger := testNode(Trigger, nil)
	tmpl := testNode(Template, StringMap{"template": "Hello {{name}}"})
	output := testNode(Output, StringMap{"value": "[{{nodes." + ids.Upper(tmpl.Id.UUID) + ".text}}]"})
	run := runGraph(t, v, e, testDefinition(Manual, []WorkflowNodeDTO{trigger, tmpl, output}, chain(trigger, tmpl, output)),
		StringMap{"name": "Ann"})

	s := readRun(t, v, run)
	require.Equal(t, "succeeded", s.status)
	require.Nil(t, s.leaseOwner)
	status, out := nodeOutput(t, v, run, output.Id.UUID)
	require.Equal(t, "succeeded", status)
	require.Equal(t, "[Hello Ann]", out["text"])
	require.Equal(t, []string{
		"runQueued", "runStarted", "nodeStarted", "nodeOutput", "nodeCompleted",
		"nodeStarted", "nodeOutput", "nodeCompleted", "runCompleted",
	}, eventKinds(t, v, run))
	require.Equal(t, []string{"succeeded"}, push.states())
}

// A condition activates only the edge whose port matches its result.
func TestEngineConditionTakesOneBranch(t *testing.T) {
	v := engineEnv(t)
	e := v.m.NewEngine(EngineDeps{})
	trigger := testNode(Trigger, nil)
	cond := testNode(Condition, StringMap{"left": "{{answer}}", "right": "yes"})
	yes := testNode(Output, StringMap{"value": "took yes"})
	no := testNode(Output, StringMap{"value": "took no"})
	edges := []WorkflowEdgeDTO{testEdge(trigger, cond), testEdge(cond, yes), testEdge(cond, no)}
	edges[1].SourcePort, edges[2].SourcePort = "true", "false"
	run := runGraph(t, v, e, testDefinition(Manual, []WorkflowNodeDTO{trigger, cond, yes, no}, edges), StringMap{"answer": "yes"})

	require.Equal(t, "succeeded", readRun(t, v, run).status)
	_, out := nodeOutput(t, v, run, cond.Id.UUID)
	require.Equal(t, StringMap{"result": "true", "text": "true"}, out)
	status, _ := nodeOutput(t, v, run, yes.Id.UUID)
	require.Equal(t, "succeeded", status)
	var noRuns int
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM workflow_node_runs WHERE run_id = $1 AND node_id = $2`, run, no.Id.UUID).Scan(&noRuns))
	require.Zero(t, noRuns)
}

// An approval node parks the run with a pending approval (24 h) and a waiting node
// run; once the node run succeeds and the run is queued again, the engine skips
// the finished nodes and completes.
func TestEnginePausesForApprovalAndResumes(t *testing.T) {
	v := engineEnv(t)
	ctx := context.Background()
	push := &stubPush{}
	e := v.m.NewEngine(EngineDeps{Push: push})
	trigger := testNode(Trigger, nil)
	tmpl := testNode(Template, StringMap{"value": "draft"})
	gate := testNode(Approval, StringMap{"message": "Ship it?"})
	output := testNode(Output, StringMap{"value": "{{nodes." + ids.Upper(tmpl.Id.UUID) + ".text}} shipped"})
	run := runGraph(t, v, e, testDefinition(Manual, []WorkflowNodeDTO{trigger, tmpl, gate, output}, chain(trigger, tmpl, gate, output)), nil)

	require.Equal(t, "waitingForApproval", readRun(t, v, run).status)
	var title string
	var message *string
	var expires time.Time
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT title, message, expires_at FROM workflow_approvals
		WHERE run_id = $1 AND node_id = $2 AND status = 'pending'`, run, gate.Id.UUID).Scan(&title, &message, &expires))
	require.Equal(t, gate.Name, title)
	require.Equal(t, "Ship it?", *message)
	require.WithinDuration(t, time.Now().Add(24*time.Hour), expires, time.Minute)
	status, _ := nodeOutput(t, v, run, gate.Id.UUID)
	require.Equal(t, "waitingForApproval", status)
	require.Equal(t, []string{"waiting_for_approval"}, push.states())

	_, err := v.pool.Exec(ctx, `UPDATE workflow_node_runs SET status = 'succeeded', output_snapshot = '{}' WHERE run_id = $1 AND node_id = $2`, run, gate.Id.UUID)
	require.NoError(t, err)
	_, err = v.pool.Exec(ctx, `UPDATE workflow_runs SET status = 'queued' WHERE id = $1`, run)
	require.NoError(t, err)
	e.Drain(ctx)

	require.Equal(t, "succeeded", readRun(t, v, run).status)
	_, out := nodeOutput(t, v, run, output.Id.UUID)
	require.Equal(t, "draft shipped", out["text"])
	var tmplRuns int
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_node_runs WHERE run_id = $1 AND node_id = $2`,
		run, tmpl.Id.UUID).Scan(&tmplRuns))
	require.Equal(t, 1, tmplRuns, "a succeeded node isn't run again")
}

// A managed llm node reserves the per-run allowance, calls the routed transport in
// workflow scope, records model, tokens and cost, and charges the cost.
func TestEngineLLMNodeChargesManagedSpend(t *testing.T) {
	v := engineEnv(t)
	tr := &stubTransport{replies: []func(llm.Values) (llm.Metadata, error){func(values llm.Values) (llm.Metadata, error) {
		values.RouteOutcomeSink(llm.ModelProvenance{Provider: "openrouter", Model: "routed/model"})
		return llm.Metadata{Data: []byte(`{"model":"vendor/model","choices":[{"message":{"content":"Answer"}}],
			"usage":{"prompt_tokens":12,"completion_tokens":34,"cost":0.012345}}`)}, nil
	}}}
	e := v.m.NewEngine(EngineDeps{Transport: tr, DefaultModel: "hermes-3"})
	trigger := testNode(Trigger, nil)
	call := testNode(Llm, StringMap{"prompt": "Say {{word}}"})
	run := runGraph(t, v, e, testDefinition(Manual, []WorkflowNodeDTO{trigger, call}, chain(trigger, call)), StringMap{"word": "hi"})

	s := readRun(t, v, run)
	require.Equal(t, "succeeded", s.status)
	require.EqualValues(t, 12345, s.spent)
	_, out := nodeOutput(t, v, run, call.Id.UUID)
	// The snapshot redacts the token counts, as Swift's does ("token" is sensitive);
	// the columns keep them.
	require.Equal(t, StringMap{
		"text": "Answer", "provider": "openrouter", "model": "vendor/model",
		"tokensIn": "[REDACTED]", "tokensOut": "[REDACTED]", "managedCostUsdMicros": "12345",
	}, out)
	var model, provider string
	var tokensIn, cost int64
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT selected_model, selected_provider, tokens_in, managed_cost_usd_micros
		FROM workflow_node_runs WHERE run_id = $1 AND node_id = $2`, run, call.Id.UUID).Scan(&model, &provider, &tokensIn, &cost))
	require.Equal(t, "vendor/model", model)
	require.EqualValues(t, 12, tokensIn)
	require.EqualValues(t, 12345, cost)

	require.Len(t, tr.calls, 1)
	scope := tr.calls[0].CerberusScope
	require.Equal(t, llm.SurfaceWorkflow, scope.Surface)
	require.Equal(t, llm.ModeManaged, tr.calls[0].CredentialMode)
	require.Nil(t, tr.calls[0].ForcedRoute)
	require.Equal(t, wfAnn, tr.calls[0].CurrentUser.ID)

	var reserved, spent int64
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT reserved_usd_micros, spent_usd_micros FROM workflow_spend_buckets
		WHERE scope_key = $1 AND period_kind = 'day'`, "tenant:"+ids.Upper(wfAnn)).Scan(&reserved, &spent))
	require.Zero(t, reserved, "the reservation is released")
	require.EqualValues(t, 12345, spent)
}

// A managed call that fails retries once on the free lane (forced route, no
// charge, freeFallbackReason); a second failure pauses the run.
func TestEngineLLMFallsBackToFreeLaneThenPauses(t *testing.T) {
	v := engineEnv(t)
	tr := &stubTransport{replies: []func(llm.Values) (llm.Metadata, error){
		failing,
		completion(`{"choices":[{"message":{"content":"free answer"}}],"usage":{"prompt_tokens":"3","completion_tokens":4.9}}`),
	}}
	e := v.m.NewEngine(EngineDeps{Transport: tr, DefaultModel: "hermes-3"})
	trigger := testNode(Trigger, nil)
	call := testNode(Llm, StringMap{"prompt": "x", "model": "auto"})
	run := runGraph(t, v, e, testDefinition(Manual, []WorkflowNodeDTO{trigger, call}, chain(trigger, call)), nil)

	s := readRun(t, v, run)
	require.Equal(t, "succeeded", s.status)
	require.Zero(t, s.spent)
	_, out := nodeOutput(t, v, run, call.Id.UUID)
	require.Equal(t, StringMap{
		"text": "free answer", "provider": "openRouter", "model": "hermes-3",
		"tokensIn": "[REDACTED]", "tokensOut": "[REDACTED]", "managedCostUsdMicros": "0", "freeFallbackReason": "providerUnavailable",
	}, out)
	var tokensIn, tokensOut int64
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT tokens_in, tokens_out FROM workflow_node_runs
		WHERE run_id = $1 AND node_id = $2`, run, call.Id.UUID).Scan(&tokensIn, &tokensOut))
	require.EqualValues(t, 3, tokensIn)
	require.EqualValues(t, 4, tokensOut)
	require.Len(t, tr.calls, 2)
	require.Equal(t, &llm.WireRoute{Provider: llm.ProviderID(llm.OpenRouter), Model: llm.FreeLaneOpenRouterModel}, tr.calls[1].ForcedRoute)

	tr.replies = []func(llm.Values) (llm.Metadata, error){failing, failing}
	push := &stubPush{}
	e = v.m.NewEngine(EngineDeps{Transport: tr, DefaultModel: "hermes-3", Push: push})
	run = runGraph(t, v, e, testDefinition(Manual, []WorkflowNodeDTO{trigger, call}, chain(trigger, call)), nil)
	s = readRun(t, v, run)
	require.Equal(t, "paused", s.status)
	require.Equal(t, "providerUnavailable", *s.pauseReason)
	require.Equal(t, "Managed inference paused: providerUnavailable", *s.errorMessage)
	require.Equal(t, []string{"paused"}, push.states())
	require.Contains(t, eventKinds(t, v, run), "runPaused")
}

// A tier with no per-run allowance can't reserve: the run pauses on runSpendLimit
// before any call; a failed reservation never reaches the transport.
func TestEngineLLMPausesWhenSpendDenied(t *testing.T) {
	v := engineEnv(t)
	tr := &stubTransport{}
	e := v.m.NewEngine(EngineDeps{Transport: tr})
	trigger := testNode(Trigger, nil)
	call := testNode(Llm, StringMap{"prompt": "x"})
	ctx := context.Background()
	created, err := v.m.createWorkflow(ctx, wfAnn, "Denied", nil, testDefinition(Manual, []WorkflowNodeDTO{trigger, call}, chain(trigger, call)))
	require.NoError(t, err)
	_, err = v.m.publishWorkflow(ctx, wfAnn, created.Workflow.Id.UUID)
	require.NoError(t, err)
	r, err := v.m.enqueue(ctx, wfAnn, created.Workflow.Id.UUID, Manual, StringMap{}, nil, nil)
	require.NoError(t, err)
	// The per-run allowance is already used up.
	_, err = v.pool.Exec(ctx, `UPDATE workflow_runs SET managed_spend_usd_micros = 200000 WHERE id = $1`, r.Id.UUID)
	require.NoError(t, err)
	e.Drain(ctx)

	s := readRun(t, v, r.Id.UUID)
	require.Equal(t, "paused", s.status)
	require.Equal(t, "runSpendLimit", *s.pauseReason)
	require.Empty(t, tr.calls)
}

// The skill node runs the named skill with the interpolated input; an unknown
// skill fails the run with Swift's executorUnavailable("skill").
func TestEngineSkillNode(t *testing.T) {
	v := engineEnv(t)
	model := "skill/model"
	var gotInput string
	e := v.m.NewEngine(EngineDeps{Skills: stubSkills{run: func(_ context.Context, tenant uuid.UUID, name, input string) (string, *string, bool, error) {
		if name != "brief" {
			return "", nil, false, nil
		}
		gotInput = input
		return "# Brief", &model, true, nil
	}}})
	trigger := testNode(Trigger, nil)
	skill := testNode(Skill, StringMap{"skillName": "brief", "input": "topic={{topic}}"})
	run := runGraph(t, v, e, testDefinition(Manual, []WorkflowNodeDTO{trigger, skill}, chain(trigger, skill)), StringMap{"topic": "go"})
	require.Equal(t, "succeeded", readRun(t, v, run).status)
	require.Equal(t, "topic=go", gotInput)
	_, out := nodeOutput(t, v, run, skill.Id.UUID)
	require.Equal(t, StringMap{"text": "# Brief", "model": "skill/model"}, out)

	missing := testNode(Skill, StringMap{"skillName": "nope"})
	push := &stubPush{}
	e.d.Push = push
	run = runGraph(t, v, e, testDefinition(Manual, []WorkflowNodeDTO{trigger, missing}, chain(trigger, missing)), nil)
	s := readRun(t, v, run)
	require.Equal(t, "failed", s.status)
	require.Equal(t, `executorUnavailable("skill")`, *s.errorMessage)
	status, _ := nodeOutput(t, v, run, missing.Id.UUID)
	require.Equal(t, "failed", status)
	require.Equal(t, []string{"failed"}, push.states())
}

// forEach maps a template over JSON or comma/newline items; whileLoop repeats
// while its condition holds; both stop at maxIterations.
func TestEngineLoops(t *testing.T) {
	each, err := executeForEach(testNode(ForEach, StringMap{"items": "{{list}}", "template": "{{index}}:{{item}}", "separator": "|"}),
		StringMap{"list": `["a","b"]`})
	require.NoError(t, err)
	require.Equal(t, StringMap{"text": "0:a|1:b", "count": "2"}, each)
	each, err = executeForEach(testNode(ForEach, StringMap{"items": " x, y \n z ,,"}), StringMap{})
	require.NoError(t, err)
	require.Equal(t, StringMap{"text": "x\ny\nz", "count": "3"}, each)
	_, err = executeForEach(testNode(ForEach, StringMap{"items": "a,b,c", "maxIterations": "2"}), StringMap{})
	require.EqualError(t, err, "iterationLimitExceeded(2)")

	w, err := executeWhile(testNode(WhileLoop, StringMap{"condition": "{{iteration}}", "equals": "0", "template": "step{{iteration}}"}), StringMap{})
	require.NoError(t, err)
	require.Equal(t, StringMap{"text": "step0", "count": "1"}, w)
	_, err = executeWhile(testNode(WhileLoop, StringMap{"condition": "true", "maxIterations": "500"}), StringMap{})
	require.EqualError(t, err, "iterationLimitExceeded(20)")
}

// A run cancelled while a node runs keeps its cancelled status: the next heartbeat
// finds it no longer running and the worker only drops its lease.
func TestEngineLeavesCancelledRunAlone(t *testing.T) {
	v := engineEnv(t)
	ctx := context.Background()
	var runID uuid.UUID
	e := v.m.NewEngine(EngineDeps{Skills: stubSkills{run: func(ctx context.Context, _ uuid.UUID, _, _ string) (string, *string, bool, error) {
		_, err := v.pool.Exec(ctx, `UPDATE workflow_runs SET status = 'cancelled' WHERE id = $1`, runID)
		return "done", nil, true, err
	}}})
	trigger := testNode(Trigger, nil)
	skill := testNode(Skill, StringMap{"skillName": "any"})
	output := testNode(Output, StringMap{"value": "never"})
	created, err := v.m.createWorkflow(ctx, wfAnn, "Cancel", nil, testDefinition(Manual, []WorkflowNodeDTO{trigger, skill, output}, chain(trigger, skill, output)))
	require.NoError(t, err)
	_, err = v.m.publishWorkflow(ctx, wfAnn, created.Workflow.Id.UUID)
	require.NoError(t, err)
	r, err := v.m.enqueue(ctx, wfAnn, created.Workflow.Id.UUID, Manual, StringMap{}, nil, nil)
	require.NoError(t, err)
	runID = r.Id.UUID
	e.Drain(ctx)

	s := readRun(t, v, runID)
	require.Equal(t, "cancelled", s.status)
	require.Nil(t, s.leaseOwner)
	require.Nil(t, s.errorMessage)
}

// A running run whose lease lapsed (its worker died) is claimed again; a live
// lease is left alone.
func TestEngineReclaimsExpiredLease(t *testing.T) {
	v := engineEnv(t)
	ctx := context.Background()
	e := v.m.NewEngine(EngineDeps{})
	trigger, output := testNode(Trigger, nil), testNode(Output, StringMap{"value": "ok"})
	created, err := v.m.createWorkflow(ctx, wfAnn, "Lease", nil, testDefinition(Manual, []WorkflowNodeDTO{trigger, output}, chain(trigger, output)))
	require.NoError(t, err)
	_, err = v.m.publishWorkflow(ctx, wfAnn, created.Workflow.Id.UUID)
	require.NoError(t, err)
	r, err := v.m.enqueue(ctx, wfAnn, created.Workflow.Id.UUID, Manual, StringMap{}, nil, nil)
	require.NoError(t, err)

	_, err = v.pool.Exec(ctx, `UPDATE workflow_runs SET status = 'running', lease_owner = 'dead:0', lease_expires_at = NOW() + INTERVAL '1 minute' WHERE id = $1`, r.Id.UUID)
	require.NoError(t, err)
	e.Drain(ctx)
	require.Equal(t, "running", readRun(t, v, r.Id.UUID).status)

	_, err = v.pool.Exec(ctx, `UPDATE workflow_runs SET lease_expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, r.Id.UUID)
	require.NoError(t, err)
	e.Drain(ctx)
	require.Equal(t, "succeeded", readRun(t, v, r.Id.UUID).status)
}

// A published version that no longer validates (here: a cycle written straight to
// the row) fails the run with the validator's error, before any node runs.
func TestEngineFailsInvalidPublishedDefinition(t *testing.T) {
	v := engineEnv(t)
	ctx := context.Background()
	e := v.m.NewEngine(EngineDeps{})
	trigger, a, b := testNode(Trigger, nil), testNode(Template, nil), testNode(Output, nil)
	created, err := v.m.createWorkflow(ctx, wfAnn, "Broken", nil, testDefinition(Manual, []WorkflowNodeDTO{trigger, a, b}, chain(trigger, a, b)))
	require.NoError(t, err)
	_, err = v.m.publishWorkflow(ctx, wfAnn, created.Workflow.Id.UUID)
	require.NoError(t, err)
	r, err := v.m.enqueue(ctx, wfAnn, created.Workflow.Id.UUID, Manual, StringMap{}, nil, nil)
	require.NoError(t, err)
	cyclic := testDefinition(Manual, []WorkflowNodeDTO{trigger, a, b}, append(chain(trigger, a, b), testEdge(b, a)))
	raw, err := json.Marshal(cyclic)
	require.NoError(t, err)
	_, err = v.pool.Exec(ctx, `UPDATE workflow_versions SET definition = $2 WHERE workflow_id = $1`, created.Workflow.Id.UUID, raw)
	require.NoError(t, err)
	e.Drain(ctx)

	s := readRun(t, v, r.Id.UUID)
	require.Equal(t, "failed", s.status)
	require.Equal(t, validateDefinition(cyclic).Error(), *s.errorMessage)
	var nodeRuns int
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_node_runs WHERE run_id = $1`, r.Id.UUID).Scan(&nodeRuns))
	require.Zero(t, nodeRuns)
}

// redact masks sensitive keys and interpolate replaces every placeholder.
func TestEngineRedactAndInterpolate(t *testing.T) {
	require.Equal(t, StringMap{"apiKey": "[REDACTED]", "Authorization": "[REDACTED]", "name": "x"},
		redact(StringMap{"apiKey": "k", "Authorization": "a", "name": "x"}))
	require.Equal(t, "a-b-{{c}}", interpolate("{{x}}-{{y}}-{{c}}", StringMap{"x": "a", "y": "b"}))
}
