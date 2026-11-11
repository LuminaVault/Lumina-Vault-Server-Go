-- name: Username :one
SELECT username FROM users WHERE id = $1;

-- name: Settings :one
SELECT * FROM self_improvement_settings WHERE tenant_id = $1 LIMIT 1;

-- ImprovementSettings(tenantID:) saved: the safe defaults, due in 168 hours.
-- name: InsertSettings :one
INSERT INTO self_improvement_settings
    (id, tenant_id, enabled, curator_enabled, interval_hours, minimum_idle_hours, consolidate, prune_builtins,
     backup_keep, soul_review_enabled, review_complex_sessions, soul_review_window_days, soul_review_cooldown_hours,
     model_mode, next_review_at, created_at, updated_at)
VALUES ($1, $2, TRUE, TRUE, 168, 2, TRUE, FALSE, 5, TRUE, TRUE, 14, 24, 'economy', $3, $4, $4)
RETURNING *;

-- name: UpdateSettings :one
UPDATE self_improvement_settings
SET enabled = $2, curator_enabled = $3, interval_hours = $4, minimum_idle_hours = $5, consolidate = $6,
    prune_builtins = $7, backup_keep = $8, soul_review_enabled = $9, review_complex_sessions = $10,
    soul_review_window_days = $11, soul_review_cooldown_hours = $12, model_mode = $13,
    next_review_at = $14, updated_at = $15
WHERE id = $1
RETURNING *;

-- name: CountPendingChanges :one
SELECT count(*) FROM self_improvement_changes WHERE tenant_id = $1 AND state = 'pending';

-- name: ActiveRun :one
SELECT * FROM self_improvement_runs
WHERE tenant_id = $1 AND kind = $2 AND (status = 'queued' OR status = 'running')
ORDER BY created_at DESC
LIMIT 1;

-- name: InsertRun :one
INSERT INTO self_improvement_runs
    (id, tenant_id, kind, status, trigger, dry_run, actions_applied, actions_skipped, created_at)
VALUES ($1, $2, $3, 'queued', $4, $5, 0, 0, $6)
RETURNING *;

-- name: ListRuns :many
SELECT * FROM self_improvement_runs WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT $2;

-- name: Run :one
SELECT * FROM self_improvement_runs WHERE id = $1 AND tenant_id = $2 LIMIT 1;

-- name: SetRunStatus :one
UPDATE self_improvement_runs SET status = $2 WHERE id = $1 RETURNING *;

-- name: ListChanges :many
SELECT * FROM self_improvement_changes WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 100;

-- name: Change :one
SELECT * FROM self_improvement_changes WHERE id = $1 AND tenant_id = $2 LIMIT 1;

-- name: UpdateChange :one
UPDATE self_improvement_changes
SET state = $2, failure_reason = $3, decided_at = $4, applied_at = $5
WHERE id = $1
RETURNING *;

-- name: VaultSkillStates :many
SELECT name, curator_pinned, curator_state, curator_last_activity_at
FROM skills_state WHERE tenant_id = $1 AND source = 'vault';

-- name: VaultSkillState :one
SELECT name, curator_pinned, curator_state, curator_last_activity_at
FROM skills_state WHERE tenant_id = $1 AND source = 'vault' AND name = $2 LIMIT 1;

-- SkillsState(tenantID:source:name:) saved.
-- name: InsertSkillState :exec
INSERT INTO skills_state (tenant_id, source, name, enabled, curator_pinned, curator_state)
VALUES ($1, 'vault', $2, TRUE, $3, $4);

-- Not a typo. SkillsState's Fluent @ID is tenant_id alone, so Swift's save() on a
-- row it loaded updates every skills_state row the tenant has, whatever its name
-- or source.
-- name: SetTenantSkillsPinned :exec
UPDATE skills_state SET curator_pinned = $2 WHERE tenant_id = $1;

-- Also every row of the tenant, for the same reason.
-- name: SetTenantSkillsCuratorState :exec
UPDATE skills_state SET curator_state = $2, curator_archived_at = NULL WHERE tenant_id = $1;
