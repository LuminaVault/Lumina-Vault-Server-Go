-- Multimodal ingestion (Swift: Sources/App/Ingestion/MultimodalIngestionService.swift).
-- Every query is scoped to the resolved vault id, which Swift stores in tenant_id,
-- except the source lookup, which a token alone authorises. Updates set only the
-- columns Swift's model sets, as the worker writes the same rows.

-- name: GetBatch :one
SELECT * FROM ingestion_batches WHERE tenant_id = $1 AND id = $2 LIMIT 1;

-- name: ListBatches :many
SELECT * FROM ingestion_batches WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 50;

-- name: InsertBatch :exec
INSERT INTO ingestion_batches (id, tenant_id, space_id, state, created_at, updated_at)
VALUES ($1, $2, $3, 'active', $4, $4);

-- name: SetBatchState :exec
UPDATE ingestion_batches SET state = $3, updated_at = $4 WHERE tenant_id = $1 AND id = $2;

-- name: InsertItem :exec
INSERT INTO ingestion_items (id, tenant_id, batch_id, kind, state, file_name, content_type, size_bytes,
                             uploaded_bytes, expected_sha256, url, attempts, pipeline_version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 0, $9, $10, 0, 'multimodal-v2', $11, $11);

-- name: GetItem :one
SELECT * FROM ingestion_items WHERE tenant_id = $1 AND batch_id = $2 AND id = $3 LIMIT 1;

-- name: ListItems :many
SELECT * FROM ingestion_items WHERE tenant_id = $1 AND batch_id = $2 ORDER BY created_at ASC;

-- name: ItemStates :many
SELECT state FROM ingestion_items WHERE tenant_id = $1 AND batch_id = $2;

-- name: SetUploadedBytes :exec
UPDATE ingestion_items SET uploaded_bytes = $2, updated_at = $3 WHERE id = $1;

-- name: QueueCompletedItem :exec
UPDATE ingestion_items
SET vault_file_id = $2, content_sha256 = $3, state = 'queued', updated_at = $4
WHERE id = $1;

-- name: RetryItem :exec
UPDATE ingestion_items
SET state = 'queued', error_message = NULL, next_attempt_at = NULL, lease_expires_at = NULL,
    source_token_hash = NULL, source_token_expires_at = NULL, terminal_notified_at = NULL, updated_at = $2
WHERE id = $1;

-- name: CancelItem :exec
UPDATE ingestion_items
SET state = 'cancelled', error_message = NULL, next_attempt_at = NULL, lease_expires_at = NULL,
    source_token_hash = NULL, source_token_expires_at = NULL, updated_at = $2
WHERE id = $1;

-- name: InsertEvent :exec
INSERT INTO ingestion_events (tenant_id, batch_id, item_id, type, state, uploaded_bytes)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListEvents :many
SELECT id, batch_id, item_id, type, state, uploaded_bytes, created_at
FROM ingestion_events
WHERE tenant_id = $1 AND batch_id = $2 AND id > $3
ORDER BY id ASC LIMIT 200;

-- name: GraphEntities :many
SELECT DISTINCT n.id, n.label, n.confidence
FROM knowledge_nodes n JOIN knowledge_evidence e ON e.node_id = n.id
WHERE e.tenant_id = $1 AND e.memory_id = $2 AND n.kind = 'entity'
ORDER BY n.label LIMIT 100;

-- name: GraphEdges :many
SELECT DISTINCT ON (edge.id) edge.id, edge.from_node_id, edge.predicate, edge.to_node_id,
       edge.confidence, evidence.quote
FROM knowledge_edges edge JOIN knowledge_evidence evidence ON evidence.edge_id = edge.id
WHERE evidence.tenant_id = $1 AND evidence.memory_id = $2
ORDER BY edge.id, evidence.created_at LIMIT 200;

-- name: ItemByTokenHash :one
SELECT * FROM ingestion_items WHERE source_token_hash = $1 LIMIT 1;

-- name: StoredBytes :one
SELECT COALESCE(SUM(size_bytes), 0)::bigint AS total FROM vault_files WHERE tenant_id = $1;

-- name: UserTier :one
SELECT tier FROM users WHERE id = $1 LIMIT 1;

-- name: SpaceSlug :one
SELECT slug FROM spaces WHERE tenant_id = $1 AND id = $2 LIMIT 1;
