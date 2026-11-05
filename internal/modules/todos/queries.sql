-- name: ProjectExists :one
SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND tenant_id = $2);
