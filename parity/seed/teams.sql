-- Teams parity seed (parity/scenarios/teams). Ids use the 7ea5 prefix.
-- Own users, so no settingsByUser bucket (30/min) shared with other modules fills.
-- Times relative to now() are evaluated once when the template is seeded, so
-- both servers see the same rows (restore window 30 days, invitation expiry).

INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at)
VALUES
  ('7ea50000-0000-4000-8000-0000000000a1', 'olga@parity.test', 'x', true,  'team_olga', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('7ea50000-0000-4000-8000-0000000000a2', 'adam@parity.test', 'x', true,  'team_adam', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('7ea50000-0000-4000-8000-0000000000a3', 'mia@parity.test',  'x', true,  'team_mia',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('7ea50000-0000-4000-8000-0000000000a4', 'out@parity.test',  'x', true,  'team_out',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('7ea50000-0000-4000-8000-0000000000a5', 'Ivy@Parity.test',  'x', true,  'team_ivy',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('7ea50000-0000-4000-8000-0000000000a6', 'una@parity.test',  'x', false, 'team_una',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('7ea50000-0000-4000-8000-0000000000a7', 'rex@parity.test',  'x', true,  'team_rex',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('7ea50000-0000-4000-8000-0000000000a8', 'bea@parity.test',  'x', true,  'team_bea',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('7ea50000-0000-4000-8000-0000000000a9', 'max@parity.test',  'x', true,  'team_max',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('7ea50000-0000-4000-8000-0000000000aa', 'tom@parity.test',  'x', true,  'team_tom',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('7ea50000-0000-4000-8000-0000000000ab', 'ada@parity.test',  'x', true,  'team_ada',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('7ea50000-0000-4000-8000-0000000000ac', 'ned@parity.test',  'x', true,  'team_ned',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('7ea50000-0000-4000-8000-0000000000ad', 'kim@parity.test',  'x', true,  'team_kim',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('7ea50000-0000-4000-8000-0000000000ae', 'zoe@parity.test',  'x', true,  'team_zoe',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Personal vaults and their admin memberships, as M90_CreateTeamVaults backfills them.
INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
SELECT id, id, username || '''s Vault', created_at, updated_at FROM users WHERE id::text LIKE '7ea50000-%';
INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
SELECT ('7ea54000' || substr(id::text, 9))::uuid, id, id, 'admin', true, id, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'
FROM users WHERE id::text LIKE '7ea50000-%';

-- Alpha (olga) and Beta (rex) are live; Delta (tom) gets archived and restored
-- by the scenarios; Epsilon was archived yesterday (restorable); Ancient was
-- archived long ago, so the first GET /v1/teams purges it.
INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, archived_at, created_at, updated_at)
VALUES
  ('7ea51000-0000-4000-8000-000000000001', 'Team Alpha',   '7ea50000-0000-4000-8000-0000000000a1', '7ea50000-0000-4000-8000-0000000000a1', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea51000-0000-4000-8000-000000000002', 'Team Beta',    '7ea50000-0000-4000-8000-0000000000a7', '7ea50000-0000-4000-8000-0000000000a7', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea51000-0000-4000-8000-000000000003', 'Team Delta',   '7ea50000-0000-4000-8000-0000000000aa', '7ea50000-0000-4000-8000-0000000000aa', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea51000-0000-4000-8000-000000000004', 'Team Epsilon', '7ea50000-0000-4000-8000-0000000000aa', '7ea50000-0000-4000-8000-0000000000aa', now() - interval '1 day', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea51000-0000-4000-8000-000000000005', 'Team Ancient', '7ea50000-0000-4000-8000-0000000000aa', '7ea50000-0000-4000-8000-0000000000aa', '2026-01-03T00:00:00Z', '2026-01-02T00:00:00Z', '2026-01-03T00:00:00Z');

INSERT INTO team_memberships (id, team_id, user_id, role, created_at, updated_at)
VALUES
  ('7ea52000-0000-4000-8000-000000000001', '7ea51000-0000-4000-8000-000000000001', '7ea50000-0000-4000-8000-0000000000a1', 'owner',  '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea52000-0000-4000-8000-000000000002', '7ea51000-0000-4000-8000-000000000001', '7ea50000-0000-4000-8000-0000000000a2', 'admin',  '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea52000-0000-4000-8000-000000000003', '7ea51000-0000-4000-8000-000000000001', '7ea50000-0000-4000-8000-0000000000a3', 'member', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea52000-0000-4000-8000-000000000004', '7ea51000-0000-4000-8000-000000000002', '7ea50000-0000-4000-8000-0000000000a7', 'owner',  '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea52000-0000-4000-8000-000000000005', '7ea51000-0000-4000-8000-000000000002', '7ea50000-0000-4000-8000-0000000000a8', 'admin',  '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea52000-0000-4000-8000-000000000006', '7ea51000-0000-4000-8000-000000000002', '7ea50000-0000-4000-8000-0000000000a9', 'member', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea52000-0000-4000-8000-000000000007', '7ea51000-0000-4000-8000-000000000002', '7ea50000-0000-4000-8000-0000000000a5', 'member', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea52000-0000-4000-8000-000000000008', '7ea51000-0000-4000-8000-000000000003', '7ea50000-0000-4000-8000-0000000000aa', 'owner',  '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea52000-0000-4000-8000-000000000009', '7ea51000-0000-4000-8000-000000000003', '7ea50000-0000-4000-8000-0000000000ab', 'admin',  '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea52000-0000-4000-8000-00000000000a', '7ea51000-0000-4000-8000-000000000004', '7ea50000-0000-4000-8000-0000000000aa', 'owner',  '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea52000-0000-4000-8000-00000000000b', '7ea51000-0000-4000-8000-000000000005', '7ea50000-0000-4000-8000-0000000000aa', 'owner',  '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  -- zoe: a second Alpha admin without vault seats
  ('7ea52000-0000-4000-8000-00000000000c', '7ea51000-0000-4000-8000-000000000001', '7ea50000-0000-4000-8000-0000000000ae', 'admin',  '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

INSERT INTO vaults (id, team_id, name, archived_at, created_at, updated_at)
VALUES
  ('7ea53000-0000-4000-8000-000000000001', '7ea51000-0000-4000-8000-000000000001', 'Alpha Main',    NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea53000-0000-4000-8000-000000000002', '7ea51000-0000-4000-8000-000000000001', 'alpha lab',     NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea53000-0000-4000-8000-000000000003', '7ea51000-0000-4000-8000-000000000001', 'Alpha Archive', '2026-01-03T00:00:00Z', '2026-01-02T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('7ea53000-0000-4000-8000-000000000004', '7ea51000-0000-4000-8000-000000000002', 'Beta Main',     NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea53000-0000-4000-8000-000000000005', '7ea51000-0000-4000-8000-000000000003', 'Delta Main',    NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea53000-0000-4000-8000-000000000006', '7ea51000-0000-4000-8000-000000000004', 'Epsilon Main',  now() - interval '1 day', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea53000-0000-4000-8000-000000000007', '7ea51000-0000-4000-8000-000000000005', 'Ancient Main',  '2026-01-03T00:00:00Z', '2026-01-02T00:00:00Z', '2026-01-03T00:00:00Z');

-- Alpha Main: olga and adam admins, mia viewer. alpha lab: olga admin, adam
-- editor (mia has no seat, so GET /v1/teams/{alpha}/vaults hides it from her).
INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
VALUES
  ('7ea54100-0000-4000-8000-000000000001', '7ea53000-0000-4000-8000-000000000001', '7ea50000-0000-4000-8000-0000000000a1', 'admin',  true,  '7ea50000-0000-4000-8000-0000000000a1', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea54100-0000-4000-8000-000000000002', '7ea53000-0000-4000-8000-000000000001', '7ea50000-0000-4000-8000-0000000000a2', 'admin',  true,  '7ea50000-0000-4000-8000-0000000000a1', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea54100-0000-4000-8000-000000000003', '7ea53000-0000-4000-8000-000000000001', '7ea50000-0000-4000-8000-0000000000a3', 'viewer', false, '7ea50000-0000-4000-8000-0000000000a1', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea54100-0000-4000-8000-000000000004', '7ea53000-0000-4000-8000-000000000002', '7ea50000-0000-4000-8000-0000000000a1', 'admin',  true,  '7ea50000-0000-4000-8000-0000000000a1', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea54100-0000-4000-8000-000000000005', '7ea53000-0000-4000-8000-000000000002', '7ea50000-0000-4000-8000-0000000000a2', 'editor', false, '7ea50000-0000-4000-8000-0000000000a1', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea54100-0000-4000-8000-000000000006', '7ea53000-0000-4000-8000-000000000003', '7ea50000-0000-4000-8000-0000000000a1', 'admin',  true,  '7ea50000-0000-4000-8000-0000000000a1', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea54100-0000-4000-8000-000000000007', '7ea53000-0000-4000-8000-000000000004', '7ea50000-0000-4000-8000-0000000000a7', 'admin',  true,  '7ea50000-0000-4000-8000-0000000000a7', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea54100-0000-4000-8000-000000000008', '7ea53000-0000-4000-8000-000000000004', '7ea50000-0000-4000-8000-0000000000a8', 'admin',  true,  '7ea50000-0000-4000-8000-0000000000a7', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea54100-0000-4000-8000-000000000009', '7ea53000-0000-4000-8000-000000000004', '7ea50000-0000-4000-8000-0000000000a9', 'editor', false, '7ea50000-0000-4000-8000-0000000000a7', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea54100-0000-4000-8000-00000000000a', '7ea53000-0000-4000-8000-000000000004', '7ea50000-0000-4000-8000-0000000000a5', 'viewer', false, '7ea50000-0000-4000-8000-0000000000a7', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea54100-0000-4000-8000-00000000000b', '7ea53000-0000-4000-8000-000000000005', '7ea50000-0000-4000-8000-0000000000aa', 'admin',  true,  '7ea50000-0000-4000-8000-0000000000aa', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea54100-0000-4000-8000-00000000000c', '7ea53000-0000-4000-8000-000000000005', '7ea50000-0000-4000-8000-0000000000ab', 'admin',  true,  '7ea50000-0000-4000-8000-0000000000aa', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea54100-0000-4000-8000-00000000000d', '7ea53000-0000-4000-8000-000000000006', '7ea50000-0000-4000-8000-0000000000aa', 'admin',  true,  '7ea50000-0000-4000-8000-0000000000aa', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea54100-0000-4000-8000-00000000000e', '7ea53000-0000-4000-8000-000000000007', '7ea50000-0000-4000-8000-0000000000aa', 'admin',  true,  '7ea50000-0000-4000-8000-0000000000aa', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  -- kim: a reader of both Alpha vaults without being on the team
  ('7ea54100-0000-4000-8000-00000000000f', '7ea53000-0000-4000-8000-000000000001', '7ea50000-0000-4000-8000-0000000000ad', 'viewer', false, '7ea50000-0000-4000-8000-0000000000a1', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('7ea54100-0000-4000-8000-000000000010', '7ea53000-0000-4000-8000-000000000002', '7ea50000-0000-4000-8000-0000000000ad', 'viewer', false, '7ea50000-0000-4000-8000-0000000000a1', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

-- Invitations. Swift only ever hands the token to the mailer, so the accept
-- scenarios use seeded tokens 'parity-invite-<n>' (token_hash = sha256 hex).
INSERT INTO team_invitations (id, team_id, email, token_hash, vault_grants, invited_by_user_id, expires_at, accepted_at, revoked_at, created_at)
VALUES
  -- 1: ivy joins Alpha (new team member, new seat in alpha lab)
  ('7ea55000-0000-4000-8000-000000000001', '7ea51000-0000-4000-8000-000000000001', 'ivy@parity.test', encode(sha256('parity-invite-1'::bytea), 'hex'),
   '{"7EA53000-0000-4000-8000-000000000002":{"role":"editor","canUseAI":true}}', '7ea50000-0000-4000-8000-0000000000a1', now() + interval '3 days', NULL, NULL, '2026-01-04T00:00:00Z'),
  -- 2: ivy is already a Beta member and a viewer of Beta Main: both are kept / updated
  ('7ea55000-0000-4000-8000-000000000002', '7ea51000-0000-4000-8000-000000000002', 'ivy@parity.test', encode(sha256('parity-invite-2'::bytea), 'hex'),
   '{"7ea53000-0000-4000-8000-000000000004":{"role":"admin","canUseAI":true}}', '7ea50000-0000-4000-8000-0000000000a7', now() + interval '2 days', NULL, NULL, '2026-01-04T00:00:01Z'),
  -- 3: unverified una
  ('7ea55000-0000-4000-8000-000000000003', '7ea51000-0000-4000-8000-000000000001', 'una@parity.test', encode(sha256('parity-invite-3'::bytea), 'hex'),
   '{"7EA53000-0000-4000-8000-000000000001":{"role":"viewer","canUseAI":false}}', '7ea50000-0000-4000-8000-0000000000a1', now() + interval '4 days', NULL, NULL, '2026-01-04T00:00:02Z'),
  -- 4: expired (still listed: the list filters accepted/revoked only)
  ('7ea55000-0000-4000-8000-000000000004', '7ea51000-0000-4000-8000-000000000001', 'ivy@parity.test', encode(sha256('parity-invite-4'::bytea), 'hex'),
   '{"7EA53000-0000-4000-8000-000000000001":{"role":"viewer","canUseAI":false}}', '7ea50000-0000-4000-8000-0000000000a1', now() - interval '1 day', NULL, NULL, '2026-01-04T00:00:03Z'),
  -- 5: revoked
  ('7ea55000-0000-4000-8000-000000000005', '7ea51000-0000-4000-8000-000000000001', 'ivy@parity.test', encode(sha256('parity-invite-5'::bytea), 'hex'),
   '{"7EA53000-0000-4000-8000-000000000001":{"role":"viewer","canUseAI":false}}', '7ea50000-0000-4000-8000-0000000000a1', now() + interval '6 days', NULL, '2026-01-05T00:00:00Z', '2026-01-04T00:00:04Z'),
  -- 6: already accepted
  ('7ea55000-0000-4000-8000-000000000006', '7ea51000-0000-4000-8000-000000000001', 'ivy@parity.test', encode(sha256('parity-invite-6'::bytea), 'hex'),
   '{"7EA53000-0000-4000-8000-000000000001":{"role":"viewer","canUseAI":false}}', '7ea50000-0000-4000-8000-0000000000a1', now() + interval '6 days', '2026-01-05T00:00:00Z', NULL, '2026-01-04T00:00:05Z'),
  -- 7: team Epsilon is archived
  ('7ea55000-0000-4000-8000-000000000007', '7ea51000-0000-4000-8000-000000000004', 'ivy@parity.test', encode(sha256('parity-invite-7'::bytea), 'hex'),
   '{"7EA53000-0000-4000-8000-000000000006":{"role":"viewer","canUseAI":false}}', '7ea50000-0000-4000-8000-0000000000aa', now() + interval '6 days', NULL, NULL, '2026-01-04T00:00:06Z'),
  -- 8: pending, resent by the scenarios
  ('7ea55000-0000-4000-8000-000000000008', '7ea51000-0000-4000-8000-000000000001', 'resend@parity.test', encode(sha256('parity-invite-8'::bytea), 'hex'),
   '{"7EA53000-0000-4000-8000-000000000001":{"role":"viewer","canUseAI":false}}', '7ea50000-0000-4000-8000-0000000000a1', now() + interval '1 day', NULL, NULL, '2026-01-04T00:00:07Z'),
  -- 9: pending, revoked by the scenarios
  ('7ea55000-0000-4000-8000-000000000009', '7ea51000-0000-4000-8000-000000000001', 'revoke@parity.test', encode(sha256('parity-invite-9'::bytea), 'hex'),
   '{"7EA53000-0000-4000-8000-000000000001":{"role":"viewer","canUseAI":false}}', '7ea50000-0000-4000-8000-0000000000a1', now() + interval '5 days', NULL, NULL, '2026-01-04T00:00:08Z'),
  -- 10: Beta's, for foreign-team checks from Alpha
  ('7ea55000-0000-4000-8000-00000000000a', '7ea51000-0000-4000-8000-000000000002', 'beta@parity.test', encode(sha256('parity-invite-10'::bytea), 'hex'),
   '{"7EA53000-0000-4000-8000-000000000004":{"role":"viewer","canUseAI":false}}', '7ea50000-0000-4000-8000-0000000000a7', now() + interval '5 days', NULL, NULL, '2026-01-04T00:00:09Z'),
  -- 11: Epsilon's pending, for resend on an archived team
  ('7ea55000-0000-4000-8000-00000000000b', '7ea51000-0000-4000-8000-000000000004', 'eps@parity.test', encode(sha256('parity-invite-11'::bytea), 'hex'),
   '{"7EA53000-0000-4000-8000-000000000006":{"role":"viewer","canUseAI":false}}', '7ea50000-0000-4000-8000-0000000000aa', now() + interval '5 days', NULL, NULL, '2026-01-04T00:00:10Z');

-- Activity: two rows on Alpha Main (one with a deleted actor and no target),
-- and 101 on alpha lab so GET .../activity shows its limit of 100.
INSERT INTO vault_activity_events (id, vault_id, actor_user_id, actor_name, action, target_type, target_id, target_title, created_at)
VALUES
  ('7ea56000-0000-4000-8000-000000000001', '7ea53000-0000-4000-8000-000000000001', '7ea50000-0000-4000-8000-0000000000a1', 'team_olga', 'vault.created', 'vault', '7ea53000-0000-4000-8000-000000000001', 'Alpha Main', '2026-01-02T00:00:00Z'),
  ('7ea56000-0000-4000-8000-000000000002', '7ea53000-0000-4000-8000-000000000001', NULL, 'Former member', 'note.deleted', 'note', NULL, NULL, '2026-01-02T00:00:30.250Z');
INSERT INTO vault_activity_events (id, vault_id, actor_user_id, actor_name, action, target_type, target_id, target_title, created_at)
SELECT md5('teams-activity-' || g)::uuid, '7ea53000-0000-4000-8000-000000000002', '7ea50000-0000-4000-8000-0000000000a2', 'team_adam',
       'space.created', 'space', md5('teams-target-' || g)::uuid, 'Space ' || g, '2026-01-03T00:00:00Z'::timestamptz + g * interval '1 minute'
FROM generate_series(1, 101) g;

-- Snapshot view: token_hash is random per server, so the scenarios compare
-- whether it is a sha256 hex digest and whether it is still a seeded token.
-- vault_grants is compared as JSON (Swift's key order is per-process random).
CREATE VIEW parity_team_invitations AS
SELECT id, team_id, email,
       token_hash ~ '^[0-9a-f]{64}$' AS token_hash_is_sha256_hex,
       token_hash IN (SELECT encode(sha256(('parity-invite-' || g)::bytea), 'hex') FROM generate_series(1, 20) g) AS token_is_seeded,
       vault_grants::jsonb AS vault_grants, invited_by_user_id, expires_at, accepted_at, revoked_at, created_at
FROM team_invitations;
