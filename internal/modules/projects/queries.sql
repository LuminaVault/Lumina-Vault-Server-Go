-- Projects (Swift: Sources/App/Projects/ProjectsController.swift). Every query
-- is tenant-scoped, as Fluent's TenantModel queries are.

-- name: ListProjects :many
SELECT id, tenant_id, name, description, archived, created_at
FROM projects
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: CreateProject :one
INSERT INTO projects (id, tenant_id, name, description, archived, created_at)
VALUES ($1, $2, $3, $4, false, $5)
RETURNING id, tenant_id, name, description, archived, created_at;

-- name: GetProject :one
SELECT id, tenant_id, name, description, archived, created_at
FROM projects
WHERE id = $1 AND tenant_id = $2;

-- name: UpdateProject :one
UPDATE projects
SET name = $3, description = $4, archived = $5
WHERE id = $1 AND tenant_id = $2
RETURNING id, tenant_id, name, description, archived, created_at;

-- name: DeleteProject :execrows
DELETE FROM projects WHERE id = $1 AND tenant_id = $2;

-- name: TenantFileMetadata :many
-- todoCount is computed in Go from the JSON metadata, as Swift does in-process.
SELECT metadata FROM vault_files WHERE tenant_id = $1 AND metadata IS NOT NULL;
