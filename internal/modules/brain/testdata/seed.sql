-- Alice reads a team vault as a viewer and belongs to an archived one; bob is in neither.
INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at)
VALUES ('7ea40000-0000-4000-8000-000000000001', 'Brain Team', 'b0b00000-0000-4000-8000-000000000002', 'b0b00000-0000-4000-8000-000000000002', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, team_id, name, archived_at, created_at, updated_at)
VALUES
  ('7ea40000-0000-4000-8000-0000000000a1', '7ea40000-0000-4000-8000-000000000001', 'Team Vault', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('7ea40000-0000-4000-8000-0000000000a2', '7ea40000-0000-4000-8000-000000000001', 'Old Vault', '2026-02-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
VALUES
  ('7ea40000-0000-4000-8000-0000000000b1', '7ea40000-0000-4000-8000-0000000000a1', 'a11ce000-0000-4000-8000-000000000001', 'viewer', false, 'b0b00000-0000-4000-8000-000000000002', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('7ea40000-0000-4000-8000-0000000000b2', '7ea40000-0000-4000-8000-0000000000a2', 'a11ce000-0000-4000-8000-000000000001', 'viewer', false, 'b0b00000-0000-4000-8000-000000000002', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Items. Alice's vault: notes N1 (..0a1), N2 (..0a2), memory M1 sourced from N1, and an unresolved link.
-- Team vault: note N3, memory M2. Bob's vault: a note and a memory that nobody else may see.
INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at)
VALUES
  ('00000000-0000-4000-8000-0000000001a1', 'a11ce000-0000-4000-8000-000000000001', 'notes/First Note.md', 'text/markdown', 1, 'x', '2026-03-01T00:00:00Z', '2026-03-05T00:00:00Z'),
  ('00000000-0000-4000-8000-0000000001a2', 'a11ce000-0000-4000-8000-000000000001', 'Second.md', 'text/markdown', 1, 'x', '2026-03-01T00:00:00Z', '2026-03-04T00:00:00Z'),
  ('00000000-0000-4000-8000-0000000001a3', '7ea40000-0000-4000-8000-0000000000a1', 'team/Plan.md', 'text/markdown', 1, 'x', '2026-03-01T00:00:00Z', '2026-03-03T00:00:00Z'),
  ('00000000-0000-4000-8000-0000000001b1', 'b0b00000-0000-4000-8000-000000000002', 'Bob Secret.md', 'text/markdown', 1, 'x', '2026-03-01T00:00:00Z', '2026-03-09T00:00:00Z');

INSERT INTO vault_links (link_id, tenant_id, source_vault_file_id, source_line, raw_target, target_slug, target_vault_file_id, resolved, resolution_state)
VALUES
  (9001, 'a11ce000-0000-4000-8000-000000000001', '00000000-0000-4000-8000-0000000001a1', 1, 'Second', 'second', '00000000-0000-4000-8000-0000000001a2', true, 'resolved'),
  (9002, 'a11ce000-0000-4000-8000-000000000001', '00000000-0000-4000-8000-0000000001a1', 2, 'Missing', 'missing', NULL, false, 'unresolved');

INSERT INTO memories (id, tenant_id, content, created_at, updated_at, source_vault_file_id, graph_x, graph_y, graph_z)
VALUES
  ('00000000-0000-4000-8000-0000000002a1', 'a11ce000-0000-4000-8000-000000000001', E'\nAlice memory\nmore', '2026-03-01T00:00:00Z', '2026-03-06T00:00:00Z', '00000000-0000-4000-8000-0000000001a1', 1, 2, 3),
  ('00000000-0000-4000-8000-0000000002a2', '7ea40000-0000-4000-8000-0000000000a1', 'Team memory', '2026-03-01T00:00:00Z', '2026-03-02T00:00:00Z', NULL, NULL, NULL, NULL),
  ('00000000-0000-4000-8000-0000000002b1', 'b0b00000-0000-4000-8000-000000000002', 'Bob memory', '2026-03-01T00:00:00Z', '2026-03-10T00:00:00Z', NULL, NULL, NULL, NULL);
