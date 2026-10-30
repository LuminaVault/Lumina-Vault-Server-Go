-- Spaces fixtures (parity/scenarios/spaces, internal/modules/spaces tests).
-- Alpha has a stale stored note_count (7) and two vault files pointing at it,
-- so the live count (2) is what both servers must return. Zeta has none.
-- Neither has a folder on disk, which delete must tolerate.
INSERT INTO spaces (id, tenant_id, name, slug, description, color, icon, category, note_count, created_at, updated_at)
VALUES
  ('5bace000-0000-4000-8000-0000000000a1', 'a11ce000-0000-4000-8000-000000000001', 'Alpha', 'alpha', 'first', '#ff0000', 'star', 'ai', 7, '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z'),
  ('5bace000-0000-4000-8000-0000000000a2', 'a11ce000-0000-4000-8000-000000000001', 'Zeta', 'zeta', NULL, NULL, NULL, NULL, 0, '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z');

INSERT INTO vault_files (id, tenant_id, space_id, path, content_type, size_bytes, sha256, created_at, updated_at)
VALUES
  ('5bace000-0000-4000-8000-0000000000f1', 'a11ce000-0000-4000-8000-000000000001', '5bace000-0000-4000-8000-0000000000a1', 'raw/alpha/one.md', 'text/markdown', 3, 'sp1', '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z'),
  ('5bace000-0000-4000-8000-0000000000f2', 'a11ce000-0000-4000-8000-000000000001', '5bace000-0000-4000-8000-0000000000a1', 'raw/alpha/two.md', 'text/markdown', 3, 'sp2', '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z');

-- A team vault alice sponsors, where bob is a viewer and alice holds no
-- membership (owning the team grants nothing on its own).
INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at)
VALUES ('5bace000-0000-4000-8000-0000000000c0', 'Parity Team', 'a11ce000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001', '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z');
INSERT INTO vaults (id, team_id, name, created_at, updated_at)
VALUES ('5bace000-0000-4000-8000-0000000000c1', '5bace000-0000-4000-8000-0000000000c0', 'Team Vault', '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z');
INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
VALUES ('5bace000-0000-4000-8000-0000000000d1', '5bace000-0000-4000-8000-0000000000c1', 'b0b00000-0000-4000-8000-000000000002', 'viewer', false, 'a11ce000-0000-4000-8000-000000000001', '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z');
INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at)
VALUES ('5bace000-0000-4000-8000-0000000000a3', '5bace000-0000-4000-8000-0000000000c1', 'Team Notes', 'team-notes', 0, '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z');
