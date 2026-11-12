package workflows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/jobs"
	"time"

	"github.com/google/uuid"
)

// SchedulerTick is WorkflowScheduler.tickInterval; MaintenanceInterval is
// WorkflowMaintenanceService.interval.
const (
	SchedulerTick       = 60 * time.Second
	MaintenanceInterval = time.Hour
	// snapshotRetention is the documented 30-day debugging window for payload
	// snapshots and run events.
	snapshotRetention = 30 * 24 * time.Hour
)

// SchedulerJob is WorkflowScheduler (Workflows/WorkflowScheduler.swift): once a
// minute, every enabled, published workflow whose published version has a schedule
// trigger gets a run when its cron matches the minute or its one-shot runAt has
// passed. The dedupe key names the workflow and the minute, so two processes
// evaluating the same minute enqueue once (the partial unique index decides).
func (m *Module) SchedulerJob(log *slog.Logger) jobs.Job {
	return jobs.Job{
		Name:     "WorkflowScheduler",
		Schedule: jobs.Every(SchedulerTick, 0),
		Run: func(ctx context.Context) error {
			if _, err := m.ScheduleTick(ctx, m.now(), log); err != nil {
				log.Warn(fmt.Sprintf("workflow.scheduler tick failed: %v", err))
				return err
			}
			return nil
		},
	}
}

type scheduledWorkflow struct {
	id, tenant uuid.UUID
	definition []byte
}

// ScheduleTick is WorkflowScheduler.tick(at:). It returns how many runs it enqueued.
func (m *Module) ScheduleTick(ctx context.Context, now time.Time, log *slog.Logger) (int, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT w.id, w.tenant_id, v.definition
		FROM workflows w
		JOIN workflow_versions v ON v.id = w.published_version_id
		WHERE w.enabled = true AND w.published_version_id IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	var candidates []scheduledWorkflow
	for rows.Next() {
		var w scheduledWorkflow
		if err := rows.Scan(&w.id, &w.tenant, &w.definition); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	enqueued := 0
	for _, w := range candidates {
		def, err := decodeDefinition(w.definition)
		if err != nil {
			return enqueued, err
		}
		if def.Trigger != Schedule {
			continue
		}
		oneShotDue := false
		if raw, ok := def.TriggerConfiguration[runAtKey]; ok {
			if at, err := time.Parse(time.RFC3339, raw); err == nil {
				oneShotDue = !now.Before(at)
			}
		}
		cronDue := false
		if raw, ok := def.TriggerConfiguration[cronKey]; ok {
			if cron, ok := parseCron(raw); ok {
				identifier, ok := def.TriggerConfiguration[timezoneKey]
				if !ok {
					identifier = "UTC"
				}
				cronDue = cron.matches(now, scheduleZone(identifier))
			}
		}
		if !oneShotDue && !cronDue {
			continue
		}
		key := fmt.Sprintf("schedule:%s:%d", ids.Upper(w.id), now.Unix()/60)
		if _, found, err := m.existingRun(ctx, w.tenant, w.id, &key); err != nil {
			return enqueued, err
		} else if found {
			continue
		}
		input := StringMap{"scheduledAt": now.UTC().Format("2006-01-02T15:04:05Z07:00")}
		_, err = m.enqueue(ctx, w.tenant, w.id, Schedule, input, nil, &key)
		switch {
		case errors.Is(err, errActiveRunLimit):
			log.Info("workflow.schedule deferred by active-run limit: " + ids.Upper(w.id))
			continue
		case errors.Is(err, errForbidden):
			log.Info("workflow.schedule skipped for inactive entitlement: " + ids.Upper(w.id))
			continue
		case err != nil:
			// A competing process may have won the unique key race.
			log.Debug("workflow.schedule duplicate suppressed: " + key)
			continue
		}
		enqueued++
		if oneShotDue {
			if _, err := m.pool.Exec(ctx,
				`UPDATE workflows SET enabled = false, updated_at = NOW() WHERE id = $1`, w.id); err != nil {
				return enqueued, err
			}
		}
	}
	return enqueued, nil
}

// MaintenanceJob is WorkflowMaintenanceService
// (Workflows/WorkflowMaintenanceService.swift): hourly, expire abandoned approvals
// (timing out the run they hold) and drop payload snapshots and run events past the
// 30-day window. Run metadata stays for history.
func (m *Module) MaintenanceJob(log *slog.Logger) jobs.Job {
	return jobs.Job{
		Name:     "WorkflowMaintenanceService",
		Schedule: jobs.Every(MaintenanceInterval, 0),
		Run: func(ctx context.Context) error {
			if err := m.MaintenanceTick(ctx, m.now(), log); err != nil {
				log.Warn(fmt.Sprintf("workflow.maintenance failed: %v", err))
				return err
			}
			return nil
		},
	}
}

// MaintenanceTick is WorkflowMaintenanceService.tick(at:).
func (m *Module) MaintenanceTick(ctx context.Context, now time.Time, log *slog.Logger) error {
	rows, err := m.pool.Query(ctx,
		`SELECT id, run_id FROM workflow_approvals WHERE status = 'pending' AND expires_at < $1`, now)
	if err != nil {
		return err
	}
	type approval struct{ id, run uuid.UUID }
	var expired []approval
	for rows.Next() {
		var a approval
		if err := rows.Scan(&a.id, &a.run); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range expired {
		if _, err := m.pool.Exec(ctx,
			`UPDATE workflow_approvals SET status = 'expired', updated_at = NOW() WHERE id = $1`, a.id); err != nil {
			return err
		}
		if _, err := m.pool.Exec(ctx, `
			UPDATE workflow_runs
			SET status = $2, error_message = 'Approval expired', ended_at = $3, updated_at = NOW()
			WHERE id = $1 AND status = $4`,
			a.run, string(WorkflowRunStatusTimedOut), now, string(WorkflowRunStatusWaitingForApproval)); err != nil {
			return err
		}
	}

	cutoff := now.Add(-snapshotRetention)
	// WorkflowEventStore.prune logs its own failure and never fails the tick.
	if _, err := m.pool.Exec(ctx, `DELETE FROM workflow_run_events WHERE created_at < $1`, cutoff); err != nil {
		log.Error("workflow event pruning failed", "error", err.Error())
	}
	_, err = m.pool.Exec(ctx, `
		UPDATE workflow_node_runs SET input_snapshot = NULL, output_snapshot = NULL
		WHERE created_at < $1 AND (input_snapshot IS NOT NULL OR output_snapshot IS NOT NULL)`, cutoff)
	return err
}
