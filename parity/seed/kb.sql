-- /v1/memory-compile/pending fixtures: kb_amy has two unprocessed files and one
-- processed; kb_ben has none.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, created_at, updated_at)
VALUES
  ('6b0b0000-0000-4000-8000-0000000000a1', 'kb_amy@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'kb_amy', 'trial', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0b0000-0000-4000-8000-0000000000a2', 'kb_ben@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'kb_ben', 'trial', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
VALUES
  ('6b0b0000-0000-4000-8000-0000000000a1', '6b0b0000-0000-4000-8000-0000000000a1', 'kb_amy''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0b0000-0000-4000-8000-0000000000a2', '6b0b0000-0000-4000-8000-0000000000a2', 'kb_ben''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z');

INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at, processed_at)
VALUES
  ('6b0b0000-0000-4000-8000-0000000000f1', '6b0b0000-0000-4000-8000-0000000000a1', 'notes/a.md', 'text/markdown', 12, 'kb1', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z', NULL),
  ('6b0b0000-0000-4000-8000-0000000000f2', '6b0b0000-0000-4000-8000-0000000000a1', 'notes/b.md', 'text/markdown', 12, 'kb2', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z', NULL),
  ('6b0b0000-0000-4000-8000-0000000000f3', '6b0b0000-0000-4000-8000-0000000000a1', 'notes/c.md', 'text/markdown', 12, 'kb3', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z');
