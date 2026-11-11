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

-- name: AppendAssistantMessage :exec
-- The line a confirm or cancel leaves in the conversation (Swift:
-- QuickAssistantActionsController.appendMessage). Written straight to the
-- transcript, so it is never indexed (ruling 28); the conversation moves up
-- the inbox, which sorts by updated_at. Columns as chat's insertMessage.
WITH inserted AS (
    INSERT INTO conversation_messages
        (id, conversation_id, role, content, source_memory_ids, parallel_execution_id, local_execution_id, tool_call_count, created_at)
    VALUES (@id, @conversation_id, 'assistant', @content, '{}'::uuid[], NULL, NULL, 0, NOW())
    RETURNING conversation_id
)
UPDATE conversations SET updated_at = NOW()
WHERE id = (SELECT conversation_id FROM inserted);
