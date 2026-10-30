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
