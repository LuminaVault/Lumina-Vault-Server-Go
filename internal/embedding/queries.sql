-- Tables behind internal/embedding. The usage rows are EmbeddingUsageRepository
-- (Sources/App/Repositories), the install row PerTenantEmbeddingResolver.build.

-- name: EmbeddingUsageFor :one
SELECT id, tokens_used FROM embedding_usage
WHERE tenant_id = $1 AND year_month = $2
LIMIT 1;

-- name: SetEmbeddingUsage :exec
UPDATE embedding_usage SET tokens_used = $2, updated_at = $3 WHERE id = $1;

-- name: InsertEmbeddingUsage :exec
INSERT INTO embedding_usage (id, tenant_id, year_month, tokens_used, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $5);

-- name: EnabledPluginConfig :one
SELECT config_ciphertext, config_nonce FROM plugin_installs
WHERE tenant_id = $1 AND plugin_slug = $2 AND status = 'enabled'
LIMIT 1;
