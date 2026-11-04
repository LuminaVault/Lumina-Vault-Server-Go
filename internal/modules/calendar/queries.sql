-- Calendar module. Sources: CalendarController, AppleCalendarController,
-- CalendarTokenStore, GoogleCalendarOAuthService, CalendarSyncService,
-- MeetingBriefService and the two arms of HybridMemorySearch
-- (MemoryRepository.semanticSearch, MemoryChunkRepository.hybridSearch).

-- ─── /v1/calendar/events ────────────────────────────────────────────────────

-- name: UpcomingEvents :many
SELECT id, source, external_id, title, notes, location, starts_at, ends_at,
       all_day, status, organizer, html_link
FROM calendar_events
WHERE tenant_id = sqlc.arg(tenant_id)
  AND status <> 'cancelled'
  AND ends_at >= sqlc.arg(now)::timestamptz
ORDER BY starts_at ASC
LIMIT sqlc.arg(lim)::int;

-- name: UpsertGoogleEvent :one
-- CalendarSyncService.upsert: Swift reads then saves; one statement gives the
-- same row (calendar_id and created_at untouched on update) and cannot race
-- CalendarSyncWorker on the unique key.
INSERT INTO calendar_events
    (id, tenant_id, source, external_id, title, notes, location, starts_at, ends_at,
     all_day, status, organizer, attendees, recurrence, html_link, etag,
     remote_updated_at, created_at, updated_at)
VALUES
    (sqlc.arg(id), sqlc.arg(tenant_id), 'google', sqlc.arg(external_id), sqlc.arg(title),
     sqlc.narg(notes), sqlc.narg(location), sqlc.arg(starts_at)::timestamptz, sqlc.arg(ends_at)::timestamptz,
     sqlc.arg(all_day), sqlc.arg(status), sqlc.narg(organizer),
     sqlc.narg(attendees)::text::jsonb, sqlc.narg(recurrence)::text::jsonb,
     sqlc.narg(html_link), sqlc.narg(etag), sqlc.arg(remote_updated_at)::timestamptz,
     sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT (tenant_id, source, external_id) DO UPDATE
    SET title = EXCLUDED.title,
        notes = EXCLUDED.notes,
        location = EXCLUDED.location,
        starts_at = EXCLUDED.starts_at,
        ends_at = EXCLUDED.ends_at,
        all_day = EXCLUDED.all_day,
        status = EXCLUDED.status,
        organizer = EXCLUDED.organizer,
        attendees = EXCLUDED.attendees,
        recurrence = EXCLUDED.recurrence,
        html_link = EXCLUDED.html_link,
        etag = EXCLUDED.etag,
        remote_updated_at = EXCLUDED.remote_updated_at,
        updated_at = EXCLUDED.updated_at
RETURNING id, source, external_id, title, notes, location, starts_at, ends_at,
          all_day, status, organizer, html_link;

-- name: DeleteGoogleEvents :exec
DELETE FROM calendar_events WHERE tenant_id = sqlc.arg(tenant_id) AND source = 'google';

-- ─── POST /v1/calendar/sync (AppleCalendarController) ───────────────────────

-- name: CalendarConsent :one
SELECT allowed FROM apple_consent WHERE tenant_id = sqlc.arg(tenant_id) AND domain = 'calendar';

-- name: UpsertAppleEvent :one
-- No row back means the last-writer-wins guard rejected a stale delta.
INSERT INTO calendar_events
    (tenant_id, source, external_id, calendar_id, title, notes, location,
     starts_at, ends_at, all_day, status, organizer, attendees, remote_updated_at,
     created_at, updated_at)
VALUES
    (sqlc.arg(tenant_id), 'apple_eventkit', sqlc.arg(external_id), sqlc.narg(calendar_id),
     sqlc.arg(title), sqlc.narg(notes), sqlc.narg(location),
     sqlc.arg(starts_at)::timestamptz, sqlc.arg(ends_at)::timestamptz, sqlc.arg(all_day),
     sqlc.arg(status), sqlc.narg(organizer), sqlc.narg(attendees)::text::jsonb,
     COALESCE(sqlc.narg(remote_updated_at)::timestamptz, NOW()),
     NOW(), NOW())
ON CONFLICT (tenant_id, source, external_id) DO UPDATE
    SET calendar_id = EXCLUDED.calendar_id,
        title = EXCLUDED.title,
        notes = EXCLUDED.notes,
        location = EXCLUDED.location,
        starts_at = EXCLUDED.starts_at,
        ends_at = EXCLUDED.ends_at,
        all_day = EXCLUDED.all_day,
        status = EXCLUDED.status,
        organizer = EXCLUDED.organizer,
        attendees = COALESCE(EXCLUDED.attendees, calendar_events.attendees),
        remote_updated_at = EXCLUDED.remote_updated_at,
        updated_at = NOW()
    WHERE calendar_events.remote_updated_at IS NULL
       OR EXCLUDED.remote_updated_at IS NULL
       OR EXCLUDED.remote_updated_at >= calendar_events.remote_updated_at
RETURNING (xmax = 0)::bool AS inserted;

-- name: StampCalendarConsentSync :exec
UPDATE apple_consent SET last_sync_at = NOW()
WHERE tenant_id = sqlc.arg(tenant_id) AND domain = 'calendar';

-- ─── calendar_accounts ──────────────────────────────────────────────────────

-- name: GoogleAccount :one
SELECT * FROM calendar_accounts WHERE tenant_id = sqlc.arg(tenant_id) AND provider = 'google';

-- name: DeleteGoogleAccount :exec
DELETE FROM calendar_accounts WHERE tenant_id = sqlc.arg(tenant_id) AND provider = 'google';

-- name: SetAccountScope :exec
UPDATE calendar_accounts SET scope = sqlc.arg(scope), updated_at = sqlc.arg(now)::timestamptz
WHERE tenant_id = sqlc.arg(tenant_id) AND provider = 'google';

-- name: SetAccountScopeClearSyncToken :exec
UPDATE calendar_accounts SET scope = sqlc.arg(scope), sync_token = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE tenant_id = sqlc.arg(tenant_id) AND provider = 'google';

-- name: StoreInitialTokens :exec
-- CalendarTokenStore.storeInitialTokens: upsert the (tenant, provider) row.
-- A re-link resets the incremental cursor and any failure, and keeps the
-- previous account_email when the id_token had none.
INSERT INTO calendar_accounts
    (id, tenant_id, provider, account_email, scope,
     refresh_ciphertext, refresh_nonce, access_ciphertext, access_nonce, access_expires_at,
     status, created_at, updated_at)
VALUES
    (sqlc.arg(id), sqlc.arg(tenant_id), 'google', sqlc.narg(account_email), sqlc.arg(scope),
     sqlc.arg(refresh_ciphertext), sqlc.arg(refresh_nonce), sqlc.arg(access_ciphertext), sqlc.arg(access_nonce),
     sqlc.arg(access_expires_at)::timestamptz, 'connected', sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT (tenant_id, provider) DO UPDATE
    SET account_email = COALESCE(EXCLUDED.account_email, calendar_accounts.account_email),
        scope = EXCLUDED.scope,
        refresh_ciphertext = EXCLUDED.refresh_ciphertext,
        refresh_nonce = EXCLUDED.refresh_nonce,
        access_ciphertext = EXCLUDED.access_ciphertext,
        access_nonce = EXCLUDED.access_nonce,
        access_expires_at = EXCLUDED.access_expires_at,
        status = 'connected',
        last_failure_at = NULL,
        last_failure_code = NULL,
        sync_token = NULL,
        updated_at = EXCLUDED.updated_at;

-- name: StoreRefreshedAccess :exec
-- refresh_ciphertext/nonce stay when Google did not rotate the refresh token.
UPDATE calendar_accounts
SET access_ciphertext = sqlc.arg(access_ciphertext),
    access_nonce = sqlc.arg(access_nonce),
    access_expires_at = sqlc.arg(access_expires_at)::timestamptz,
    refresh_ciphertext = COALESCE(sqlc.narg(refresh_ciphertext), refresh_ciphertext),
    refresh_nonce = COALESCE(sqlc.narg(refresh_nonce), refresh_nonce),
    updated_at = sqlc.arg(now)::timestamptz
WHERE tenant_id = sqlc.arg(tenant_id) AND provider = 'google';

-- name: MarkNeedsReauth :exec
UPDATE calendar_accounts
SET status = 'needs_reauth', last_failure_at = sqlc.arg(now)::timestamptz,
    last_failure_code = 'refresh_rejected', updated_at = sqlc.arg(now)::timestamptz
WHERE tenant_id = sqlc.arg(tenant_id) AND provider = 'google';

-- name: StoreSyncResult :exec
UPDATE calendar_accounts
SET sync_token = sqlc.narg(sync_token),
    last_synced_at = sqlc.arg(now)::timestamptz,
    window_start = sqlc.arg(window_start)::timestamptz,
    window_end = sqlc.arg(window_end)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE tenant_id = sqlc.arg(tenant_id) AND provider = 'google';

-- ─── GET /v1/calendar/events/{id}/brief (MeetingBriefService) ───────────────

-- name: BriefEvent :one
SELECT id, title, notes, location, starts_at, ends_at, COALESCE(attendees::text, '')::text AS attendees
FROM calendar_events
WHERE tenant_id = sqlc.arg(tenant_id) AND id = sqlc.arg(id);

-- name: StoredBrief :one
SELECT memories::text AS memories FROM meeting_briefs
WHERE tenant_id = sqlc.arg(tenant_id) AND event_id = sqlc.arg(event_id);

-- name: BriefMemories :many
SELECT id, content, kind, created_at FROM memories
WHERE tenant_id = sqlc.arg(tenant_id)
  AND id = ANY(sqlc.arg(ids)::uuid[])
  AND review_state <> 'rejected';

-- name: UserForBrief :one
SELECT email, tier FROM users WHERE id = sqlc.arg(id);

-- name: SearchDocuments :many
-- MemoryRepository.semanticSearch without the space filter (the brief passes none).
SELECT id, origin_kind, (embedding <=> sqlc.arg(vec)::text::vector)::float8 AS distance
FROM memories
WHERE tenant_id = sqlc.arg(tenant_id)
  AND embedding IS NOT NULL
  AND review_state <> 'rejected'
ORDER BY distance ASC
LIMIT sqlc.arg(lim)::int;

-- name: SearchChunks :many
-- MemoryChunkRepository.hybridSearch without the space filter, selecting only
-- what the brief reads. A hit only the lexical arm found has NULL distance,
-- which Swift reports as 1.0.
WITH tsq AS (
    SELECT websearch_to_tsquery('english', sqlc.arg(query)::text) AS query
),
dense AS (
    SELECT chunk_id,
           ROW_NUMBER() OVER (ORDER BY embedding <=> sqlc.arg(vec)::text::vector ASC) AS rank,
           embedding <=> sqlc.arg(vec)::text::vector AS distance
    FROM memory_chunks
    WHERE tenant_id = sqlc.arg(tenant_id)
      AND embedding IS NOT NULL
    ORDER BY embedding <=> sqlc.arg(vec)::text::vector ASC
    LIMIT sqlc.arg(depth)::int
),
lexical AS (
    SELECT c.chunk_id,
           ROW_NUMBER() OVER (ORDER BY ts_rank_cd(c.content_tsv, tsq.query) DESC) AS rank
    FROM memory_chunks c
    CROSS JOIN tsq
    WHERE c.tenant_id = sqlc.arg(tenant_id)
      AND c.content_tsv @@ tsq.query
    ORDER BY ts_rank_cd(c.content_tsv, tsq.query) DESC
    LIMIT sqlc.arg(depth)::int
),
fused AS (
    SELECT COALESCE(d.chunk_id, l.chunk_id) AS chunk_id,
           COALESCE(1.0 / (60 + d.rank), 0) + COALESCE(1.0 / (60 + l.rank), 0) AS rrf,
           d.distance AS distance
    FROM dense d
    FULL OUTER JOIN lexical l ON d.chunk_id = l.chunk_id
)
SELECT c.memory_id, COALESCE(f.distance, 1.0)::float8 AS distance
FROM fused f
JOIN memory_chunks c ON c.chunk_id = f.chunk_id
JOIN memories m ON m.id = c.memory_id
WHERE c.tenant_id = sqlc.arg(tenant_id)
  AND m.tenant_id = sqlc.arg(tenant_id)
  AND m.review_state <> 'rejected'
ORDER BY f.rrf DESC, f.distance ASC NULLS LAST
LIMIT sqlc.arg(lim)::int;
