package workflows

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func newScheduledWorkflow(t *testing.T, v env, config StringMap) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	trigger, output := testNode(Trigger, nil), testNode(Output, StringMap{"value": "{{scheduledAt}}"})
	def := testDefinition(Schedule, []WorkflowNodeDTO{trigger, output}, []WorkflowEdgeDTO{testEdge(trigger, output)})
	def.TriggerConfiguration = config
	created, err := v.m.createWorkflow(ctx, wfAnn, "Scheduled "+uuid.NewString()[:8], nil, def)
	require.NoError(t, err)
	id := created.Workflow.Id.UUID
	_, err = v.m.publishWorkflow(ctx, wfAnn, id)
	require.NoError(t, err)
	return id
}

func runsFor(t *testing.T, v env, workflow uuid.UUID) (count int, keys []string) {
	t.Helper()
	rows, err := v.pool.Query(context.Background(),
		`SELECT dedupe_key FROM workflow_runs WHERE workflow_id = $1 AND trigger_kind = 'schedule' ORDER BY created_at`, workflow)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var k *string
		require.NoError(t, rows.Scan(&k))
		count++
		if k != nil {
			keys = append(keys, *k)
		}
	}
	return count, keys
}

// WorkflowScheduler: a cron that matches the minute enqueues one run keyed by
// workflow and minute; the same minute again enqueues nothing; a minute the cron
// doesn't match enqueues nothing.
func TestSchedulerEnqueuesCronOncePerMinute(t *testing.T) {
	v := setup(t, func(o *Options) { o.ManagedInferenceAvailable = true })
	ctx := context.Background()
	quiet := slog.New(slog.DiscardHandler)
	id := newScheduledWorkflow(t, v, StringMap{"cron": "0 * * * *", "timezone": "UTC"})
	_, err := v.pool.Exec(ctx, `UPDATE workflow_runs SET status = 'cancelled' WHERE tenant_id = $1`, wfAnn)
	require.NoError(t, err)

	top := time.Date(2026, 11, 12, 18, 0, 30, 0, time.UTC)
	n, err := v.m.ScheduleTick(ctx, top, quiet)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	count, keys := runsFor(t, v, id)
	require.Equal(t, 1, count)
	require.Equal(t, []string{"schedule:" + upper(id) + ":" + itoa(top.Unix()/60)}, keys)

	var input string
	require.NoError(t, v.pool.QueryRow(ctx,
		`SELECT input::text FROM workflow_runs WHERE workflow_id = $1`, id).Scan(&input))
	require.JSONEq(t, `{"scheduledAt":"2026-11-12T18:00:30Z"}`, input)

	n, err = v.m.ScheduleTick(ctx, top.Add(10*time.Second), quiet)
	require.NoError(t, err)
	require.Zero(t, n)
	n, err = v.m.ScheduleTick(ctx, top.Add(time.Minute), quiet)
	require.NoError(t, err)
	require.Zero(t, n)
	count, _ = runsFor(t, v, id)
	require.Equal(t, 1, count)
}

// A one-shot runAt in the past enqueues once and disables the workflow; a future
// runAt does nothing.
func TestSchedulerOneShotRunsOnceAndDisables(t *testing.T) {
	v := setup(t, func(o *Options) { o.ManagedInferenceAvailable = true })
	ctx := context.Background()
	quiet := slog.New(slog.DiscardHandler)
	now := time.Date(2026, 11, 12, 18, 7, 0, 0, time.UTC)

	future := newScheduledWorkflow(t, v, StringMap{"runAt": "2026-11-12T19:00:00Z"})
	n, err := v.m.ScheduleTick(ctx, now, quiet)
	require.NoError(t, err)
	require.Zero(t, n)
	count, _ := runsFor(t, v, future)
	require.Zero(t, count)

	past := newScheduledWorkflow(t, v, StringMap{"runAt": "2026-11-12T18:00:00Z"})
	n, err = v.m.ScheduleTick(ctx, now, quiet)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	var enabled bool
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT enabled FROM workflows WHERE id = $1`, past).Scan(&enabled))
	require.False(t, enabled)

	n, err = v.m.ScheduleTick(ctx, now.Add(time.Minute), quiet)
	require.NoError(t, err)
	require.Zero(t, n)
	count, _ = runsFor(t, v, past)
	require.Equal(t, 1, count)
}

// WorkflowMaintenanceService: an expired pending approval expires and times out its
// waiting run; a live one is untouched; snapshots and events older than 30 days go,
// newer ones stay.
func TestMaintenanceExpiresApprovalsAndPrunesSnapshots(t *testing.T) {
	v := setup(t, func(o *Options) { o.ManagedInferenceAvailable = true })
	ctx := context.Background()
	now := time.Now().UTC()
	trigger, output := testNode(Trigger, nil), testNode(Output, StringMap{"value": "{{event}}"})
	created, err := v.m.createWorkflow(ctx, wfAnn, "Approvals", nil,
		testDefinition(Manual, []WorkflowNodeDTO{trigger, output}, []WorkflowEdgeDTO{testEdge(trigger, output)}))
	require.NoError(t, err)
	wf := created.Workflow.Id.UUID
	_, err = v.m.publishWorkflow(ctx, wfAnn, wf)
	require.NoError(t, err)

	run := func(status string) uuid.UUID {
		r, err := v.m.enqueue(ctx, wfAnn, wf, Manual, StringMap{}, nil, nil)
		require.NoError(t, err)
		_, err = v.pool.Exec(ctx, `UPDATE workflow_runs SET status = $2 WHERE id = $1`, r.Id.UUID, status)
		require.NoError(t, err)
		return r.Id.UUID
	}
	approval := func(runID uuid.UUID, expires time.Time) uuid.UUID {
		id := uuid.New()
		_, err := v.pool.Exec(ctx, `INSERT INTO workflow_approvals (id, tenant_id, run_id, workflow_id, node_id, title, expires_at)
			VALUES ($1, $2, $3, $4, $5, 'Approve?', $6)`, id, wfAnn, runID, wf, output.Id.UUID, expires)
		require.NoError(t, err)
		return id
	}
	// Enqueue both under the active-run limit, then park them on approval.
	stale, live := run("cancelled"), run("cancelled")
	_, err = v.pool.Exec(ctx, `UPDATE workflow_runs SET status = 'waitingForApproval' WHERE id IN ($1, $2)`, stale, live)
	require.NoError(t, err)
	staleApproval := approval(stale, now.Add(-time.Minute))
	liveApproval := approval(live, now.Add(time.Hour))

	old, recent := now.Add(-31*24*time.Hour), now.Add(-29*24*time.Hour)
	for i, at := range []time.Time{old, recent} {
		_, err = v.pool.Exec(ctx, `INSERT INTO workflow_node_runs (run_id, node_id, node_name, status, attempt, input_snapshot, output_snapshot, created_at)
			VALUES ($1, $2, 'Output', 'completed', $4, '{"a":"1"}', '{"b":"2"}', $3)`, live, output.Id.UUID, at, i+1)
		require.NoError(t, err)
	}
	_, err = v.pool.Exec(ctx, `DELETE FROM workflow_run_events WHERE run_id IN ($1, $2)`, stale, live)
	require.NoError(t, err)
	for i, at := range []time.Time{old, recent} {
		_, err = v.pool.Exec(ctx, `INSERT INTO workflow_run_events (sequence, tenant_id, run_id, kind, created_at)
			VALUES ($1, $2, $3, 'run.queued', $4)`, 900001+i, wfAnn, live, at)
		require.NoError(t, err)
	}

	require.NoError(t, v.m.MaintenanceTick(ctx, now, slog.New(slog.DiscardHandler)))

	status := func(query string, id uuid.UUID) string {
		var s string
		require.NoError(t, v.pool.QueryRow(ctx, query, id).Scan(&s))
		return s
	}
	require.Equal(t, "expired", status(`SELECT status FROM workflow_approvals WHERE id = $1`, staleApproval))
	require.Equal(t, "pending", status(`SELECT status FROM workflow_approvals WHERE id = $1`, liveApproval))
	require.Equal(t, "timedOut", status(`SELECT status FROM workflow_runs WHERE id = $1`, stale))
	require.Equal(t, "Approval expired", status(`SELECT error_message FROM workflow_runs WHERE id = $1`, stale))
	require.Equal(t, "waitingForApproval", status(`SELECT status FROM workflow_runs WHERE id = $1`, live))

	var withSnapshots, events int
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_node_runs
		WHERE run_id = $1 AND input_snapshot IS NOT NULL`, live).Scan(&withSnapshots))
	require.Equal(t, 1, withSnapshots)
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_run_events WHERE run_id = $1`, live).Scan(&events))
	require.Equal(t, 1, events)
}

func upper(id uuid.UUID) string { return strings.ToUpper(id.String()) }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
