-- Kanban fixtures (parity/scenarios/kanban). Loaded after base.sql.
--
-- Swift rate-limits /v1/boards and /v1/cards with `settingsByUser` (30
-- requests per 60 s per user, and per IP when there is no identity). The
-- kanban scenarios therefore spread their requests over alice, bob and three
-- users of their own (parity/seed/users.yaml) so no bucket ever fills; a
-- filled bucket would make the run depend on wall-clock time.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
VALUES
  ('ca4b0000-0000-4000-8000-0000000000a1', 'kanban-ann@parity.test', 'x', true, 'kanban_ann', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca4b0000-0000-4000-8000-0000000000b2', 'kanban-ben@parity.test', 'x', true, 'kanban_ben', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca4b0000-0000-4000-8000-0000000000c3', 'kanban-cy@parity.test',  'x', true, 'kanban_cy',  'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  -- Billing sponsor of the second team; archived, so its vault has no AI (402).
  ('ca4b0000-0000-4000-8000-0000000000d4', 'kanban-zed@parity.test', 'x', true, 'kanban_zed', 'archived', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
VALUES
  ('ca4b0000-0000-4000-8000-0000000000a1', 'ca4b0000-0000-4000-8000-0000000000a1', 'kanban_ann''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca4b0000-0000-4000-8000-0000000000b2', 'ca4b0000-0000-4000-8000-0000000000b2', 'kanban_ben''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca4b0000-0000-4000-8000-0000000000c3', 'ca4b0000-0000-4000-8000-0000000000c3', 'kanban_cy''s Vault',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca4b0000-0000-4000-8000-0000000000d4', 'ca4b0000-0000-4000-8000-0000000000d4', 'kanban_zed''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Team 1 (sponsor alice): one live shared vault, one archived.
-- Team 2 (sponsor zed, archived tier): one shared vault.
INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at)
VALUES
  ('ca4b7000-0000-4000-8000-000000000001', 'Kanban Team', 'a11ce000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca4b7000-0000-4000-8000-000000000002', 'Lapsed Team', 'ca4b0000-0000-4000-8000-0000000000d4', 'ca4b0000-0000-4000-8000-0000000000d4', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, team_id, name, archived_at, created_at, updated_at)
VALUES
  ('ca4bf000-0000-4000-8000-000000000001', 'ca4b7000-0000-4000-8000-000000000001', 'Team Vault', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca4bf000-0000-4000-8000-000000000002', 'ca4b7000-0000-4000-8000-000000000002', 'Lapsed Vault', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca4bf000-0000-4000-8000-000000000003', 'ca4b7000-0000-4000-8000-000000000001', 'Old Vault', '2026-01-05T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-05T00:00:00Z');

-- alice admin+AI, bob viewer, kanban_ben editor without AI; alice is an AI
-- member of the lapsed team's vault and of the archived vault.
INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
VALUES
  ('ca4be000-0000-4000-8000-000000000001', 'ca4bf000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001', 'admin',  true,  'a11ce000-0000-4000-8000-000000000001', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca4be000-0000-4000-8000-000000000002', 'ca4bf000-0000-4000-8000-000000000001', 'b0b00000-0000-4000-8000-000000000002', 'viewer', false, 'a11ce000-0000-4000-8000-000000000001', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca4be000-0000-4000-8000-000000000003', 'ca4bf000-0000-4000-8000-000000000001', 'ca4b0000-0000-4000-8000-0000000000b2', 'editor', false, 'a11ce000-0000-4000-8000-000000000001', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca4be000-0000-4000-8000-000000000004', 'ca4bf000-0000-4000-8000-000000000002', 'a11ce000-0000-4000-8000-000000000001', 'editor', true,  'ca4b0000-0000-4000-8000-0000000000d4', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca4be000-0000-4000-8000-000000000005', 'ca4bf000-0000-4000-8000-000000000003', 'a11ce000-0000-4000-8000-000000000001', 'admin',  true,  'a11ce000-0000-4000-8000-000000000001', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Boards. alice: one live personal board and one archived; bob: one; the
-- shared team vault: one at version 7.
INSERT INTO kanban_boards (id, tenant_id, title, version, archived_at, created_at, updated_at, created_by_user_id, updated_by_user_id)
VALUES
  ('ca4bb000-0000-4000-8000-0000000000a1', 'a11ce000-0000-4000-8000-000000000001', 'Alice Board', 2, NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z', 'a11ce000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001'),
  ('ca4bb000-0000-4000-8000-0000000000a2', 'a11ce000-0000-4000-8000-000000000001', 'Old Board', 4, '2026-01-04T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-04T00:00:00Z', NULL, NULL),
  ('ca4bb000-0000-4000-8000-0000000000b1', 'b0b00000-0000-4000-8000-000000000002', 'Bob Board', 1, NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z', NULL, NULL),
  ('ca4bb000-0000-4000-8000-0000000000f1', 'ca4bf000-0000-4000-8000-000000000001', 'Team Board', 7, NULL, '2026-01-02T00:00:00Z', '2026-01-03T00:00:00Z', 'a11ce000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001');

INSERT INTO kanban_columns (id, tenant_id, board_id, title, rank, created_at, updated_at)
VALUES
  ('ca4bc000-0000-4000-8000-0000000000a1', 'a11ce000-0000-4000-8000-000000000001', 'ca4bb000-0000-4000-8000-0000000000a1', 'Todo', 'i', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('ca4bc000-0000-4000-8000-0000000000a2', 'a11ce000-0000-4000-8000-000000000001', 'ca4bb000-0000-4000-8000-0000000000a1', 'Doing', 'r', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('ca4bc000-0000-4000-8000-0000000000b1', 'b0b00000-0000-4000-8000-000000000002', 'ca4bb000-0000-4000-8000-0000000000b1', 'Inbox', 'i', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('ca4bc000-0000-4000-8000-0000000000f1', 'ca4bf000-0000-4000-8000-000000000001', 'ca4bb000-0000-4000-8000-0000000000f1', 'Backlog', 'i', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('ca4bc000-0000-4000-8000-0000000000f2', 'ca4bf000-0000-4000-8000-000000000001', 'ca4bb000-0000-4000-8000-0000000000f1', 'Shipped', 'r', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

-- extra is exactly what Fluent writes for CardExtra: camelCase keys, the
-- UUID uppercase, dates as seconds since 2001-01-01, nil keys omitted.
INSERT INTO kanban_cards (id, tenant_id, board_id, column_id, title, body, rank, priority, due_at, created_at, updated_at, extra, created_by_user_id, updated_by_user_id)
VALUES
  ('ca4bd000-0000-4000-8000-0000000000a1', 'a11ce000-0000-4000-8000-000000000001', 'ca4bb000-0000-4000-8000-0000000000a1', 'ca4bc000-0000-4000-8000-0000000000a1', 'Alpha', 'first', 'i', 'urgent', '2026-11-01T09:00:00Z', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z', NULL, 'a11ce000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001'),
  ('ca4bd000-0000-4000-8000-0000000000a2', 'a11ce000-0000-4000-8000-000000000001', 'ca4bb000-0000-4000-8000-0000000000a1', 'ca4bc000-0000-4000-8000-0000000000a1', 'Beta', NULL, 'r', NULL, NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z',
   '{"job": {"source": "vault", "cron": "0 7 * * *", "runAt": 815216400.5, "domain": "tech", "prompt": "Scan", "spaceID": "5EED0000-0000-4000-8000-0000000000A1"}}', NULL, NULL),
  ('ca4bd000-0000-4000-8000-0000000000b1', 'b0b00000-0000-4000-8000-000000000002', 'ca4bb000-0000-4000-8000-0000000000b1', 'ca4bc000-0000-4000-8000-0000000000b1', 'Bob card', NULL, 'i', 'low', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z', NULL, NULL, NULL),
  ('ca4bd000-0000-4000-8000-0000000000f1', 'ca4bf000-0000-4000-8000-000000000001', 'ca4bb000-0000-4000-8000-0000000000f1', 'ca4bc000-0000-4000-8000-0000000000f1', 'Team card', 'from seed', 'i', 'high', '2026-11-01T09:00:00Z', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z', NULL, 'a11ce000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001'),
  ('ca4bd000-0000-4000-8000-0000000000f2', 'ca4bf000-0000-4000-8000-000000000001', 'ca4bb000-0000-4000-8000-0000000000f1', 'ca4bc000-0000-4000-8000-0000000000f1', 'Promoted card', NULL, 'r', NULL, NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z',
   '{"job": {"source": "vault", "cron": "0 9 * * 1", "prompt": "Seeded job", "jobSlug": "job-seeded", "promotedAt": 815216400}}', NULL, NULL);
