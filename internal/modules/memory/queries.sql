-- Queries for /v1/memory (wave 4). Each one is the SQL Swift runs, from
-- LuminaVaultServer Sources/App/Memory/MemoryController.swift. The memories table's
-- reads and writes live in internal/memorystore; the graph's raw SQL (uuid arrays
-- and pgvector operators, as in MemoryGraphService.swift) is in graph.go.

-- name: UserTier :one
-- context.identity?.tierEnum, for the tiered rate limit.
SELECT tier FROM users WHERE id = $1;

-- name: SpaceByID :one
SELECT id, slug FROM spaces WHERE tenant_id = $1 AND id = $2 LIMIT 1;

-- name: SpaceIDBySlug :one
SELECT id FROM spaces WHERE tenant_id = $1 AND slug = $2 LIMIT 1;

-- name: InsertMemoryCreatedEvent :exec
INSERT INTO analytics_events (vault_id, actor_user_id, event_name, source, dimensions, idempotency_key)
VALUES ($1, $2, 'memory_created', 'server', '{}'::jsonb, $3)
ON CONFLICT (actor_user_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING;

-- name: InsertMemoryReviewedEvent :exec
INSERT INTO analytics_events (vault_id, actor_user_id, event_name, source, dimensions)
VALUES ($1, $2, 'memory_reviewed', 'server', '{}'::jsonb);

-- name: LocalSyncMemories :many
-- A row with no updated_at sorts last and fills the limit, then localSync drops it.
SELECT id, content, origin_kind, created_at, updated_at
FROM memories
WHERE tenant_id = @tenant_id
  AND (sqlc.narg('after')::timestamptz IS NULL
       OR updated_at > sqlc.narg('after')
       OR (updated_at = sqlc.narg('after') AND id > @after_id))
ORDER BY updated_at ASC, id ASC
LIMIT @row_limit;

-- name: LocalSyncTombstones :many
-- After a memory cursor, a tombstone at the same instant and id still counts.
SELECT id, memory_id, deleted_at
FROM memory_sync_tombstones
WHERE tenant_id = @tenant_id
  AND (sqlc.narg('after')::timestamptz IS NULL
       OR deleted_at > sqlc.narg('after')
       OR (deleted_at = sqlc.narg('after')
           AND (id > @after_id OR (@inclusive::bool AND id = @after_id))))
ORDER BY deleted_at ASC
LIMIT @row_limit;
