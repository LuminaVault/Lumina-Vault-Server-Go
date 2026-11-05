-- name: RoutingUser :one
SELECT id, tier, tier_override, privacy_no_cn_origin FROM users WHERE id = $1;

-- name: ListMemos :many
SELECT id, path, created_at FROM vault_files
WHERE tenant_id = $1 AND path LIKE 'memos/%'
ORDER BY created_at DESC
LIMIT 200;

-- name: RecordMemoryRetrieved :exec
INSERT INTO analytics_events (vault_id, actor_user_id, event_name, source, dimensions)
VALUES ($1, $2, 'memory_retrieved', 'server', '{}'::jsonb);
