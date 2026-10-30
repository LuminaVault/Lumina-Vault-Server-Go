-- Apple module fixtures (apple, devices, reminders flip together). Loaded
-- after base.sql by scripts/parity-up.sh. Ids are lowercase here; Swift
-- writes them with Postgres' uuid type, so case is irrelevant in the DB.

-- Consent: alice already allows calendar (with a sync stamp) and has a row for
-- a domain the enum does not know, which the snapshot must ignore.
INSERT INTO apple_consent (tenant_id, domain, allowed, allow_writes, last_sync_at, updated_at)
VALUES
  ('a11ce000-0000-4000-8000-000000000001', 'calendar', true, true, '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('a11ce000-0000-4000-8000-000000000001', 'bogus', true, false, NULL, '2026-01-05T00:00:00Z');

-- A last-known location for alice, which revoking `location` clears.
UPDATE users SET last_location_lat = 52.37, last_location_lng = 4.89,
  last_location_place = 'Amsterdam', last_location_at = '2026-01-06T00:00:00Z'
WHERE id = 'a11ce000-0000-4000-8000-000000000001';

-- One synced Apple reminder for alice (last-writer-wins tests).
INSERT INTO apple_reminders (id, tenant_id, external_id, title, notes, due_at, completed, completed_at, list_name, priority, remote_updated_at, created_at, updated_at)
VALUES ('a9900000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001', 'seed-1', 'Seeded reminder', 'n', '2026-02-01T09:00:00Z', false, NULL, 'Home', 1, '2026-01-10T00:00:00Z', '2026-01-10T00:00:00Z', '2026-01-10T00:00:00Z');

-- An indexed photo of alice's, which revoking `photos` purges.
INSERT INTO photo_index (id, tenant_id, asset_local_id, taken_at, is_screenshot, ocr_text, scene_tags, embedding, created_at, updated_at)
VALUES ('a9900000-0000-4000-8000-000000000002', 'a11ce000-0000-4000-8000-000000000001', 'seed-asset', '2026-01-04T00:00:00Z', true, 'seeded', ARRAY['receipt'], NULL, '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z');

-- bob has used his whole monthly embedding budget (cap 2,000,000 tokens).
INSERT INTO embedding_usage (id, tenant_id, year_month, tokens_used, created_at, updated_at)
VALUES ('a9900000-0000-4000-8000-000000000003', 'b0b00000-0000-4000-8000-000000000002', to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM'), 2000000, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Queued phone writes. expires_at is relative to load time, so "pending"
-- holds for the whole run.
INSERT INTO device_command_queue (id, tenant_id, kind, domain, payload, created_at, expires_at, delivered_at, result_ok, result_error)
VALUES
  -- alice: pending, oldest first
  ('c0000000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001', 'reminder_create', 'reminders', '{"title":"Call mom","due":""}', '2026-01-01T00:00:01Z', now() + interval '1 day', NULL, NULL, NULL),
  ('c0000000-0000-4000-8000-000000000002', 'a11ce000-0000-4000-8000-000000000001', 'calendar_create', 'calendar', '{"title":"Dentist"}', '2026-01-01T00:00:02Z', now() + interval '1 day', NULL, NULL, NULL),
  -- unknown domain → domain omitted; unreadable payload → {}
  ('c0000000-0000-4000-8000-000000000003', 'a11ce000-0000-4000-8000-000000000001', 'ping', 'mars', 'not json', '2026-01-01T00:00:03Z', now() + interval '1 day', NULL, NULL, NULL),
  -- unknown kind → dropped from the list
  ('c0000000-0000-4000-8000-000000000004', 'a11ce000-0000-4000-8000-000000000001', 'teleport', NULL, '{}', '2026-01-01T00:00:04Z', now() + interval '1 day', NULL, NULL, NULL),
  -- expired → never handed out
  ('c0000000-0000-4000-8000-000000000005', 'a11ce000-0000-4000-8000-000000000001', 'reminder_create', 'reminders', '{"title":"Old"}', '2026-01-01T00:00:05Z', now() - interval '1 minute', NULL, NULL, NULL),
  -- already delivered
  ('c0000000-0000-4000-8000-000000000006', 'a11ce000-0000-4000-8000-000000000001', 'reminder_create', 'reminders', '{"title":"Done"}', '2026-01-01T00:00:06Z', now() + interval '1 day', '2026-01-02T00:00:00Z', true, NULL),
  -- bob: one pending write
  ('c0000000-0000-4000-8000-000000000007', 'b0b00000-0000-4000-8000-000000000002', 'reminder_create', 'reminders', '{"title":"Bob task"}', '2026-01-01T00:00:07Z', now() + interval '1 day', NULL, NULL, NULL);

-- Device tokens: bob already holds one (device_tokens.token is globally unique).
INSERT INTO device_tokens (id, tenant_id, token, platform, last_seen_at, created_at, updated_at, bundle_id)
VALUES ('d0000000-0000-4000-8000-000000000001', 'b0b00000-0000-4000-8000-000000000002', 'tok-bob', 'ios', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', NULL);

-- Reminders (the app-scheduled kind): one fired, one recurring and pending.
INSERT INTO reminders (id, tenant_id, title, body, fire_at, recurrence_cron, fired_at, created_at)
VALUES
  ('e0000000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001', 'Seeded fired', 'water plants', '2026-01-05T09:00:00Z', NULL, '2026-01-05T09:00:05Z', '2026-01-01T00:00:00Z'),
  ('e0000000-0000-4000-8000-000000000002', 'a11ce000-0000-4000-8000-000000000001', 'Standup', '', '2026-02-02T09:00:00Z', '0 9 * * 1-5', NULL, '2026-01-01T00:00:01Z');
