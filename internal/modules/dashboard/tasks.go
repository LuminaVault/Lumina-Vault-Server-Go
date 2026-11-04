package dashboard

import (
	"context"
	"lumina/internal/auth"
	"lumina/internal/modules/dashboard/db"
	"lumina/internal/wire"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

const (
	activePreviewLimit = 5
	tasksMaxLimit      = 100
	tasksDefaultLimit  = 50
)

// taskRank is ActiveTasksQuery.rank: running first, then queued. No other state
// is produced by the active query.
func taskRank(state string) int {
	if state == "running" {
		return 0
	}
	return 1
}

// parseTaskState is TaskState(rawValue:): an unknown state means no filter.
func parseTaskState(raw string) string {
	switch raw {
	case "running", "queued", "completed", "failed":
		return raw
	}
	return ""
}

// tasks is TasksController.list.
func (m *Module) tasks(c *echo.Context) error {
	out, err := m.activeTasks(requestContext(c), auth.UserID(c), parseTaskState(query(c, "state")),
		clampedLimit(query(c, "limit"), tasksDefaultLimit, tasksMaxLimit))
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, TaskListResponse{Tasks: out})
}

// activeTasks is ActiveTasksQuery.list. Quirk kept: each source is capped at
// limit before the state filter, so `state=queued&limit=2` can come back empty
// when two running workflows fill the cap.
func (m *Module) activeTasks(ctx context.Context, tenant uuid.UUID, state string, limit int) ([]TaskDTO, error) {
	capped := int32(max(1, min(limit, tasksMaxLimit))) //nolint:gosec // clamped to 1..100
	runs, err := m.q.ActiveWorkflowRuns(ctx, db.ActiveWorkflowRunsParams{TenantID: tenant, RowLimit: capped})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tasks := make([]TaskDTO, 0, len(runs))
	for _, r := range runs {
		started := r.CreatedAt
		if r.StartedAt != nil {
			started = *r.StartedAt
		}
		taskState := "queued"
		if r.Status == "running" {
			taskState = "running"
		}
		tasks = append(tasks, newTask(r.ID, "workflow", r.WorkflowName, taskState, started, r.ErrorMessage, now))
	}
	jobs, err := m.q.RunningGatewayApplyJobs(ctx, db.RunningGatewayApplyJobsParams{TenantID: tenant, RowLimit: capped})
	if err != nil {
		return nil, err
	}
	now = time.Now()
	for _, j := range jobs {
		tasks = append(tasks, newTask(j.ID, "gateway_apply", "Applying gateway config", "running", j.CreatedAt, j.ErrorMessage, now))
	}

	if state != "" {
		kept := tasks[:0]
		for _, t := range tasks {
			if t.State == state {
				kept = append(kept, t)
			}
		}
		tasks = kept
	}
	// Running first, then queued; newest first within each band.
	sort.SliceStable(tasks, func(i, j int) bool {
		if ri, rj := taskRank(tasks[i].State), taskRank(tasks[j].State); ri != rj {
			return ri < rj
		}
		return tasks[i].StartedAt.After(tasks[j].StartedAt.Time)
	})
	if len(tasks) > int(capped) {
		tasks = tasks[:capped]
	}
	return tasks, nil
}

func newTask(id uuid.UUID, kind, label, state string, started time.Time, taskErr *string, now time.Time) TaskDTO {
	elapsed := int(now.Sub(started).Seconds())
	return TaskDTO{
		ID: wire.UUID{UUID: id}, Kind: kind, Label: label, State: state,
		StartedAt: &wire.Time{Time: started}, ElapsedSeconds: &elapsed, Error: taskErr,
	}
}

// activeTasksCount is ActiveTasksQuery.count.
func (m *Module) activeTasksCount(ctx context.Context, tenant uuid.UUID) (int, error) {
	workflows, err := m.q.CountActiveWorkflowRuns(ctx, tenant)
	if err != nil {
		return 0, err
	}
	applies, err := m.q.CountRunningGatewayApplyJobs(ctx, tenant)
	if err != nil {
		return 0, err
	}
	return int(workflows) + int(applies), nil
}
