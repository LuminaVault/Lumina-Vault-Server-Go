-- Spaces (Swift: Sources/App/Spaces/SpacesService.swift). Every query is
-- scoped to the resolved vault id, which Swift stores in tenant_id.

-- name: ListSpaces :many
SELECT * FROM spaces WHERE tenant_id = $1 ORDER BY name ASC;

-- name: GetSpace :one
SELECT * FROM spaces WHERE tenant_id = $1 AND id = $2 LIMIT 1;

-- name: SlugTaken :one
SELECT EXISTS (SELECT 1 FROM spaces WHERE tenant_id = $1 AND slug = $2);

-- name: CreateSpace :one
INSERT INTO spaces (id, tenant_id, name, slug, description, color, icon, category, note_count,
                    created_by_user_id, updated_by_user_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 0, $9, $9, $10, $10)
RETURNING *;

-- name: UpdateSpace :one
-- note_count is written too: Swift's update saves the model after the live
-- count has been stamped onto it.
UPDATE spaces
SET name = $3, description = $4, color = $5, icon = $6, category = $7,
    note_count = $8, updated_by_user_id = $9, updated_at = $10
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: DeleteSpace :exec
DELETE FROM spaces WHERE tenant_id = $1 AND id = $2;

-- name: NoteCounts :many
-- Live note counts (HER-105): Swift counts vault_files on read.
SELECT space_id::uuid AS space_id, COUNT(*)::int AS n
FROM vault_files
WHERE tenant_id = $1 AND space_id = ANY(@space_ids::uuid[])
GROUP BY space_id;
