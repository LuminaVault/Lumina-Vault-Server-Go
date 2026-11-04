-- Fixtures for parity/scenarios/base/{strict-ids,body-limit}.yaml. Loaded after
-- every other seed. Their own users keep their requests out of everyone else's
-- rate-limit buckets.
INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
VALUES
  ('57c70000-0000-4000-8000-0000000000a1', 'strict-ann@parity.test', 'x', true, 'strict_ann', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('57c70000-0000-4000-8000-0000000000a2', 'limit-ann@parity.test',  'x', true, 'limit_ann',  'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
VALUES
  ('57c70000-0000-4000-8000-0000000000a1', '57c70000-0000-4000-8000-0000000000a1', 'strict_ann''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('57c70000-0000-4000-8000-0000000000a2', '57c70000-0000-4000-8000-0000000000a2', 'limit_ann''s Vault',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO projects (id, tenant_id, name, description, archived, created_at)
VALUES ('57c70000-0000-4000-8000-0000000000b1', '57c70000-0000-4000-8000-0000000000a1', 'Counted', NULL, false, '2026-01-02T00:00:00Z');

-- A todo naming the project, so the list shows its todoCount.
INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at, processed_at, metadata)
VALUES
  ('57c70000-0000-4000-8000-0000000000c1', '57c70000-0000-4000-8000-0000000000a1', 'inbox/a.md', 'text/markdown', 5, 'seed1', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z',
   '{"done": false, "title": "Canonical", "isTodo": true, "projectID": "57C70000-0000-4000-8000-0000000000B1"}');
