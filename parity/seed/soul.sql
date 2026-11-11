-- SOUL.md fixtures (parity/scenarios/soul). One user per scenario file, so no soulByUser
-- bucket (30 a minute) fills by accident. Timestamps are absolute.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at) VALUES
  ('50e10000-0000-4000-8000-0000000000a1', 'soul-ann@parity.test', 'x', true, 'soul_ann', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('50e10000-0000-4000-8000-0000000000b2', 'soul-ben@parity.test', 'x', true, 'soul_ben', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('50e10000-0000-4000-8000-0000000000c3', 'soul-cy@parity.test', 'x', true, 'soul_cy', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('50e10000-0000-4000-8000-0000000000d4', 'soul-dee@parity.test', 'x', true, 'soul_dee', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('50e10000-0000-4000-8000-0000000000e5', 'soul-byo@parity.test', 'x', true, 'soul_byo', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('50e10000-0000-4000-8000-0000000000f6', 'soul-priv@parity.test', 'x', true, 'soul_priv', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('50e10000-0000-4000-8000-0000000000a7', 'soul-rl@parity.test', 'x', true, 'soul_rl', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('50e10000-0000-4000-8000-0000000000b8', 'soul-smoke@parity.test', 'x', true, 'soul_smoke', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('50e10000-0000-4000-8000-0000000000c9', 'soul-eve@parity.test', 'x', true, 'soul_eve', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- BYO Hermes, as self-improvement.sql: a public literal passes the SSRF check with no DNS and
-- is never called, so soul_byo is an override. The private address fails the check, which
-- HermesRemoteCapabilitiesService.isUserOverride counts as no override.
INSERT INTO user_hermes_config (id, tenant_id, base_url, verified_at, created_at, updated_at) VALUES
  ('50e15000-0000-4000-8000-000000000001', '50e10000-0000-4000-8000-0000000000e5', 'https://8.8.8.8:8642', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('50e15000-0000-4000-8000-000000000002', '50e10000-0000-4000-8000-0000000000f6', 'http://10.20.30.40:8642', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
