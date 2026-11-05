-- name: User :one
SELECT id, email, username, tier, tier_expires_at, is_verified, vault_initialized, created_at, updated_at
FROM users WHERE id = $1;

-- name: UserTier :one
SELECT tier FROM users WHERE id = $1;

-- name: MarkInitialized :one
UPDATE users SET vault_initialized = TRUE, updated_at = $2 WHERE id = $1 RETURNING updated_at;

-- name: SpaceSlug :one
SELECT slug FROM spaces WHERE tenant_id = $1 AND id = $2 LIMIT 1;

-- name: SpaceIDBySlug :one
SELECT id FROM spaces WHERE tenant_id = $1 AND slug = $2 LIMIT 1;

-- name: DanglingLinks :many
SELECT source_path, source_line, raw_target, resolution_state
FROM vault_links
WHERE tenant_id = $1 AND resolution_state <> 'resolved'
ORDER BY source_path NULLS LAST, source_line;

-- name: AllPaths :many
SELECT path FROM vault_files WHERE tenant_id = $1 ORDER BY path;

-- name: LintFiles :many
SELECT path, size_bytes FROM vault_files
WHERE tenant_id = $1
ORDER BY path
LIMIT $2;

-- name: ExportMemories :many
SELECT id, content, tags, created_at FROM memories
WHERE tenant_id = $1 AND (sqlc.narg('since')::timestamptz IS NULL OR created_at >= sqlc.narg('since')::timestamptz)
ORDER BY created_at ASC, id ASC
OFFSET $2 LIMIT $3;
