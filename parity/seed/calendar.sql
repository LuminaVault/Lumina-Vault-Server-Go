-- Calendar and mail fixtures. Loaded after base.sql by scripts/parity-up.sh.
--
-- No row here may make a server call Google: the parity environment has no
-- Google. So a `connected` row is only used on paths that never need its token
-- (status, and disconnect when the grant also serves the other product), and
-- revoke-and-delete is exercised on `needs_reauth` rows, whose token lookup fails
-- before any network call. Token columns stay NULL.
--
--   cal_ann  connected Calendar grant; events to list; sync consent
--   cal_ben  nothing: no consent, no grant, no events
--   cal_wid  connected through the broader scopes (calendar, mail.google.com)
--   cal_two  connected Calendar + Gmail; disconnects Calendar
--   cal_rea  needs_reauth Calendar only; disconnects (row deleted)
--   cal_gre  needs_reauth Gmail only; disconnects Gmail (row deleted)
--   cal_gmo  connected Gmail only
--   cal_syn  sync writes
--   cal_off  a consent row that says no
--   cal_bri  briefs: stored, live, rejected and foreign notes
--   cal_bal  briefs: the search arms, the 40% quota, the floor
--   cal_cap  briefs: monthly embedding cap used up
--   cal_quo  briefs: the 40% quota on prior model answers

INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at, timezone, tier)
VALUES
  ('ca1e0000-0000-4000-8000-0000000000a1', 'cal_ann@parity.test', 'x', true, 'cal_ann', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'pro'),
  ('ca1e0000-0000-4000-8000-0000000000b2', 'cal_ben@parity.test', 'x', true, 'cal_ben', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'trial'),
  ('ca1e0000-0000-4000-8000-0000000000c3', 'cal_wid@parity.test', 'x', true, 'cal_wid', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'trial'),
  ('ca1e0000-0000-4000-8000-0000000000d4', 'cal_two@parity.test', 'x', true, 'cal_two', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'trial'),
  ('ca1e0000-0000-4000-8000-0000000000e5', 'cal_rea@parity.test', 'x', true, 'cal_rea', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'trial'),
  ('ca1e0000-0000-4000-8000-0000000000f6', 'cal_gre@parity.test', 'x', true, 'cal_gre', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'trial'),
  ('ca1e0000-0000-4000-8000-0000000000a7', 'cal_gmo@parity.test', 'x', true, 'cal_gmo', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'trial'),
  ('ca1e0000-0000-4000-8000-0000000000b8', 'cal_syn@parity.test', 'x', true, 'cal_syn', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'trial'),
  ('ca1e0000-0000-4000-8000-0000000000c9', 'cal_off@parity.test', 'x', true, 'cal_off', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'trial'),
  ('ca1e0000-0000-4000-8000-0000000000da', 'cal_bri@parity.test', 'x', true, 'cal_bri', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'pro'),
  ('ca1e0000-0000-4000-8000-0000000000eb', 'cal_bal@parity.test', 'x', true, 'cal_bal', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'trial'),
  ('ca1e0000-0000-4000-8000-0000000000fc', 'cal_cap@parity.test', 'x', true, 'cal_cap', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'trial'),
  ('ca1e0000-0000-4000-8000-0000000000ad', 'cal_quo@parity.test', 'x', true, 'cal_quo', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z', 'UTC', 'trial');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
SELECT id, id, 'Personal', '2025-06-01T00:00:00Z', '2025-06-01T00:00:00Z'
FROM users WHERE username LIKE 'cal\_%';

INSERT INTO apple_consent (tenant_id, domain, allowed, allow_writes, last_sync_at, updated_at)
VALUES
  ('ca1e0000-0000-4000-8000-0000000000a1', 'calendar', true, false, NULL, '2026-01-05T00:00:00Z'),
  ('ca1e0000-0000-4000-8000-0000000000b8', 'calendar', true, true, '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('ca1e0000-0000-4000-8000-0000000000c9', 'calendar', false, false, NULL, '2026-01-05T00:00:00Z');

INSERT INTO calendar_accounts (id, tenant_id, provider, account_email, scope, sync_token, last_synced_at, status, last_failure_at, last_failure_code, created_at, updated_at)
VALUES
  ('ca1a0000-0000-4000-8000-000000000001', 'ca1e0000-0000-4000-8000-0000000000a1', 'google', 'ann@gmail.test',
   'https://www.googleapis.com/auth/calendar.events openid email', 'tok-ann', '2026-02-01T08:00:00Z', 'connected', NULL, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1a0000-0000-4000-8000-000000000003', 'ca1e0000-0000-4000-8000-0000000000c3', 'google', 'wid@gmail.test',
   'https://www.googleapis.com/auth/calendar https://mail.google.com/ openid https://www.googleapis.com/auth/gmail.modify', NULL, NULL, 'connected', NULL, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1a0000-0000-4000-8000-000000000004', 'ca1e0000-0000-4000-8000-0000000000d4', 'google', 'two@gmail.test',
   'openid  https://www.googleapis.com/auth/calendar.events https://www.googleapis.com/auth/calendar https://www.googleapis.com/auth/gmail.readonly email', 'tok-two', '2026-02-01T08:00:00Z', 'connected', NULL, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1a0000-0000-4000-8000-000000000005', 'ca1e0000-0000-4000-8000-0000000000e5', 'google', 'rea@gmail.test',
   'https://www.googleapis.com/auth/calendar.events openid email', NULL, '2026-02-01T08:00:00Z', 'needs_reauth', '2026-02-02T08:00:00Z', 'refresh_rejected', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1a0000-0000-4000-8000-000000000006', 'ca1e0000-0000-4000-8000-0000000000f6', 'google', 'gre@gmail.test',
   'https://www.googleapis.com/auth/gmail.readonly openid email', NULL, NULL, 'needs_reauth', '2026-02-02T08:00:00Z', 'refresh_rejected', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1a0000-0000-4000-8000-000000000007', 'ca1e0000-0000-4000-8000-0000000000a7', 'google', 'gmo@gmail.test',
   'https://www.googleapis.com/auth/gmail.readonly openid email', NULL, NULL, 'connected', NULL, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO calendar_events (id, tenant_id, source, external_id, calendar_id, title, notes, location, starts_at, ends_at, all_day, status, organizer, attendees, html_link, remote_updated_at, created_at, updated_at)
VALUES
  -- cal_ann: listed in start order. Not listed: finished, cancelled.
  ('ca1b0000-0000-4000-8000-000000000001', 'ca1e0000-0000-4000-8000-0000000000a1', 'google', 'g-3', NULL, 'Third', NULL, NULL, '2099-03-01T10:00:00Z', '2099-03-01T11:00:00Z', false, 'confirmed', NULL, NULL, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1b0000-0000-4000-8000-000000000002', 'ca1e0000-0000-4000-8000-0000000000a1', 'google', 'g-1', NULL, 'First', 'Bring the deck', 'Room 4', '2099-01-01T10:00:00Z', '2099-01-01T11:00:00Z', false, 'confirmed', 'boss@gmail.test', '{"list":[{"email":"boss@gmail.test","displayName":"Boss","responseStatus":"accepted"}]}', 'https://calendar.test/e1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1b0000-0000-4000-8000-000000000003', 'ca1e0000-0000-4000-8000-0000000000a1', 'apple_eventkit', 'ek-2', 'work', 'Second (all day)', NULL, NULL, '2099-02-01T00:00:00Z', '2099-02-02T00:00:00Z', true, 'confirmed', NULL, NULL, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1b0000-0000-4000-8000-000000000004', 'ca1e0000-0000-4000-8000-0000000000a1', 'google', 'g-past', NULL, 'Finished', NULL, NULL, '2020-01-01T10:00:00Z', '2020-01-01T11:00:00Z', false, 'confirmed', NULL, NULL, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1b0000-0000-4000-8000-000000000005', 'ca1e0000-0000-4000-8000-0000000000a1', 'google', 'g-can', NULL, 'Cancelled', NULL, NULL, '2099-01-02T10:00:00Z', '2099-01-02T11:00:00Z', false, 'cancelled', NULL, NULL, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  -- Disconnect purges google rows only.
  ('ca1b0000-0000-4000-8000-000000000011', 'ca1e0000-0000-4000-8000-0000000000d4', 'google', 'two-g', NULL, 'Two google', NULL, NULL, '2099-01-01T10:00:00Z', '2099-01-01T11:00:00Z', false, 'confirmed', NULL, NULL, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1b0000-0000-4000-8000-000000000012', 'ca1e0000-0000-4000-8000-0000000000d4', 'apple_eventkit', 'two-a', NULL, 'Two apple', NULL, NULL, '2099-01-01T12:00:00Z', '2099-01-01T13:00:00Z', false, 'confirmed', NULL, NULL, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1b0000-0000-4000-8000-000000000013', 'ca1e0000-0000-4000-8000-0000000000e5', 'google', 'rea-g', NULL, 'Rea google', NULL, NULL, '2099-01-01T10:00:00Z', '2099-01-01T11:00:00Z', false, 'confirmed', NULL, NULL, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1b0000-0000-4000-8000-000000000014', 'ca1e0000-0000-4000-8000-0000000000f6', 'google', 'gre-g', NULL, 'Gre google stays', NULL, NULL, '2099-01-01T10:00:00Z', '2099-01-01T11:00:00Z', false, 'confirmed', NULL, NULL, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1b0000-0000-4000-8000-000000000015', 'ca1e0000-0000-4000-8000-0000000000a7', 'google', 'gmo-g', NULL, 'Gmo google', NULL, NULL, '2099-01-01T10:00:00Z', '2099-01-01T11:00:00Z', false, 'confirmed', NULL, NULL, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  -- cal_syn: last-writer-wins needs a row with a known timestamp, and one with attendees.
  ('ca1b0000-0000-4000-8000-000000000021', 'ca1e0000-0000-4000-8000-0000000000b8', 'apple_eventkit', 'seed-1', 'home', 'Seeded', 'n', 'l', '2026-05-01T10:00:00Z', '2026-05-01T11:00:00Z', false, 'confirmed', 'o@x.test', '{"list":[{"email":"keep@x.test","displayName":"Keep"}]}', NULL, '2026-03-01T10:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  -- The same external id under the google source must not collide.
  ('ca1b0000-0000-4000-8000-000000000022', 'ca1e0000-0000-4000-8000-0000000000b8', 'google', 'seed-1', NULL, 'Google twin', NULL, NULL, '2026-05-01T10:00:00Z', '2026-05-01T11:00:00Z', false, 'confirmed', NULL, NULL, NULL, '2026-03-01T10:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- The deterministic embedder's vector for a text: bytes mapped to b/127.5 - 1.
-- pg_temp, so it does not outlive this seed.
CREATE FUNCTION pg_temp.det(q text) RETURNS vector LANGUAGE sql AS $$
  SELECT ('[' || string_agg((get_byte(convert_to(q, 'UTF8'), (i % octet_length(convert_to(q, 'UTF8'))))::float8 / 127.5 - 1.0)::text, ',' ORDER BY i) || ']')::vector
  FROM generate_series(0, 1535) AS i
$$;

-- det(q) with its first `n` dimensions zeroed: further away as n grows.
CREATE FUNCTION pg_temp.near(q text, n int) RETURNS vector LANGUAGE sql AS $$
  SELECT ('[' || string_agg(CASE WHEN i < n THEN '0' ELSE (get_byte(convert_to(q, 'UTF8'), (i % octet_length(convert_to(q, 'UTF8'))))::float8 / 127.5 - 1.0)::text END, ',' ORDER BY i) || ']')::vector
  FROM generate_series(0, 1535) AS i
$$;

-- A vector on one axis, far from anything det() makes.
CREATE FUNCTION pg_temp.axis(k int) RETURNS vector LANGUAGE sql AS $$
  SELECT ('[' || string_agg(CASE WHEN i = k THEN '1' ELSE '0' END, ',' ORDER BY i) || ']')::vector
  FROM generate_series(0, 1535) AS i
$$;

-- ─── cal_bri ────────────────────────────────────────────────────────────────
INSERT INTO calendar_events (id, tenant_id, source, external_id, title, notes, location, starts_at, ends_at, attendees, remote_updated_at, created_at, updated_at)
VALUES
  ('ca1c0000-0000-4000-8000-000000000001', 'ca1e0000-0000-4000-8000-0000000000da', 'apple_eventkit', 'b-q4', 'Q4 plan', NULL, 'Room 1', '2099-01-01T10:00:00Z', '2099-01-01T10:30:00Z',
   '{"list":[{"email":"ana@x.test","displayName":"Ana Silva"},{"email":"CAL_BRI@parity.test","displayName":"Myself"},{"email":"nobody@x.test"},{"email":"ana2@x.test","displayName":" Ana   Silva "}]}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1c0000-0000-4000-8000-000000000002', 'ca1e0000-0000-4000-8000-0000000000da', 'apple_eventkit', 'b-stored', 'Garden', NULL, NULL, '2099-01-02T10:00:00Z', '2099-01-02T10:30:00Z', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1c0000-0000-4000-8000-000000000003', 'ca1e0000-0000-4000-8000-0000000000da', 'apple_eventkit', 'b-filter', 'Filtered', NULL, NULL, '2099-01-03T10:00:00Z', '2099-01-03T10:30:00Z', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1c0000-0000-4000-8000-000000000004', 'ca1e0000-0000-4000-8000-0000000000da', 'apple_eventkit', 'b-bad1', 'Dentist', NULL, NULL, '2099-01-04T10:00:00Z', '2099-01-04T10:30:00Z', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1c0000-0000-4000-8000-000000000005', 'ca1e0000-0000-4000-8000-0000000000da', 'apple_eventkit', 'b-bad2', 'Dentist two', NULL, NULL, '2099-01-05T10:00:00Z', '2099-01-05T10:30:00Z', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1c0000-0000-4000-8000-000000000006', 'ca1e0000-0000-4000-8000-0000000000da', 'apple_eventkit', 'b-empty', 'Dentist three', NULL, NULL, '2099-01-06T10:00:00Z', '2099-01-06T10:30:00Z', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1c0000-0000-4000-8000-000000000007', 'ca1e0000-0000-4000-8000-0000000000da', 'apple_eventkit', 'b-blank', '   ', '  ', NULL, '2099-01-07T10:00:00Z', '2099-01-07T10:30:00Z', '{"list":[]}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1c0000-0000-4000-8000-000000000008', 'ca1e0000-0000-4000-8000-0000000000da', 'apple_eventkit', 'b-badatt', 'Bad attendees', NULL, NULL, '2099-01-08T10:00:00Z', '2099-01-08T10:30:00Z', '{"other":1}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO memories (id, tenant_id, content, created_at, embedding, review_state, kind, origin_kind)
VALUES
  ('ca1d0000-0000-4000-8000-000000000001', 'ca1e0000-0000-4000-8000-0000000000da', 'Ana wants hiring in the Q4 plan', '2026-01-10T09:00:00Z', pg_temp.det(E'Q4 plan\nAna Silva'), 'auto', 'decision', 'legacy'),
  ('ca1d0000-0000-4000-8000-000000000002', 'ca1e0000-0000-4000-8000-0000000000da', 'Rejected: Ana', '2026-01-10T09:00:00Z', pg_temp.det(E'Q4 plan\nAna Silva'), 'rejected', 'fact', 'legacy'),
  ('ca1d0000-0000-4000-8000-000000000003', 'ca1e0000-0000-4000-8000-0000000000da', 'Garden plans', '2026-01-11T09:00:00Z', pg_temp.axis(7), 'auto', NULL, 'legacy'),
  ('ca1d0000-0000-4000-8000-000000000004', 'ca1e0000-0000-4000-8000-0000000000da', E'A   long\n\nnote ' || repeat('about the plan ', 20), NULL, pg_temp.axis(8), 'auto', 'fact', 'legacy'),
  ('ca1d0000-0000-4000-8000-000000000005', 'ca1e0000-0000-4000-8000-0000000000da', 'Rejected after the push', '2026-01-12T09:00:00Z', pg_temp.axis(9), 'rejected', 'fact', 'legacy'),
  -- Another tenant's note: a stored brief naming it must not show it.
  ('ca1d0000-0000-4000-8000-000000000006', 'ca1e0000-0000-4000-8000-0000000000eb', 'Foreign note', '2026-01-12T09:00:00Z', pg_temp.axis(10), 'auto', 'fact', 'legacy');

INSERT INTO meeting_briefs (tenant_id, event_id, claimed_at, sent_at, outcome, memories)
VALUES
  ('ca1e0000-0000-4000-8000-0000000000da', 'ca1c0000-0000-4000-8000-000000000002', '2026-01-20T00:00:00Z', '2026-01-20T00:00:00Z', 'sent', '[{"id":"ca1d0000-0000-4000-8000-000000000003","similarity":0.77}]'),
  ('ca1e0000-0000-4000-8000-0000000000da', 'ca1c0000-0000-4000-8000-000000000003', '2026-01-20T00:00:00Z', '2026-01-20T00:00:00Z', 'sent',
   '[{"id":"ca1d0000-0000-4000-8000-000000000005","similarity":0.9},{"id":"ca1d0000-0000-4000-8000-000000000006","similarity":0.8},{"id":"ca1d0000-0000-4000-8000-000000000004","similarity":0.61},{"id":"ca1d0000-0000-4000-8000-000000000099","similarity":0.6}]'),
  ('ca1e0000-0000-4000-8000-0000000000da', 'ca1c0000-0000-4000-8000-000000000004', '2026-01-20T00:00:00Z', NULL, 'failed', '{"oops":1}'),
  ('ca1e0000-0000-4000-8000-0000000000da', 'ca1c0000-0000-4000-8000-000000000005', '2026-01-20T00:00:00Z', NULL, 'failed', '[{"id":"not-a-uuid","similarity":1}]'),
  ('ca1e0000-0000-4000-8000-0000000000da', 'ca1c0000-0000-4000-8000-000000000006', '2026-01-20T00:00:00Z', NULL, 'empty', '[]');

-- ─── cal_bal: event title "Sync notes", query text "Sync notes" ─────────────
INSERT INTO calendar_events (id, tenant_id, source, external_id, title, notes, starts_at, ends_at, attendees, remote_updated_at, created_at, updated_at)
VALUES
  ('ca1c0000-0000-4000-8000-000000000011', 'ca1e0000-0000-4000-8000-0000000000eb', 'apple_eventkit', 'bal-1', 'Sync notes', NULL, '2099-02-01T10:00:00Z', '2099-02-01T10:30:00Z', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1c0000-0000-4000-8000-000000000012', 'ca1e0000-0000-4000-8000-0000000000eb', 'apple_eventkit', 'bal-2', 'Retro', E'Bring\nthe  notes', '2099-02-02T10:00:00Z', '2099-02-02T10:30:00Z', '{"list":[{"displayName":"Rui Costa"},{"displayName":"Ána Lima"}]}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO memories (id, tenant_id, content, created_at, embedding, review_state, kind, origin_kind)
SELECT ('ca1d1000-0000-4000-8000-0000000000' || lpad(to_hex(n), 2, '0'))::uuid,
       'ca1e0000-0000-4000-8000-0000000000eb',
       'Fact number ' || n, '2026-01-01T00:00:00Z'::timestamptz + (n || ' hours')::interval,
       pg_temp.near('Sync notes', 40 * n), 'auto', 'fact', 'legacy'
FROM generate_series(1, 8) AS n;

INSERT INTO memories (id, tenant_id, content, created_at, embedding, review_state, kind, origin_kind)
SELECT ('ca1d2000-0000-4000-8000-0000000000' || lpad(to_hex(n), 2, '0'))::uuid,
       'ca1e0000-0000-4000-8000-0000000000eb',
       'Model answer ' || n, '2026-01-02T00:00:00Z'::timestamptz + (n || ' hours')::interval,
       pg_temp.near('Sync notes', 10 * n), 'auto', 'preference', CASE WHEN n % 2 = 0 THEN 'chat' ELSE 'query' END
FROM generate_series(1, 6) AS n;

-- Two identical vectors, ordered by UPPERCASE id string; a note past the floor.
INSERT INTO memories (id, tenant_id, content, created_at, embedding, review_state, kind, origin_kind)
VALUES
  ('ca1d3000-0000-4000-8000-00000000000b', 'ca1e0000-0000-4000-8000-0000000000eb', 'Twin b', '2026-01-03T00:00:00Z', pg_temp.det('Sync notes'), 'auto', 'fact', 'legacy'),
  ('ca1d3000-0000-4000-8000-00000000000a', 'ca1e0000-0000-4000-8000-0000000000eb', 'Twin a', '2026-01-03T00:00:00Z', pg_temp.det('Sync notes'), 'auto', 'fact', 'legacy'),
  ('ca1d3000-0000-4000-8000-00000000000c', 'ca1e0000-0000-4000-8000-0000000000eb', 'Far away', '2026-01-03T00:00:00Z', pg_temp.axis(3), 'auto', 'fact', 'legacy'),
  -- Exact document vector, but a lexical-only chunk: the chunk hit (distance 1.0)
  -- wins the merge, so the floor drops it.
  ('ca1d3000-0000-4000-8000-00000000000d', 'ca1e0000-0000-4000-8000-0000000000eb', 'Hidden by its chunk', '2026-01-03T00:00:00Z', pg_temp.det('Sync notes'), 'auto', 'fact', 'legacy'),
  -- No document vector: only its chunk can find it.
  ('ca1d3000-0000-4000-8000-00000000000e', 'ca1e0000-0000-4000-8000-0000000000eb', 'Found by its chunk', '2026-01-03T00:00:00Z', NULL, 'auto', 'decision', 'legacy'),
  ('ca1d3000-0000-4000-8000-00000000000f', 'ca1e0000-0000-4000-8000-0000000000eb', 'Rejected chunk owner', '2026-01-03T00:00:00Z', NULL, 'rejected', 'fact', 'legacy'),
  -- For the "Retro" event: names counted in contents, accents folded, whole words.
  ('ca1d3000-0000-4000-8000-000000000011', 'ca1e0000-0000-4000-8000-0000000000eb', 'ana said yes to Rui', '2026-01-04T00:00:00Z', pg_temp.det(E'Retro\nRui Costa, Ána Lima\nBring the notes'), 'auto', 'fact', 'legacy'),
  ('ca1d3000-0000-4000-8000-000000000012', 'ca1e0000-0000-4000-8000-0000000000eb', 'Banana bread for ANA', '2026-01-04T00:00:00Z', pg_temp.det(E'Retro\nRui Costa, Ána Lima\nBring the notes'), 'auto', 'fact', 'legacy');

INSERT INTO memory_chunks (chunk_id, document_id, tenant_id, memory_id, ordinal, start_line, end_line, text, content_sha256, embedding)
VALUES
  ('cal-chunk-lexical', 'cal-doc-1', 'ca1e0000-0000-4000-8000-0000000000eb', 'ca1d3000-0000-4000-8000-00000000000d', 0, 1, 1, 'Sync notes for the lexical arm', 'x', NULL),
  ('cal-chunk-dense', 'cal-doc-2', 'ca1e0000-0000-4000-8000-0000000000eb', 'ca1d3000-0000-4000-8000-00000000000e', 0, 1, 1, 'Dense only words', 'x', pg_temp.near('Sync notes', 5)),
  ('cal-chunk-rejected', 'cal-doc-3', 'ca1e0000-0000-4000-8000-0000000000eb', 'ca1d3000-0000-4000-8000-00000000000f', 0, 1, 1, 'Sync notes rejected', 'x', pg_temp.det('Sync notes'));

-- ─── cal_cap ────────────────────────────────────────────────────────────────
INSERT INTO embedding_usage (id, tenant_id, year_month, tokens_used, created_at, updated_at)
VALUES ('ca1d4000-0000-4000-8000-000000000001', 'ca1e0000-0000-4000-8000-0000000000fc', to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM'), 2000000, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO calendar_events (id, tenant_id, source, external_id, title, starts_at, ends_at, remote_updated_at, created_at, updated_at)
VALUES
  ('ca1c0000-0000-4000-8000-000000000021', 'ca1e0000-0000-4000-8000-0000000000fc', 'apple_eventkit', 'cap-1', 'Needs an embed', '2099-03-01T10:00:00Z', '2099-03-01T10:30:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1c0000-0000-4000-8000-000000000022', 'ca1e0000-0000-4000-8000-0000000000fc', 'apple_eventkit', 'cap-2', 'Stored brief', '2099-03-02T10:00:00Z', '2099-03-02T10:30:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca1c0000-0000-4000-8000-000000000023', 'ca1e0000-0000-4000-8000-0000000000fc', 'apple_eventkit', 'cap-3', ' ', '2099-03-03T10:00:00Z', '2099-03-03T10:30:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO memories (id, tenant_id, content, created_at, embedding, review_state, kind, origin_kind)
VALUES ('ca1d3000-0000-4000-8000-000000000021', 'ca1e0000-0000-4000-8000-0000000000fc', 'Stored note', '2026-01-05T00:00:00Z', pg_temp.axis(2), 'auto', 'note', 'legacy');

INSERT INTO meeting_briefs (tenant_id, event_id, claimed_at, sent_at, outcome, memories)
VALUES ('ca1e0000-0000-4000-8000-0000000000fc', 'ca1c0000-0000-4000-8000-000000000022', '2026-01-20T00:00:00Z', '2026-01-20T00:00:00Z', 'sent', '[{"id":"ca1d3000-0000-4000-8000-000000000021","similarity":0.5}]');

-- ─── cal_quo: seven facts, six closer model answers ─────────────────────────
-- The search keeps six facts and four answers. The nearest five after that are
-- four answers and a fact, never a fifth answer.
INSERT INTO calendar_events (id, tenant_id, source, external_id, title, starts_at, ends_at, remote_updated_at, created_at, updated_at)
VALUES ('ca1c0000-0000-4000-8000-000000000031', 'ca1e0000-0000-4000-8000-0000000000ad', 'apple_eventkit', 'quo-1', 'Quota', '2099-04-01T10:00:00Z', '2099-04-01T10:30:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO memories (id, tenant_id, content, created_at, embedding, review_state, kind, origin_kind)
SELECT ('ca1d5000-0000-4000-8000-0000000000' || lpad(to_hex(n), 2, '0'))::uuid, 'ca1e0000-0000-4000-8000-0000000000ad',
       'Answer ' || n, '2026-01-02T00:00:00Z', pg_temp.near('Quota', 10 * n), 'auto', 'note', CASE WHEN n % 2 = 0 THEN 'chat' ELSE 'query' END
FROM generate_series(1, 6) AS n;

INSERT INTO memories (id, tenant_id, content, created_at, embedding, review_state, kind, origin_kind)
SELECT ('ca1d6000-0000-4000-8000-0000000000' || lpad(to_hex(n), 2, '0'))::uuid, 'ca1e0000-0000-4000-8000-0000000000ad',
       'Fact ' || n, '2026-01-02T00:00:00Z', pg_temp.near('Quota', 100 + 100 * n), 'auto', 'fact', 'legacy'
FROM generate_series(1, 7) AS n;

-- cal_off: a note past the floor and nothing else, so the brief is empty.
INSERT INTO calendar_events (id, tenant_id, source, external_id, title, starts_at, ends_at, remote_updated_at, created_at, updated_at)
VALUES ('ca1c0000-0000-4000-8000-000000000041', 'ca1e0000-0000-4000-8000-0000000000c9', 'apple_eventkit', 'off-1', 'Alone', '2099-04-01T10:00:00Z', '2099-04-01T10:30:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO memories (id, tenant_id, content, created_at, embedding, review_state, kind, origin_kind)
VALUES ('ca1d3000-0000-4000-8000-000000000041', 'ca1e0000-0000-4000-8000-0000000000c9', 'Unrelated', '2026-01-05T00:00:00Z', pg_temp.axis(11), 'auto', 'note', 'legacy');

DROP FUNCTION pg_temp.det(text);
DROP FUNCTION pg_temp.near(text, int);
DROP FUNCTION pg_temp.axis(int);
