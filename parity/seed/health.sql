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

-- Billing gate on POST /v1/health (parity/scenarios/health/05-gate.yaml). Tiers
-- are set here; the gate only reads them with BILLING_ENFORCEMENT_ENABLED=true.
-- Sealed values: parity LV_SECRET_MASTER_KEY, bound to the tenant, test strings
-- only.
INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, tier_expires_at, created_at, updated_at)
VALUES
  ('4ea20000-0000-4000-8000-0000000000a1', 'gate-lapsed@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-lapsed', 'lapsed', 'none', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000a2', 'gate-free@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-free', 'free', 'none', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000a3', 'gate-pro@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-pro', 'pro', 'none', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000a4', 'gate-override@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-override', 'lapsed', 'pro', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000a5', 'gate-archived@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-archived', 'archived', 'none', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000a6', 'gate-key@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-key', 'lapsed', 'none', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000a7', 'gate-ollama@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-ollama', 'free', 'none', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000a8', 'gate-urlonly@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-urlonly', 'lapsed', 'none', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000a9', 'gate-empty@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-empty', 'lapsed', 'none', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000aa', 'gate-pool@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-pool', 'lapsed', 'none', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000ab', 'gate-badpool@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-badpool', 'lapsed', 'none', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000ac', 'gate-xlinked@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-xlinked', 'lapsed', 'none', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000ad', 'gate-xunlinked@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-xunlinked', 'lapsed', 'none', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000ae', 'gate-xnobox@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-xnobox', 'lapsed', 'none', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000af', 'gate-xbadkey@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-xbadkey', 'lapsed', 'none', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000b0', 'gate-expired@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-expired', 'pro', 'none', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea20000-0000-4000-8000-0000000000b1', 'gate-bad@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'gate-bad', 'lapsed', 'none', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO user_provider_credentials (id, tenant_id, provider, credential_kind, ciphertext, nonce, base_url)
VALUES
  ('4ea2c000-0000-4000-8000-0000000000a5', '4ea20000-0000-4000-8000-0000000000a5', 'anthropic', 'apiKey', '\xe79c71cc28f4782c9c29ba4e981e33b9fa29d54bc2c91458cd9639beb01368ad5c01', '\x1a7fe94752363c54345b58c8', NULL),
  ('4ea2c000-0000-4000-8000-0000000000a6', '4ea20000-0000-4000-8000-0000000000a6', 'anthropic', 'apiKey', '\x58a91e733866dbd2e723c842203df51d54204636c5d223a5a629a2ea34eab870028f', '\xefd16578f8f0e8a553c20dde', NULL),
  ('4ea2c000-0000-4000-8000-0000000000a7', '4ea20000-0000-4000-8000-0000000000a7', 'ollama', 'baseUrl', NULL, NULL, 'http://10.0.0.2:11434'),
  ('4ea2c000-0000-4000-8000-0000000000a8', '4ea20000-0000-4000-8000-0000000000a8', 'openai', 'apiKey', NULL, NULL, 'https://api.example.test'),
  ('4ea2c000-0000-4000-8000-0000000000a9', '4ea20000-0000-4000-8000-0000000000a9', 'openai', 'apiKey', '\x17d4f197f15a3dd05198a5ad5c750896', '\x5250680d25f7840aff3241b7', NULL),
  ('4ea2c000-0000-4000-8000-0000000000ab', '4ea20000-0000-4000-8000-0000000000ab', 'anthropic', 'apiKey', '\x3b7742642e897cb3ae2c82cb39bcf04c5165560789bfb487bba46f4f6f7c658354ac', '\x827cde15e2528eedaf88eb53', NULL),
  ('4ea2c000-0000-4000-8000-0000000000ac', '4ea20000-0000-4000-8000-0000000000ac', 'xai', 'oauth', NULL, NULL, NULL),
  ('4ea2c000-0000-4000-8000-0000000000ad', '4ea20000-0000-4000-8000-0000000000ad', 'xai', 'oauth', NULL, NULL, NULL),
  ('4ea2c000-0000-4000-8000-0000000000ae', '4ea20000-0000-4000-8000-0000000000ae', 'xai', 'oauth', NULL, NULL, NULL),
  ('4ea2c000-0000-4000-8000-0000000000af', '4ea20000-0000-4000-8000-0000000000af', 'xai', 'oauth', NULL, NULL, NULL);
-- gate-pool: one pool key and no primary. gate-badpool's pool key is sealed for
-- another tenant, so it never opens.
INSERT INTO user_provider_credential_pool_keys (id, tenant_id, provider, ciphertext, nonce, created_at)
VALUES
  ('4ea2d000-0000-4000-8000-0000000000aa', '4ea20000-0000-4000-8000-0000000000aa', 'openai', '\x2478bad165a7ea6cf4675436f4eb051919f9dd0e7f55a92e96a202e1a90942484f2b', '\x87d64d2ea554b9c3afd679c0', '2026-01-01T00:00:00Z'),
  ('4ea2d000-0000-4000-8000-0000000000ab', '4ea20000-0000-4000-8000-0000000000ab', 'anthropic', '\x58a91e733866dbd2e723c842203df51d54204636c5d223a5a629a2ea34eab870028f', '\xefd16578f8f0e8a553c20dde', '2026-01-01T00:00:00Z');
-- xlinked has completed the xAI connect, xunlinked has not, xnobox has no
-- container, and xbadkey's server key is sealed for another tenant.
INSERT INTO hermes_tenant_containers (id, tenant_id, container_name, port, api_server_key_ciphertext, api_server_key_nonce, xai_connected_at, created_at)
VALUES
  ('4ea2e000-0000-4000-8000-0000000000ac', '4ea20000-0000-4000-8000-0000000000ac', 'gate-xlinked', 19001, '\x26cdc6653fbf9886dc38909f38a235318810f0c053031f2006d1665ada8e273d98', '\xfc91f35f784336b070215db6', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4ea2e000-0000-4000-8000-0000000000ad', '4ea20000-0000-4000-8000-0000000000ad', 'gate-xunlinked', 19002, '\x843d81feb521f3899378772b230ce11bed1f33a82b11b3827a348b87e86ce4f11c', '\xe1c4528623285f3e540d4a3d', NULL, '2026-01-01T00:00:00Z'),
  ('4ea2e000-0000-4000-8000-0000000000af', '4ea20000-0000-4000-8000-0000000000af', 'gate-xbadkey', 19003, '\x58a91e733866dbd2e723c842203df51d54204636c5d223a5a629a2ea34eab870028f', '\xefd16578f8f0e8a553c20dde', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
