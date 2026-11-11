-- Billing fixtures (parity/scenarios/billing, scenarios-switched/{unconfigured,billing-on}/billing).
-- Webhook targets: bl_amy (by id), bl_bo (by revenuecat_user_id rc_bo), bl_cy
-- (billing issue), bl_dee (expiration), bl_eve (alias). Read paths: bl_fox (usage
-- rows this month), bl_gus (lapsed with an ultimate override), bl_hal (admin target).
-- Expiries are whole seconds: Swift stores expiration_at_ms / 1000.0 through a Double.
INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, tier_expires_at, revenuecat_user_id, created_at, updated_at)
VALUES
  ('b1110000-0000-4000-8000-0000000000a1', 'bl_amy@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'bl_amy', 'trial', 'none', '2026-01-15T00:00:00Z', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('b1110000-0000-4000-8000-0000000000b2', 'bl_bo@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'bl_bo', 'pro', 'none', '2030-01-01T00:00:00Z', 'rc_bo', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('b1110000-0000-4000-8000-0000000000c3', 'bl_cy@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'bl_cy', 'pro', 'none', '2026-01-01T00:00:00Z', 'rc_cy', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('b1110000-0000-4000-8000-0000000000d4', 'bl_dee@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'bl_dee', 'ultimate', 'none', '2030-01-01T00:00:00Z', 'rc_dee', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('b1110000-0000-4000-8000-0000000000e5', 'bl_eve@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'bl_eve', 'trial', 'none', NULL, NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('b1110000-0000-4000-8000-0000000000f6', 'bl_fox@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'bl_fox', 'pro', 'none', '2031-06-01T12:00:00Z', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('b1110000-0000-4000-8000-0000000000a7', 'bl_gus@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'bl_gus', 'lapsed', 'ultimate', '2025-01-01T00:00:00Z', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('b1110000-0000-4000-8000-0000000000a8', 'bl_hal@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'bl_hal', 'free', 'none', NULL, 'rc_hal', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z');

-- bl_fox's usage: storage, this month's meter and compile events, and an old month
-- that must not count. Days are relative so the scenario stays in the current month
-- (it can still straddle a month boundary at midnight UTC on the 1st).
INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
VALUES ('b1110000-0000-4000-8000-0000000000f6', 'b1110000-0000-4000-8000-0000000000f6', 'Personal', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z');
INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at)
VALUES ('b111f000-0000-4000-8000-000000000001', 'b1110000-0000-4000-8000-0000000000f6', 'usage.md', 'text/markdown', 4242, 'x', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z');
INSERT INTO usage_meter (tenant_id, day, model, mtok_in, mtok_out, chars_out)
VALUES ('b1110000-0000-4000-8000-0000000000f6', current_date, 'hermes-3', 100, 50, 300),
       ('b1110000-0000-4000-8000-0000000000f6', current_date, 'hermes-3-small', 20, 30, 50),
       ('b1110000-0000-4000-8000-0000000000f6', current_date - 40, 'hermes-3', 999, 999, 999);
INSERT INTO usage_events (tenant_id, occurred_at, metric, amount, source, idempotency_key)
VALUES ('b1110000-0000-4000-8000-0000000000f6', date_trunc('hour', now()), 'memory_compile_run', 1, 'memory_compile', 'bl-run'),
       ('b1110000-0000-4000-8000-0000000000f6', date_trunc('hour', now()), 'memory_compile_file', 5, 'memory_compile', 'bl-files'),
       ('b1110000-0000-4000-8000-0000000000f6', now() - interval '40 days', 'memory_compile_file', 77, 'memory_compile', 'bl-old');

-- Every bl_ user's billing columns. A BILLING_ISSUE hold is now + 16 days, so it is
-- compared as "hold" rather than as a clock time.
CREATE VIEW parity_bl_users AS
  SELECT username, tier, tier_override, revenuecat_user_id,
         CASE WHEN tier_expires_at BETWEEN now() + interval '15 days' AND now() + interval '17 days' THEN 'hold'
              ELSE to_char(tier_expires_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US') END AS tier_expires_at
  FROM users WHERE username LIKE 'bl\_%' ORDER BY username;

CREATE VIEW parity_bl_events AS
  SELECT event_id, event_type, user_id FROM billing_event_logs ORDER BY event_id;
