-- Sessions (Swift: Sources/App/Sessions/SessionsController.swift). The same
-- SQL as Swift's two raw queries; a NULL workspace means every space.

-- name: ListSessions :many
SELECT c.id, c.title, c.updated_at, c.space_id,
       COALESCE(m.cnt, 0)::int AS message_count,
       -- Swift decodes NULL and maps it to ""; COALESCE does the same here.
       COALESCE((
          SELECT content
          FROM conversation_messages
          WHERE conversation_id = c.id
          ORDER BY created_at DESC
          LIMIT 1
       ), '')::text AS last_preview
FROM conversations c
LEFT JOIN (
    SELECT conversation_id, COUNT(*) AS cnt
    FROM conversation_messages
    GROUP BY conversation_id
) m ON m.conversation_id = c.id
WHERE c.tenant_id = sqlc.arg(tenant_id)
  AND (sqlc.narg(workspace_id)::uuid IS NULL OR c.space_id = sqlc.narg(workspace_id)::uuid)
ORDER BY c.updated_at DESC
LIMIT sqlc.arg(row_limit);
