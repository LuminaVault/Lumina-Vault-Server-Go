-- Health (GET/POST /v1/health, GET /v1/health/daily). Ids match
-- parity/seed/users.yaml. hea_bea has no events (isolation).
--
-- Rows dated relative to now() exercise the default windows (list: the last
-- 7 days; daily: the last N UTC days). The seed runs once into the template,
-- so both servers see identical values. Fixed-date rows (2026-09) are read
-- through explicit from/to. No two rows of one tenant share recorded_at, so
-- the DESC order is total.
INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at)
VALUES
  ('4ea10000-0000-4000-8000-0000000000a1', 'hea-hal@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'hea-hal', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea10000-0000-4000-8000-0000000000b2', 'hea-bea@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'hea-bea', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- d(n, h) = n UTC days before today, at hour h.
INSERT INTO health_events (id, tenant_id, event_type, value_numeric, value_text, unit, recorded_at, source, metadata, created_at)
VALUES
  ('4ea1e000-0000-4000-8000-000000000001', '4ea10000-0000-4000-8000-0000000000a1', 'steps', 1200, NULL, 'count',
   (date_trunc('day', now() AT TIME ZONE 'UTC') - interval '1 day' + interval '9 hours') AT TIME ZONE 'UTC', 'apple_health', '{"device": "watch"}', '2026-01-01T00:00:00Z'),
  ('4ea1e000-0000-4000-8000-000000000002', '4ea10000-0000-4000-8000-0000000000a1', 'steps', 800.5, NULL, 'count',
   (date_trunc('day', now() AT TIME ZONE 'UTC') - interval '1 day' + interval '15 hours') AT TIME ZONE 'UTC', 'apple_health', NULL, '2026-01-01T00:00:00Z'),
  ('4ea1e000-0000-4000-8000-000000000003', '4ea10000-0000-4000-8000-0000000000a1', 'steps', 4000, NULL, 'count',
   (date_trunc('day', now() AT TIME ZONE 'UTC') - interval '3 days' + interval '12 hours') AT TIME ZONE 'UTC', 'manual', NULL, '2026-01-01T00:00:00Z'),
  -- 10 days ago: outside the default list window and the default 7-day daily window.
  ('4ea1e000-0000-4000-8000-000000000004', '4ea10000-0000-4000-8000-0000000000a1', 'steps', 999, NULL, 'count',
   (date_trunc('day', now() AT TIME ZONE 'UTC') - interval '10 days' + interval '12 hours') AT TIME ZONE 'UTC', NULL, NULL, '2026-01-01T00:00:00Z'),
  -- Averaged type: two samples on one day average to 70.
  ('4ea1e000-0000-4000-8000-000000000005', '4ea10000-0000-4000-8000-0000000000a1', 'hr_bpm', 60, NULL, 'bpm',
   (date_trunc('day', now() AT TIME ZONE 'UTC') - interval '2 days' + interval '8 hours') AT TIME ZONE 'UTC', 'apple_health', NULL, '2026-01-01T00:00:00Z'),
  ('4ea1e000-0000-4000-8000-000000000006', '4ea10000-0000-4000-8000-0000000000a1', 'hr_bpm', 80, NULL, 'bpm',
   (date_trunc('day', now() AT TIME ZONE 'UTC') - interval '2 days' + interval '20 hours') AT TIME ZONE 'UTC', 'apple_health', NULL, '2026-01-01T00:00:00Z'),
  -- Text-only sample: listed, but never aggregated (value_numeric IS NULL).
  ('4ea1e000-0000-4000-8000-000000000007', '4ea10000-0000-4000-8000-0000000000a1', 'sleep_quality', NULL, 'deep', NULL,
   (date_trunc('day', now() AT TIME ZONE 'UTC') - interval '1 day' + interval '3 hours') AT TIME ZONE 'UTC', 'manual', '{"stage": "n3"}', '2026-01-01T00:00:00Z'),
  -- Fixed dates.
  ('4ea1e000-0000-4000-8000-000000000008', '4ea10000-0000-4000-8000-0000000000a1', 'weight_kg', 80.25, NULL, 'kg', '2026-09-01T07:00:00Z', 'withings', NULL, '2026-01-01T00:00:00Z'),
  ('4ea1e000-0000-4000-8000-000000000009', '4ea10000-0000-4000-8000-0000000000a1', 'weight_kg', 79.75, NULL, 'kg', '2026-09-08T07:00:00Z', 'withings', NULL, '2026-01-01T00:00:00Z'),
  ('4ea1e000-0000-4000-8000-00000000000a', '4ea10000-0000-4000-8000-0000000000a1', 'weight_kg', 79.5, NULL, 'kg', '2026-09-15T07:00:00Z', 'withings', NULL, '2026-01-01T00:00:00Z'),
  ('4ea1e000-0000-4000-8000-00000000000b', '4ea10000-0000-4000-8000-0000000000a1', 'weight_kg', 79, NULL, 'kg', '2026-09-22T07:00:00Z', 'withings', NULL, '2026-01-01T00:00:00Z');
