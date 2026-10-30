-- Devices (Swift: Sources/App/Devices/DeviceController.swift, model DeviceToken).

-- name: FindDeviceToken :one
SELECT id, token, platform, bundle_id FROM device_tokens
WHERE tenant_id = $1 AND token = $2
LIMIT 1;

-- name: TouchDeviceToken :exec
-- Fluent save of an existing row: platform, last_seen_at, bundle_id, updated_at.
UPDATE device_tokens
SET platform = $2, last_seen_at = $3, bundle_id = $4, updated_at = $3
WHERE id = $1;

-- name: InsertDeviceToken :exec
INSERT INTO device_tokens (id, tenant_id, token, platform, last_seen_at, bundle_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $5, $5);

-- name: DeleteDeviceTokens :exec
DELETE FROM device_tokens WHERE tenant_id = $1 AND token = $2;
