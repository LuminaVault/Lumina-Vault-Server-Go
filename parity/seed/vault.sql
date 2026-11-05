-- Fixtures for parity/scenarios/vault. All ids start 5a017000; dates are fixed, so
-- nothing depends on when the run happens. A vault file row with no file on disk
-- is deliberate wherever one appears: read and lint report it.
INSERT INTO users (id, email, password_hash, is_verified, username, tier, vault_initialized, created_at, updated_at)
VALUES
  ('5a017000-0000-4000-8000-000000000001', 'vault-up@parity.test',    'x', true, 'vault_up',    'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-000000000002', 'vault-list@parity.test',  'x', true, 'vault_list',  'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-000000000003', 'vault-rw@parity.test',    'x', true, 'vault_rw',    'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-000000000004', 'vault-move@parity.test',  'x', true, 'vault_move',  'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-000000000005', 'vault-note@parity.test',  'x', true, 'vault_note',  'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-000000000006', 'vault-err@parity.test',   'x', true, 'vault_err',   'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-000000000007', 'vault-idem@parity.test',  'x', true, 'vault_idem',  'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-000000000008', 'vault-new@parity.test',   'x', true, 'vault_new',   'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-000000000009', 'vault-init@parity.test',  'x', true, 'vault_init',  'trial', true,  '2026-01-01T00:00:00Z', '2026-01-02T03:04:05.5Z'),
  ('5a017000-0000-4000-8000-00000000000a', 'vault-idx@parity.test',   'x', true, 'vault_idx',   'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-00000000000b', 'vault-lim@parity.test',   'x', true, 'vault_lim',   'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-00000000000c', 'vault-exp@parity.test',   'x', true, 'vault_exp',   'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-00000000000d', 'vault-free@parity.test',  'x', true, 'vault_free',  'free',  false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-00000000000e', 'vault-big@parity.test',   'x', true, 'vault_big',   'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-00000000000f', 'vault-ed@parity.test',    'x', true, 'vault_ed',    'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-000000000010', 'vault-view@parity.test',  'x', true, 'vault_view',  'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-000000000011', 'vault-out@parity.test',   'x', true, 'vault_out',   'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-000000000012', 'vault-empty@parity.test', 'x', true, 'vault_empty', 'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-000000000013', 'vault-smoke@parity.test', 'x', true, 'vault_smoke', 'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-000000000014', 'vault-pro@parity.test',   'x', true, 'vault_pro',   'pro',   false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-000000000015', 'vault-soul@parity.test',  'x', true, 'vault_soul',  'trial', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
SELECT id, id, username || '''s Vault', created_at, updated_at FROM users WHERE id::text LIKE '5a017000-%';

-- Spaces: upload targets, list filters, context siblings.
INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at)
VALUES
  ('5a017000-0000-4000-8000-0000000001a1', '5a017000-0000-4000-8000-000000000001', 'Papers', 'papers', 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-0000000001a2', '5a017000-0000-4000-8000-000000000002', 'Notes', 'notes', 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-0000000001a3', '5a017000-0000-4000-8000-00000000000a', 'Wiki', 'wiki', 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-0000000001a4', '5a017000-0000-4000-8000-000000000005', 'Journal', 'journal', 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- List: four rows on four dates, with the metadata shapes the column can hold. A
-- suggestion on a filed row must not be served.
INSERT INTO vault_files (id, tenant_id, space_id, path, content_type, size_bytes, sha256, created_at, updated_at, processed_at, metadata, created_by_user_id, updated_by_user_id)
VALUES
  ('5a017000-0000-4000-8000-0000000002a1', '5a017000-0000-4000-8000-000000000002', NULL, 'inbox/a.md', 'text/markdown', 1, 'l1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', NULL, NULL, NULL, NULL),
  ('5a017000-0000-4000-8000-0000000002a2', '5a017000-0000-4000-8000-000000000002', '5a017000-0000-4000-8000-0000000001a2', 'notes/b_c.md', 'text/markdown', 2, 'l2', '2026-01-02T00:00:00Z', '2026-01-02T01:02:03.25Z', '2026-01-02T00:00:00Z',
   '{"title": "Filed", "tags": ["x", "y"], "isTodo": true, "done": false, "dueAt": 790000000.5, "enrichmentStatus": "pending", "suggestedSpaceID": "5A017000-0000-4000-8000-0000000001A2"}',
   '5a017000-0000-4000-8000-000000000002', '5a017000-0000-4000-8000-000000000002'),
  ('5a017000-0000-4000-8000-0000000002a3', '5a017000-0000-4000-8000-000000000002', NULL, 'inbox/100%.md', 'text/markdown', 3, 'l3', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', NULL,
   '{"tags": [], "suggestedSpaceID": "5A017000-0000-4000-8000-0000000001A2", "provenance": "seed", "unknownKey": 1}', NULL, NULL),
  ('5a017000-0000-4000-8000-0000000002a4', '5a017000-0000-4000-8000-000000000002', '5a017000-0000-4000-8000-0000000001a2', 'notes/x.txt', 'text/plain', 4, 'l4', '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z', NULL, '{}', NULL, NULL);

-- Read: a row whose file doesn't exist.
INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at)
VALUES ('5a017000-0000-4000-8000-0000000003a1', '5a017000-0000-4000-8000-000000000003', 'inbox/ghost.md', 'text/markdown', 1, 'g', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Move: rows the scenario re-uploads to give them a file, so the fixed ids below
-- stay: a chunk, a memory and links that follow the move. vault_move's second row
-- is deleted to show the cascade.
INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at)
VALUES
  ('5a017000-0000-4000-8000-0000000004a1', '5a017000-0000-4000-8000-000000000004', 'inbox/chunky.md', 'text/markdown', 1, 'm1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-0000000004a2', '5a017000-0000-4000-8000-000000000004', 'inbox/gone.md', 'text/markdown', 1, 'm2', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-0000000004a3', '5a017000-0000-4000-8000-000000000004', 'inbox/ghost.md', 'text/markdown', 1, 'm3', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO memories (id, tenant_id, content, created_at, source_vault_file_id, review_state, origin_kind)
VALUES
  ('5a017000-0000-4000-8000-0000000004b1', '5a017000-0000-4000-8000-000000000004', 'chunky', '2026-01-01T00:00:00Z', '5a017000-0000-4000-8000-0000000004a1', 'auto', 'legacy'),
  ('5a017000-0000-4000-8000-0000000004b2', '5a017000-0000-4000-8000-000000000004', 'gone', '2026-01-01T00:00:00Z', '5a017000-0000-4000-8000-0000000004a2', 'auto', 'legacy');
INSERT INTO memory_chunks (chunk_id, document_id, tenant_id, memory_id, vault_file_id, source_path, ordinal, heading_path, start_line, end_line, text, content_sha256, created_at)
VALUES
  ('vault-move-1', 'doc-chunky', '5a017000-0000-4000-8000-000000000004', '5a017000-0000-4000-8000-0000000004b1', '5a017000-0000-4000-8000-0000000004a1', 'inbox/chunky.md', 0, '["Top"]', 1, 2, 'chunky', 'c1', '2026-01-01T00:00:00Z'),
  ('vault-move-2', 'doc-gone', '5a017000-0000-4000-8000-000000000004', '5a017000-0000-4000-8000-0000000004b2', '5a017000-0000-4000-8000-0000000004a2', 'inbox/gone.md', 0, '[]', 1, 2, 'gone', 'c2', '2026-01-01T00:00:00Z');
INSERT INTO vault_links (tenant_id, source_vault_file_id, source_path, source_line, raw_target, target_slug, target_vault_file_id, resolved, resolution_state, created_at)
VALUES
  ('5a017000-0000-4000-8000-000000000004', '5a017000-0000-4000-8000-0000000004a2', 'inbox/gone.md', 1, 'chunky', 'chunky', '5a017000-0000-4000-8000-0000000004a1', true, 'resolved', '2026-01-01T00:00:00Z');

-- Index, lint, context and links. hub.md links to a resolved, an unresolved and an
-- ambiguous target and has two headings; dup/hub.md shares its stem; stale.md
-- changed after its chunks; big.md is over 5 MiB by its row; ../escape.md leaves
-- the vault; none of these has a file, so lint reports them unreadable unless a
-- scenario uploads one.
INSERT INTO vault_files (id, tenant_id, space_id, path, content_type, size_bytes, sha256, created_at, updated_at)
VALUES
  ('5a017000-0000-4000-8000-0000000005a1', '5a017000-0000-4000-8000-00000000000a', '5a017000-0000-4000-8000-0000000001a3', 'wiki/hub.md', 'text/markdown', 1, 'i1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-0000000005a2', '5a017000-0000-4000-8000-00000000000a', '5a017000-0000-4000-8000-0000000001a3', 'wiki/leaf.md', 'text/markdown', 1, 'i2', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('5a017000-0000-4000-8000-0000000005a3', '5a017000-0000-4000-8000-00000000000a', '5a017000-0000-4000-8000-0000000001a3', 'wiki/sib.md', 'text/markdown', 1, 'i3', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('5a017000-0000-4000-8000-0000000005a4', '5a017000-0000-4000-8000-00000000000a', NULL, 'dup/hub.md', 'text/markdown', 1, 'i4', '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z'),
  ('5a017000-0000-4000-8000-0000000005a5', '5a017000-0000-4000-8000-00000000000a', NULL, 'inbox/stale.md', 'text/markdown', 1, 'i5', '2026-01-05T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-0000000005a6', '5a017000-0000-4000-8000-00000000000a', NULL, 'inbox/big.md', 'text/markdown', 6291456, 'i6', '2026-01-06T00:00:00Z', '2026-01-06T00:00:00Z'),
  ('5a017000-0000-4000-8000-0000000005a7', '5a017000-0000-4000-8000-00000000000a', NULL, '../escape.md', 'text/markdown', 1, 'i7', '2026-01-07T00:00:00Z', '2026-01-07T00:00:00Z'),
  ('5a017000-0000-4000-8000-0000000005a8', '5a017000-0000-4000-8000-00000000000a', NULL, 'inbox/unindexed.md', 'text/markdown', 1, 'i8', '2026-01-08T00:00:00Z', '2026-01-08T00:00:00Z');
INSERT INTO memories (id, tenant_id, content, created_at, source_vault_file_id, review_state, origin_kind)
VALUES
  ('5a017000-0000-4000-8000-0000000005b1', '5a017000-0000-4000-8000-00000000000a', 'hub', '2026-01-01T00:00:00Z', '5a017000-0000-4000-8000-0000000005a1', 'auto', 'legacy'),
  ('5a017000-0000-4000-8000-0000000005b2', '5a017000-0000-4000-8000-00000000000a', 'stale', '2026-01-05T00:00:00Z', '5a017000-0000-4000-8000-0000000005a5', 'auto', 'legacy');
INSERT INTO memory_chunks (chunk_id, document_id, tenant_id, memory_id, vault_file_id, source_path, ordinal, heading_path, start_line, end_line, text, content_sha256, created_at)
VALUES
  ('vault-idx-1', 'doc-hub', '5a017000-0000-4000-8000-00000000000a', '5a017000-0000-4000-8000-0000000005b1', '5a017000-0000-4000-8000-0000000005a1', 'wiki/hub.md', 0, '["Hub"]', 1, 3, 'one', 'c1', '2026-01-10T00:00:00Z'),
  ('vault-idx-2', 'doc-hub', '5a017000-0000-4000-8000-00000000000a', '5a017000-0000-4000-8000-0000000005b1', '5a017000-0000-4000-8000-0000000005a1', 'wiki/hub.md', 1, '["Hub", "Sub"]', 4, 6, 'two', 'c2', '2026-01-11T00:00:00Z'),
  ('vault-idx-3', 'doc-hub', '5a017000-0000-4000-8000-00000000000a', '5a017000-0000-4000-8000-0000000005b1', '5a017000-0000-4000-8000-0000000005a1', 'wiki/hub.md', 2, '[]', 7, 8, 'three', 'c3', '2026-01-11T00:00:00Z'),
  ('vault-idx-4', 'doc-stale', '5a017000-0000-4000-8000-00000000000a', '5a017000-0000-4000-8000-0000000005b2', '5a017000-0000-4000-8000-0000000005a5', 'inbox/stale.md', 0, '[]', 1, 2, 'stale', 'c4', '2026-01-12T00:00:00Z');
INSERT INTO vault_links (tenant_id, source_vault_file_id, source_path, source_line, raw_target, target_slug, target_heading, label, target_vault_file_id, resolved, resolution_state, created_at)
VALUES
  ('5a017000-0000-4000-8000-00000000000a', '5a017000-0000-4000-8000-0000000005a1', 'wiki/hub.md', 2, 'leaf', 'leaf', 'Top', 'Leaf', '5a017000-0000-4000-8000-0000000005a2', true, 'resolved', '2026-01-10T00:00:00Z'),
  ('5a017000-0000-4000-8000-00000000000a', '5a017000-0000-4000-8000-0000000005a1', 'wiki/hub.md', 3, 'nowhere', 'nowhere', NULL, NULL, NULL, false, 'unresolved', '2026-01-10T00:00:00Z'),
  ('5a017000-0000-4000-8000-00000000000a', '5a017000-0000-4000-8000-0000000005a1', 'wiki/hub.md', 4, 'nowhere', 'nowhere', NULL, NULL, NULL, false, 'unresolved', '2026-01-10T00:00:00Z'),
  ('5a017000-0000-4000-8000-00000000000a', '5a017000-0000-4000-8000-0000000005a1', 'wiki/hub.md', 5, 'hub', 'hub', NULL, NULL, NULL, false, 'ambiguous', '2026-01-10T00:00:00Z'),
  ('5a017000-0000-4000-8000-00000000000a', '5a017000-0000-4000-8000-0000000005a2', 'wiki/leaf.md', 1, 'hub', 'hub', NULL, NULL, '5a017000-0000-4000-8000-0000000005a1', true, 'resolved', '2026-01-10T00:00:00Z');

-- Export: memories in fixed order and with text that JSON escapes differently
-- between encoders, plus enough to cross no page: paging is a Go test.
INSERT INTO memories (id, tenant_id, content, created_at, tags, review_state, origin_kind)
VALUES
  ('5a017000-0000-4000-8000-0000000006b1', '5a017000-0000-4000-8000-000000000014', E'first "line" \\ a/b\nsecond\ttab   café <&>', '2026-01-01T00:00:00.75Z', ARRAY['x', 'y/z'], 'auto', 'legacy'),
  ('5a017000-0000-4000-8000-0000000006b2', '5a017000-0000-4000-8000-000000000014', E'\u0001 ctrl', '2026-02-01T00:00:00Z', NULL, 'auto', 'legacy'),
  ('5a017000-0000-4000-8000-0000000006b3', '5a017000-0000-4000-8000-000000000014', 'no time', NULL, ARRAY[]::text[], 'auto', 'legacy'),
  ('5a017000-0000-4000-8000-0000000006b4', '5a017000-0000-4000-8000-00000000000c', 'exported', '2026-01-01T00:00:00Z', NULL, 'auto', 'legacy');

-- A team vault: an editor can write, a viewer can only read, an outsider neither.
INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at)
VALUES ('5a017000-0000-4000-8000-0000000007c0', 'Vault Team', '5a017000-0000-4000-8000-00000000000f', '5a017000-0000-4000-8000-00000000000f', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO vaults (id, team_id, name, created_at, updated_at)
VALUES ('5a017000-0000-4000-8000-0000000007c1', '5a017000-0000-4000-8000-0000000007c0', 'Team Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
VALUES
  ('5a017000-0000-4000-8000-0000000007d1', '5a017000-0000-4000-8000-0000000007c1', '5a017000-0000-4000-8000-00000000000f', 'editor', true, '5a017000-0000-4000-8000-00000000000f', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5a017000-0000-4000-8000-0000000007d2', '5a017000-0000-4000-8000-0000000007c1', '5a017000-0000-4000-8000-000000000010', 'viewer', false, '5a017000-0000-4000-8000-00000000000f', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at)
VALUES ('5a017000-0000-4000-8000-0000000007a1', '5a017000-0000-4000-8000-0000000007c1', 'Team Notes', 'team-notes', 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
