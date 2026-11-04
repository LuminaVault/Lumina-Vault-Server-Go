-- Community board (Sources/App/Community/CommunityService.swift). Every query
-- is cross-tenant. A post leaves the server only through the projection below
-- (posts joined to profiles, handle + display name), never through users.

-- name: UserInfo :one
SELECT username, is_admin FROM users WHERE id = $1;

-- name: ProfileByUser :one
SELECT id, handle, display_name, terms_version, terms_accepted_at, posting_suspended_until
FROM community_profiles WHERE user_id = $1;

-- name: HandleTaken :one
SELECT EXISTS (
  SELECT 1 FROM community_profiles WHERE lower(handle) = sqlc.arg(handle)::text AND user_id <> sqlc.arg(user_id)
) AS taken;

-- name: UpsertProfile :exec
INSERT INTO community_profiles
  (id, user_id, handle, display_name, terms_version, terms_accepted_at, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $6, $6)
ON CONFLICT (user_id) DO UPDATE SET
  handle = EXCLUDED.handle,
  display_name = EXCLUDED.display_name,
  terms_version = EXCLUDED.terms_version,
  terms_accepted_at = EXCLUDED.terms_accepted_at,
  updated_at = EXCLUDED.updated_at;

-- name: GetPost :one
SELECT p.id, p.parent_id, p.title, p.body, p.author_kind, p.author_profile_id,
       pr.handle, pr.display_name, p.status, p.reply_count, p.reaction_count,
       p.created_at, p.last_activity_at,
       EXISTS (SELECT 1 FROM community_daily_posts d WHERE d.post_id = p.id) AS is_daily,
       (extract(epoch FROM p.last_activity_at) * 1000000)::bigint AS activity_us,
       (extract(epoch FROM p.created_at) * 1000000)::bigint AS created_us
FROM community_posts p
LEFT JOIN community_profiles pr ON pr.id = p.author_profile_id
WHERE p.id = sqlc.arg(id)
  AND (p.status = 'visible' OR (p.status = 'pending' AND p.author_profile_id = sqlc.narg(viewer_profile)))
  AND NOT EXISTS (
    SELECT 1 FROM community_blocks b
    WHERE b.blocker_profile_id = sqlc.narg(viewer_profile) AND b.blocked_profile_id = p.author_profile_id
  );

-- name: ListThreads :many
SELECT p.id, p.parent_id, p.title, p.body, p.author_kind, p.author_profile_id,
       pr.handle, pr.display_name, p.status, p.reply_count, p.reaction_count,
       p.created_at, p.last_activity_at,
       EXISTS (SELECT 1 FROM community_daily_posts d WHERE d.post_id = p.id) AS is_daily,
       (extract(epoch FROM p.last_activity_at) * 1000000)::bigint AS activity_us,
       (extract(epoch FROM p.created_at) * 1000000)::bigint AS created_us
FROM community_posts p
LEFT JOIN community_profiles pr ON pr.id = p.author_profile_id
WHERE p.parent_id IS NULL
  AND (p.status = 'visible' OR (p.status = 'pending' AND p.author_profile_id = sqlc.narg(viewer_profile)))
  AND NOT EXISTS (
    SELECT 1 FROM community_blocks b
    WHERE b.blocker_profile_id = sqlc.narg(viewer_profile) AND b.blocked_profile_id = p.author_profile_id
  )
  AND (sqlc.narg(cursor_us)::bigint IS NULL
       OR (p.last_activity_at, p.id) < (TIMESTAMPTZ 'epoch' + sqlc.narg(cursor_us)::bigint * INTERVAL '1 microsecond', sqlc.narg(cursor_id)::uuid))
ORDER BY p.last_activity_at DESC, p.id DESC
LIMIT sqlc.arg(row_limit);

-- name: ListReplies :many
SELECT p.id, p.parent_id, p.title, p.body, p.author_kind, p.author_profile_id,
       pr.handle, pr.display_name, p.status, p.reply_count, p.reaction_count,
       p.created_at, p.last_activity_at,
       EXISTS (SELECT 1 FROM community_daily_posts d WHERE d.post_id = p.id) AS is_daily,
       (extract(epoch FROM p.last_activity_at) * 1000000)::bigint AS activity_us,
       (extract(epoch FROM p.created_at) * 1000000)::bigint AS created_us
FROM community_posts p
LEFT JOIN community_profiles pr ON pr.id = p.author_profile_id
WHERE p.parent_id = sqlc.arg(parent_id)
  AND (p.status = 'visible' OR (p.status = 'pending' AND p.author_profile_id = sqlc.narg(viewer_profile)))
  AND NOT EXISTS (
    SELECT 1 FROM community_blocks b
    WHERE b.blocker_profile_id = sqlc.narg(viewer_profile) AND b.blocked_profile_id = p.author_profile_id
  )
  AND (sqlc.narg(cursor_us)::bigint IS NULL
       OR (p.created_at, p.id) > (TIMESTAMPTZ 'epoch' + sqlc.narg(cursor_us)::bigint * INTERVAL '1 microsecond', sqlc.narg(cursor_id)::uuid))
ORDER BY p.created_at ASC, p.id ASC
LIMIT sqlc.arg(row_limit);

-- name: TodayDaily :one
SELECT p.id, p.parent_id, p.title, p.body, p.author_kind, p.author_profile_id,
       pr.handle, pr.display_name, p.status, p.reply_count, p.reaction_count,
       p.created_at, p.last_activity_at,
       EXISTS (SELECT 1 FROM community_daily_posts d WHERE d.post_id = p.id) AS is_daily,
       (extract(epoch FROM p.last_activity_at) * 1000000)::bigint AS activity_us,
       (extract(epoch FROM p.created_at) * 1000000)::bigint AS created_us
FROM community_posts p
LEFT JOIN community_profiles pr ON pr.id = p.author_profile_id
JOIN community_daily_posts dp ON dp.post_id = p.id
WHERE dp.day = sqlc.arg(day)::text::date AND dp.status = 'published'
  AND (p.status = 'visible' OR (p.status = 'pending' AND p.author_profile_id = sqlc.narg(viewer_profile)))
  AND NOT EXISTS (
    SELECT 1 FROM community_blocks b
    WHERE b.blocker_profile_id = sqlc.narg(viewer_profile) AND b.blocked_profile_id = p.author_profile_id
  );

-- name: AdminQueue :many
SELECT p.id, p.parent_id, p.title, p.body, p.author_kind, p.author_profile_id,
       pr.handle, pr.display_name, p.status, p.reply_count, p.reaction_count,
       p.created_at, p.last_activity_at,
       EXISTS (SELECT 1 FROM community_daily_posts d WHERE d.post_id = p.id) AS is_daily,
       p.moderation_category, p.hidden_reason,
       (SELECT count(*) FROM community_reports r WHERE r.post_id = p.id AND r.status = 'open') AS open_reports,
       (SELECT coalesce(array_agg(DISTINCT r.reason), '{}') FROM community_reports r
        WHERE r.post_id = p.id AND r.status = 'open')::text[] AS report_reasons,
       (SELECT max(r.created_at) FROM community_reports r WHERE r.post_id = p.id) AS latest_report_at
FROM community_posts p
LEFT JOIN community_profiles pr ON pr.id = p.author_profile_id
WHERE (sqlc.arg(filter)::text = 'all' AND (p.status IN ('pending', 'hidden')
         OR EXISTS (SELECT 1 FROM community_reports r WHERE r.post_id = p.id AND r.status = 'open')))
   OR (sqlc.arg(filter)::text = 'open' AND EXISTS (SELECT 1 FROM community_reports r WHERE r.post_id = p.id AND r.status = 'open'))
   OR (sqlc.arg(filter)::text = 'pending' AND p.status = 'pending')
   OR (sqlc.arg(filter)::text = 'hidden' AND p.status = 'hidden')
ORDER BY latest_report_at DESC NULLS LAST, p.created_at DESC
LIMIT 200;

-- name: InsertPost :exec
INSERT INTO community_posts
  (id, parent_id, author_kind, author_profile_id, title, body, status,
   moderated_at, last_activity_at, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.narg(parent_id), 'user', sqlc.arg(author_profile_id), sqlc.narg(title), sqlc.arg(body), sqlc.arg(status),
        sqlc.narg(moderated_at), sqlc.arg(at), sqlc.arg(at), sqlc.arg(at));

-- name: SoftDeletePost :one
UPDATE community_posts
SET status = 'deleted', body = '[deleted]', updated_at = now()
WHERE id = sqlc.arg(id) AND author_profile_id = sqlc.arg(author_profile_id) AND status <> 'deleted'
RETURNING parent_id;

-- name: InsertReport :exec
INSERT INTO community_reports (id, post_id, reporter_profile_id, reason, note)
VALUES ($1, $2, $3, $4, sqlc.narg(note))
ON CONFLICT (post_id, reporter_profile_id) DO NOTHING;

-- name: AutoHide :one
UPDATE community_posts
SET status = 'hidden', hidden_reason = 'reports', updated_at = now()
WHERE community_posts.id = sqlc.arg(post_id) AND status = 'visible'
  AND (SELECT count(*) FROM community_reports r
       WHERE r.post_id = sqlc.arg(post_id) AND r.status = 'open') >= sqlc.arg(threshold)::bigint
RETURNING parent_id;

-- name: ListBlocks :many
SELECT b.blocked_profile_id, pr.handle, pr.display_name, b.created_at
FROM community_blocks b JOIN community_profiles pr ON pr.id = b.blocked_profile_id
WHERE b.blocker_profile_id = $1
ORDER BY b.created_at DESC;

-- name: ProfileExists :one
SELECT EXISTS (SELECT 1 FROM community_profiles WHERE id = $1) AS hit;

-- name: InsertBlock :exec
INSERT INTO community_blocks (blocker_profile_id, blocked_profile_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: DeleteBlock :exec
DELETE FROM community_blocks
WHERE blocker_profile_id = $1 AND blocked_profile_id = $2;

-- name: Decide :one
UPDATE community_posts
SET status = sqlc.arg(status),
    body = coalesce(sqlc.narg(body)::text, body),
    hidden_reason = sqlc.narg(hidden_reason),
    moderated_at = now(), updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING parent_id;

-- name: ResolveReports :exec
UPDATE community_reports
SET status = sqlc.arg(status), resolved_at = now(), resolved_by_user_id = sqlc.arg(resolved_by)
WHERE post_id = sqlc.arg(post_id) AND status = 'open';

-- name: Suspend :one
UPDATE community_profiles SET posting_suspended_until = sqlc.narg(until), updated_at = now()
WHERE id = sqlc.arg(id) RETURNING id;

-- name: CountRecentPosts :one
SELECT count(*)::int AS n FROM community_posts
WHERE author_profile_id = sqlc.arg(author_profile_id)
  AND (parent_id IS NOT NULL) = sqlc.arg(replies)::boolean
  AND created_at > now() - interval '24 hours';

-- name: RefreshThread :exec
UPDATE community_posts SET
  reply_count = (SELECT count(*) FROM community_posts c
                 WHERE c.parent_id = sqlc.arg(id) AND c.status = 'visible'),
  last_activity_at = greatest(last_activity_at, coalesce(sqlc.narg(touched_at)::timestamptz, last_activity_at))
WHERE community_posts.id = sqlc.arg(id);

-- Day row (Sources/App/Community/DailyPostWorker.swift); the worker itself stays in Swift.

-- name: ClaimDay :one
INSERT INTO community_daily_posts (day, status, attempts, claimed_at, updated_at)
VALUES (sqlc.arg(day)::text::date, 'claimed', 1, sqlc.arg(at), sqlc.arg(at))
ON CONFLICT (day) DO NOTHING
RETURNING attempts;

-- name: ReclaimDay :one
UPDATE community_daily_posts
SET status = 'claimed', attempts = attempts + 1, claimed_at = sqlc.arg(at), updated_at = sqlc.arg(at)
WHERE day = sqlc.arg(day)::text::date AND attempts < sqlc.arg(max_attempts)
  AND (status = 'failed' OR (status = 'claimed' AND claimed_at < sqlc.arg(stale_before)))
RETURNING attempts;

-- name: MarkDayFailed :exec
UPDATE community_daily_posts
SET status = CASE WHEN attempts >= sqlc.arg(max_attempts) THEN 'skipped' ELSE 'failed' END,
    last_error = sqlc.arg(last_error), updated_at = now()
WHERE day = sqlc.arg(day)::text::date AND status = 'claimed';

-- name: DayOutcome :one
SELECT status, post_id, source_kind, topic_key FROM community_daily_posts WHERE day = sqlc.arg(day)::text::date;
