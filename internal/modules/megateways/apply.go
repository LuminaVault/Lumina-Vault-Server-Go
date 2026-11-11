package megateways

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/hermescontainers"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// applyStep is HermesGatewayApplyStep.
type applyStep struct {
	ID         string     `json:"id"`
	State      string     `json:"state"`
	Detail     *string    `json:"detail,omitempty"`
	StartedAt  *wire.Time `json:"startedAt,omitempty"`
	FinishedAt *wire.Time `json:"finishedAt,omitempty"`
}

// ApplyJobStatus is HermesGatewayApplyJobStatus.
type ApplyJobStatus struct {
	JobID        wire.UUID   `json:"jobID"`
	State        string      `json:"state"`
	Steps        []applyStep `json:"steps"`
	ErrorMessage *string     `json:"errorMessage,omitempty"`
	StartedAt    wire.Time   `json:"startedAt"`
	UpdatedAt    wire.Time   `json:"updatedAt"`
}

// applyEvent is HermesGatewayApplyEvent, coded as {type, payload}.
type applyEvent struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

var pipeline = []string{"writeEnv", "restartContainer", "healthCheck"}

var errApplyRunning = errors.New("alreadyRunning")

// healthProbe is the apply job's check that the restarted container answers.
type healthProbe func(ctx context.Context, port int, apiKey string) bool

// defaultHealthProbe is App+build's gatewayHealthProbe: GET /v1/models on the
// container's loopback port, 5 s.
func defaultHealthProbe(ctx context.Context, port int, apiKey string) bool {
	cx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/models", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// applier is HermesGatewayApplyService. Jobs live in hermes_gateway_apply_jobs;
// only the live subscribers are in-memory.
type applier struct {
	pool          *pgxpool.Pool
	containers    hermescontainers.Containers
	probe         healthProbe
	healthTimeout time.Duration
	log           *slog.Logger
	now           func() time.Time

	mu   sync.Mutex
	subs map[uuid.UUID]map[int]chan applyEvent
	next int
}

type jobRow struct {
	id                   uuid.UUID
	state, stepsJSON     string
	errorMessage         *string
	createdAt, updatedAt time.Time
}

func (r jobRow) steps() []applyStep {
	var steps []applyStep
	if json.Unmarshal([]byte(r.stepsJSON), &steps) != nil {
		return []applyStep{}
	}
	return steps
}

func (r jobRow) snapshot() ApplyJobStatus {
	state := r.state
	switch state {
	case "running", "succeeded", "failed":
	default:
		state = "failed"
	}
	return ApplyJobStatus{
		JobID: wire.UUID{UUID: r.id}, State: state, Steps: r.steps(), ErrorMessage: r.errorMessage,
		StartedAt: wire.Time{Time: r.createdAt}, UpdatedAt: wire.Time{Time: r.updatedAt},
	}
}

func encodeSteps(steps []applyStep) string {
	b, err := wire.MarshalFoundation(steps)
	if err != nil {
		return "[]"
	}
	return string(b)
}

const jobColumns = `id, state, steps_json, error_message, created_at, updated_at`

func scanJob(row pgx.Row) (*jobRow, error) {
	var r jobRow
	err := row.Scan(&r.id, &r.state, &r.stepsJSON, &r.errorMessage, &r.createdAt, &r.updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // Swift's Optional
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (a *applier) startApply(ctx context.Context, tenant uuid.UUID) (ApplyJobStatus, error) {
	var running bool
	if err := a.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hermes_gateway_apply_jobs WHERE tenant_id = $1 AND state = 'running')`, tenant).Scan(&running); err != nil {
		return ApplyJobStatus{}, err
	}
	if running {
		return ApplyJobStatus{}, errApplyRunning
	}
	steps := make([]applyStep, len(pipeline))
	for i, id := range pipeline {
		steps[i] = applyStep{ID: id, State: "pending"}
	}
	now := a.now()
	r, err := scanJob(a.pool.QueryRow(ctx, `INSERT INTO hermes_gateway_apply_jobs (id, tenant_id, state, steps_json, created_at, updated_at)
		VALUES ($1, $2, 'running', $3, $4, $4) RETURNING `+jobColumns, uuid.New(), tenant, encodeSteps(steps), now))
	if err != nil {
		return ApplyJobStatus{}, err
	}
	go a.execute(context.WithoutCancel(ctx), r.id, tenant)
	return r.snapshot(), nil
}

func (a *applier) currentJob(ctx context.Context, tenant uuid.UUID) (*jobRow, error) {
	r, err := scanJob(a.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM hermes_gateway_apply_jobs
		WHERE tenant_id = $1 AND state = 'running' ORDER BY created_at DESC LIMIT 1`, tenant))
	if err != nil || r != nil {
		return r, err
	}
	return scanJob(a.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM hermes_gateway_apply_jobs
		WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 1`, tenant))
}

func (a *applier) job(ctx context.Context, id, tenant uuid.UUID) (*jobRow, error) {
	return scanJob(a.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM hermes_gateway_apply_jobs WHERE id = $1 AND tenant_id = $2`, id, tenant))
}

func (a *applier) jobAny(ctx context.Context, id uuid.UUID) (*jobRow, error) {
	return scanJob(a.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM hermes_gateway_apply_jobs WHERE id = $1`, id))
}

// subscribe replays the job's steps, then follows it to done. The channel closes
// after done.
func (a *applier) subscribe(ctx context.Context, id, tenant uuid.UUID) (<-chan applyEvent, func()) {
	out := make(chan applyEvent, 64)
	r, err := a.job(ctx, id, tenant)
	if err != nil || r == nil {
		out <- applyEvent{Type: "error", Payload: "notFound"}
		close(out)
		return out, func() {}
	}
	snap := r.snapshot()
	for _, s := range snap.Steps {
		out <- applyEvent{Type: "step", Payload: s}
	}
	if snap.State != "running" {
		out <- applyEvent{Type: "done", Payload: snap}
		close(out)
		return out, func() {}
	}
	a.mu.Lock()
	n := a.next
	a.next++
	if a.subs[id] == nil {
		a.subs[id] = map[int]chan applyEvent{}
	}
	a.subs[id][n] = out
	a.mu.Unlock()
	return out, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if c, ok := a.subs[id][n]; ok {
			delete(a.subs[id], n)
			close(c)
		}
	}
}

func (a *applier) broadcast(id uuid.UUID, ev applyEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.subs[id] {
		select {
		case c <- ev:
		default:
		}
	}
}

func (a *applier) finishSubscribers(id uuid.UUID, snap ApplyJobStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.subs[id] {
		select {
		case c <- applyEvent{Type: "done", Payload: snap}:
		default:
		}
		close(c)
	}
	delete(a.subs, id)
}

func (a *applier) execute(ctx context.Context, id, tenant uuid.UUID) {
	err := a.run(ctx, id, tenant)
	if err == nil {
		return
	}
	a.log.ErrorContext(ctx, "hermes gateway apply failed", "jobID", id, "tenantID", tenant, "error", err)
	a.markActiveStepFailed(ctx, id, err)
	a.markGatewayRows(ctx, tenant, false, ptr("apply_failed"))
	a.finalize(ctx, id, "failed", ptr(fmt.Sprintf("Applying your gateway settings failed: %v", err)))
}

func (a *applier) run(ctx context.Context, id, tenant uuid.UUID) error {
	if err := a.mark(ctx, id, "writeEnv", "running", nil); err != nil {
		return err
	}
	written, err := a.containers.SeedGatewayConfig(ctx, tenant)
	if err != nil {
		return err
	}
	if written != nil {
		err = a.mark(ctx, id, "writeEnv", "succeeded", ptr(fmt.Sprintf("wrote %d gateway variable(s)", *written)))
	} else {
		err = a.mark(ctx, id, "writeEnv", "skipped", ptr("no container yet — will seed on launch"))
	}
	if err != nil {
		return err
	}
	if err := a.mark(ctx, id, "restartContainer", "running", nil); err != nil {
		return err
	}
	if err := a.containers.ApplyGatewayConfig(ctx, tenant); err != nil {
		return err
	}
	if err := a.mark(ctx, id, "restartContainer", "succeeded", nil); err != nil {
		return err
	}
	if err := a.mark(ctx, id, "healthCheck", "running", nil); err != nil {
		return err
	}
	h, err := a.containers.Handle(ctx, tenant)
	if err != nil {
		return err
	}
	if h == nil {
		if err := a.mark(ctx, id, "healthCheck", "failed", ptr("container did not start")); err != nil {
			return err
		}
		a.markGatewayRows(ctx, tenant, false, ptr("no_container"))
		a.finalize(ctx, id, "failed", ptr("Your assistant didn't start. Please try again."))
		return nil
	}
	if !a.awaitHealthy(ctx, h.Port, h.APIServerKey) {
		if err := a.mark(ctx, id, "healthCheck", "failed", ptr("gateway didn't respond in time")); err != nil {
			return err
		}
		a.markGatewayRows(ctx, tenant, false, ptr("unhealthy"))
		a.finalize(ctx, id, "failed", ptr("Your assistant restarted but didn't respond in time. Double-check your tokens and try again."))
		return nil
	}
	if err := a.mark(ctx, id, "healthCheck", "succeeded", nil); err != nil {
		return err
	}
	a.markGatewayRows(ctx, tenant, true, nil)
	a.finalize(ctx, id, "succeeded", nil)
	return nil
}

func (a *applier) awaitHealthy(ctx context.Context, port int, key string) bool {
	deadline := a.now().Add(a.healthTimeout)
	for a.now().Before(deadline) {
		if a.probe(ctx, port, key) {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return a.probe(ctx, port, key)
}

func (a *applier) markGatewayRows(ctx context.Context, tenant uuid.UUID, verified bool, code *string) {
	ts := a.now()
	if verified {
		_, _ = a.pool.Exec(ctx, `UPDATE user_hermes_gateways SET status = 'verified', verified_at = $2, last_failure_at = NULL,
			last_failure_code = NULL, updated_at = $2 WHERE tenant_id = $1`, tenant, ts)
		return
	}
	_, _ = a.pool.Exec(ctx, `UPDATE user_hermes_gateways SET status = 'error', last_failure_at = $2, last_failure_code = $3,
		updated_at = $2 WHERE tenant_id = $1`, tenant, ts, code)
}

func (a *applier) mark(ctx context.Context, id uuid.UUID, step, state string, detail *string) error {
	r, err := a.jobAny(ctx, id)
	if err != nil || r == nil {
		return err
	}
	steps := r.steps()
	ts := a.now()
	var updated *applyStep
	for i := range steps {
		if steps[i].ID != step {
			continue
		}
		prior := steps[i]
		next := applyStep{ID: step, State: state, Detail: prior.Detail, StartedAt: prior.StartedAt, FinishedAt: prior.FinishedAt}
		if detail != nil {
			next.Detail = detail
		}
		if state == "running" {
			next.StartedAt = &wire.Time{Time: ts}
		}
		if state == "succeeded" || state == "failed" || state == "skipped" {
			next.FinishedAt = &wire.Time{Time: ts}
		}
		steps[i] = next
		updated = &steps[i]
		break
	}
	if _, err := a.pool.Exec(ctx, `UPDATE hermes_gateway_apply_jobs SET steps_json = $2, updated_at = $3 WHERE id = $1`, id, encodeSteps(steps), ts); err != nil {
		return err
	}
	if updated != nil {
		a.broadcast(id, applyEvent{Type: "step", Payload: *updated})
	}
	return nil
}

func (a *applier) markActiveStepFailed(ctx context.Context, id uuid.UUID, cause error) {
	r, err := a.jobAny(ctx, id)
	if err != nil || r == nil {
		return
	}
	var active string
	for _, want := range []string{"running", "pending"} {
		for _, s := range r.steps() {
			if s.State == want {
				active = s.ID
				break
			}
		}
		if active != "" {
			break
		}
	}
	if active != "" {
		_ = a.mark(ctx, id, active, "failed", ptr(cause.Error()))
	}
}

func (a *applier) finalize(ctx context.Context, id uuid.UUID, state string, msg *string) {
	r, err := scanJob(a.pool.QueryRow(ctx, `UPDATE hermes_gateway_apply_jobs SET state = $2, error_message = $3, updated_at = $4
		WHERE id = $1 RETURNING `+jobColumns, id, state, msg, a.now()))
	if err != nil || r == nil {
		return
	}
	a.broadcast(id, applyEvent{Type: "status", Payload: state})
	a.finishSubscribers(id, r.snapshot())
}
