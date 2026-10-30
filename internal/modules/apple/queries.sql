-- Apple (Swift: Sources/App/Apple/*). Raw SQL mirrors the Swift sql.raw
-- statements; Fluent model writes are reproduced column for column.

-- name: ConsentRows :many
SELECT domain, allowed, allow_writes, last_sync_at
FROM apple_consent WHERE tenant_id = $1;

-- name: ConsentFor :one
SELECT allowed, allow_writes FROM apple_consent
WHERE tenant_id = $1 AND domain = $2;

-- name: UpsertConsent :exec
INSERT INTO apple_consent (tenant_id, domain, allowed, allow_writes, updated_at)
VALUES ($1, $2, $3, $4, NOW())
ON CONFLICT (tenant_id, domain) DO UPDATE
  SET allowed = EXCLUDED.allowed,
      allow_writes = EXCLUDED.allow_writes,
      updated_at = NOW();

-- name: PurgePhotoIndex :exec
DELETE FROM photo_index WHERE tenant_id = $1;

-- name: ClearLastKnownLocation :exec
UPDATE users
SET last_location_lat = NULL, last_location_lng = NULL,
    last_location_place = NULL, last_location_at = NULL
WHERE id = $1;

-- name: UpsertAppleReminder :one
-- (xmax = 0) is true for a fresh INSERT; no row = the WHERE rejected a stale delta.
INSERT INTO apple_reminders
  (id, tenant_id, external_id, title, notes, due_at, completed,
   completed_at, list_name, priority, remote_updated_at,
   created_at, updated_at)
VALUES
  (gen_random_uuid(), sqlc.arg(tenant_id), sqlc.arg(external_id),
   sqlc.arg(title), sqlc.narg(notes), sqlc.narg(due_at),
   sqlc.arg(completed), sqlc.narg(completed_at),
   sqlc.narg(list_name), sqlc.narg(priority),
   sqlc.narg(remote_updated_at), NOW(), NOW())
ON CONFLICT (tenant_id, external_id) DO UPDATE
  SET title = EXCLUDED.title,
      notes = EXCLUDED.notes,
      due_at = EXCLUDED.due_at,
      completed = EXCLUDED.completed,
      completed_at = EXCLUDED.completed_at,
      list_name = EXCLUDED.list_name,
      priority = EXCLUDED.priority,
      remote_updated_at = EXCLUDED.remote_updated_at,
      updated_at = NOW()
  WHERE COALESCE(EXCLUDED.remote_updated_at, 'epoch'::timestamptz)
        >= COALESCE(apple_reminders.remote_updated_at, 'epoch'::timestamptz)
RETURNING (xmax = 0)::boolean AS did_insert;

-- name: UpsertPhoto :one
INSERT INTO photo_index
    (id, tenant_id, asset_local_id, taken_at, is_screenshot, ocr_text, scene_tags, embedding, created_at, updated_at)
VALUES
    (sqlc.arg(id), sqlc.arg(tenant_id), sqlc.arg(asset_local_id), sqlc.narg(taken_at),
     sqlc.arg(is_screenshot), sqlc.narg(ocr_text),
     sqlc.narg(scene_tags)::text[], sqlc.narg(embedding)::text::vector, NOW(), NOW())
ON CONFLICT (tenant_id, asset_local_id) DO UPDATE
  SET taken_at = EXCLUDED.taken_at,
      is_screenshot = EXCLUDED.is_screenshot,
      ocr_text = EXCLUDED.ocr_text,
      scene_tags = EXCLUDED.scene_tags,
      embedding = EXCLUDED.embedding,
      updated_at = NOW()
RETURNING (xmax = 0)::boolean AS was_inserted;

-- name: PendingCommands :many
-- Fluent: delivered_at IS NULL, expires_at > now, sort created_at, limit 50.
SELECT id, kind, domain, payload
FROM device_command_queue
WHERE tenant_id = $1 AND delivered_at IS NULL AND expires_at > sqlc.arg(now)
ORDER BY created_at
LIMIT 50;

-- name: MarkDelivered :exec
UPDATE device_command_queue
SET delivered_at = sqlc.arg(delivered_at), result_ok = sqlc.arg(result_ok), result_error = sqlc.narg(result_error)
WHERE tenant_id = sqlc.arg(tenant_id) AND id = sqlc.arg(id) AND delivered_at IS NULL;

-- name: EmbeddingUsageFor :one
SELECT id, tokens_used FROM embedding_usage
WHERE tenant_id = $1 AND year_month = $2
LIMIT 1;

-- name: SetEmbeddingUsage :exec
UPDATE embedding_usage SET tokens_used = $2, updated_at = $3 WHERE id = $1;

-- name: InsertEmbeddingUsage :exec
INSERT INTO embedding_usage (id, tenant_id, year_month, tokens_used, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $5);
