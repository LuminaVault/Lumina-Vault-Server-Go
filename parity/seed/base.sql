-- Fixture data loaded into parity_template after the Swift `migrate`
-- (scripts/parity-up.sh). Both servers start from these rows. Ids match
-- parity/seed/users.yaml. Shapes copied from rows the Swift server wrote
-- (golden capture, 2026-10-29).

-- Users. The password is irrelevant: the harness mints session tokens.
INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at)
VALUES
  ('a11ce000-0000-4000-8000-000000000001', 'alice@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'alice', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('b0b00000-0000-4000-8000-000000000002', 'bob@parity.test',   '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'bob',   '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Personal vaults: id = owner user id (vault_files.tenant_id references vaults.id).
INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
VALUES
  ('a11ce000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001', 'alice''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('b0b00000-0000-4000-8000-000000000002', 'b0b00000-0000-4000-8000-000000000002', 'bob''s Vault',   '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- A project of alice's with two todos and one done todo pointing at it, so
-- todoCount is exercised (done todos still count in Swift).
INSERT INTO projects (id, tenant_id, name, description, archived, created_at)
VALUES ('5eed0000-0000-4000-8000-0000000000a1', 'a11ce000-0000-4000-8000-000000000001', 'Seeded', 'from base.sql', false, '2026-01-02T00:00:00Z');

-- metadata exactly as Swift's Fluent writes it: projectID as an UPPERCASE
-- string, dates as seconds since 2001-01-01 (Foundation deferredToDate).
INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at, processed_at, metadata)
VALUES
  ('7d000000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001', 'inbox/7D000000-0000-4000-8000-000000000001.md', 'text/markdown', 5, 'seed1', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z',
   '{"done": false, "title": "Paint", "isTodo": true, "projectID": "5EED0000-0000-4000-8000-0000000000A1"}'),
  ('7d000000-0000-4000-8000-000000000002', 'a11ce000-0000-4000-8000-000000000001', 'inbox/7D000000-0000-4000-8000-000000000002.md', 'text/markdown', 5, 'seed2', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z',
   '{"done": true, "dueAt": 815216400.5, "title": "Sand", "isTodo": true, "projectID": "5EED0000-0000-4000-8000-0000000000A1"}'),
  -- not a todo: must not count even though it names the project
  ('7d000000-0000-4000-8000-000000000003', 'a11ce000-0000-4000-8000-000000000001', 'notes/plan.md', 'text/markdown', 5, 'seed3', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z',
   '{"title": "Plan", "projectID": "5EED0000-0000-4000-8000-0000000000A1"}');
