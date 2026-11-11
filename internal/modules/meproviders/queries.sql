-- me-providers: user_provider_credentials and its pool keys (UserCredentialStore's
-- write half), user_llm_preferences (UserLLMPreferenceRepository.upsert), and the
-- rows ConnectionsController reads and writes.

-- name: ListCredentials :many
SELECT id, provider, credential_kind, ciphertext, nonce, base_url, label, verified_at,
       last_failure_at, last_failure_code, supports_tools, tools_probed_at
FROM user_provider_credentials WHERE tenant_id = $1;

-- name: GetCredential :one
SELECT id, provider, credential_kind, ciphertext, nonce, base_url, label, verified_at,
       last_failure_at, last_failure_code, supports_tools, tools_probed_at
FROM user_provider_credentials WHERE tenant_id = $1 AND provider = $2 LIMIT 1;

-- name: InsertCredential :exec
INSERT INTO user_provider_credentials
    (id, tenant_id, provider, credential_kind, ciphertext, nonce, base_url, label,
     verified_at, last_failure_at, last_failure_code, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULL, NULL, NULL, $9, $9);

-- name: UpdateCredential :exec
UPDATE user_provider_credentials
SET credential_kind = $2, ciphertext = $3, nonce = $4, base_url = $5, label = $6,
    verified_at = NULL, last_failure_at = NULL, last_failure_code = NULL, updated_at = $7
WHERE id = $1;

-- name: DeleteCredential :exec
DELETE FROM user_provider_credentials WHERE tenant_id = $1 AND provider = $2;

-- name: RecordSuccess :execrows
UPDATE user_provider_credentials
SET verified_at = $3, last_failure_at = NULL, last_failure_code = NULL, updated_at = $3
WHERE tenant_id = $1 AND provider = $2;

-- name: RecordFailure :execrows
UPDATE user_provider_credentials
SET last_failure_at = $3, last_failure_code = $4, updated_at = $3
WHERE tenant_id = $1 AND provider = $2;

-- name: RecordToolSupport :execrows
UPDATE user_provider_credentials
SET supports_tools = $3, tools_probed_at = $4, updated_at = $4
WHERE tenant_id = $1 AND provider = $2;

-- name: ListPoolKeys :many
SELECT id, label, created_at FROM user_provider_credential_pool_keys
WHERE tenant_id = $1 AND provider = $2 ORDER BY created_at;

-- name: InsertPoolKey :exec
INSERT INTO user_provider_credential_pool_keys (id, tenant_id, provider, ciphertext, nonce, label, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: DeletePoolKey :exec
DELETE FROM user_provider_credential_pool_keys WHERE tenant_id = $1 AND provider = $2 AND id = $3;

-- name: GetPreferenceID :one
SELECT id FROM user_llm_preferences WHERE tenant_id = $1 LIMIT 1;

-- name: InsertPreference :exec
INSERT INTO user_llm_preferences
    (id, tenant_id, mode, primary_provider, primary_model, fallback_chain, allowed_providers, blocked_providers, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9);

-- name: UpdatePreference :exec
UPDATE user_llm_preferences
SET mode = $2, primary_provider = $3, primary_model = $4, fallback_chain = $5,
    allowed_providers = $6, blocked_providers = $7, updated_at = $8
WHERE id = $1;

-- name: UserTier :one
SELECT tier, tier_override FROM users WHERE id = $1;

-- name: ListGateways :many
SELECT gateway_id, status, verified_at, last_failure_at, last_failure_code
FROM user_hermes_gateways WHERE tenant_id = $1;

-- name: GetHermesConfig :one
SELECT name, base_url, verified_at FROM user_hermes_config WHERE tenant_id = $1 LIMIT 1;

-- name: GetGoogleCalendarAccount :one
SELECT scope, status, account_email, last_synced_at, last_failure_code
FROM calendar_accounts WHERE tenant_id = $1 AND provider = 'google' LIMIT 1;

-- name: GetHermesContainer :one
SELECT xai_connected_at, nous_connected_at FROM hermes_tenant_containers WHERE tenant_id = $1 LIMIT 1;

-- name: ListPluginInstalls :many
SELECT plugin_slug, status, last_sync_at FROM plugin_installs WHERE tenant_id = $1;

-- name: InsertDiagnosticEvent :exec
INSERT INTO connection_diagnostic_events
    (id, tenant_id, occurred_at, kind, connection_id, connection_title, severity, message, code, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: ListDiagnosticEvents :many
SELECT id, occurred_at, kind, connection_id, connection_title, severity, message, code
FROM connection_diagnostic_events WHERE tenant_id = $1
ORDER BY occurred_at DESC LIMIT $2;
