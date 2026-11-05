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

-- name: IndexCounts :one
SELECT
    (SELECT COUNT(*) FROM vault_files v WHERE v.tenant_id = $1)::bigint AS documents,
    (SELECT COUNT(*) FROM memory_chunks c WHERE c.tenant_id = $1)::bigint AS chunks,
    (SELECT COUNT(*) FROM memories m WHERE m.tenant_id = $1)::bigint AS memories;

-- name: LastIndexedAt :one
SELECT MAX(created_at)::timestamptz FROM memory_chunks WHERE tenant_id = $1;

-- name: UnindexedPaths :many
SELECT v.path
FROM vault_files v
WHERE v.tenant_id = $1
  AND NOT EXISTS (
      SELECT 1 FROM memory_chunks c
      WHERE c.tenant_id = v.tenant_id AND c.vault_file_id = v.id
  )
ORDER BY v.path;

-- name: StalePaths :many
SELECT v.path
FROM vault_files v
JOIN (
    SELECT c.vault_file_id, MAX(c.created_at) AS indexed_at
    FROM memory_chunks c
    WHERE c.tenant_id = $1 AND c.vault_file_id IS NOT NULL
    GROUP BY c.vault_file_id
) c ON c.vault_file_id = v.id
WHERE v.tenant_id = $1
  AND v.processed_at IS NOT NULL
  AND v.processed_at > c.indexed_at
ORDER BY v.path;

-- name: HeadingRows :many
SELECT heading_path::text AS heading_path_json, MIN(start_line)::int AS start_line
FROM memory_chunks
WHERE tenant_id = $1 AND vault_file_id = $2
GROUP BY heading_path::text
ORDER BY MIN(start_line);

-- name: ChunkCount :one
SELECT COUNT(*) FROM memory_chunks WHERE tenant_id = $1 AND vault_file_id = $2;

-- name: Siblings :many
SELECT id, path FROM vault_files
WHERE tenant_id = $1 AND space_id = $2 AND id <> $3
ORDER BY path
LIMIT $4;

-- name: NeighboursByPath :many
SELECT id, path FROM vault_files
WHERE tenant_id = $1 AND path = ANY($2::text[])
ORDER BY path;

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
