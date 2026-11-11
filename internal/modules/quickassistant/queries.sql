-- Quick Assistant pending actions (Swift: AssistantPendingActionStore in
-- Sources/App/Models/AssistantPendingAction.swift). Every transition out of
-- 'pending' also clears args and summary, in the same statement.

-- name: InsertPendingAction :exec
INSERT INTO assistant_pending_actions
    (id, tenant_id, vault_id, conversation_id, tool, args, summary, status, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', $8, NOW());

-- name: ClaimPendingAction :one
-- RETURNING sees the row after the UPDATE, whose args are already cleared, so
-- the args come from the locked row the CTE read first. A second caller waits
-- on that lock, then finds the row no longer pending.
WITH claimed AS (
    SELECT p.id, p.args FROM assistant_pending_actions AS p
    WHERE p.id = $1 AND p.tenant_id = $2
      AND p.status = 'pending' AND p.expires_at > NOW()
    FOR UPDATE
)
UPDATE assistant_pending_actions AS a
SET status = 'confirmed', args = '{}'::jsonb, summary = ''
FROM claimed
WHERE a.id = claimed.id
RETURNING a.tool, a.vault_id, a.conversation_id, claimed.args AS claimed_args;

-- name: CancelPendingAction :one
UPDATE assistant_pending_actions SET status = 'cancelled', args = '{}'::jsonb, summary = ''
WHERE id = $1 AND tenant_id = $2 AND status = 'pending'
RETURNING conversation_id;

-- name: MarkPendingActionFailed :exec
UPDATE assistant_pending_actions SET status = 'failed', args = '{}'::jsonb, summary = ''
WHERE id = $1 AND status = 'confirmed';

-- name: DeleteLongExpiredPendingActions :exec
DELETE FROM assistant_pending_actions
WHERE tenant_id = $1 AND expires_at < NOW() - INTERVAL '1 day';

-- name: PendingActionStatus :one
SELECT status FROM assistant_pending_actions
WHERE id = $1 AND tenant_id = $2;

-- Vault tool reads (Swift: MCPService, Fluent queries scoped by tenant_id).

-- name: SpacesForVault :many
SELECT id, slug FROM spaces WHERE tenant_id = $1;

-- name: ExistingMemory :one
-- memory_save's duplicate check: the same text, unless the user rejected it.
SELECT id FROM memories
WHERE tenant_id = $1 AND content = $2 AND review_state <> 'rejected'
LIMIT 1;

-- name: ContextMemories :many
SELECT id, kind, content, created_at FROM memories
WHERE tenant_id = sqlc.arg(tenant_id)
  AND kind IN ('decision', 'preference')
  AND review_state <> 'rejected'
  AND (sqlc.narg(space_id)::uuid IS NULL OR space_id = sqlc.narg(space_id))
ORDER BY created_at DESC
LIMIT sqlc.arg(row_limit);

-- name: BrowseFiles :many
-- With a prefix, the half-open range [prefix, prefix + U+FFFF), as Swift's.
-- Compared bytewise: under a libc collation such as en_US.utf8, U+FFFF has no
-- weight, so the range is empty and the prefix would match nothing.
SELECT id, path, size_bytes FROM vault_files
WHERE tenant_id = sqlc.arg(tenant_id)
  AND (sqlc.narg(low)::text IS NULL
       OR (path COLLATE "C" >= sqlc.narg(low)::text AND path COLLATE "C" < sqlc.narg(high)::text))
ORDER BY path
LIMIT sqlc.arg(row_limit);

-- name: RecentFiles :many
SELECT path, updated_at FROM vault_files
WHERE tenant_id = $1
ORDER BY updated_at DESC
LIMIT $2;

-- name: FileAtPath :one
SELECT path FROM vault_files
WHERE tenant_id = $1 AND path = $2
LIMIT 1;

-- Personal reads (Swift: PersonalDataTools.swift), always on the actor's own
-- account.

-- name: HealthDaily :many
-- Daily aggregates; Swift reads a missing sum or average as 0.
SELECT event_type, unit, date_trunc('day', recorded_at)::timestamptz AS day,
       COALESCE(SUM(value_numeric), 0)::float8 AS total,
       COALESCE(AVG(value_numeric), 0)::float8 AS avg
FROM health_events
WHERE tenant_id = sqlc.arg(tenant_id)
  AND (sqlc.narg(metric)::text IS NULL OR event_type = sqlc.narg(metric)::text)
  AND recorded_at >= NOW() - (sqlc.arg(days)::int * INTERVAL '1 day')
GROUP BY event_type, unit, day ORDER BY day;

-- name: CalendarWindow :many
SELECT title, starts_at, ends_at, location
FROM calendar_events
WHERE tenant_id = sqlc.arg(tenant_id)
  AND status <> 'cancelled'
  AND starts_at >= NOW()
  AND starts_at < NOW() + (sqlc.arg(days)::int * INTERVAL '1 day')
ORDER BY starts_at ASC;

-- name: OpenAppleReminders :many
SELECT title, due_at, notes
FROM apple_reminders
WHERE tenant_id = $1 AND completed = false
ORDER BY due_at ASC NULLS LAST
LIMIT $2;
