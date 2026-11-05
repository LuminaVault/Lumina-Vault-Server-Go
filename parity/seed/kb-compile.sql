-- kb-compile fixtures. A row with no file on disk still counts as a pending row.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
VALUES
  ('5b000000-0000-4000-8000-0000000000a1', 'kbc_amy@parity.test', 'x', true, 'kbc_amy', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a2', 'kbc_ben@parity.test', 'x', true, 'kbc_ben', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a3', 'kbc_cid@parity.test', 'x', true, 'kbc_cid', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a4', 'kbc_dan@parity.test', 'x', true, 'kbc_dan', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a5', 'kbc_eve@parity.test', 'x', true, 'kbc_eve', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a6', 'kbc_fay@parity.test', 'x', true, 'kbc_fay', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a7', 'kbc_gus@parity.test', 'x', true, 'kbc_gus', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a8', 'kbc_hal@parity.test', 'x', true, 'kbc_hal', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a9', 'kbc_ian@parity.test', 'x', true, 'kbc_ian', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
VALUES
  ('5b000000-0000-4000-8000-0000000000a1', '5b000000-0000-4000-8000-0000000000a1', 'kbc_amy''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a2', '5b000000-0000-4000-8000-0000000000a2', 'kbc_ben''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a3', '5b000000-0000-4000-8000-0000000000a3', 'kbc_cid''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a4', '5b000000-0000-4000-8000-0000000000a4', 'kbc_dan''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a5', '5b000000-0000-4000-8000-0000000000a5', 'kbc_eve''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a6', '5b000000-0000-4000-8000-0000000000a6', 'kbc_fay''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a7', '5b000000-0000-4000-8000-0000000000a7', 'kbc_gus''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a8', '5b000000-0000-4000-8000-0000000000a8', 'kbc_hal''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5b000000-0000-4000-8000-0000000000a9', '5b000000-0000-4000-8000-0000000000a9', 'kbc_ian''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at, processed_at)
VALUES
  ('5b100000-0000-4000-8000-000000000001', '5b000000-0000-4000-8000-0000000000a5', 'notes/missing.md', 'text/markdown', 5, 'kb1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', NULL),
  ('5b100000-0000-4000-8000-000000000002', '5b000000-0000-4000-8000-0000000000a6', 'notes/done.md', 'text/markdown', 5, 'kb2', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z');

CREATE VIEW parity_kbc_files AS
SELECT id, tenant_id, space_id, path, content_type, size_bytes, sha256, processed_at IS NOT NULL AS processed, metadata, created_at
FROM vault_files WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'kbc\_%');

CREATE VIEW parity_kbc_memories AS
SELECT id, tenant_id, content, tags, kind, source_vault_file_id, review_state, space_id, origin_kind, origin_source_id, created_at
FROM memories WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'kbc\_%');

CREATE VIEW parity_kbc_commitments AS
SELECT tenant_id, text, normalized_text, due_at, state, memory_id, source_vault_file_id
FROM commitments WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'kbc\_%');

CREATE VIEW parity_kbc_usage AS
SELECT tenant_id, metric, amount, source, metadata
FROM usage_events WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'kbc\_%');

CREATE VIEW parity_kbc_onboarding AS
SELECT tenant_id, first_memory_compile_completed AS compiled
FROM onboarding_state WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'kbc\_%');
