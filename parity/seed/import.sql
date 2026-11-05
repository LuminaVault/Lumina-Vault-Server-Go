-- import fixtures. Own users: capture's enrichment writes after the response.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
VALUES
  ('1a000000-0000-4000-8000-0000000000a1', 'imp_amy@parity.test', 'x', true, 'imp_amy', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a000000-0000-4000-8000-0000000000a2', 'imp_ben@parity.test', 'x', true, 'imp_ben', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a000000-0000-4000-8000-0000000000a3', 'imp_cid@parity.test', 'x', true, 'imp_cid', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a000000-0000-4000-8000-0000000000a4', 'imp_dan@parity.test', 'x', true, 'imp_dan', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a000000-0000-4000-8000-0000000000a5', 'imp_eve@parity.test', 'x', true, 'imp_eve', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a000000-0000-4000-8000-0000000000a6', 'imp_fay@parity.test', 'x', true, 'imp_fay', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a000000-0000-4000-8000-0000000000a7', 'imp_gus@parity.test', 'x', true, 'imp_gus', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a000000-0000-4000-8000-0000000000a8', 'imp_ned@parity.test', 'x', true, 'imp_ned', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
VALUES
  ('1a000000-0000-4000-8000-0000000000a1', '1a000000-0000-4000-8000-0000000000a1', 'imp_amy''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a000000-0000-4000-8000-0000000000a2', '1a000000-0000-4000-8000-0000000000a2', 'imp_ben''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a000000-0000-4000-8000-0000000000a3', '1a000000-0000-4000-8000-0000000000a3', 'imp_cid''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a000000-0000-4000-8000-0000000000a4', '1a000000-0000-4000-8000-0000000000a4', 'imp_dan''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a000000-0000-4000-8000-0000000000a5', '1a000000-0000-4000-8000-0000000000a5', 'imp_eve''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a000000-0000-4000-8000-0000000000a6', '1a000000-0000-4000-8000-0000000000a6', 'imp_fay''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a000000-0000-4000-8000-0000000000a7', '1a000000-0000-4000-8000-0000000000a7', 'imp_gus''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a000000-0000-4000-8000-0000000000a8', '1a000000-0000-4000-8000-0000000000a8', 'imp_ned''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO spaces (id, tenant_id, name, slug, description, note_count, created_at, updated_at)
VALUES
  ('1a200000-0000-4000-8000-000000000001', '1a000000-0000-4000-8000-0000000000a3', 'Reading', 'reading', 'Long reads', 0, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('1a200000-0000-4000-8000-000000000002', '1a000000-0000-4000-8000-0000000000a5', 'Reading', 'reading', NULL, 0, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at, processed_at)
VALUES
  ('1a300000-0000-4000-8000-000000000001', '1a000000-0000-4000-8000-0000000000a3', 'inbox/photo.jpg', 'image/jpeg', 5, 'im1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('1a300000-0000-4000-8000-000000000002', '1a000000-0000-4000-8000-0000000000a3', 'inbox/notes/doc.md', 'text/markdown', 5, 'im2', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('1a300000-0000-4000-8000-000000000003', '1a000000-0000-4000-8000-0000000000a2', 'inbox/bens.md', 'text/markdown', 5, 'im3', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', NULL),
  ('1a300000-0000-4000-8000-000000000004', '1a000000-0000-4000-8000-0000000000a5', 'inbox/eve.jpg', 'image/jpeg', 5, 'im4', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z');

CREATE VIEW parity_import_sessions AS
SELECT id, tenant_id, source_type, status, total_items, staged_items, created_at, updated_at
FROM import_sessions WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'imp\_%');

CREATE VIEW parity_import_items AS
SELECT id, tenant_id, session_id, vault_file_id, url, title, proposed_space, status, created_at
FROM import_items WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'imp\_%');

CREATE VIEW parity_import_files AS
SELECT id, tenant_id, space_id,
       regexp_replace(path, '\d{4}-\d{2}-\d{2}-\d{6}-(.*)-[0-9a-f]{8}\.md$', '<time>-\1-<suffix>.md') AS path,
       content_type, size_bytes, sha256, processed_at IS NOT NULL AS processed, metadata, created_at, updated_at
FROM vault_files WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'imp\_%');

CREATE VIEW parity_import_memories AS
SELECT id, tenant_id, content, tags, source_vault_file_id, review_state, space_id, origin_kind, created_at
FROM memories WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'imp\_%');

CREATE VIEW parity_import_spaces AS
SELECT id, tenant_id, name, slug, description, color, icon, category, note_count, created_by_user_id, created_at
FROM spaces WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'imp\_%');

CREATE VIEW parity_import_chunks AS
SELECT tenant_id, memory_id, source_path, created_at
FROM memory_chunks WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'imp\_%');

CREATE VIEW parity_import_pending AS
SELECT f.id FROM vault_files f
WHERE f.tenant_id IN (SELECT id FROM users WHERE username LIKE 'imp\_%')
  AND f.metadata->>'enrichmentStatus' = 'pending';
