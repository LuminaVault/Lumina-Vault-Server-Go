-- name: SpaceSlug :one
SELECT slug FROM spaces WHERE id = $1 AND tenant_id = $2 LIMIT 1;

-- name: EnabledPluginSlugs :many
SELECT plugin_slug FROM plugin_installs WHERE tenant_id = $1 AND status = 'enabled';
