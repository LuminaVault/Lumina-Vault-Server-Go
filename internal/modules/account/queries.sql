-- Account deletion (Swift: Sources/App/Account/AccountDeletionService.swift,
-- TenantDataEraser.swift).

-- name: UserPasswordHash :one
SELECT password_hash FROM users WHERE id = $1;

-- name: HermesProfileID :one
-- HermesProfile.query(tenantID:).first(): read before the cascade drops it.
SELECT hermes_profile_id FROM hermes_profiles WHERE tenant_id = $1 LIMIT 1;

-- TenantDataEraser.erasedTables, in Swift's order.

-- name: EraseAgentRooms :exec
DELETE FROM agent_rooms WHERE tenant_id = $1;

-- name: EraseDeviceCommandQueue :exec
DELETE FROM device_command_queue WHERE tenant_id = $1;

-- name: EraseIdempotencyKeys :exec
DELETE FROM idempotency_keys WHERE tenant_id = $1;

-- name: EraseEmbeddingUsage :exec
DELETE FROM embedding_usage WHERE tenant_id = $1;

-- name: EraseRetrievalTelemetryEvents :exec
DELETE FROM retrieval_telemetry_events WHERE tenant_id = $1;

-- name: EraseRetrievalLeakReports :exec
DELETE FROM retrieval_leak_reports WHERE tenant_id = $1;

-- name: EraseSpendBuckets :exec
-- Both spend-gate spellings, compared case-insensitively.
DELETE FROM workflow_spend_buckets
WHERE lower(scope_key) IN (sqlc.arg(tenant_key)::text, sqlc.arg(freelane_key)::text);

-- name: AnonymizeBillingEvents :exec
UPDATE billing_event_logs SET user_id = NULL WHERE user_id = sqlc.arg(user_id)::uuid;

-- name: AnonymizeVaultActivity :exec
UPDATE vault_activity_events SET actor_name = sqlc.arg(actor_name)::text
WHERE actor_user_id = sqlc.arg(actor_user_id)::uuid;

-- name: DeleteUser :exec
-- Every other per-tenant table cascades from here.
DELETE FROM users WHERE id = $1;
