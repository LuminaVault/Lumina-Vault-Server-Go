-- name: InsertSession :exec
INSERT INTO import_sessions (id, tenant_id, source_type, status, total_items, staged_items, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, 0, $6, $6);

-- name: FindSession :one
SELECT id, source_type, status, total_items, staged_items FROM import_sessions
WHERE tenant_id = $1 AND id = $2
LIMIT 1;

-- name: SetSessionStatus :exec
UPDATE import_sessions SET status = $2, updated_at = $3 WHERE id = $1;

-- name: FinishStaging :exec
UPDATE import_sessions SET status = $2, staged_items = $3, updated_at = $4 WHERE id = $1;

-- name: PriorURLs :many
SELECT url FROM import_items WHERE tenant_id = $1;

-- name: InsertItem :exec
INSERT INTO import_items (id, tenant_id, session_id, vault_file_id, url, title, status, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ActiveItems :many
SELECT id, vault_file_id, url, proposed_space FROM import_items
WHERE tenant_id = $1 AND session_id = $2 AND status <> 'skipped';

-- name: SessionItems :many
SELECT id, url, title, proposed_space, status FROM import_items
WHERE tenant_id = $1 AND session_id = $2
ORDER BY created_at ASC;

-- name: SetItemFiled :exec
UPDATE import_items SET status = 'filed' WHERE id = $1;

-- name: SetItemCategorized :exec
UPDATE import_items SET proposed_space = $2, status = 'categorized' WHERE id = $1;

-- name: SpaceIDBySlug :one
SELECT id FROM spaces WHERE tenant_id = $1 AND slug = $2 LIMIT 1;

-- name: TenantSpaces :many
SELECT slug, name, description FROM spaces WHERE tenant_id = $1;
