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
