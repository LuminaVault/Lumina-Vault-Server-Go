-- MCP fixtures (parity/scenarios/mcp). One user per file so neither mcpByUser (120 a minute)
-- nor settingsByUser (30 a minute) fills. Agent keys are seeded with known plaintext:
-- token_hash is SHA-256 of the token, as AgentConnectionService.hash.
INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at) VALUES
  ('3c900000-0000-4000-8000-0000000000a1', 'mcp-ann@parity.test', 'x', true, 'mcp_ann', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3c900000-0000-4000-8000-0000000000b2', 'mcp-ben@parity.test', 'x', true, 'mcp_ben', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3c900000-0000-4000-8000-0000000000c3', 'mcp-cy@parity.test', 'x', true, 'mcp_cy', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3c900000-0000-4000-8000-0000000000d4', 'mcp-dee@parity.test', 'x', true, 'mcp_dee', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3c900000-0000-4000-8000-0000000000e5', 'mcp-eve@parity.test', 'x', true, 'mcp_eve', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- mcp_ben: a read-only key with no personal grant; mcp_cy: read-write with personal data.
-- A revoked key for mcp_ben, which must authenticate as nobody.
INSERT INTO agent_connections (id, tenant_id, name, client_kind, token_hash, token_prefix, created_at, last_used_at, revoked_at, allow_personal_data, access) VALUES
  ('3c901000-0000-4000-8000-000000000001', '3c900000-0000-4000-8000-0000000000b2', 'ben laptop', 'claude_code',
   sha256(convert_to('lv_parity_ben_read_only_key_0000000000000', 'UTF8')), 'lv_parity', '2026-01-02T00:00:00Z', NULL, NULL, false, 'read'),
  ('3c901000-0000-4000-8000-000000000002', '3c900000-0000-4000-8000-0000000000c3', 'cy desktop', 'codex',
   sha256(convert_to('lv_parity_cy_read_write_key_0000000000000', 'UTF8')), 'lv_parity', '2026-01-02T00:00:00Z', NULL, NULL, true, 'read_write'),
  ('3c901000-0000-4000-8000-000000000003', '3c900000-0000-4000-8000-0000000000b2', 'ben old', 'hermes',
   sha256(convert_to('lv_parity_ben_revoked_key_000000000000000', 'UTF8')), 'lv_parity', '2026-01-01T00:00:00Z', NULL, '2026-01-03T00:00:00Z', false, 'read');

-- mcp_dee: one listed key, for list/update/revoke.
INSERT INTO agent_connections (id, tenant_id, name, client_kind, token_hash, token_prefix, created_at, last_used_at, revoked_at, allow_personal_data, access) VALUES
  ('3c901000-0000-4000-8000-000000000004', '3c900000-0000-4000-8000-0000000000d4', 'dee phone', 'other',
   sha256(convert_to('lv_parity_dee_listed_key_00000000000000000', 'UTF8')), 'lv_parity', '2026-01-02T00:00:00Z', '2026-01-04T00:00:00Z', NULL, false, 'read');

-- Personal vaults, which spaces and vault access need (id = the owner's id).
INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES
  ('3c900000-0000-4000-8000-0000000000a1', '3c900000-0000-4000-8000-0000000000a1', 'Personal', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3c900000-0000-4000-8000-0000000000b2', '3c900000-0000-4000-8000-0000000000b2', 'Personal', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3c900000-0000-4000-8000-0000000000c3', '3c900000-0000-4000-8000-0000000000c3', 'Personal', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3c900000-0000-4000-8000-0000000000d4', '3c900000-0000-4000-8000-0000000000d4', 'Personal', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3c900000-0000-4000-8000-0000000000e5', '3c900000-0000-4000-8000-0000000000e5', 'Personal', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')
ON CONFLICT DO NOTHING;

-- mcp_ann: a Space, two vault files with no file on disk, and memories for memory_context.
INSERT INTO spaces (id, tenant_id, name, slug, created_at, updated_at) VALUES
  ('3c902000-0000-4000-8000-000000000001', '3c900000-0000-4000-8000-0000000000a1', 'Work', 'work', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at) VALUES
  ('3c903000-0000-4000-8000-000000000001', '3c900000-0000-4000-8000-0000000000a1', 'notes/alpha.md', 'text/markdown', 12, 'x', '2026-01-01T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('3c903000-0000-4000-8000-000000000002', '3c900000-0000-4000-8000-0000000000a1', 'notes/beta.md', 'text/markdown', 34, 'y', '2026-01-01T00:00:00Z', '2026-01-06T00:00:00Z');
INSERT INTO memories (id, tenant_id, content, review_state, origin_kind, kind, space_id, created_at, updated_at) VALUES
  ('3c904000-0000-4000-8000-000000000001', '3c900000-0000-4000-8000-0000000000a1', 'Chose Postgres for storage.', 'auto', 'manual', 'decision', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('3c904000-0000-4000-8000-000000000002', '3c900000-0000-4000-8000-0000000000a1', 'Likes dark mode.', 'auto', 'manual', 'preference', '3c902000-0000-4000-8000-000000000001', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('3c904000-0000-4000-8000-000000000003', '3c900000-0000-4000-8000-0000000000a1', 'Has a cat.', 'auto', 'manual', 'fact', NULL, '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z'),
  ('3c904000-0000-4000-8000-000000000004', '3c900000-0000-4000-8000-0000000000a1', 'Rejected idea.', 'rejected', 'manual', 'decision', NULL, '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z');
