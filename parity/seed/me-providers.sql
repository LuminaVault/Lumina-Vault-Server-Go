-- me-providers parity seed (parity/scenarios/me-providers). Ids use the 3e9a prefix.
-- Own users, so no settingsByUser bucket (30/min, shared by every settings
-- group) fills. Every time is fixed: nothing here moves with the clock.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, created_at, updated_at)
VALUES
  ('3e9a0000-0000-4000-8000-0000000000a1', 'mp_ann@parity.test', 'x', true, 'mp_ann', 'trial',    'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a2', 'mp_ben@parity.test', 'x', true, 'mp_ben', 'trial',    'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a3', 'mp_cid@parity.test', 'x', true, 'mp_cid', 'trial',    'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a4', 'mp_dee@parity.test', 'x', true, 'mp_dee', 'trial',    'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a5', 'mp_eve@parity.test', 'x', true, 'mp_eve', 'trial',    'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a6', 'mp_fay@parity.test', 'x', true, 'mp_fay', 'trial',    'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a7', 'mp_gus@parity.test', 'x', true, 'mp_gus', 'free',     'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a8', 'mp_hal@parity.test', 'x', true, 'mp_hal', 'trial',    'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a9', 'mp_ivy@parity.test', 'x', true, 'mp_ivy', 'trial',    'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000aa', 'mp_jon@parity.test', 'x', true, 'mp_jon', 'lapsed',   'ultimate', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000ab', 'mp_kit@parity.test', 'x', true, 'mp_kit', 'pro',      'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Personal vaults: id = owner user id (spaces and other tenant rows reference vaults.id).
INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
VALUES
  ('3e9a0000-0000-4000-8000-0000000000a1', '3e9a0000-0000-4000-8000-0000000000a1', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a2', '3e9a0000-0000-4000-8000-0000000000a2', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a3', '3e9a0000-0000-4000-8000-0000000000a3', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a4', '3e9a0000-0000-4000-8000-0000000000a4', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a5', '3e9a0000-0000-4000-8000-0000000000a5', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a6', '3e9a0000-0000-4000-8000-0000000000a6', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a7', '3e9a0000-0000-4000-8000-0000000000a7', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a8', '3e9a0000-0000-4000-8000-0000000000a8', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000a9', '3e9a0000-0000-4000-8000-0000000000a9', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000aa', '3e9a0000-0000-4000-8000-0000000000aa', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e9a0000-0000-4000-8000-0000000000ab', '3e9a0000-0000-4000-8000-0000000000ab', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Saved BYOK preferences: gus is on the free tier, jon is lapsed with an
-- Ultimate override, kit is pro. None holds a usable key, so the free lane
-- decides what GET reports.
INSERT INTO user_llm_preferences (id, tenant_id, mode, primary_provider, primary_model, fallback_chain, allowed_providers, blocked_providers, created_at, updated_at)
VALUES
  ('3e9a1000-0000-4000-8000-000000000001', '3e9a0000-0000-4000-8000-0000000000a7', 'byok', 'openai', 'gpt-4o',
   '{"steps":[{"provider":"anthropic","model":"claude-sonnet-4-6"},{"provider":"together","model":"meta-llama"}]}',
   '{"providers":["openai","groq"]}', '{"providers":["xai"]}', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('3e9a1000-0000-4000-8000-000000000002', '3e9a0000-0000-4000-8000-0000000000aa', 'byok', 'anthropic', 'claude-sonnet-4-6',
   '{"steps":[]}', '{"providers":[]}', '{"providers":[]}', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('3e9a1000-0000-4000-8000-000000000003', '3e9a0000-0000-4000-8000-0000000000ab', 'byok', 'hermesGateway', 'hermes-default',
   '{"steps":[{"provider":"openai","model":"gpt-4o-mini"}]}', NULL, NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

-- eve's connections: a BYO Hermes, a Google account with the full calendar scope
-- that needs reauth, a linked xAI account, plugins, gateways in every status, and
-- credential rows that need no decryption (oauth marker, base URL only, a label).
INSERT INTO user_hermes_config (id, tenant_id, base_url, name, verified_at, created_at, updated_at)
VALUES ('3e9a2000-0000-4000-8000-000000000001', '3e9a0000-0000-4000-8000-0000000000a5', 'http://127.0.0.1:9', NULL, '2026-02-01T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z');

INSERT INTO calendar_accounts (id, tenant_id, provider, account_email, scope, status, last_synced_at, last_failure_code, created_at, updated_at)
VALUES
  ('3e9a3000-0000-4000-8000-000000000001', '3e9a0000-0000-4000-8000-0000000000a5', 'google', 'eve@example.com',
   'openid email https://www.googleapis.com/auth/calendar', 'needs_reauth', '2026-02-02T00:00:00Z', 'invalid_grant', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  -- fay's grant came from "Connect Gmail" alone: no Calendar scope.
  ('3e9a3000-0000-4000-8000-000000000002', '3e9a0000-0000-4000-8000-0000000000a6', 'google', 'fay@example.com',
   'https://www.googleapis.com/auth/gmail.readonly', 'connected', '2026-02-02T00:00:00Z', NULL, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  -- A non-Google account is not the calendar row.
  ('3e9a3000-0000-4000-8000-000000000003', '3e9a0000-0000-4000-8000-0000000000a8', 'microsoft', 'hal@example.com',
   'https://www.googleapis.com/auth/calendar.events', 'connected', NULL, NULL, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z');

INSERT INTO hermes_tenant_containers (id, tenant_id, container_name, port, api_server_key_ciphertext, api_server_key_nonce, xai_connected_at, nous_connected_at, created_at)
VALUES ('3e9a4000-0000-4000-8000-000000000001', '3e9a0000-0000-4000-8000-0000000000a5', 'mp-hermes-eve', 19301, '\x00', '\x00', '2026-02-03T00:00:00Z', NULL, '2026-01-03T00:00:00Z');

INSERT INTO plugin_installs (id, tenant_id, plugin_slug, status, config_ciphertext, config_nonce, last_sync_at, created_at, updated_at)
VALUES
  ('3e9a5000-0000-4000-8000-000000000001', '3e9a0000-0000-4000-8000-0000000000a5', 'readwise', 'enabled',  '\x00', '\x00', '2026-02-04T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('3e9a5000-0000-4000-8000-000000000002', '3e9a0000-0000-4000-8000-0000000000a5', 'pocket',   'disabled', '\x00', '\x00', NULL,                   '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z');

INSERT INTO user_hermes_gateways (id, tenant_id, gateway_id, config_ciphertext, config_nonce, status, verified_at, last_failure_at, last_failure_code, created_at, updated_at)
VALUES
  ('3e9a6000-0000-4000-8000-000000000001', '3e9a0000-0000-4000-8000-0000000000a5', 'telegram', '\x00', '\x00', 'verified',   '2026-02-05T00:00:00Z', NULL, NULL, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('3e9a6000-0000-4000-8000-000000000002', '3e9a0000-0000-4000-8000-0000000000a5', 'discord',  '\x00', '\x00', 'configured', '2026-02-05T00:00:00Z', '2026-02-06T00:00:00Z', 'bot_token_rejected', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('3e9a6000-0000-4000-8000-000000000003', '3e9a0000-0000-4000-8000-0000000000a5', 'slack',    '\x00', '\x00', 'configured', '2026-02-07T00:00:00Z', '2026-02-06T00:00:00Z', NULL, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('3e9a6000-0000-4000-8000-000000000004', '3e9a0000-0000-4000-8000-0000000000a5', 'whatsapp', '\x00', '\x00', 'configured', NULL, NULL, NULL, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('3e9a6000-0000-4000-8000-000000000005', '3e9a0000-0000-4000-8000-0000000000a5', 'email',    '\x00', '\x00', 'error',      NULL, '2026-02-08T00:00:00Z', NULL, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('3e9a6000-0000-4000-8000-000000000006', '3e9a0000-0000-4000-8000-0000000000a5', 'matrix',   '\x00', '\x00', 'paused',     NULL, NULL, NULL, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('3e9a6000-0000-4000-8000-000000000007', '3e9a0000-0000-4000-8000-0000000000a5', 'irc',      '\x00', '\x00', 'verified',   NULL, NULL, NULL, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z');

INSERT INTO user_provider_credentials (id, tenant_id, provider, credential_kind, ciphertext, nonce, base_url, label, verified_at, last_failure_at, last_failure_code, supports_tools, tools_probed_at, created_at, updated_at)
VALUES
  ('3e9a7000-0000-4000-8000-000000000001', '3e9a0000-0000-4000-8000-0000000000a5', 'xai',       'oauth',   NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('3e9a7000-0000-4000-8000-000000000002', '3e9a0000-0000-4000-8000-0000000000a5', 'ollama',    'hostURL', NULL, NULL, 'http://127.0.0.1:9', '', '2026-02-09T00:00:00Z', NULL, NULL, true, '2026-02-09T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('3e9a7000-0000-4000-8000-000000000003', '3e9a0000-0000-4000-8000-0000000000a5', 'anthropic', 'apiKey',  '\x00', '\x00', NULL, 'Work key', '2026-02-09T00:00:00Z', '2026-02-10T00:00:00Z', 'upstream_rejected', false, '2026-02-09T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('3e9a7000-0000-4000-8000-000000000004', '3e9a0000-0000-4000-8000-0000000000a5', 'custom',    'mystery', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('3e9a7000-0000-4000-8000-000000000005', '3e9a0000-0000-4000-8000-0000000000a5', 'openai',    'apiKey',  NULL, NULL, NULL, NULL, '2026-02-09T00:00:00Z', NULL, NULL, NULL, NULL, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('3e9a7000-0000-4000-8000-000000000006', '3e9a0000-0000-4000-8000-0000000000a5', 'together',  'apiKey',  '\x00', '\x00', NULL, NULL, NULL, NULL, NULL, NULL, NULL, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z');

-- fay's diagnostic history, newest first: one row of a kind and one of a severity
-- the wire enums don't know, which the list drops.
INSERT INTO connection_diagnostic_events (id, tenant_id, occurred_at, kind, connection_id, connection_title, severity, message, code, created_at)
VALUES
  ('3e9a8000-0000-4000-8000-000000000001', '3e9a0000-0000-4000-8000-0000000000a6', '2026-03-01T00:00:01Z', 'llm_provider', 'provider:openai', 'OpenAI', 'error', 'OpenAI: upstream_rejected', 'connection_error', '2026-03-01T00:00:01Z'),
  ('3e9a8000-0000-4000-8000-000000000002', '3e9a0000-0000-4000-8000-0000000000a6', '2026-03-01T00:00:02Z', 'mainframe',    NULL, NULL, 'info', 'unknown kind', NULL, '2026-03-01T00:00:02Z'),
  ('3e9a8000-0000-4000-8000-000000000003', '3e9a0000-0000-4000-8000-0000000000a6', '2026-03-01T00:00:03Z', 'server',       'server:api', 'LuminaVault API', 'critical', 'unknown severity', NULL, '2026-03-01T00:00:03Z'),
  ('3e9a8000-0000-4000-8000-000000000004', '3e9a0000-0000-4000-8000-0000000000a6', '2026-03-01T00:00:04Z', 'plugin',       NULL, NULL, 'warning', 'no connection fields', NULL, NULL),
  ('3e9a8000-0000-4000-8000-000000000005', '3e9a0000-0000-4000-8000-0000000000a8', '2026-03-01T00:00:05Z', 'server',       'server:api', 'LuminaVault API', 'info', 'hal''s own event', NULL, '2026-03-01T00:00:05Z');

-- Snapshot views: the sealed secret is random per seal, so only its presence is compared.
CREATE VIEW parity_mp_credentials AS
SELECT tenant_id, provider, credential_kind, ciphertext IS NOT NULL AS has_secret, nonce IS NOT NULL AS has_nonce,
       base_url, label, verified_at, last_failure_at, last_failure_code, supports_tools, tools_probed_at, created_at, updated_at
FROM user_provider_credentials WHERE tenant_id::text LIKE '3e9a0000-%';

-- Snapshots sort on the row's text, so a view leads with stable columns and
-- puts server-minted ids last.
CREATE VIEW parity_mp_pool_keys AS
SELECT tenant_id, provider, label, ciphertext IS NOT NULL AS has_secret, created_at, id
FROM user_provider_credential_pool_keys WHERE tenant_id::text LIKE '3e9a0000-%';

CREATE VIEW parity_mp_preferences AS
SELECT tenant_id, mode, primary_provider, primary_model, fallback_chain, allowed_providers, blocked_providers, created_at, updated_at, id
FROM user_llm_preferences WHERE tenant_id::text LIKE '3e9a0000-%';

CREATE VIEW parity_mp_router_profiles AS
SELECT tenant_id, name, mode, is_preset, revision, document, created_at, updated_at, id
FROM router_profiles WHERE tenant_id::text LIKE '3e9a0000-%';

CREATE VIEW parity_mp_router_bindings AS
SELECT tenant_id, scope, scope_id, profile_id, created_at, updated_at, id
FROM router_bindings WHERE tenant_id::text LIKE '3e9a0000-%';

-- Events leave out ids and times: test-all stamps one time per run in Go and
-- one per row in Swift, and the snapshot's text order would follow them.
CREATE VIEW parity_mp_events AS
SELECT tenant_id, kind, connection_id, severity, message, code, connection_title, created_at IS NOT NULL AS has_created_at
FROM connection_diagnostic_events WHERE tenant_id::text LIKE '3e9a0000-%';

-- The verdict fields are written by a detached probe, so the LLM state leaves them out.
CREATE VIEW parity_mp_credentials_verified AS
SELECT tenant_id, provider, credential_kind, ciphertext IS NOT NULL AS has_secret, base_url, label,
       verified_at IS NOT NULL AS verified, last_failure_at, last_failure_code
FROM user_provider_credentials WHERE tenant_id::text LIKE '3e9a0000-%';
