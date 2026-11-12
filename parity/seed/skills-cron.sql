-- /v1/me/hermes/cron fixtures (parity/scenarios/skills-cron). There is no docker in
-- either server and no reachable Hermes dashboard, so a managed tenant's calls end
-- in the failed spawn's empty 500, and a BYO tenant's dashboard URL is one the SSRF
-- guard refuses before any request.
--
-- cr_amy: no container, no BYO config.
-- cr_mia: a managed container; api_server_key sealed with the parity
--         LV_SECRET_MASTER_KEY for cr_mia.
-- cr_bad: a container whose key does not open (as plugins.sql's pl_cy).
-- cr_byo: a BYO dashboard on plain-HTTP loopback; token sealed for cr_byo.
-- cr_cfg: no rows; PUT config is tried against it.
INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at) VALUES
  ('c20c0000-0000-4000-8000-0000000000a1', 'cr-amy@parity.test', 'x', true, 'cr_amy', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c20c0000-0000-4000-8000-0000000000b2', 'cr-mia@parity.test', 'x', true, 'cr_mia', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c20c0000-0000-4000-8000-0000000000c3', 'cr-bad@parity.test', 'x', true, 'cr_bad', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c20c0000-0000-4000-8000-0000000000d4', 'cr-byo@parity.test', 'x', true, 'cr_byo', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c20c0000-0000-4000-8000-0000000000e5', 'cr-cfg@parity.test', 'x', true, 'cr_cfg', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO hermes_tenant_containers (id, tenant_id, container_name, port, api_server_key_ciphertext, api_server_key_nonce, created_at, last_used_at)
VALUES
  ('c20ce000-0000-4000-8000-0000000000b2', 'c20c0000-0000-4000-8000-0000000000b2', 'hermes-tenant-parity-cron-mia', 39101,
   '\xd949e0d45393705f759c4670a2e3c4ebcdf3ba4dd8ff5236f2afae730b9d2c', '\x497671258b440150d840ed2a', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c20ce000-0000-4000-8000-0000000000c3', 'c20c0000-0000-4000-8000-0000000000c3', 'hermes-tenant-parity-cron-bad', 39102,
   '\x00'::bytea, '\x00'::bytea, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO user_hermes_config (id, tenant_id, base_url, cron_dashboard_url, cron_dashboard_token_ciphertext, cron_dashboard_token_nonce, created_at, updated_at)
VALUES ('c20cf000-0000-4000-8000-0000000000d4', 'c20c0000-0000-4000-8000-0000000000d4', '', 'http://127.0.0.1:9',
        '\xdf885789b79e1a4e997ebfd929d30b678df7ce48e7876cba085285db4703c41678', '\xa49e839af560e3e160617eba',
        '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- The cron dashboard columns without the random id, clocks or ciphertext bytes.
CREATE VIEW parity_cr_config AS
  SELECT tenant_id, base_url, cron_dashboard_url,
         cron_dashboard_token_ciphertext IS NOT NULL AS has_token
    FROM user_hermes_config
   WHERE tenant_id::text LIKE 'c20c0000-%';
