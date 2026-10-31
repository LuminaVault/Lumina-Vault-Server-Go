-- Achievements (Swift: Sources/App/Achievements/AchievementsService.swift).
-- Read-only: progress rows are written by Swift's AchievementsWorker.

-- name: TenantProgress :many
-- AchievementsService.progress(for:): every row for the tenant.
SELECT achievement_key, progress_count, unlocked_at
FROM achievement_progress
WHERE tenant_id = $1;

-- name: RecentUnlocks :many
-- AchievementsService.recentUnlocks(for:limit:).
SELECT achievement_key, unlocked_at
FROM achievement_progress
WHERE tenant_id = $1 AND unlocked_at IS NOT NULL
ORDER BY unlocked_at DESC
LIMIT $2;

-- Write path of AchievementsService.record(tenantID:event:). Not reached by
-- any Go route: Swift's AchievementsWorker owns writes until its producers move.

-- name: GetProgress :one
SELECT id, progress_count, unlocked_at
FROM achievement_progress
WHERE tenant_id = $1 AND achievement_key = $2;

-- name: InsertProgress :exec
INSERT INTO achievement_progress (id, tenant_id, achievement_key, progress_count, unlocked_at, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $6);

-- name: UpdateProgress :exec
UPDATE achievement_progress
SET progress_count = $2, unlocked_at = $3, updated_at = $4
WHERE id = $1;
