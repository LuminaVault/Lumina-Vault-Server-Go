-- Plugins fixtures (parity/scenarios/plugins, scenarios-switched/{agents,import-links}/plugins).
-- One user per file so settingsByUser (30 a minute) never fills.
INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at) VALUES
  ('91a90000-0000-4000-8000-0000000000a1', 'pl-amy@parity.test', 'x', true, 'pl_amy', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('91a90000-0000-4000-8000-0000000000b2', 'pl-bo@parity.test', 'x', true, 'pl_bo', 'pro', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('91a90000-0000-4000-8000-0000000000c3', 'pl-cy@parity.test', 'x', true, 'pl_cy', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('91a90000-0000-4000-8000-0000000000d4', 'pl-dee@parity.test', 'x', true, 'pl_dee', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('91a90000-0000-4000-8000-0000000000e5', 'pl-eve@parity.test', 'x', true, 'pl_eve', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('91a90000-0000-4000-8000-0000000000f6', 'pl-fox@parity.test', 'x', true, 'pl_fox', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
SELECT id, id, 'Personal', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z' FROM users WHERE username LIKE 'pl\_%'
ON CONFLICT DO NOTHING;

-- pl_eve: a BYO Hermes on the LLM stub. Plain http to a private name passes only in the
-- agents state; elsewhere the resolution fails and /v1/plugins answers 502.
INSERT INTO user_hermes_config (id, tenant_id, base_url, verified_at, created_at, updated_at) VALUES
  ('91a95000-0000-4000-8000-000000000001', '91a90000-0000-4000-8000-0000000000e5', 'http://llmstub:8080', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- pl_cy: a container row whose key does not decrypt, so hub install fails before any docker exec.
INSERT INTO hermes_tenant_containers (id, tenant_id, container_name, port, api_server_key_ciphertext, api_server_key_nonce, created_at, last_used_at)
VALUES ('91a96000-0000-4000-8000-000000000001', '91a90000-0000-4000-8000-0000000000c3', 'hermes-tenant-parity-cy', 39001,
        '\x00'::bytea, '\x00'::bytea, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Installs and skill state without random ids or clocks.
CREATE VIEW parity_pl_installs AS
  SELECT tenant_id, plugin_slug, status, length(config_ciphertext) > 0 AS has_config,
         last_sync_at IS NOT NULL AS synced, marketplace_version_id, granted_permissions
    FROM plugin_installs
   WHERE tenant_id::text LIKE '91a90000-%';
CREATE VIEW parity_pl_skills AS
  SELECT tenant_id, source, name, enabled FROM skills_state WHERE tenant_id::text LIKE '91a90000-%';
-- pl_dee's syncs, as parity/seed/import.sql's views for imp_ users.
CREATE VIEW parity_pl_import_sessions AS
SELECT id, tenant_id, source_type, status, total_items, staged_items, created_at, updated_at
FROM import_sessions WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'pl\_%');
CREATE VIEW parity_pl_import_items AS
SELECT id, tenant_id, session_id, vault_file_id, url, title, proposed_space, status, created_at
FROM import_items WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'pl\_%');
CREATE VIEW parity_pl_import_pending AS
SELECT f.id FROM vault_files f
WHERE f.tenant_id IN (SELECT id FROM users WHERE username LIKE 'pl\_%')
  AND (f.metadata->>'enrichmentStatus' = 'pending'
       OR (f.metadata->>'enrichmentStatus' = 'done' AND f.size_bytes > 100
           AND NOT EXISTS (SELECT 1 FROM memories m WHERE m.source_vault_file_id = f.id)));
