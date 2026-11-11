-- me-gateways and integrations parity seed (parity/scenarios/me-gateways,
-- parity/scenarios/integrations). Gateway ids use the 6a7e prefix, integration
-- ids 1e7a. Own users, so no settingsByUser bucket (30/min) fills. Every time is
-- fixed: nothing here moves with the clock.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, created_at, updated_at)
VALUES
  ('6a7e0000-0000-4000-8000-0000000000a1', 'gw_ann@parity.test', 'x', true, 'gw_ann', 'trial', 'none', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('6a7e0000-0000-4000-8000-0000000000a2', 'gw_ben@parity.test', 'x', true, 'gw_ben', 'trial', 'none', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('6a7e0000-0000-4000-8000-0000000000a3', 'gw_cid@parity.test', 'x', true, 'gw_cid', 'trial', 'none', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('6a7e0000-0000-4000-8000-0000000000a4', 'gw_dee@parity.test', 'x', true, 'gw_dee', 'trial', 'none', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('6a7e0000-0000-4000-8000-0000000000a5', 'gw_eve@parity.test', 'x', true, 'gw_eve', 'trial', 'none', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('6a7e0000-0000-4000-8000-0000000000a6', 'gw_fay@parity.test', 'x', true, 'gw_fay', 'trial', 'none', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1e7a0000-0000-4000-8000-0000000000a1', 'in_ann@parity.test', 'x', true, 'in_ann', 'trial', 'none', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1e7a0000-0000-4000-8000-0000000000a2', 'in_ben@parity.test', 'x', true, 'in_ben', 'free',  'none', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1e7a0000-0000-4000-8000-0000000000a3', 'in_cid@parity.test', 'x', true, 'in_cid', 'pro',   'none', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1e7a0000-0000-4000-8000-0000000000a4', 'in_dee@parity.test', 'x', true, 'in_dee', 'trial', 'none', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
SELECT id, id, 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'
FROM users WHERE id::text LIKE '6a7e0000-%' OR id::text LIKE '1e7a0000-%';

-- gw_cid has a finished apply job: status, current and stream read it back.
INSERT INTO hermes_gateway_apply_jobs (id, tenant_id, state, steps_json, error_message, created_at, updated_at)
VALUES
  ('6a7e1000-0000-4000-8000-000000000001', '6a7e0000-0000-4000-8000-0000000000a3', 'failed',
   '[{"id":"writeEnv","state":"failed","detail":"no container"},{"id":"restartContainer","state":"pending"},{"id":"healthCheck","state":"pending"}]',
   'Applying your gateway settings failed: no container', '2026-01-02T00:00:00Z', '2026-01-02T00:00:05Z');

-- in_cid has a container linked to SuperGrok and an xAI oauth marker; in_dee one
-- linked to Nous. The API keys are sealed for their tenants with the parity
-- master key. No docker answers in either server, so revoke's CLI call fails and
-- is ignored, as in Swift.
INSERT INTO hermes_tenant_containers (id, tenant_id, container_name, port, api_server_key_ciphertext, api_server_key_nonce, xai_connected_at, nous_connected_at, created_at)
VALUES
  ('1e7ae000-0000-4000-8000-0000000000a3', '1e7a0000-0000-4000-8000-0000000000a3', 'in-hermes-cid', 19201, '\x7023b4667fad5d353fd176a0bdcd18e24c31cf5d1de8dc4059dc6f8efa8a6ba9e9cc6c7e0c8484', '\x64917d835e89a7637efb0f29', '2026-01-01T00:00:00Z', NULL, '2026-01-01T00:00:00Z'),
  ('1e7ae000-0000-4000-8000-0000000000a4', '1e7a0000-0000-4000-8000-0000000000a4', 'in-hermes-dee', 19202, '\x1e55aa260c1e9b794ae1ccc802beafee5ce0006b596fb0e54b09d121e15cbea092b299efb07fe2', '\x871610340aba5ece2afe3c6c', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO user_provider_credentials (id, tenant_id, provider, credential_kind, label, created_at, updated_at)
VALUES
  ('1e7a2000-0000-4000-8000-000000000001', '1e7a0000-0000-4000-8000-0000000000a3', 'xai', 'oauth', 'SuperGrok (linked)', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Snapshots sort on the row's text, so views lead with stable columns and leave
-- out server-minted ids and clock times.
CREATE VIEW parity_gw_rows AS
SELECT tenant_id, gateway_id, status, config_ciphertext IS NOT NULL AS has_secret, verified_at IS NOT NULL AS verified,
       last_failure_code, last_failure_at IS NOT NULL AS failed
FROM user_hermes_gateways WHERE tenant_id::text LIKE '6a7e0000-%';

CREATE VIEW parity_gw_apply_jobs AS
SELECT tenant_id, state, error_message IS NOT NULL AS has_error
FROM hermes_gateway_apply_jobs WHERE tenant_id::text LIKE '6a7e0000-%';

CREATE VIEW parity_in_users AS
SELECT id, tier FROM users WHERE id::text LIKE '1e7a0000-%';

CREATE VIEW parity_in_containers AS
SELECT tenant_id, xai_connected_at, nous_connected_at FROM hermes_tenant_containers WHERE tenant_id::text LIKE '1e7a0000-%';

CREATE VIEW parity_in_credentials AS
SELECT tenant_id, provider, credential_kind, label FROM user_provider_credentials WHERE tenant_id::text LIKE '1e7a0000-%';
