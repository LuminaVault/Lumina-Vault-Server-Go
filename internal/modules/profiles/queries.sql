-- Profiles (Swift: Sources/App/Profiles/ProfilesController.swift, model
-- UserHermesProfile). Tenant scoping mirrors Fluent's TenantModel queries;
-- updates and deletes by id mirror Model.save / Model.delete.

-- name: ListProfiles :many
SELECT id, tenant_id, slug, label, system_prompt, is_default, skills_enabled, created_at, updated_at
FROM user_hermes_profiles
WHERE tenant_id = $1
ORDER BY created_at ASC;

-- name: GetProfile :one
SELECT id, tenant_id, slug, label, system_prompt, is_default, skills_enabled, created_at, updated_at
FROM user_hermes_profiles
WHERE tenant_id = $1 AND slug = $2
LIMIT 1;

-- name: CountProfiles :one
SELECT count(*) FROM user_hermes_profiles WHERE tenant_id = $1;

-- name: CreateProfile :exec
INSERT INTO user_hermes_profiles (id, tenant_id, slug, label, system_prompt, is_default, skills_enabled, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8);

-- name: UpdateProfile :exec
UPDATE user_hermes_profiles
SET label = $2, system_prompt = $3, skills_enabled = $4, updated_at = $5
WHERE id = $1;

-- name: DeleteProfile :exec
DELETE FROM user_hermes_profiles WHERE id = $1;

-- Fluent's query-builder update also stamps updated_at on every row it touches.
-- name: ClearDefault :exec
UPDATE user_hermes_profiles
SET is_default = false, updated_at = $2
WHERE tenant_id = $1 AND is_default = true;

-- name: SetDefault :exec
UPDATE user_hermes_profiles
SET is_default = true, updated_at = $2
WHERE id = $1;
