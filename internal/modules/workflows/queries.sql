-- name: UserTier :one
SELECT tier, tier_override FROM users WHERE id = $1;

-- name: ListWorkflows :many
SELECT * FROM workflows WHERE tenant_id = $1 ORDER BY updated_at DESC;

-- name: GetWorkflow :one
SELECT * FROM workflows WHERE tenant_id = $1 AND id = $2 LIMIT 1;

-- name: InsertWorkflow :exec
INSERT INTO workflows (id, tenant_id, name, description_text, enabled, draft_definition, draft_revision, is_legacy_job, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(tenant_id), sqlc.arg(name), sqlc.narg(description_text), TRUE, sqlc.arg(draft_definition), 1, FALSE, sqlc.arg(now), sqlc.arg(now));

-- name: UpdateDraft :exec
UPDATE workflows
SET name = sqlc.arg(name), description_text = sqlc.narg(description_text), enabled = sqlc.arg(enabled),
    draft_definition = sqlc.arg(draft_definition), draft_revision = sqlc.arg(draft_revision), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: SetPublishedVersion :exec
UPDATE workflows SET published_version_id = sqlc.arg(version_id), updated_at = sqlc.arg(updated_at) WHERE id = sqlc.arg(id);

-- name: RestoreDraft :exec
UPDATE workflows
SET draft_definition = sqlc.arg(draft_definition), draft_revision = sqlc.arg(draft_revision), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: LatestVersionNumber :one
SELECT version FROM workflow_versions WHERE tenant_id = $1 AND workflow_id = $2 ORDER BY version DESC LIMIT 1;

-- name: InsertVersion :exec
INSERT INTO workflow_versions (id, tenant_id, workflow_id, version, definition, created_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: VersionNumberByID :one
SELECT version FROM workflow_versions WHERE id = $1;

-- name: ListVersions :many
SELECT * FROM workflow_versions WHERE tenant_id = $1 AND workflow_id = $2 ORDER BY version DESC;

-- A path version is an Int: bound as bigint, so a value past int4 finds nothing
-- instead of failing to encode.
-- name: GetVersion :one
SELECT * FROM workflow_versions
WHERE tenant_id = sqlc.arg(tenant_id) AND workflow_id = sqlc.arg(workflow_id) AND version = sqlc.arg(version)::bigint
LIMIT 1;

-- name: LatestRun :one
SELECT status, started_at, created_at FROM workflow_runs
WHERE tenant_id = $1 AND workflow_id = $2 ORDER BY created_at DESC LIMIT 1;

-- name: CountPendingApprovals :one
SELECT count(*) FROM workflow_approvals WHERE tenant_id = $1 AND workflow_id = $2 AND status = 'pending';

-- name: ListRuns :many
SELECT * FROM workflow_runs WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 100;

-- name: ListWorkflowRuns :many
SELECT * FROM workflow_runs WHERE tenant_id = $1 AND workflow_id = $2 ORDER BY created_at DESC LIMIT 100;

-- name: GetRun :one
SELECT * FROM workflow_runs WHERE tenant_id = $1 AND id = $2 LIMIT 1;

-- name: RunByDedupeKey :one
SELECT * FROM workflow_runs WHERE tenant_id = $1 AND workflow_id = $2 AND dedupe_key = $3 LIMIT 1;

-- name: CountActiveRuns :one
SELECT count(*) FROM workflow_runs
WHERE tenant_id = $1 AND (status = 'queued' OR status = 'running' OR status = 'waitingForApproval');

-- name: InsertRun :exec
INSERT INTO workflow_runs (id, tenant_id, workflow_id, version_id, status, trigger_kind, input, conversation_id, dedupe_key,
    managed_spend_usd_micros, managed_spend_limit_usd_micros, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(tenant_id), sqlc.arg(workflow_id), sqlc.arg(version_id), sqlc.arg(status), sqlc.arg(trigger_kind), sqlc.arg(input),
    sqlc.narg(conversation_id), sqlc.narg(dedupe_key), 0, sqlc.arg(managed_spend_limit_usd_micros), sqlc.arg(now), sqlc.arg(now));

-- name: CancelRun :exec
UPDATE workflow_runs
SET status = 'cancelled', ended_at = sqlc.arg(now), lease_owner = NULL, lease_expires_at = NULL, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: CancelActiveNodeRuns :exec
UPDATE workflow_node_runs SET status = 'cancelled', ended_at = sqlc.arg(now)
WHERE run_id = sqlc.arg(run_id) AND (status = 'pending' OR status = 'running' OR status = 'waitingForApproval');

-- name: ResumeRun :exec
UPDATE workflow_runs
SET status = 'queued', pause_reason = NULL, error_message = NULL, lease_owner = NULL, lease_expires_at = NULL, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: NodeRuns :many
SELECT * FROM workflow_node_runs WHERE run_id = $1 ORDER BY created_at ASC;

-- name: NodeRunForApproval :one
SELECT id FROM workflow_node_runs WHERE run_id = $1 AND node_id = $2 LIMIT 1;

-- name: DecideNodeRun :exec
UPDATE workflow_node_runs SET status = sqlc.arg(status), output_snapshot = sqlc.arg(output_snapshot), ended_at = sqlc.arg(ended_at)
WHERE id = sqlc.arg(id);

-- name: PendingApprovals :many
SELECT * FROM workflow_approvals WHERE tenant_id = $1 AND status = 'pending' ORDER BY created_at DESC;

-- name: GetApproval :one
SELECT * FROM workflow_approvals WHERE tenant_id = $1 AND id = $2 LIMIT 1;

-- name: DecideApproval :exec
UPDATE workflow_approvals
SET status = sqlc.arg(status), decision_note = sqlc.narg(decision_note), memory_ids = sqlc.arg(memory_ids), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: DecideRun :exec
UPDATE workflow_runs
SET status = sqlc.arg(status), error_message = sqlc.narg(error_message), ended_at = COALESCE(sqlc.narg(ended_at), ended_at), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: AttachedMemories :many
SELECT id, content FROM memories WHERE tenant_id = sqlc.arg(tenant_id) AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: TenantSpend :many
SELECT period_kind, spent_usd_micros FROM workflow_spend_buckets
WHERE scope_key = sqlc.arg(scope_key)
  AND ((period_kind = 'day' AND period_start = sqlc.arg(day)::date)
    OR (period_kind = 'month' AND period_start = sqlc.arg(month)::date));

-- name: InsertEvent :exec
INSERT INTO workflow_run_events (tenant_id, run_id, kind, node_id, message, data)
VALUES (sqlc.arg(tenant_id), sqlc.arg(run_id), sqlc.arg(kind), sqlc.narg(node_id), sqlc.narg(message), sqlc.arg(data));

-- name: ListEvents :many
SELECT sequence, run_id, kind, node_id, message, data, created_at FROM workflow_run_events
WHERE tenant_id = $1 AND run_id = $2 AND sequence > $3
ORDER BY sequence ASC
LIMIT 250;

-- name: RunStatus :one
SELECT status FROM workflow_runs WHERE tenant_id = $1 AND id = $2 LIMIT 1;

-- name: GetWebhook :one
SELECT * FROM workflow_webhooks WHERE id = $1;

-- name: WebhookIDForWorkflow :one
SELECT id FROM workflow_webhooks WHERE tenant_id = $1 AND workflow_id = $2 LIMIT 1;

-- name: InsertWebhook :exec
INSERT INTO workflow_webhooks (id, tenant_id, workflow_id, secret_ciphertext, secret_nonce, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(tenant_id), sqlc.arg(workflow_id), sqlc.arg(secret_ciphertext), sqlc.arg(secret_nonce), sqlc.arg(now), sqlc.arg(now));

-- name: UpdateWebhookSecret :exec
UPDATE workflow_webhooks SET secret_ciphertext = sqlc.arg(secret_ciphertext), secret_nonce = sqlc.arg(secret_nonce), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);
