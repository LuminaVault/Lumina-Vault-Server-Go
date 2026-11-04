-- Queries for /v1/me (wave 3). Each one is the SQL Swift runs, from
-- LuminaVaultServer Sources/App/Me/*.swift (file named per query). Constant
-- lists Swift splices in with MeRecapService.sqlList are written out here.

-- ─── User (AppRequestContext.requireIdentity loads the users row) ─────────

-- name: MeUser :one
SELECT tier, timezone, created_at FROM users WHERE id = $1;

-- ─── /v1/me/today (MeTodayService.swift) ──────────────────────────────────

-- MemoryRepository.listPaginated(limit: 1): every state but rejected,
-- created_at DESC (NULLs first, as Postgres sorts them), id DESC.
-- name: TodayLastMemory :one
SELECT id, content, created_at FROM memories
WHERE tenant_id = $1 AND review_state <> 'rejected'
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- AchievementsService.recentUnlocks(limit: 20).
-- name: TodayRecentUnlocks :many
SELECT achievement_key, unlocked_at FROM achievement_progress
WHERE tenant_id = $1 AND unlocked_at IS NOT NULL
ORDER BY unlocked_at DESC
LIMIT 20;

-- SpacesService.list(tenantID:).count
-- name: TodaySpaceCount :one
SELECT COUNT(*)::int AS n FROM spaces WHERE tenant_id = $1;

-- name: TodayStepsTotal :one
SELECT COALESCE(SUM(value_numeric), 0)::BIGINT AS total
FROM health_events
WHERE tenant_id = sqlc.arg(tenant_id)
  AND event_type = 'steps'
  AND recorded_at >= sqlc.arg(day_start)::timestamptz;

-- name: TodaySleepTotal :one
SELECT COALESCE(SUM(value_numeric), 0)::BIGINT AS total
FROM health_events
WHERE tenant_id = sqlc.arg(tenant_id)
  AND event_type = 'sleep_session'
  AND recorded_at >= sqlc.arg(prev_day)::timestamptz
  AND recorded_at <  sqlc.arg(day_start)::timestamptz;

-- ─── /v1/me/brief (MeBriefService.swift) ──────────────────────────────────

-- name: BriefByDate :one
SELECT id, date::text AS date, cards::text AS cards, end_note, created_at
FROM briefs
WHERE tenant_id = sqlc.arg(tenant_id) AND date = sqlc.arg(day)::text::date;

-- name: BriefByID :one
SELECT id, date::text AS date, cards::text AS cards, end_note, created_at
FROM briefs
WHERE tenant_id = $1 AND id = $2;

-- name: UpsertBriefFeedback :exec
INSERT INTO brief_feedback (tenant_id, brief_id, card_id, card_kind, action, created_at)
VALUES (sqlc.arg(tenant_id), sqlc.arg(brief_id), sqlc.arg(card_id), sqlc.arg(card_kind), sqlc.arg(action), sqlc.arg(now)::timestamptz)
ON CONFLICT (brief_id, card_id) DO UPDATE
SET action = EXCLUDED.action, created_at = EXCLUDED.created_at;

-- The Fluent read-modify-write of MeBriefService.feedback in one statement:
-- lastReviewedAt = now, reviewCount += 1, applyReviewAction(.later),
-- updatedByUserID, and Fluent's updated_at timestamp.
-- name: SnoozeMemory :exec
UPDATE memories
SET last_reviewed_at = sqlc.arg(now)::timestamptz,
    review_count = review_count + 1,
    next_review_at = sqlc.arg(next_review_at)::timestamptz,
    resurface_muted = false,
    updated_by_user_id = sqlc.arg(actor),
    updated_at = sqlc.arg(updated_at)::timestamptz
WHERE tenant_id = sqlc.arg(tenant_id) AND id = sqlc.arg(id);

-- ─── /v1/me/commitments (CommitmentsService.swift) ────────────────────────

-- name: CommitmentsAll :many
SELECT id, text, due_at, state, surfaced_count, memory_id, source_vault_file_id, created_at, done_at FROM commitments
WHERE tenant_id = $1
ORDER BY (state = 'open') DESC, due_at ASC NULLS LAST, created_at DESC, id
LIMIT 200;

-- name: CommitmentsOpen :many
SELECT id, text, due_at, state, surfaced_count, memory_id, source_vault_file_id, created_at, done_at FROM commitments
WHERE tenant_id = $1 AND state = 'open'
ORDER BY due_at ASC NULLS LAST, created_at ASC, id
LIMIT 200;

-- name: CommitmentsClosed :many
SELECT id, text, due_at, state, surfaced_count, memory_id, source_vault_file_id, created_at, done_at FROM commitments
WHERE tenant_id = sqlc.arg(tenant_id) AND state = sqlc.arg(state)
ORDER BY done_at DESC NULLS LAST, created_at DESC, id
LIMIT 200;

-- name: CommitmentClash :one
SELECT other.id FROM commitments me
JOIN commitments other
  ON other.tenant_id = me.tenant_id
 AND other.normalized_text = me.normalized_text
 AND other.state = 'open'
 AND other.id <> me.id
WHERE me.tenant_id = $1 AND me.id = $2
LIMIT 1;

-- name: UpdateCommitment :one
UPDATE commitments
SET done_at = CASE WHEN sqlc.arg(state)::text = 'open' THEN NULL
                   WHEN state = sqlc.arg(state)::text THEN done_at
                   ELSE sqlc.arg(now)::timestamptz END,
    state = sqlc.arg(state)::text
WHERE tenant_id = sqlc.arg(tenant_id) AND id = sqlc.arg(id)
RETURNING id, text, due_at, state, surfaced_count, memory_id, source_vault_file_id, created_at, done_at;

-- ─── /v1/me/learned (MeLearnedService.swift) ──────────────────────────────

-- name: LearnedSoulChanges :many
SELECT id, title, summary, created_at
FROM self_improvement_changes
WHERE tenant_id = sqlc.arg(tenant_id)
  AND kind = 'soul'
  AND state = 'pending'
  AND created_at >= sqlc.arg(since)::timestamptz
  AND created_at <= sqlc.arg(now)::timestamptz
ORDER BY created_at DESC, id DESC
LIMIT 20;

-- name: LearnedMemories :many
SELECT id, content, kind, review_state, created_at
FROM memories
WHERE tenant_id = sqlc.arg(tenant_id)
  AND kind IN ('preference', 'decision')
  AND review_state IN ('pending', 'auto')
  AND created_at >= sqlc.arg(since)::timestamptz
  AND created_at <= sqlc.arg(now)::timestamptz
ORDER BY created_at DESC, id DESC
LIMIT 20;

-- ─── /v1/me/resurface (MeResurfaceService.swift) ──────────────────────────
-- Every lane shares MeResurfaceService.eligible: not rejected, not muted,
-- not a model-made memory still pending, not reviewed in the last 24 h, not
-- scheduled for later.

-- name: ResurfaceDue :many
SELECT id, content, kind, created_at, space_id
FROM memories
WHERE tenant_id = sqlc.arg(tenant_id)
  AND review_state <> 'rejected'
  AND NOT resurface_muted
  AND NOT (review_state = 'pending'
           AND origin_kind IN ('knowledge_compile', 'chat', 'query'))
  AND (last_reviewed_at IS NULL OR last_reviewed_at < sqlc.arg(reviewed_before)::timestamptz)
  AND (next_review_at IS NULL OR next_review_at <= sqlc.arg(now)::timestamptz)
  AND next_review_at IS NOT NULL AND next_review_at <= sqlc.arg(now)::timestamptz
ORDER BY next_review_at ASC, id ASC
LIMIT sqlc.arg(lim)::int;

-- name: ResurfaceOnThisDay :many
SELECT id, content, kind, created_at, space_id, local_day::text AS local_day
FROM (
    SELECT id, content, kind, created_at, space_id,
           (created_at AT TIME ZONE sqlc.arg(tz)::text)::date AS local_day
    FROM memories
    WHERE tenant_id = sqlc.arg(tenant_id)
      AND review_state <> 'rejected'
      AND NOT resurface_muted
      AND NOT (review_state = 'pending'
               AND origin_kind IN ('knowledge_compile', 'chat', 'query'))
      AND (last_reviewed_at IS NULL OR last_reviewed_at < sqlc.arg(reviewed_before)::timestamptz)
      AND (next_review_at IS NULL OR next_review_at <= sqlc.arg(now)::timestamptz)
      AND created_at < sqlc.arg(created_before)::timestamptz
) dated
WHERE local_day::text IN (sqlc.arg(month_ago)::text, sqlc.arg(half_year_ago)::text, sqlc.arg(year_ago)::text)
   OR (to_char(local_day, 'MM-DD') = sqlc.arg(month_day)::text
       AND EXTRACT(YEAR FROM local_day) <= sqlc.arg(two_years_ago)::int)
ORDER BY local_day ASC, id ASC
LIMIT 50;

-- name: ResurfaceFading :many
SELECT id, content, kind, created_at, space_id,
       (COALESCE(last_reviewed_at, created_at) AT TIME ZONE sqlc.arg(tz)::text)::date::text AS local_day
FROM memories
WHERE tenant_id = sqlc.arg(tenant_id)
  AND review_state <> 'rejected'
  AND NOT resurface_muted
  AND NOT (review_state = 'pending'
           AND origin_kind IN ('knowledge_compile', 'chat', 'query'))
  AND (last_reviewed_at IS NULL OR last_reviewed_at < sqlc.arg(reviewed_before)::timestamptz)
  AND (next_review_at IS NULL OR next_review_at <= sqlc.arg(now)::timestamptz)
  AND kind IN ('decision', 'preference')
  AND COALESCE(last_reviewed_at, created_at) < sqlc.arg(cutoff)::timestamptz
ORDER BY COALESCE(last_reviewed_at, created_at) ASC, id ASC
LIMIT sqlc.arg(lim)::int;

-- ─── /v1/me/recap (MeRecapService.swift, DashboardController.swift) ───────

-- name: RecapDailyCounts :many
WITH events AS (
    SELECT (created_at AT TIME ZONE sqlc.arg(tz)::text)::date AS day,
           CASE WHEN origin_kind IN ('manual', 'vault', 'link', 'import', 'legacy') THEN 1 ELSE 0 END AS captures,
           CASE WHEN origin_kind IN ('knowledge_compile', 'chat', 'query') THEN 1 ELSE 0 END AS learned,
           0 AS asked,
           0 AS reviewed
    FROM memories
    WHERE tenant_id = sqlc.arg(tenant_id)::uuid
      AND created_at >= sqlc.arg(start)::timestamptz AND created_at < sqlc.arg(end_at)::timestamptz
      AND review_state <> 'rejected'
    UNION ALL
    SELECT (m.created_at AT TIME ZONE sqlc.arg(tz)::text)::date, 0, 0, 1, 0
    FROM conversation_messages m
    JOIN conversations c ON c.id = m.conversation_id
    WHERE c.tenant_id = sqlc.arg(tenant_id)::uuid
      AND m.role = 'user'
      AND m.created_at >= sqlc.arg(start)::timestamptz AND m.created_at < sqlc.arg(end_at)::timestamptz
    UNION ALL
    SELECT (occurred_at AT TIME ZONE sqlc.arg(tz)::text)::date, 0, 0, 0, 1
    FROM analytics_events
    WHERE vault_id = sqlc.arg(tenant_id)::uuid
      AND event_name = 'memory_reviewed'
      AND occurred_at >= sqlc.arg(start)::timestamptz AND occurred_at < sqlc.arg(end_at)::timestamptz
)
SELECT day::text AS day,
       SUM(captures)::int AS captures,
       SUM(learned)::int AS learned,
       SUM(asked)::int AS asked,
       SUM(reviewed)::int AS reviewed
FROM events
GROUP BY day;

-- DashboardController.currentStreakDays with asOfDay.
-- name: StreakDays :one
WITH activity_days AS (
    SELECT DISTINCT (created_at AT TIME ZONE sqlc.arg(tz)::text)::date AS day
    FROM memories
    WHERE tenant_id = sqlc.arg(tenant_id)::uuid
    UNION
    SELECT DISTINCT (created_at AT TIME ZONE sqlc.arg(tz)::text)::date AS day
    FROM conversations
    WHERE tenant_id = sqlc.arg(tenant_id)::uuid
),
bounded AS (
    SELECT day FROM activity_days
    WHERE day <= sqlc.arg(as_of_day)::text::date
),
ranked AS (
    SELECT day,
           day - (row_number() OVER (ORDER BY day))::int AS island_key
    FROM bounded
),
latest AS (
    SELECT island_key
    FROM ranked
    WHERE day >= sqlc.arg(as_of_day)::text::date - 1
    ORDER BY day DESC
    LIMIT 1
)
SELECT COUNT(*)::int AS count
FROM ranked
WHERE island_key = (SELECT island_key FROM latest);

-- name: RecapKindCounts :many
SELECT kind::text AS kind, COUNT(*)::int AS count
FROM memories
WHERE tenant_id = sqlc.arg(tenant_id)
  AND created_at >= sqlc.arg(start)::timestamptz AND created_at < sqlc.arg(end_at)::timestamptz
  AND review_state <> 'rejected'
  AND kind IS NOT NULL
GROUP BY kind;

-- name: RecapHighlightMemories :many
SELECT id, content, kind::text AS kind, created_at AS at
FROM (
    SELECT id, content, kind, created_at,
           row_number() OVER (PARTITION BY kind ORDER BY created_at DESC) AS rn
    FROM memories
    WHERE tenant_id = sqlc.arg(tenant_id)
      AND created_at >= sqlc.arg(start)::timestamptz AND created_at < sqlc.arg(end_at)::timestamptz
      AND review_state <> 'rejected'
      AND kind IN ('decision', 'preference')
) ranked
WHERE rn <= 2;

-- name: UnlocksBetweenDesc :many
SELECT achievement_key, unlocked_at
FROM achievement_progress
WHERE tenant_id = sqlc.arg(tenant_id)
  AND unlocked_at >= sqlc.arg(start)::timestamptz AND unlocked_at < sqlc.arg(end_at)::timestamptz
ORDER BY unlocked_at DESC
LIMIT 5;

-- name: RecapMostRevisited :one
SELECT id, content,
       GREATEST(
           COALESCE(last_reviewed_at, '-infinity'::timestamptz),
           COALESCE(last_accessed_at, '-infinity'::timestamptz)
       )::timestamptz AS at
FROM memories
WHERE tenant_id = sqlc.arg(tenant_id)
  AND review_state <> 'rejected'
  AND review_count + query_hit_count > 0
  AND (
      (last_reviewed_at >= sqlc.arg(start)::timestamptz AND last_reviewed_at < sqlc.arg(end_at)::timestamptz)
      OR (last_accessed_at >= sqlc.arg(start)::timestamptz AND last_accessed_at < sqlc.arg(end_at)::timestamptz)
  )
ORDER BY review_count DESC, query_hit_count DESC, at DESC
LIMIT 1;

-- ─── /v1/me/recap/period (PeriodRecapService.swift, PeriodRecapThemes.swift)

-- name: PeriodDailyCounts :many
WITH events AS (
    SELECT created_at AS at,
           CASE WHEN origin_kind IN ('manual', 'vault', 'link', 'import', 'legacy') THEN 1 ELSE 0 END AS captures,
           CASE WHEN origin_kind IN ('knowledge_compile', 'chat', 'query') THEN 1 ELSE 0 END AS learned,
           0 AS asked
    FROM memories
    WHERE tenant_id = sqlc.arg(tenant_id)::uuid
      AND review_state <> 'rejected'
      AND ((created_at >= sqlc.arg(start)::timestamptz AND created_at < sqlc.arg(end_at)::timestamptz)
        OR (created_at >= sqlc.arg(prev_start)::timestamptz AND created_at < sqlc.arg(prev_end)::timestamptz))
    UNION ALL
    SELECT m.created_at, 0, 0, 1
    FROM conversation_messages m
    JOIN conversations c ON c.id = m.conversation_id
    WHERE c.tenant_id = sqlc.arg(tenant_id)::uuid
      AND m.role = 'user'
      AND ((m.created_at >= sqlc.arg(start)::timestamptz AND m.created_at < sqlc.arg(end_at)::timestamptz)
        OR (m.created_at >= sqlc.arg(prev_start)::timestamptz AND m.created_at < sqlc.arg(prev_end)::timestamptz))
)
SELECT (at AT TIME ZONE sqlc.arg(tz)::text)::date::text AS day,
       SUM(captures)::int AS captures,
       SUM(learned)::int AS learned,
       SUM(asked)::int AS asked
FROM events
GROUP BY 1;

-- name: PeriodHourCounts :many
SELECT EXTRACT(hour FROM (at AT TIME ZONE sqlc.arg(tz)::text))::int AS hour, COUNT(*)::int AS count
FROM (
    SELECT created_at AS at
    FROM memories
    WHERE tenant_id = sqlc.arg(tenant_id)::uuid
      AND created_at >= sqlc.arg(start)::timestamptz AND created_at < sqlc.arg(end_at)::timestamptz
      AND review_state <> 'rejected'
      AND origin_kind IN ('manual', 'vault', 'link', 'import', 'legacy')
    UNION ALL
    SELECT m.created_at
    FROM conversation_messages m
    JOIN conversations c ON c.id = m.conversation_id
    WHERE c.tenant_id = sqlc.arg(tenant_id)::uuid
      AND m.role = 'user'
      AND m.created_at >= sqlc.arg(start)::timestamptz AND m.created_at < sqlc.arg(end_at)::timestamptz
) events
GROUP BY 1;

-- name: PeriodKindCounts :many
SELECT kind::text AS label, COUNT(*)::int AS count
FROM memories
WHERE tenant_id = sqlc.arg(tenant_id)
  AND created_at >= sqlc.arg(start)::timestamptz AND created_at < sqlc.arg(end_at)::timestamptz
  AND review_state <> 'rejected'
  AND kind IS NOT NULL
GROUP BY kind
ORDER BY count DESC, label ASC;

-- name: PeriodTopTags :many
SELECT lower(t.tag)::text AS label, COUNT(*)::int AS count
FROM memories, unnest(memories.tags) AS t(tag)
WHERE memories.tenant_id = sqlc.arg(tenant_id)
  AND memories.created_at >= sqlc.arg(start)::timestamptz AND memories.created_at < sqlc.arg(end_at)::timestamptz
  AND memories.review_state <> 'rejected'
  AND btrim(t.tag) <> ''
GROUP BY lower(t.tag)
ORDER BY count DESC, label ASC
LIMIT 5;

-- name: PeriodTopSpace :one
SELECT s.name AS label, COUNT(*)::int AS count
FROM memories m
JOIN spaces s ON s.id = m.space_id
WHERE m.tenant_id = sqlc.arg(tenant_id)
  AND m.created_at >= sqlc.arg(start)::timestamptz AND m.created_at < sqlc.arg(end_at)::timestamptz
  AND m.review_state <> 'rejected'
GROUP BY s.id, s.name
ORDER BY count DESC, label ASC
LIMIT 1;

-- name: PeriodConversationCount :one
SELECT COUNT(*)::int AS count
FROM conversations
WHERE tenant_id = sqlc.arg(tenant_id)
  AND created_at >= sqlc.arg(start)::timestamptz AND created_at < sqlc.arg(end_at)::timestamptz;

-- name: PeriodSkillRuns :one
SELECT name AS label, COUNT(*)::int AS count, (SUM(COUNT(*)) OVER ())::int AS total
FROM skill_run_log
WHERE tenant_id = sqlc.arg(tenant_id)
  AND started_at >= sqlc.arg(start)::timestamptz AND started_at < sqlc.arg(end_at)::timestamptz
GROUP BY name
ORDER BY count DESC, label ASC
LIMIT 1;

-- name: UnlocksBetweenAsc :many
SELECT achievement_key, unlocked_at
FROM achievement_progress
WHERE tenant_id = sqlc.arg(tenant_id)
  AND unlocked_at >= sqlc.arg(start)::timestamptz AND unlocked_at < sqlc.arg(end_at)::timestamptz
ORDER BY unlocked_at ASC;

-- name: PeriodFirstCapture :one
SELECT id, content, (created_at AT TIME ZONE sqlc.arg(tz)::text)::date::text AS day
FROM memories
WHERE tenant_id = sqlc.arg(tenant_id)
  AND created_at >= sqlc.arg(start)::timestamptz AND created_at < sqlc.arg(end_at)::timestamptz
  AND review_state <> 'rejected'
  AND origin_kind IN ('manual', 'vault', 'link', 'import', 'legacy')
ORDER BY created_at ASC
LIMIT 1;

-- PeriodRecapThemesStore.load
-- name: RecapThemesLoad :one
SELECT status, headline, sentence, themes, attempts, requested_at, generated_at
FROM recap_themes
WHERE user_id = $1 AND period_key = $2;

-- PeriodRecapThemesStore.claim: true only for the caller that won. Swift's
-- PeriodRecapThemesWorker (in-process inbox) only drains jobs its own process
-- claimed, so a row Go claims is generated by nobody (see module.go).
-- name: RecapThemesClaim :many
INSERT INTO recap_themes (user_id, period_key, status, attempts, requested_at)
VALUES (sqlc.arg(user_id), sqlc.arg(period_key), 'pending', 1, sqlc.arg(now)::timestamptz)
ON CONFLICT (user_id, period_key) DO UPDATE SET
    attempts = recap_themes.attempts + 1,
    requested_at = EXCLUDED.requested_at,
    status = CASE WHEN recap_themes.status = 'ready' THEN 'ready' ELSE 'pending' END
WHERE recap_themes.requested_at IS NULL OR recap_themes.requested_at < sqlc.arg(cutoff)::timestamptz
RETURNING attempts;

-- ─── /v1/me/location (Weather/LastKnownLocationStore.swift) ───────────────

-- name: LocationLoad :one
SELECT last_location_lat, last_location_lng, last_location_place, last_location_at
FROM users WHERE id = $1;

-- name: LocationClear :exec
UPDATE users
SET last_location_lat = NULL, last_location_lng = NULL,
    last_location_place = NULL, last_location_at = NULL
WHERE id = $1;

-- ─── /v1/me/apns-categories (ApnsCategoryPrefsController.swift) ───────────

-- name: ApnsPrefsGet :one
SELECT chat_enabled, nudge_enabled, digest_enabled, approval_enabled, run_completed_enabled, meeting_brief_enabled
FROM apns_category_prefs WHERE tenant_id = $1;

-- Fluent create: every field plus the .update timestamp.
-- name: ApnsPrefsInsert :exec
INSERT INTO apns_category_prefs (tenant_id, chat_enabled, nudge_enabled, digest_enabled, approval_enabled, run_completed_enabled, meeting_brief_enabled, updated_at)
VALUES (sqlc.arg(tenant_id), sqlc.arg(chat), sqlc.arg(nudge), sqlc.arg(digest), sqlc.arg(approval), sqlc.arg(run_completed), sqlc.arg(meeting_brief), sqlc.arg(now)::timestamptz);

-- Fluent update of a row with changes (only called when the body set a field).
-- name: ApnsPrefsUpdate :exec
UPDATE apns_category_prefs
SET chat_enabled = sqlc.arg(chat), nudge_enabled = sqlc.arg(nudge), digest_enabled = sqlc.arg(digest),
    approval_enabled = sqlc.arg(approval), run_completed_enabled = sqlc.arg(run_completed),
    meeting_brief_enabled = sqlc.arg(meeting_brief), updated_at = sqlc.arg(now)::timestamptz
WHERE tenant_id = sqlc.arg(tenant_id);
