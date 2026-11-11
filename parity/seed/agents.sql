-- /v1/agents fixtures (parity/scenarios/agents, scenarios-switched/agents). One user per
-- scenario file, so no settingsByUser bucket (30 a minute) fills.
--
-- Conversations are the central agent's sessions. Their times are fixed and old, so
-- isActive is false at any hour, except "Live one", whose updated_at is in 2099 and so
-- always within five minutes of now as Swift computes it (a negative interval).
INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, created_at, updated_at)
VALUES
  ('a9e70000-0000-4000-8000-0000000000a1', 'agents_ann@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'agents_ann', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('a9e70000-0000-4000-8000-0000000000a2', 'agents_ben@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'agents_ben', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('a9e70000-0000-4000-8000-0000000000a3', 'agents_cy@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'agents_cy', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('a9e70000-0000-4000-8000-0000000000a4', 'agents_dee@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'agents_dee', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('a9e70000-0000-4000-8000-0000000000a5', 'agents_eve@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'agents_eve', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('a9e70000-0000-4000-8000-0000000000a6', 'agents_fay@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'agents_fay', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('a9e70000-0000-4000-8000-0000000000a7', 'agents_gus@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'agents_gus', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('a9e70000-0000-4000-8000-0000000000a8', 'agents_hal@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'agents_hal', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('a9e70000-0000-4000-8000-0000000000a9', 'agents_ivy@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'agents_ivy', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('a9e70000-0000-4000-8000-0000000000aa', 'agents_jon@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'agents_jon', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('a9e70000-0000-4000-8000-0000000000ab', 'agents_kim@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'agents_kim', 'trial', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z');

INSERT INTO conversations (id, tenant_id, title, created_at, updated_at)
VALUES
  ('a9e7c000-0000-4000-8000-000000000001', 'a9e70000-0000-4000-8000-0000000000a1', 'Ann plans', '2026-02-01T00:00:00Z', '2026-02-05T10:20:30.5Z'),
  ('a9e7c000-0000-4000-8000-000000000002', 'a9e70000-0000-4000-8000-0000000000a1', '', '2026-02-01T00:00:00Z', '2026-02-03T00:00:00Z'),
  ('a9e7c000-0000-4000-8000-000000000003', 'a9e70000-0000-4000-8000-0000000000a1', 'Live one', '2026-02-01T00:00:00Z', '2099-01-01T00:00:00Z'),
  ('a9e7c000-0000-4000-8000-000000000004', 'a9e70000-0000-4000-8000-0000000000a2', 'Ben plans', '2026-02-01T00:00:00Z', '2026-02-04T00:00:00Z');

INSERT INTO conversation_messages (id, conversation_id, role, content, created_at)
VALUES
  ('a9e7d000-0000-4000-8000-000000000001', 'a9e7c000-0000-4000-8000-000000000001', 'user', 'Where to in May?', '2026-02-05T10:00:00Z'),
  ('a9e7d000-0000-4000-8000-000000000002', 'a9e7c000-0000-4000-8000-000000000001', 'assistant', 'Lisbon, by train.', '2026-02-05T10:00:01.25Z'),
  ('a9e7d000-0000-4000-8000-000000000003', 'a9e7c000-0000-4000-8000-000000000003', 'user', 'still here', '2026-02-06T00:00:00Z'),
  ('a9e7d000-0000-4000-8000-000000000004', 'a9e7c000-0000-4000-8000-000000000004', 'user', 'Bob only', '2026-02-04T00:00:00Z');

-- Personas: the candidates a room can seat. Ann's labels sort differently from their
-- slugs, and her default sorts first. Ivy's and Jon's are the agents in the LLM state.
INSERT INTO user_hermes_profiles (id, tenant_id, slug, label, system_prompt, is_default, created_at, updated_at)
VALUES
  ('a9e76000-0000-4000-8000-000000000001', 'a9e70000-0000-4000-8000-0000000000a1', 'research', 'Research', 'p', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a9e76000-0000-4000-8000-000000000002', 'a9e70000-0000-4000-8000-0000000000a1', 'ops', 'Ops desk', 'p', true, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a9e76000-0000-4000-8000-000000000003', 'a9e70000-0000-4000-8000-0000000000a1', 'Mac MCP', 'Apple side', 'p', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a9e76000-0000-4000-8000-000000000004', 'a9e70000-0000-4000-8000-0000000000a3', 'default', 'Lumina', 'p', true, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a9e76000-0000-4000-8000-000000000005', 'a9e70000-0000-4000-8000-0000000000a3', 'stocks', 'Stocks', 'p', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a9e76000-0000-4000-8000-000000000006', 'a9e70000-0000-4000-8000-0000000000a4', 'default', 'Lumina', 'p', true, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a9e76000-0000-4000-8000-000000000007', 'a9e70000-0000-4000-8000-0000000000a4', 'stocks', 'Stocks', 'p', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a9e76000-0000-4000-8000-000000000008', 'a9e70000-0000-4000-8000-0000000000a5', 'alpha', 'Alpha', 'p', true, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a9e76000-0000-4000-8000-000000000009', 'a9e70000-0000-4000-8000-0000000000a9', 'alpha', 'Alpha', 'p', true, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a9e76000-0000-4000-8000-00000000000a', 'a9e70000-0000-4000-8000-0000000000a9', 'beta', 'Beta', 'p', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a9e76000-0000-4000-8000-00000000000b', 'a9e70000-0000-4000-8000-0000000000aa', 'alpha', 'Alpha', 'p', true, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Ivy has a Hermes slot, so her session key is <hermes_profile_id>:<slug>; Jon has
-- none, so his is pending-<TENANT>:<slug>.
INSERT INTO hermes_profiles (id, tenant_id, hermes_profile_id, status, created_at, updated_at)
VALUES ('a9e7f000-0000-4000-8000-000000000001', 'a9e70000-0000-4000-8000-0000000000a9', 'hp-agents-ivy', 'ready', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Eve's rooms for chains that end before any agent is asked, and one whose agent
-- fails (no gateway in the main run). Members carry fixed, distinct created_at.
INSERT INTO agent_rooms (id, tenant_id, title, token_budget, spent_tokens, stop_requested_at, created_at, updated_at)
VALUES
  ('a9e7a000-0000-4000-8000-000000000001', 'a9e70000-0000-4000-8000-0000000000a5', 'Spent', 10, 10, NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('a9e7a000-0000-4000-8000-000000000002', 'a9e70000-0000-4000-8000-0000000000a5', 'Stopped ahead', 200000, 0, '2099-01-01T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('a9e7a000-0000-4000-8000-000000000003', 'a9e70000-0000-4000-8000-0000000000a5', 'No gateway', 200000, 0, NULL, '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z'),
  ('a9e7a000-0000-4000-8000-000000000004', 'a9e70000-0000-4000-8000-0000000000a5', 'Ghost member', 200000, 0, NULL, '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z');

INSERT INTO agent_room_members (id, room_id, instance_id, profile, handle, display_name, respond_mode, created_at)
VALUES
  ('a9e7b000-0000-4000-8000-000000000001', 'a9e7a000-0000-4000-8000-000000000001', 'central', 'alpha', 'alpha', 'Alpha', 'every_human_message', '2026-01-02T00:00:01Z'),
  ('a9e7b000-0000-4000-8000-000000000002', 'a9e7a000-0000-4000-8000-000000000002', 'central', 'alpha', 'alpha', 'Alpha', 'every_human_message', '2026-01-03T00:00:01Z'),
  ('a9e7b000-0000-4000-8000-000000000003', 'a9e7a000-0000-4000-8000-000000000003', 'central', 'alpha', 'alpha', 'Alpha', 'every_human_message', '2026-01-04T00:00:01Z'),
  ('a9e7b000-0000-4000-8000-000000000004', 'a9e7a000-0000-4000-8000-000000000003', 'byo', NULL, 'mine', 'My Hermes', 'mention', '2026-01-04T00:00:02Z'),
  ('a9e7b000-0000-4000-8000-000000000005', 'a9e7a000-0000-4000-8000-000000000004', 'elsewhere', NULL, 'ghost', 'Ghost', 'weird_mode', '2026-01-05T00:00:01Z');

INSERT INTO agent_room_messages (id, room_id, author_kind, member_id, body, tokens, created_at)
VALUES
  ('a9e7e000-0000-4000-8000-000000000001', 'a9e7a000-0000-4000-8000-000000000003', 'human', NULL, 'earlier', NULL, '2026-01-04T00:01:00Z'),
  ('a9e7e000-0000-4000-8000-000000000002', 'a9e7a000-0000-4000-8000-000000000003', 'agent', 'a9e7b000-0000-4000-8000-000000000003', 'earlier reply', 12, '2026-01-04T00:02:00Z'),
  ('a9e7e000-0000-4000-8000-000000000003', 'a9e7a000-0000-4000-8000-000000000003', 'odd', NULL, 'unknown author kind', NULL, '2026-01-04T00:03:00Z');
