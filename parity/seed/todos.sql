-- Todos fixtures (parity/scenarios/todos). Loaded after base.sql.
--
-- Metadata exactly as Swift's Fluent writes it: projectID as an UPPERCASE string,
-- dates as seconds since 2001-01-01. No file on disk backs these rows, which
-- delete must tolerate.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
VALUES
  ('70d00000-0000-4000-8000-0000000000a1', 'todo-tia@parity.test', 'x', true, 'todo_tia', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('70d00000-0000-4000-8000-0000000000b2', 'todo-tom@parity.test', 'x', true, 'todo_tom', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
VALUES
  ('70d00000-0000-4000-8000-0000000000a1', '70d00000-0000-4000-8000-0000000000a1', 'todo_tia''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('70d00000-0000-4000-8000-0000000000b2', '70d00000-0000-4000-8000-0000000000b2', 'todo_tom''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO projects (id, tenant_id, name, archived, created_at)
VALUES
  ('70d00000-0000-4000-8000-0000000001a1', '70d00000-0000-4000-8000-0000000000a1', 'Tia project', false, '2026-01-02T00:00:00Z'),
  ('70d00000-0000-4000-8000-0000000001a2', '70d00000-0000-4000-8000-0000000000a1', 'Tia archived', true, '2026-01-02T00:00:00Z'),
  ('70d00000-0000-4000-8000-0000000001b2', '70d00000-0000-4000-8000-0000000000b2', 'Tom project', false, '2026-01-02T00:00:00Z');

INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at, processed_at, metadata)
VALUES
  -- Listed: dates decide among the open ones, and no date sorts last. Swift's query
  -- has no ORDER BY, so a tie comes in whichever order Postgres scans the rows. Only
  -- one open row and one done row may share a sort position, and no date is a position.
  ('70d00000-0000-4000-8000-000000000f01', '70d00000-0000-4000-8000-0000000000a1', 'inbox/later.md', 'text/markdown', 5, 'seed', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z',
   '{"done": false, "dueAt": 815216400.5, "title": "Later", "isTodo": true, "projectID": "70D00000-0000-4000-8000-0000000001A1"}'),
  ('70d00000-0000-4000-8000-000000000f02', '70d00000-0000-4000-8000-0000000000a1', 'inbox/sooner.md', 'text/markdown', 5, 'seed', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z',
   '{"done": false, "dueAt": 790000000, "title": "Sooner", "isTodo": true}'),
  ('70d00000-0000-4000-8000-000000000f03', '70d00000-0000-4000-8000-0000000000a1', 'inbox/undated.md', 'text/markdown', 5, 'seed', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z',
   '{"done": false, "title": "Undated", "isTodo": true}'),
  ('70d00000-0000-4000-8000-000000000f04', '70d00000-0000-4000-8000-0000000000a1', 'inbox/finished.md', 'text/markdown', 5, 'seed', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z',
   '{"done": true, "dueAt": 700000000, "title": "Finished", "isTodo": true, "projectID": "70D00000-0000-4000-8000-0000000001A1"}'),
  -- Title falls back to the file name when empty or absent; done defaults to false.
  ('70d00000-0000-4000-8000-000000000f05', '70d00000-0000-4000-8000-0000000000a1', 'deep/er/empty-title.md', 'text/markdown', 5, 'seed', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', NULL,
   '{"dueAt": 800000000, "title": "", "isTodo": true}'),
  ('70d00000-0000-4000-8000-000000000f06', '70d00000-0000-4000-8000-0000000000a1', 'no-title.md', 'text/markdown', 5, 'seed', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', NULL,
   '{"dueAt": 800000001, "isTodo": true}'),
  -- Not listed, but PATCH makes them todos and DELETE takes them.
  ('70d00000-0000-4000-8000-000000000f07', '70d00000-0000-4000-8000-0000000000a1', 'notes/plain-note.md', 'text/markdown', 5, 'seed', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', NULL,
   '{"title": "Plain note", "tags": ["keep"], "provenance": "hermes-mirror", "enrichmentStatus": "done", "suggestedSpaceID": "70D00000-0000-4000-8000-0000000002A1", "unknownKey": 1}'),
  ('70d00000-0000-4000-8000-000000000f08', '70d00000-0000-4000-8000-0000000000a1', 'notes/null-metadata.md', 'text/markdown', 5, 'seed', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', NULL,
   NULL),
  ('70d00000-0000-4000-8000-000000000f09', '70d00000-0000-4000-8000-0000000000a1', 'notes/not-a-todo.md', 'text/markdown', 5, 'seed', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', NULL,
   '{"isTodo": false, "done": true, "title": "Not a todo"}'),
  ('70d00000-0000-4000-8000-000000000f0a', '70d00000-0000-4000-8000-0000000000a1', 'notes/to-delete.md', 'text/markdown', 5, 'seed', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', NULL,
   '{"title": "Delete me"}'),
  -- todo_tom's.
  ('70d00000-0000-4000-8000-000000000f11', '70d00000-0000-4000-8000-0000000000b2', 'inbox/tom.md', 'text/markdown', 5, 'seed', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z',
   '{"done": false, "title": "Tom todo", "isTodo": true, "projectID": "70D00000-0000-4000-8000-0000000001B2"}');

-- A memory bound to a file, for DELETE's cascade.
INSERT INTO memories (id, tenant_id, content, source_vault_file_id, created_at)
VALUES ('70d00000-0000-4000-8000-000000000e0a', '70d00000-0000-4000-8000-0000000000a1', 'Delete me too', '70d00000-0000-4000-8000-000000000f0a', '2026-01-03T00:00:00Z');

-- A created todo's file name is `inbox/<uuid>.md`, and only a view can mask a UUID
-- inside a string.
CREATE VIEW parity_todo_files AS
SELECT id, tenant_id, created_by_user_id, updated_by_user_id, space_id,
       regexp_replace(path, '[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}', '<uuid>') AS path,
       content_type, size_bytes,
       sha256,
       processed_at, metadata, created_at, updated_at
FROM vault_files
WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'todo\_%');

CREATE VIEW parity_todo_memories AS
SELECT * FROM memories
WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'todo\_%');
