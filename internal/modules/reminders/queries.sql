-- Reminders (Swift: Sources/App/Reminders/RemindersController.swift, model
-- Reminder). Every query is tenant-scoped, as Fluent's TenantModel queries are.

-- name: ListReminders :many
SELECT id, tenant_id, title, body, fire_at, recurrence_cron, fired_at, created_at
FROM reminders
WHERE tenant_id = $1
ORDER BY fire_at DESC
LIMIT $2;

-- name: CreateReminder :one
INSERT INTO reminders (id, tenant_id, title, body, fire_at, recurrence_cron, fired_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, NULL, $7)
RETURNING id, tenant_id, title, body, fire_at, recurrence_cron, fired_at, created_at;

-- name: GetReminder :one
SELECT id, tenant_id, title, body, fire_at, recurrence_cron, fired_at, created_at
FROM reminders
WHERE id = $1 AND tenant_id = $2
LIMIT 1;

-- name: UpdateReminder :one
UPDATE reminders
SET title = $3, body = $4, fire_at = $5, recurrence_cron = $6, fired_at = $7
WHERE id = $1 AND tenant_id = $2
RETURNING id, tenant_id, title, body, fire_at, recurrence_cron, fired_at, created_at;

-- name: DeleteReminder :exec
DELETE FROM reminders WHERE id = $1 AND tenant_id = $2;
