-- /v1/me fixture (parity/scenarios/me). One user per concern so each
-- meTodayByUser bucket (60/min, shared by every /v1/me read) stays small and
-- the per-process /v1/me/today cache never spans two scenario groups.
--
-- Rows the "today", "last 7 days" and "on this day" lanes must see are placed
-- relative to the seed time: T = start of today in UTC
-- (date_trunc('day', now(), 'UTC')). The CI run follows the seed within
-- minutes, and Swift and Go each run a scenario back to back.
--
--   me_ann  UTC: today, recap, resurface, learned, location
--   me_lia  America/Los_Angeles: local-day bucketing, location without a time
--   me_bad  timezone 'Mars/Olympus' (invalid, falls back to UTC), created_at NULL
--   me_new  no data, created at seed time
--   me_per  America/Los_Angeles: year and month in review (fixed 2026 dates)
--   me_cmt  commitments
--   me_brf  daily briefs and card feedback

INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at, timezone, tier)
VALUES
  ('ae000000-0000-4000-8000-0000000000a1', 'me_ann@parity.test', 'x', true, 'me_ann', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'pro'),
  ('ae000000-0000-4000-8000-0000000000b2', 'me_lia@parity.test', 'x', true, 'me_lia', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'America/Los_Angeles', 'trial'),
  ('ae000000-0000-4000-8000-0000000000c3', 'me_bad@parity.test', 'x', true, 'me_bad', NULL, NULL, 'Mars/Olympus', 'free'),
  ('ae000000-0000-4000-8000-0000000000d4', 'me_new@parity.test', 'x', true, 'me_new', now(), now(), 'UTC', 'trial'),
  ('ae000000-0000-4000-8000-0000000000e5', 'me_per@parity.test', 'x', true, 'me_per', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'America/Los_Angeles', 'pro'),
  ('ae000000-0000-4000-8000-0000000000f6', 'me_cmt@parity.test', 'x', true, 'me_cmt', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'pro'),
  ('ae000000-0000-4000-8000-0000000000a7', 'me_brf@parity.test', 'x', true, 'me_brf', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'pro');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
SELECT id, id, username || '''s Vault', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z'
FROM users WHERE username LIKE 'me\_%';

-- Last known location: me_ann complete, me_lia without a place or a time.
UPDATE users SET last_location_lat = 38.7223, last_location_lng = -9.1393,
  last_location_place = 'Lisbon, Portugal', last_location_at = '2026-10-30T07:00:00Z'
WHERE id = 'ae000000-0000-4000-8000-0000000000a1';
UPDATE users SET last_location_lat = 34.05, last_location_lng = -118.25
WHERE id = 'ae000000-0000-4000-8000-0000000000b2';

-- ─── me_ann ────────────────────────────────────────────────────────────────
INSERT INTO spaces (id, tenant_id, name, slug, created_at, updated_at)
VALUES
  ('ae05ace0-0000-4000-8000-0000000000a1', 'ae000000-0000-4000-8000-0000000000a1', 'Work', 'work', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ae05ace0-0000-4000-8000-0000000000a2', 'ae000000-0000-4000-8000-0000000000a1', 'Home', 'home', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO memories (id, tenant_id, content, created_at, updated_at, tags, review_state, origin_kind, kind,
                      last_reviewed_at, review_count, query_hit_count, next_review_at, resurface_muted, space_id)
VALUES
  -- today: a capture and (newest) a distilled decision still pending
  ('ae0e0000-0000-4000-8000-0000000000a1', 'ae000000-0000-4000-8000-0000000000a1', E'Moving the launch to Lisbon\nsecond line',
   date_trunc('day', now(), 'UTC') + interval '1 minute', NULL, '{launch}', 'auto', 'legacy', NULL, NULL, 0, 0, NULL, false, 'ae05ace0-0000-4000-8000-0000000000a1'),
  ('ae0e0000-0000-4000-8000-0000000000a2', 'ae000000-0000-4000-8000-0000000000a1', E'We ship on Tuesdays.\nBecause QA is Monday.',
   date_trunc('day', now(), 'UTC') + interval '2 minutes', NULL, NULL, 'pending', 'knowledge_compile', 'decision', NULL, 0, 0, NULL, false, NULL),
  -- three days ago: a long preference (snippet clipping) the user reviewed twice
  ('ae0e0000-0000-4000-8000-0000000000a3', 'ae000000-0000-4000-8000-0000000000a1',
   E'Prefers\n  quiet   mornings and long walks and long walks and long walks and long walks and long walks and long walks and long walks and long walks and long walks and long walks and long walks',
   now() - interval '3 days', NULL, NULL, 'auto', 'manual', 'preference', now() - interval '2 days', 2, 1, NULL, false, NULL),
  ('ae0e0000-0000-4000-8000-0000000000a4', 'ae000000-0000-4000-8000-0000000000a1', 'Already kept decision',
   now() - interval '3 days 1 hour', NULL, NULL, 'approved', 'legacy', 'decision', NULL, 0, 0, NULL, false, NULL),
  ('ae0e0000-0000-4000-8000-0000000000a5', 'ae000000-0000-4000-8000-0000000000a1', 'Forgotten decision',
   now() - interval '2 days', NULL, NULL, 'rejected', 'legacy', 'decision', NULL, 0, 0, NULL, false, NULL),
  ('ae0e0000-0000-4000-8000-0000000000a6', 'ae000000-0000-4000-8000-0000000000a1', 'Ten days old fact',
   now() - interval '10 days', NULL, NULL, 'auto', 'chat', 'fact', NULL, 0, 0, NULL, false, NULL),
  -- resurface: due, fading, and one excluded per reason
  ('ae0e0000-0000-4000-8000-0000000000a7', 'ae000000-0000-4000-8000-0000000000a1', 'Call the accountant',
   now() - interval '40 days', NULL, NULL, 'auto', 'legacy', NULL, now() - interval '5 days', 1, 0, now() - interval '1 day', false, NULL),
  ('ae0e0000-0000-4000-8000-0000000000a8', 'ae000000-0000-4000-8000-0000000000a1', 'We pay suppliers net 30',
   '2026-03-14T09:00:00Z', NULL, NULL, 'auto', 'legacy', 'decision', NULL, 0, 0, NULL, false, NULL),
  ('ae0e0000-0000-4000-8000-0000000000a9', 'ae000000-0000-4000-8000-0000000000a1', 'Aisle seats',
   '2026-01-02T09:00:00Z', NULL, NULL, 'auto', 'legacy', 'preference', '2026-05-02T09:00:00Z', 1, 0, NULL, false, NULL),
  ('ae0e0000-0000-4000-8000-0000000000aa', 'ae000000-0000-4000-8000-0000000000a1', 'muted',
   '2026-02-01T09:00:00Z', NULL, NULL, 'auto', 'legacy', 'decision', NULL, 0, 0, NULL, true, NULL),
  ('ae0e0000-0000-4000-8000-0000000000ab', 'ae000000-0000-4000-8000-0000000000a1', 'scheduled for later',
   '2026-02-01T09:00:00Z', NULL, NULL, 'auto', 'legacy', 'decision', NULL, 0, 0, now() + interval '10 days', false, NULL),
  ('ae0e0000-0000-4000-8000-0000000000ac', 'ae000000-0000-4000-8000-0000000000a1', 'just reviewed',
   '2026-02-01T09:00:00Z', NULL, NULL, 'auto', 'legacy', 'decision', now() - interval '1 hour', 1, 0, now() - interval '1 minute', false, NULL),
  ('ae0e0000-0000-4000-8000-0000000000ad', 'ae000000-0000-4000-8000-0000000000a1', 'unapproved chat memory',
   '2026-02-01T09:00:00Z', NULL, NULL, 'pending', 'chat', 'decision', NULL, 0, 0, NULL, false, NULL),
  -- on this day: a year, six months, a month and two years ago (09:00 UTC)
  ('ae0e0000-0000-4000-8000-0000000000b1', 'ae000000-0000-4000-8000-0000000000a1', 'Signed the lease',
   date_trunc('day', now(), 'UTC') - interval '1 year' + interval '9 hours', NULL, NULL, 'auto', 'legacy', NULL, NULL, 0, 0, NULL, false, NULL),
  ('ae0e0000-0000-4000-8000-0000000000b2', 'ae000000-0000-4000-8000-0000000000a1', 'six months',
   date_trunc('day', now(), 'UTC') - interval '6 months' + interval '9 hours', NULL, NULL, 'auto', 'legacy', NULL, NULL, 0, 0, NULL, false, NULL),
  ('ae0e0000-0000-4000-8000-0000000000b3', 'ae000000-0000-4000-8000-0000000000a1', 'a month',
   date_trunc('day', now(), 'UTC') - interval '1 month' + interval '9 hours', NULL, NULL, 'auto', 'legacy', NULL, NULL, 0, 0, NULL, false, NULL),
  ('ae0e0000-0000-4000-8000-0000000000b4', 'ae000000-0000-4000-8000-0000000000a1', 'two years',
   date_trunc('day', now(), 'UTC') - interval '2 years' + interval '9 hours', NULL, NULL, 'auto', 'legacy', NULL, NULL, 0, 0, NULL, false, NULL),
  -- the previous 7-day window (8 days ago)
  ('ae0e0000-0000-4000-8000-0000000000b5', 'ae000000-0000-4000-8000-0000000000a1', 'last week',
   date_trunc('day', now(), 'UTC') - interval '8 days' + interval '12 hours', NULL, NULL, 'auto', 'link', 'note', NULL, 0, 0, NULL, false, NULL);

INSERT INTO conversations (id, tenant_id, title, created_at, updated_at)
VALUES ('ae0c0000-0000-4000-8000-0000000000a1', 'ae000000-0000-4000-8000-0000000000a1', 'Plans',
        date_trunc('day', now(), 'UTC') + interval '3 minutes', date_trunc('day', now(), 'UTC') + interval '3 minutes');
INSERT INTO conversation_messages (id, conversation_id, role, content, created_at)
VALUES
  ('ae0d0000-0000-4000-8000-0000000000a1', 'ae0c0000-0000-4000-8000-0000000000a1', 'user', 'When do we ship?', date_trunc('day', now(), 'UTC') + interval '4 minutes'),
  ('ae0d0000-0000-4000-8000-0000000000a2', 'ae0c0000-0000-4000-8000-0000000000a1', 'assistant', 'Tuesdays.', date_trunc('day', now(), 'UTC') + interval '5 minutes'),
  ('ae0d0000-0000-4000-8000-0000000000a3', 'ae0c0000-0000-4000-8000-0000000000a1', 'user', 'And last week?', date_trunc('day', now(), 'UTC') - interval '9 days');

INSERT INTO analytics_events (id, vault_id, actor_user_id, event_name, source, occurred_at, dimensions)
VALUES ('ae0f0000-0000-4000-8000-0000000000a1', 'ae000000-0000-4000-8000-0000000000a1', 'ae000000-0000-4000-8000-0000000000a1',
        'memory_reviewed', 'server', date_trunc('day', now(), 'UTC') + interval '6 minutes', '{}');

INSERT INTO achievement_progress (id, tenant_id, achievement_key, progress_count, unlocked_at, created_at, updated_at)
VALUES
  ('ae0a0000-0000-4000-8000-0000000000a1', 'ae000000-0000-4000-8000-0000000000a1', 'lightbringer.first-spark', 3,
   date_trunc('day', now(), 'UTC') + interval '7 minutes', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ae0a0000-0000-4000-8000-0000000000a2', 'ae000000-0000-4000-8000-0000000000a1', 'legacy.retired-badge', 1,
   now() - interval '2 days 3 hours', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ae0a0000-0000-4000-8000-0000000000a3', 'ae000000-0000-4000-8000-0000000000a1', 'reignmaker.first-edict', 1,
   NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO health_events (id, tenant_id, event_type, value_numeric, unit, recorded_at, created_at)
VALUES
  ('ae0b0000-0000-4000-8000-0000000000a1', 'ae000000-0000-4000-8000-0000000000a1', 'steps', 1200, 'count', date_trunc('day', now(), 'UTC') + interval '10 minutes', now()),
  ('ae0b0000-0000-4000-8000-0000000000a2', 'ae000000-0000-4000-8000-0000000000a1', 'steps', 800.4, 'count', date_trunc('day', now(), 'UTC') + interval '20 minutes', now()),
  ('ae0b0000-0000-4000-8000-0000000000a3', 'ae000000-0000-4000-8000-0000000000a1', 'steps', 9999, 'count', date_trunc('day', now(), 'UTC') - interval '12 hours', now()),
  ('ae0b0000-0000-4000-8000-0000000000a4', 'ae000000-0000-4000-8000-0000000000a1', 'sleep_session', 450, 'minutes', date_trunc('day', now(), 'UTC') - interval '1 hour', now());

INSERT INTO self_improvement_changes (id, tenant_id, kind, state, trigger, title, summary, created_at)
VALUES
  ('ae05e1f0-0000-4000-8000-0000000000a1', 'ae000000-0000-4000-8000-0000000000a1', 'soul', 'pending', 'weekly', 'Runs on Sundays', 'Summary of Runs on Sundays', now() - interval '1 day'),
  ('ae05e1f0-0000-4000-8000-0000000000a2', 'ae000000-0000-4000-8000-0000000000a1', 'soul', 'pending', 'weekly', 'Prefers mornings', 'Summary of Prefers mornings', now() - interval '2 days'),
  ('ae05e1f0-0000-4000-8000-0000000000a3', 'ae000000-0000-4000-8000-0000000000a1', 'soul', 'approved', 'weekly', 'Approved', 'Summary', now() - interval '1 day'),
  ('ae05e1f0-0000-4000-8000-0000000000a4', 'ae000000-0000-4000-8000-0000000000a1', 'curator', 'pending', 'weekly', 'Curator', 'Summary', now() - interval '1 day'),
  ('ae05e1f0-0000-4000-8000-0000000000a5', 'ae000000-0000-4000-8000-0000000000a1', 'soul', 'pending', 'weekly', 'Too old', 'Summary', now() - interval '10 days'),
  ('ae05e1f0-0000-4000-8000-0000000000b1', 'b0b00000-0000-4000-8000-000000000002', 'soul', 'pending', 'weekly', 'Not mine', 'Summary', now() - interval '1 day');

-- ─── me_lia (America/Los_Angeles) ──────────────────────────────────────────
-- 23:30 local two days ago (the next UTC day), and 00:30 local a year ago
-- (the previous UTC day): both must land on the local day.
INSERT INTO memories (id, tenant_id, content, created_at, review_state, origin_kind, kind)
VALUES
  ('ae0e0000-0000-4000-8000-0000000000c1', 'ae000000-0000-4000-8000-0000000000b2', 'late',
   (date_trunc('day', now() AT TIME ZONE 'America/Los_Angeles') - interval '2 days' + interval '23 hours 30 minutes') AT TIME ZONE 'America/Los_Angeles',
   'auto', 'legacy', NULL),
  ('ae0e0000-0000-4000-8000-0000000000c2', 'ae000000-0000-4000-8000-0000000000b2', 'Local anniversary',
   (date_trunc('day', now() AT TIME ZONE 'America/Los_Angeles') - interval '1 year' + interval '30 minutes') AT TIME ZONE 'America/Los_Angeles',
   'auto', 'legacy', NULL),
  ('ae0e0000-0000-4000-8000-0000000000c3', 'ae000000-0000-4000-8000-0000000000b2', 'UTC anniversary only',
   (date_trunc('day', now() AT TIME ZONE 'America/Los_Angeles') - interval '1 year' - interval '7 hours') AT TIME ZONE 'America/Los_Angeles',
   'auto', 'legacy', NULL);

-- ─── me_bad (invalid timezone, no created_at) ──────────────────────────────
INSERT INTO memories (id, tenant_id, content, created_at, review_state, origin_kind, kind)
VALUES ('ae0e0000-0000-4000-8000-0000000000d1', 'ae000000-0000-4000-8000-0000000000c3', E'\n\n   Leading blank lines   \nthen more',
        date_trunc('day', now(), 'UTC') + interval '1 minute', 'auto', 'manual', 'note');

-- ─── me_per: year and month in review (America/Los_Angeles) ────────────────
INSERT INTO spaces (id, tenant_id, name, slug, created_at, updated_at)
VALUES ('ae05ace0-0000-4000-8000-0000000000e5', 'ae000000-0000-4000-8000-0000000000e5', 'Work', 'work', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- 2–4 March 2026, three captures a day at 08:00–10:00 PST.
INSERT INTO memories (id, tenant_id, content, created_at, tags, review_state, origin_kind, kind, space_id)
SELECT ('ae0e0e50-0000-4000-8000-0000000003' || lpad((d * 10 + h - 16)::text, 2, '0'))::uuid,
       'ae000000-0000-4000-8000-0000000000e5', 'Note ' || d || '-' || h || E'\nbody',
       make_timestamptz(2026, 3, d, h, 0, 0, 'UTC'),
       CASE WHEN d = 2 THEN ARRAY['Swift'] ELSE ARRAY['swift', 'side_project'] END,
       'auto', 'legacy', CASE WHEN h = 16 THEN 'decision' ELSE 'note' END, 'ae05ace0-0000-4000-8000-0000000000e5'
FROM generate_series(2, 4) AS d, generate_series(16, 18) AS h;

INSERT INTO memories (id, tenant_id, content, created_at, tags, review_state, origin_kind, kind)
VALUES
  -- 23:30 on 31 March in Los Angeles is 1 April in UTC: still March.
  ('ae0e0e50-0000-4000-8000-0000000000a1', 'ae000000-0000-4000-8000-0000000000e5', 'Late night', '2026-04-01T06:30:00Z', '{ }', 'auto', 'legacy', NULL),
  -- 23:30 on 28 February in Los Angeles is 1 March in UTC: February.
  ('ae0e0e50-0000-4000-8000-0000000000a2', 'ae000000-0000-4000-8000-0000000000e5', 'February', '2026-03-01T07:30:00Z', NULL, 'auto', 'legacy', NULL),
  -- distilled: counts as learned
  ('ae0e0e50-0000-4000-8000-0000000000a3', 'ae000000-0000-4000-8000-0000000000e5', 'Learned', '2026-03-03T20:00:00Z', NULL, 'auto', 'knowledge_compile', 'fact'),
  -- rejected: counts for nothing
  ('ae0e0e50-0000-4000-8000-0000000000a4', 'ae000000-0000-4000-8000-0000000000e5', 'Rejected', '2026-03-05T20:00:00Z', '{swift}', 'rejected', 'legacy', NULL),
  -- February: five more over three days
  ('ae0e0e50-0000-4000-8000-0000000002a1', 'ae000000-0000-4000-8000-0000000000e5', 'Feb a', '2026-02-10T18:00:00Z', NULL, 'auto', 'legacy', NULL),
  ('ae0e0e50-0000-4000-8000-0000000002a2', 'ae000000-0000-4000-8000-0000000000e5', 'Feb b', '2026-02-10T19:00:00Z', NULL, 'auto', 'legacy', NULL),
  ('ae0e0e50-0000-4000-8000-0000000002a3', 'ae000000-0000-4000-8000-0000000000e5', 'Feb c', '2026-02-11T18:00:00Z', NULL, 'auto', 'legacy', NULL),
  ('ae0e0e50-0000-4000-8000-0000000002a4', 'ae000000-0000-4000-8000-0000000000e5', 'Feb d', '2026-02-12T18:00:00Z', NULL, 'auto', 'link', NULL),
  ('ae0e0e50-0000-4000-8000-0000000002a5', 'ae000000-0000-4000-8000-0000000000e5', 'Feb e', '2026-02-12T19:00:00Z', NULL, 'auto', 'chat', NULL),
  -- January: five over three days
  ('ae0e0e50-0000-4000-8000-0000000001a1', 'ae000000-0000-4000-8000-0000000000e5', 'Jan a', '2026-01-05T18:00:00Z', '{running}', 'auto', 'legacy', NULL),
  ('ae0e0e50-0000-4000-8000-0000000001a2', 'ae000000-0000-4000-8000-0000000000e5', 'Jan b', '2026-01-05T19:00:00Z', '{running}', 'auto', 'legacy', NULL),
  ('ae0e0e50-0000-4000-8000-0000000001a3', 'ae000000-0000-4000-8000-0000000000e5', 'Jan c', '2026-01-06T18:00:00Z', NULL, 'auto', 'legacy', NULL),
  ('ae0e0e50-0000-4000-8000-0000000001a4', 'ae000000-0000-4000-8000-0000000000e5', 'Jan d', '2026-01-07T18:00:00Z', NULL, 'auto', 'legacy', NULL),
  ('ae0e0e50-0000-4000-8000-0000000001a5', 'ae000000-0000-4000-8000-0000000000e5', 'Jan e', '2026-01-07T19:00:00Z', NULL, 'auto', 'legacy', NULL);

INSERT INTO conversations (id, tenant_id, title, created_at, updated_at)
VALUES ('ae0c0000-0000-4000-8000-0000000000e5', 'ae000000-0000-4000-8000-0000000000e5', 'Plans', '2026-03-03T18:00:00Z', '2026-03-03T18:00:00Z');
INSERT INTO conversation_messages (id, conversation_id, role, content, created_at)
VALUES
  ('ae0d0000-0000-4000-8000-0000000000e1', 'ae0c0000-0000-4000-8000-0000000000e5', 'user', 'What did I decide?', '2026-03-03T18:05:00Z'),
  ('ae0d0000-0000-4000-8000-0000000000e2', 'ae0c0000-0000-4000-8000-0000000000e5', 'user', 'What did I decide?', '2026-03-04T18:05:00Z'),
  ('ae0d0000-0000-4000-8000-0000000000e3', 'ae0c0000-0000-4000-8000-0000000000e5', 'assistant', 'Swift.', '2026-03-04T18:06:00Z'),
  ('ae0d0000-0000-4000-8000-0000000000e4', 'ae0c0000-0000-4000-8000-0000000000e5', 'user', 'In February?', '2026-02-11T18:05:00Z');

INSERT INTO skill_run_log (id, tenant_id, source, name, started_at, status)
VALUES
  ('ae05c111-0000-4000-8000-0000000000e1', 'ae000000-0000-4000-8000-0000000000e5', 'cron', 'daily-brief', '2026-03-10T12:00:00Z', 'ok'),
  ('ae05c111-0000-4000-8000-0000000000e2', 'ae000000-0000-4000-8000-0000000000e5', 'cron', 'daily-brief', '2026-03-11T12:00:00Z', 'ok'),
  ('ae05c111-0000-4000-8000-0000000000e3', 'ae000000-0000-4000-8000-0000000000e5', 'manual', 'weekly-memo', '2026-03-12T12:00:00Z', 'ok');

INSERT INTO achievement_progress (id, tenant_id, achievement_key, progress_count, unlocked_at, created_at, updated_at)
VALUES
  ('ae0a0000-0000-4000-8000-0000000000e1', 'ae000000-0000-4000-8000-0000000000e5', 'lightbringer.first-spark', 3, '2026-03-02T17:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ae0a0000-0000-4000-8000-0000000000e2', 'ae000000-0000-4000-8000-0000000000e5', 'legacy.retired-badge', 1, '2026-03-20T17:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Themes cache: March ready, February a spent fallback, January an abandoned
-- claim with no attempts left. None of them is claimed again by a GET.
INSERT INTO recap_themes (user_id, period_key, status, headline, sentence, themes, attempts, requested_at, generated_at)
VALUES
  ('ae000000-0000-4000-8000-0000000000e5', '2026-03', 'ready', 'Early bird builder', 'You wrote about Swift before breakfast.', '{swift,mornings}', 1, NULL, '2026-04-02T10:00:00Z'),
  ('ae000000-0000-4000-8000-0000000000e5', '2026-02', 'fallback', NULL, NULL, '{}', 3, NULL, now() - interval '2 hours'),
  ('ae000000-0000-4000-8000-0000000000e5', '2026-01', 'pending', NULL, NULL, '{}', 3, now() - interval '1 hour', NULL);

-- ─── me_cmt: commitments ───────────────────────────────────────────────────
INSERT INTO commitments (id, tenant_id, memory_id, text, normalized_text, due_at, state, surfaced_count, created_at, done_at)
VALUES
  ('ae0c0331-0000-4000-8000-0000000000a1', 'ae000000-0000-4000-8000-0000000000f6', NULL, 'Send Ana the deck', 'send ana the deck', now() + interval '1 day', 'open', 1, '2026-10-01T09:00:00Z', NULL),
  ('ae0c0331-0000-4000-8000-0000000000a2', 'ae000000-0000-4000-8000-0000000000f6', NULL, 'Renew passport', 'renew passport', NULL, 'open', 0, '2026-09-01T09:00:00Z', NULL),
  ('ae0c0331-0000-4000-8000-0000000000a3', 'ae000000-0000-4000-8000-0000000000f6', NULL, 'Call the bank', 'call the bank', '2026-09-20T12:00:00Z', 'done', 3, '2026-09-05T09:00:00Z', '2026-10-10T09:00:00Z'),
  ('ae0c0331-0000-4000-8000-0000000000a4', 'ae000000-0000-4000-8000-0000000000f6', NULL, 'Old promise', 'old promise', NULL, 'dropped', 2, '2026-08-05T09:00:00Z', '2026-10-12T09:00:00Z'),
  ('ae0c0331-0000-4000-8000-0000000000a5', 'ae000000-0000-4000-8000-0000000000f6', NULL, 'Renew passport.', 'renew passport', NULL, 'done', 0, '2026-07-01T09:00:00Z', '2026-07-15T09:00:00Z'),
  ('ae0c0331-0000-4000-8000-0000000000a6', 'ae000000-0000-4000-8000-0000000000f6', NULL, 'Pay rent', 'pay rent', '2026-08-01T12:00:00Z', 'open', 3, '2026-07-20T09:00:00Z', NULL),
  ('ae0c0331-0000-4000-8000-0000000000b1', 'b0b00000-0000-4000-8000-000000000002', NULL, 'Not mine', 'not mine', NULL, 'open', 0, '2026-10-01T09:00:00Z', NULL);

-- ─── me_brf: briefs ────────────────────────────────────────────────────────
INSERT INTO memories (id, tenant_id, content, created_at, review_state, origin_kind, kind, last_reviewed_at, review_count, next_review_at)
VALUES ('ae0e0000-0000-4000-8000-0000000000f1', 'ae000000-0000-4000-8000-0000000000a7', 'Revisit me', '2026-09-01T09:00:00Z', 'auto', 'legacy', NULL, NULL, 0, '2026-10-01T09:00:00Z');

INSERT INTO briefs (id, tenant_id, date, cards, end_note, created_at, updated_at)
VALUES
  ('ae0b71ef-0000-4000-8000-0000000000a1', 'ae000000-0000-4000-8000-0000000000a7', (now() AT TIME ZONE 'UTC')::date,
   '[{"id":"openLoops","kind":"openLoops","title":"Open loops","body":"- Pay rent","why":"Things you said you''d do","refs":[{"kind":"commitment","id":"AE0C0331-0000-4000-8000-0000000000A6"}]},
     {"id":"toRevisit","kind":"toRevisit","title":"To revisit","body":"Revisit me","why":"You asked to see this again","refs":[{"kind":"memory","id":"AE0E0000-0000-4000-8000-0000000000F1"},{"kind":"memory","id":"not-a-uuid"},{"kind":"horoscope","id":"x"}]},
     {"id":"weather","kind":"weather","title":"Weather","body":"Dry.","why":"For planning your day","refs":[],"extra":1},
     {"id":"horoscope","kind":"horoscope","title":"Stars","body":"Good day.","refs":[]}]',
   'That''s it for today.', now() - interval '1 hour', now() - interval '1 hour'),
  -- a card without refs: the whole list decodes as []
  ('ae0b71ef-0000-4000-8000-0000000000a2', 'ae000000-0000-4000-8000-0000000000a7', '2026-10-01',
   '[{"id":"weather","kind":"weather","title":"Weather","body":"Dry."}]', 'That''s it for today.', '2026-10-01T07:00:00Z', '2026-10-01T07:00:00Z'),
  ('ae0b71ef-0000-4000-8000-0000000000a3', 'ae000000-0000-4000-8000-0000000000a7', '2026-09-30',
   '{"not":"an array"}', 'That''s it for today.', '2026-09-30T07:00:00Z', '2026-09-30T07:00:00Z'),
  ('ae0b71ef-0000-4000-8000-0000000000b1', 'b0b00000-0000-4000-8000-000000000002', '2026-10-01',
   '[{"id":"weather","kind":"weather","title":"Weather","body":"Rain.","refs":[]}]', 'That''s it for today.', '2026-10-01T07:00:00Z', '2026-10-01T07:00:00Z');

INSERT INTO brief_feedback (id, tenant_id, brief_id, card_id, card_kind, action, created_at)
VALUES ('ae0feedb-0000-4000-8000-0000000000a1', 'ae000000-0000-4000-8000-0000000000a7', 'ae0b71ef-0000-4000-8000-0000000000a1', 'weather', 'weather', 'down', '2026-10-01T07:00:00Z');
