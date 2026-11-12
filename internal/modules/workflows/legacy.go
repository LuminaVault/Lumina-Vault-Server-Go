package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/cronexpr"
	"lumina/internal/jobs"
	"lumina/internal/skills"
	"lumina/internal/wire"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LegacyMigratorInterval is LegacyJobWorkflowMigrator.interval.
const LegacyMigratorInterval = 5 * time.Minute

// LegacyMigratorJob is LegacyJobWorkflowMigrator
// (Workflows/LegacyJobWorkflowMigrator.swift): every five minutes, each scheduled
// skill a tenant has (catalog manifest plus skills_state) that isn't linked to a
// workflow yet becomes a published "Job · <name>" workflow (schedule trigger ->
// skill -> output), and its skills_state row is linked to it. The link makes
// CronScheduler leave the job to WorkflowScheduler. Idempotent: an existing legacy
// workflow for the skill is linked rather than created again.
func (m *Module) LegacyMigratorJob(catalog *skills.Catalog, log *slog.Logger) jobs.Job {
	return jobs.Job{
		Name:     "LegacyJobWorkflowMigrator",
		Schedule: jobs.Every(LegacyMigratorInterval, 0),
		Run: func(ctx context.Context) error {
			if _, err := m.MigrateLegacyJobs(ctx, catalog); err != nil {
				log.Warn(fmt.Sprintf("workflow legacy migration failed: %v", err))
				return err
			}
			return nil
		},
	}
}

// MigrateLegacyJobs is migrate(): the number of workflows it created.
func (m *Module) MigrateLegacyJobs(ctx context.Context, catalog *skills.Catalog) (int, error) {
	rows, err := m.pool.Query(ctx, `SELECT id, timezone FROM users`)
	if err != nil {
		return 0, err
	}
	type user struct {
		id       uuid.UUID
		timezone string
	}
	var users []user
	for rows.Next() {
		var u user
		if err := rows.Scan(&u.id, &u.timezone); err != nil {
			rows.Close()
			return 0, err
		}
		users = append(users, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	count := 0
	for _, u := range users {
		for _, mf := range catalog.Manifests(u.id) {
			var scheduleOverride *string
			var workflowID *uuid.UUID
			err := m.pool.QueryRow(ctx, `
				SELECT schedule_override, workflow_id FROM skills_state
				WHERE tenant_id = $1 AND source = $2 AND name = $3`, u.id, string(mf.Source), mf.Name).
				Scan(&scheduleOverride, &workflowID)
			hasRow := err == nil
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return count, err
			}
			if hasRow && workflowID != nil {
				continue
			}
			cron := mf.Schedule
			if hasRow && scheduleOverride != nil {
				cron = scheduleOverride
			}
			if cron == nil {
				continue
			}
			if _, ok := cronexpr.Parse(*cron); !ok {
				continue
			}
			var existing uuid.UUID
			err = m.pool.QueryRow(ctx, `
				SELECT id FROM workflows WHERE tenant_id = $1 AND legacy_skill_name = $2 LIMIT 1`, u.id, mf.Name).Scan(&existing)
			switch {
			case err == nil:
				if err := m.linkLegacy(ctx, u.id, mf, existing); err != nil {
					return count, err
				}
				continue
			case !errors.Is(err, pgx.ErrNoRows):
				return count, err
			}
			id, err := m.createLegacyWorkflow(ctx, u.id, u.timezone, mf, *cron)
			if err != nil {
				return count, err
			}
			if err := m.linkLegacy(ctx, u.id, mf, id); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}

// legacyDefinition is the three-node workflow Swift builds for a job.
func legacyDefinition(mf skills.Manifest, cron, timezone string) WorkflowDefinitionDTO {
	version := 1
	trigger := WorkflowNodeDTO{Id: wire.UUID{UUID: uuid.New()}, Kind: Trigger, Name: "Schedule", X: 0, Y: 120, Configuration: StringMap{}}
	skill := WorkflowNodeDTO{
		Id: wire.UUID{UUID: uuid.New()}, Kind: Skill, Name: mf.Name, X: 260, Y: 120,
		Configuration: StringMap{"skillName": mf.Name},
	}
	output := WorkflowNodeDTO{
		Id: wire.UUID{UUID: uuid.New()}, Kind: Output, Name: "Output", X: 520, Y: 120,
		Configuration: StringMap{"value": "{{nodes." + skill.Id.String() + ".text}}"},
	}
	edge := func(from, to WorkflowNodeDTO) WorkflowEdgeDTO {
		return WorkflowEdgeDTO{Id: wire.UUID{UUID: uuid.New()}, SourceNodeID: from.Id, SourcePort: "output", TargetNodeID: to.Id, TargetPort: "input"}
	}
	return WorkflowDefinitionDTO{
		SchemaVersion: &version, Trigger: Schedule,
		TriggerConfiguration: StringMap{"cron": cron, "timezone": timezone},
		Nodes:                []WorkflowNodeDTO{trigger, skill, output},
		Edges:                []WorkflowEdgeDTO{edge(trigger, skill), edge(skill, output)},
	}
}

func (m *Module) createLegacyWorkflow(ctx context.Context, tenant uuid.UUID, timezone string, mf skills.Manifest, cron string) (uuid.UUID, error) {
	encoded, err := json.Marshal(legacyDefinition(mf, cron, timezone))
	if err != nil {
		return uuid.Nil, err
	}
	now := m.now()
	id, versionID := uuid.New(), uuid.New()
	description := &mf.Description // Swift passes manifest.description as-is
	if _, err := m.pool.Exec(ctx, `
		INSERT INTO workflows (id, tenant_id, name, description_text, enabled, draft_definition, draft_revision,
			is_legacy_job, legacy_skill_name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, true, $5, 1, true, $6, $7, $7)`,
		id, tenant, "Job · "+mf.Name, description, encoded, mf.Name, now); err != nil {
		return uuid.Nil, err
	}
	if _, err := m.pool.Exec(ctx, `
		INSERT INTO workflow_versions (id, tenant_id, workflow_id, version, definition, created_at)
		VALUES ($1, $2, $3, 1, $4, $5)`, versionID, tenant, id, encoded, now); err != nil {
		return uuid.Nil, err
	}
	if _, err := m.pool.Exec(ctx,
		`UPDATE workflows SET published_version_id = $2, updated_at = $3 WHERE id = $1`, id, versionID, now); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// linkLegacy is link(state:...): the skills_state row, created if missing, points
// at the workflow.
func (m *Module) linkLegacy(ctx context.Context, tenant uuid.UUID, mf skills.Manifest, workflow uuid.UUID) error {
	_, err := m.pool.Exec(ctx, `
		INSERT INTO skills_state (tenant_id, source, name, workflow_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, source, name) DO UPDATE SET workflow_id = EXCLUDED.workflow_id`,
		tenant, string(mf.Source), mf.Name, workflow)
	return err
}
