package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/embedding"
	"lumina/internal/ids"
	"lumina/internal/jobs"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/telemetry"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Engine timings: WorkflowEngine's pollInterval, its 120 s lease and the 20 s
// heartbeat while a node runs.
const (
	EnginePollInterval   = time.Second
	DefaultEngineWorkers = 4
	engineLease          = 120 * time.Second
	engineHeartbeat      = 20 * time.Second
	approvalTTL          = 24 * time.Hour
)

// ChatTransport is the routed HermesChatTransport the llm node calls.
type ChatTransport interface {
	ChatCompletions(ctx context.Context, payload []byte, sessionKey, sessionID string) (llm.Metadata, error)
}

// ProfileResolver is RouterProfileRepository.resolve.
type ProfileResolver interface {
	Resolve(ctx context.Context, tenant uuid.UUID, spaceID *uuid.UUID, jobID, workflowID string) (*llm.RouterProfile, error)
}

// SkillExecutor runs a catalog skill for the skill node with trigger
// .event(name: "workflow"). found is false when the tenant has no such skill.
type SkillExecutor interface {
	RunWorkflowSkill(ctx context.Context, tenant uuid.UUID, tier, username, name, input string) (markdown string, model *string, found bool, err error)
}

// WorkflowNotifier is APNSNotificationService.notifyWorkflow.
type WorkflowNotifier interface {
	NotifyWorkflow(ctx context.Context, tenant, workflowID, runID uuid.UUID, title, body, state string) error
}

// EngineDeps are WorkflowEngine's collaborators besides the module's own.
type EngineDeps struct {
	Transport    ChatTransport
	DefaultModel string
	Skills       SkillExecutor
	Embeddings   embedding.Service
	Memories     *memorystore.Repository
	Profiles     ProfileResolver
	Push         WorkflowNotifier // nil: no pushes
	Workers      int              // cerberus.studio.workerCount; <1 means 1
	Log          *slog.Logger
}

// engineError is WorkflowEngineError; Error() is Swift's String(describing:).
type engineError string

func (e engineError) Error() string { return string(e) }

const (
	errMissingVersion     engineError = "missingVersion"
	errInvalidGraph       engineError = "invalidGraph"
	errInvalidLLMResponse engineError = "invalidLLMResponse"
	errRunCancelled       engineError = "cancelled"
)

func errExecutorUnavailable(what string) error {
	return engineError(fmt.Sprintf("executorUnavailable(%q)", what))
}

func errIterationLimit(limit int) error {
	return engineError(fmt.Sprintf("iterationLimitExceeded(%d)", limit))
}

// pausedError is WorkflowEngineError.paused(reason).
type pausedError struct{ reason WorkflowPauseReason }

func (e pausedError) Error() string { return "paused(" + string(e.reason) + ")" }

// Engine is WorkflowEngine (Workflows/WorkflowEngine.swift): the durable workflow
// worker. Postgres leases own execution, so a restart or a second process can't
// double-claim a run: a run is claimed with FOR UPDATE SKIP LOCKED, its lease is
// renewed while it runs, and a run whose lease lapsed is claimed again.
type Engine struct {
	m        *Module
	d        EngineDeps
	workerID string
	log      *slog.Logger
}

// NewEngine builds the worker over the module's pool and spend policy.
func (m *Module) NewEngine(d EngineDeps) *Engine {
	if d.Workers < 1 {
		d.Workers = 1
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Engine{m: m, d: d, workerID: strings.ToUpper(uuid.NewString()), log: log}
}

// Job polls once a second. Each tick runs the worker slots until none of them
// finds a queued (or lease-expired) run, as Swift's workerCount loops do between
// their sleeps.
func (e *Engine) Job() jobs.Job {
	return jobs.Job{
		Name:     "WorkflowEngine",
		Schedule: jobs.Every(EnginePollInterval, 0),
		Run: func(ctx context.Context) error {
			e.Drain(ctx)
			return nil
		},
	}
}

// Drain runs every worker slot until the queue is empty.
func (e *Engine) Drain(ctx context.Context) {
	var wg sync.WaitGroup
	for slot := range e.d.Workers {
		owner := fmt.Sprintf("%s:%d", e.workerID, slot)
		wg.Go(func() {
			for ctx.Err() == nil {
				run, err := e.claim(ctx, owner)
				if err != nil {
					e.log.Error("workflow worker tick failed", "error", err.Error())
					return
				}
				if run == nil {
					return
				}
				e.execute(ctx, *run, owner)
			}
		})
	}
	wg.Wait()
}

type claimedRun struct {
	id, tenant, workflow, version uuid.UUID
	input                         []byte
	conversation                  *uuid.UUID
	createdAt                     time.Time
}

func (e *Engine) claim(ctx context.Context, owner string) (*claimedRun, error) {
	now := e.m.now()
	var r claimedRun
	err := e.m.pool.QueryRow(ctx, `
		WITH candidate AS (
		  SELECT id FROM workflow_runs
		  WHERE status = 'queued'
		     OR (status = 'running' AND lease_expires_at < NOW())
		  ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1
		)
		UPDATE workflow_runs r SET status = 'running', lease_owner = $1,
		  lease_expires_at = $2, lease_heartbeat_at = $3,
		  started_at = COALESCE(started_at, $3), updated_at = NOW()
		FROM candidate WHERE r.id = candidate.id
		RETURNING r.id, r.tenant_id, r.workflow_id, r.version_id, r.input, r.conversation_id, r.created_at`,
		owner, now.Add(engineLease), now).
		Scan(&r.id, &r.tenant, &r.workflow, &r.version, &r.input, &r.conversation, &r.createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	telemetry.Global.Workflow.Claimed.Add(ctx, 1)
	telemetry.Global.Workflow.RecordQueueLatency(ctx, &r.createdAt)
	return &r, nil
}

func nodeKey(node uuid.UUID, key string) string { return "nodes." + ids.Upper(node) + "." + key }

func (e *Engine) execute(ctx context.Context, run claimedRun, owner string) {
	err := e.executeGraph(ctx, run, owner)
	var paused pausedError
	switch {
	case err == nil:
		// Succeeded; executeGraph already saved the terminal state.
	case errors.Is(err, errApprovalPause):
		// Waiting for approval; pauseForApproval saved it.
	case errors.Is(err, errRunCancelled):
		// The API already persisted the terminal cancellation state. Don't race
		// it by marking the run failed here.
		e.exec(ctx, `UPDATE workflow_runs SET lease_owner = NULL, lease_expires_at = NULL, updated_at = NOW() WHERE id = $1`, run.id)
	case errors.As(err, &paused):
		message := "Managed inference paused: " + string(paused.reason)
		e.exec(ctx, `UPDATE workflow_runs SET status = 'paused', pause_reason = $2, error_message = $3,
			lease_owner = NULL, lease_expires_at = NULL, updated_at = NOW() WHERE id = $1`, run.id, string(paused.reason), message)
		telemetry.Global.Workflow.Paused.Add(ctx, 1)
		e.m.appendEventData(ctx, run.tenant, run.id, RunPaused, nil, &message, map[string]string{"reason": string(paused.reason)})
		e.notify(ctx, run, "Workflow paused", "Open Cerberus Studio to continue this run.", "paused")
	default:
		message := err.Error()
		e.exec(ctx, `UPDATE workflow_runs SET status = 'failed', error_message = $2, ended_at = $3,
			lease_owner = NULL, lease_expires_at = NULL, updated_at = NOW() WHERE id = $1`, run.id, message, e.m.now())
		telemetry.Global.Workflow.Failed.Add(ctx, 1)
		e.m.appendEvent(ctx, run.tenant, run.id, RunFailed, nil, &message)
		e.notify(ctx, run, "Workflow needs attention", "Open Cerberus Studio to inspect the failed run.", "failed")
	}
}

// exec is try? await run.save: a failed write is only logged.
func (e *Engine) exec(ctx context.Context, sql string, args ...any) {
	if _, err := e.m.pool.Exec(ctx, sql, args...); err != nil {
		e.log.Warn("workflow run save failed", "error", err.Error())
	}
}

// errApprovalPause ends execute without a terminal state: the run waits.
var errApprovalPause = errors.New("waiting for approval")

func (e *Engine) executeGraph(ctx context.Context, run claimedRun, owner string) error {
	e.m.appendEvent(ctx, run.tenant, run.id, RunStarted, nil, nil)
	var raw []byte
	err := e.m.pool.QueryRow(ctx, `SELECT definition FROM workflow_versions WHERE id = $1`, run.version).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return errMissingVersion
	}
	if err != nil {
		return err
	}
	def, err := decodeDefinition(raw)
	if err != nil {
		return err
	}
	if err := validateDefinition(def); err != nil {
		return err
	}
	order, err := topologicalOrder(def)
	if err != nil {
		return err
	}
	context_, err := decodeStrings(run.input)
	if err != nil {
		return err
	}
	if context_ == nil {
		context_ = StringMap{}
	}
	succeeded, err := e.succeededNodes(ctx, run.id)
	if err != nil {
		return err
	}
	for node, output := range succeeded {
		for k, v := range output {
			context_[nodeKey(node, k)] = v
		}
	}

	var trigger *WorkflowNodeDTO
	for i := range order {
		if order[i].Kind == Trigger {
			trigger = &order[i]
			break
		}
	}
	if trigger == nil {
		return errInvalidGraph
	}
	activated := map[uuid.UUID]bool{trigger.Id.UUID: true}
	activateOutgoing(*trigger, def, context_, activated)

	depths := nodeDepths(def)
	grouped := map[int][]WorkflowNodeDTO{}
	for _, n := range order {
		if n.Kind != Trigger {
			grouped[depths[n.Id.UUID]] = append(grouped[depths[n.Id.UUID]], n)
		}
	}
	levels := make([]int, 0, len(grouped))
	for d := range grouped {
		levels = append(levels, d)
	}
	sort.Ints(levels)
	for _, depth := range levels {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.heartbeat(ctx, run.id, owner); err != nil {
			return err
		}
		var wave []WorkflowNodeDTO
		for _, n := range grouped[depth] {
			if activated[n.Id.UUID] {
				wave = append(wave, n)
			}
		}
		// Reliable-core v1 executes deterministically: each activated node in
		// stable ID order rather than racing shared context mutations.
		sort.Slice(wave, func(i, j int) bool { return ids.Upper(wave[i].Id.UUID) < ids.Upper(wave[j].Id.UUID) })
		for _, node := range wave {
			if _, done := succeeded[node.Id.UUID]; done {
				activateOutgoing(node, def, context_, activated)
				continue
			}
			if node.Kind == Approval {
				if err := e.pauseForApproval(ctx, run, node); err != nil {
					return err
				}
				return errApprovalPause
			}
			snapshot := copyMap(context_)
			output, err := e.withLeaseHeartbeat(ctx, run.id, owner, func(ctx context.Context) (StringMap, error) {
				return e.executePersistedNode(ctx, node, run, snapshot)
			})
			if err != nil {
				return err
			}
			for k, v := range output {
				context_[nodeKey(node.Id.UUID, k)] = v
			}
			activateOutgoing(node, def, context_, activated)
		}
	}
	if err := e.heartbeat(ctx, run.id, owner); err != nil {
		return err
	}
	if _, err := e.m.pool.Exec(ctx, `UPDATE workflow_runs SET status = 'succeeded', ended_at = $2,
		lease_owner = NULL, lease_expires_at = NULL, updated_at = NOW() WHERE id = $1`, run.id, e.m.now()); err != nil {
		return err
	}
	telemetry.Global.Workflow.Completed.Add(ctx, 1)
	e.m.appendEvent(ctx, run.tenant, run.id, RunCompleted, nil, nil)
	e.notify(ctx, run, "Workflow complete", "Your workflow finished successfully.", "succeeded")
	return nil
}

func copyMap(in StringMap) StringMap {
	out := make(StringMap, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// succeededNodes reads prior succeeded node runs: a resumed run (after approval
// or a lapsed lease) skips them and reuses their output.
func (e *Engine) succeededNodes(ctx context.Context, run uuid.UUID) (map[uuid.UUID]StringMap, error) {
	rows, err := e.m.pool.Query(ctx, `SELECT node_id, output_snapshot FROM workflow_node_runs
		WHERE run_id = $1 AND status = 'succeeded'`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]StringMap{}
	for rows.Next() {
		var node uuid.UUID
		var raw []byte
		if err := rows.Scan(&node, &raw); err != nil {
			return nil, err
		}
		output, err := decodeStrings(raw)
		if err != nil {
			return nil, err
		}
		if output == nil {
			output = StringMap{}
		}
		out[node] = output
	}
	return out, rows.Err()
}

func (e *Engine) heartbeat(ctx context.Context, run uuid.UUID, owner string) error {
	tag, err := e.m.pool.Exec(ctx, `UPDATE workflow_runs
		SET lease_expires_at = NOW() + INTERVAL '120 seconds', lease_heartbeat_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status = 'running' AND lease_owner = $2`, run, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errRunCancelled
	}
	return nil
}

// withLeaseHeartbeat runs op while renewing the lease every 20 s. A renewal that
// finds the run no longer ours (cancelled, or reclaimed) cancels op.
func (e *Engine) withLeaseHeartbeat(ctx context.Context, run uuid.UUID, owner string, op func(context.Context) (StringMap, error)) (StringMap, error) {
	opCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(engineHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := e.heartbeat(ctx, run, owner); err != nil {
					cancel(errRunCancelled)
					return
				}
			}
		}
	}()
	out, err := op(opCtx)
	close(done)
	if cause := context.Cause(opCtx); errors.Is(cause, errRunCancelled) {
		return nil, errRunCancelled
	}
	return out, err
}

func (e *Engine) executePersistedNode(ctx context.Context, node WorkflowNodeDTO, run claimedRun, input StringMap) (StringMap, error) {
	nodeRunID := uuid.New()
	inputSnapshot, err := json.Marshal(redact(input))
	if err != nil {
		return nil, err
	}
	if _, err := e.m.pool.Exec(ctx, `INSERT INTO workflow_node_runs
		(id, run_id, node_id, node_name, status, attempt, started_at, input_snapshot, created_at)
		VALUES ($1, $2, $3, $4, 'running', 1, $5, $6, NOW())`,
		nodeRunID, run.id, node.Id.UUID, node.Name, e.m.now(), inputSnapshot); err != nil {
		return nil, err
	}
	nodeID := node.Id.UUID
	name := node.Name
	e.m.appendEvent(ctx, run.tenant, run.id, NodeStarted, &nodeID, &name)
	output, err := e.executeNode(ctx, node, run, input)
	if err != nil {
		message := err.Error()
		if _, saveErr := e.m.pool.Exec(ctx, `UPDATE workflow_node_runs SET status = 'failed', error_message = $2, ended_at = $3
			WHERE id = $1`, nodeRunID, message, e.m.now()); saveErr != nil {
			return nil, saveErr
		}
		return nil, err
	}
	outputSnapshot, err := json.Marshal(redact(output))
	if err != nil {
		return nil, err
	}
	if _, err := e.m.pool.Exec(ctx, `UPDATE workflow_node_runs
		SET output_snapshot = $2, selected_provider = $3, selected_model = $4, tokens_in = $5, tokens_out = $6,
		    managed_cost_usd_micros = $7, status = 'succeeded', ended_at = $8
		WHERE id = $1`, nodeRunID, outputSnapshot, optional(output, "provider"), optional(output, "model"),
		optionalInt(output, "tokensIn"), optionalInt(output, "tokensOut"), optionalInt(output, "managedCostUsdMicros"),
		e.m.now()); err != nil {
		return nil, err
	}
	var text *string
	if t, ok := output["text"]; ok {
		p := prefixRunes(t, 2000)
		text = &p
	}
	data := StringMap{}
	for k, v := range output {
		if k != "text" {
			data[k] = v
		}
	}
	e.m.appendEventData(ctx, run.tenant, run.id, NodeOutput, &nodeID, text, data)
	e.m.appendEvent(ctx, run.tenant, run.id, NodeCompleted, &nodeID, nil)
	return output, nil
}

func optional(m StringMap, key string) *string {
	if v, ok := m[key]; ok {
		return &v
	}
	return nil
}

// optionalInt is output[key].flatMap(Int64.init).
func optionalInt(m StringMap, key string) *int64 {
	v, ok := m[key]
	if !ok {
		return nil
	}
	n, ok := swiftInt64(v)
	if !ok {
		return nil
	}
	return &n
}

func (e *Engine) pauseForApproval(ctx context.Context, run claimedRun, node WorkflowNodeDTO) error {
	var exists bool
	if err := e.m.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workflow_approvals WHERE run_id = $1 AND node_id = $2)`,
		run.id, node.Id.UUID).Scan(&exists); err != nil {
		return err
	}
	message := optional(node.Configuration, "message")
	if !exists {
		now := e.m.now()
		if _, err := e.m.pool.Exec(ctx, `INSERT INTO workflow_node_runs
			(id, run_id, node_id, node_name, status, attempt, started_at, created_at)
			VALUES ($1, $2, $3, $4, 'waitingForApproval', 1, $5, NOW())`,
			uuid.New(), run.id, node.Id.UUID, node.Name, now); err != nil {
			return err
		}
		if _, err := e.m.pool.Exec(ctx, `INSERT INTO workflow_approvals
			(id, tenant_id, run_id, workflow_id, node_id, title, message, status, memory_ids, expires_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', '{}', $8, NOW(), NOW())`,
			uuid.New(), run.tenant, run.id, run.workflow, node.Id.UUID, node.Name, message, now.Add(approvalTTL)); err != nil {
			return err
		}
	}
	if _, err := e.m.pool.Exec(ctx, `UPDATE workflow_runs SET status = 'waitingForApproval',
		lease_owner = NULL, lease_expires_at = NULL, updated_at = NOW() WHERE id = $1`, run.id); err != nil {
		return err
	}
	telemetry.Global.Workflow.ApprovalsRequired.Add(ctx, 1)
	body := node.Name
	if message != nil {
		body = *message
	}
	nodeID := node.Id.UUID
	e.m.appendEvent(ctx, run.tenant, run.id, ApprovalRequired, &nodeID, &body)
	e.notify(ctx, run, "Approval needed", body, "waiting_for_approval")
	return nil
}

func (e *Engine) notify(ctx context.Context, run claimedRun, title, body, state string) {
	if e.d.Push == nil {
		return
	}
	if err := e.d.Push.NotifyWorkflow(ctx, run.tenant, run.workflow, run.id, title, body, state); err != nil {
		e.log.Warn("workflow push failed", "run_id", ids.Upper(run.id), "error", err.Error())
	}
}

// activateOutgoing marks the targets of node's edges. A condition activates only
// the edge whose true/false port matches its result; other ports always pass.
func activateOutgoing(node WorkflowNodeDTO, def WorkflowDefinitionDTO, ctx StringMap, activated map[uuid.UUID]bool) {
	result, hasResult := ctx[nodeKey(node.Id.UUID, "result")]
	for _, edge := range def.Edges {
		if edge.SourceNodeID != node.Id {
			continue
		}
		if node.Kind == Condition && (edge.SourcePort == "true" || edge.SourcePort == "false") &&
			(!hasResult || edge.SourcePort != result) {
			continue
		}
		activated[edge.TargetNodeID.UUID] = true
	}
}

// topologicalOrder is Kahn's algorithm; a cycle (or a dangling edge) is invalidGraph.
func topologicalOrder(def WorkflowDefinitionDTO) ([]WorkflowNodeDTO, error) {
	indegree := map[uuid.UUID]int{}
	for _, n := range def.Nodes {
		indegree[n.Id.UUID] = 0
	}
	for _, edge := range def.Edges {
		indegree[edge.TargetNodeID.UUID]++
	}
	var queue []uuid.UUID
	for _, n := range def.Nodes {
		if indegree[n.Id.UUID] == 0 {
			queue = append(queue, n.Id.UUID)
		}
	}
	byID := map[uuid.UUID]WorkflowNodeDTO{}
	for _, n := range def.Nodes {
		if _, dup := byID[n.Id.UUID]; !dup {
			byID[n.Id.UUID] = n
		}
	}
	var result []WorkflowNodeDTO
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if n, ok := byID[id]; ok {
			result = append(result, n)
		}
		for _, edge := range def.Edges {
			if edge.SourceNodeID.UUID != id {
				continue
			}
			target := edge.TargetNodeID.UUID
			indegree[target]--
			if indegree[target] == 0 {
				queue = append(queue, target)
			}
		}
	}
	if len(result) != len(def.Nodes) {
		return nil, errInvalidGraph
	}
	return result, nil
}

// nodeDepths is each node's longest distance from a root.
func nodeDepths(def WorkflowDefinitionDTO) map[uuid.UUID]int {
	depths := map[uuid.UUID]int{}
	order, _ := topologicalOrder(def)
	for _, n := range order {
		best := -1
		for _, edge := range def.Edges {
			if edge.TargetNodeID != n.Id {
				continue
			}
			if d, ok := depths[edge.SourceNodeID.UUID]; ok && d > best {
				best = d
			}
		}
		depths[n.Id.UUID] = best + 1
	}
	return depths
}

// interpolate replaces every {{key}} with its context value, keys in sorted order.
func interpolate(value string, ctx StringMap) string {
	keys := make([]string, 0, len(ctx))
	for k := range ctx {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		value = strings.ReplaceAll(value, "{{"+k+"}}", ctx[k])
	}
	return value
}

var sensitiveKeys = []string{"secret", "token", "password", "authorization", "key"}

// redact masks values whose key looks sensitive and caps the rest at 16384
// characters, for the snapshots.
func redact(values StringMap) StringMap {
	out := make(StringMap, len(values))
	for k, v := range values {
		lower := strings.ToLower(k)
		sensitive := false
		for _, s := range sensitiveKeys {
			if strings.Contains(lower, s) {
				sensitive = true
				break
			}
		}
		if sensitive {
			out[k] = "[REDACTED]"
		} else {
			out[k] = prefixRunes(v, 16384)
		}
	}
	return out
}
