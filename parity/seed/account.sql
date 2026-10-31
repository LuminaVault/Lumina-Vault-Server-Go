-- Account deletion (DELETE /v1/account). Every user here exists to be deleted
-- (or to fail deletion), so no other module's scenario may use them, and the
-- account scenarios never touch alice or bob. Ids match parity/seed/users.yaml.
-- Password for all of them: "correct horse battery staple" (the golden hash).

INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at)
VALUES
  ('acc70000-0000-4000-8000-0000000000a1', 'acct-del@parity.test',    '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'acct-del',    '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('acc70000-0000-4000-8000-0000000000b2', 'acct-fresh@parity.test',  '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'acct-fresh',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('acc70000-0000-4000-8000-0000000000c3', 'acct-stale@parity.test',  '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'acct-stale',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('acc70000-0000-4000-8000-0000000000d4', 'acct-raw@parity.test',    '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'acct-raw',    '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('acc70000-0000-4000-8000-0000000000e5', 'acct-nobody@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'acct-nobody', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('acc70000-0000-4000-8000-0000000000f6', 'acct-type@parity.test',   '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'acct-type',   '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('acc70000-0000-4000-8000-0000000000a7', 'acct-owner@parity.test',  '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'acct-owner',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('acc70000-0000-4000-8000-0000000000b8', 'acct-wrong@parity.test',  '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'acct-wrong',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- acct-del owns rows in tables reached by ON DELETE CASCADE ...
INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
VALUES ('acc70000-0000-4000-8000-0000000000a1', 'acc70000-0000-4000-8000-0000000000a1', 'acct-del''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at, processed_at, metadata)
VALUES ('acc7f000-0000-4000-8000-000000000001', 'acc70000-0000-4000-8000-0000000000a1', 'notes/doomed.md', 'text/markdown', 5, 'acct1', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '{"title": "Doomed"}');

INSERT INTO projects (id, tenant_id, name, description, archived, created_at)
VALUES ('acc7f000-0000-4000-8000-000000000002', 'acc70000-0000-4000-8000-0000000000a1', 'Doomed project', NULL, false, '2026-01-02T00:00:00Z');

INSERT INTO onboarding_state (id, tenant_id, signup_completed, signup_completed_at, created_at, updated_at)
VALUES ('acc7f000-0000-4000-8000-000000000003', 'acc70000-0000-4000-8000-0000000000a1', true, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO health_events (id, tenant_id, event_type, value_numeric, unit, recorded_at, source, metadata, created_at)
VALUES ('acc7f000-0000-4000-8000-000000000004', 'acc70000-0000-4000-8000-0000000000a1', 'steps', 1200, 'count', '2026-01-05T10:00:00Z', 'apple_health', '{"device": "watch"}', '2026-01-05T10:00:01Z');

INSERT INTO device_tokens (id, tenant_id, token, platform, last_seen_at, created_at, updated_at)
VALUES ('acc7f000-0000-4000-8000-000000000005', 'acc70000-0000-4000-8000-0000000000a1', 'acct-del-apns', 'ios', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- The Hermes profile id is read before the cascade (it names the folder the
-- deletion soft-renames); the row itself cascades away.
INSERT INTO hermes_profiles (id, tenant_id, hermes_profile_id, status, created_at, updated_at)
VALUES ('acc7f000-0000-4000-8000-000000000006', 'acc70000-0000-4000-8000-0000000000a1', 'acct-del', 'ready', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- ... and in every table TenantDataEraser handles by hand (no cascade).
INSERT INTO agent_rooms (id, tenant_id, title, created_at, updated_at)
VALUES ('acc7f000-0000-4000-8000-000000000007', 'acc70000-0000-4000-8000-0000000000a1', 'room', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO agent_room_messages (id, room_id, author_kind, body, created_at)
VALUES ('acc7f000-0000-4000-8000-000000000008', 'acc7f000-0000-4000-8000-000000000007', 'user', 'private words', '2026-01-01T00:00:00Z');

INSERT INTO device_command_queue (id, tenant_id, kind, payload, created_at, expires_at)
VALUES ('acc7f000-0000-4000-8000-000000000009', 'acc70000-0000-4000-8000-0000000000a1', 'reminder_create', '{"title":"x"}', '2026-01-01T00:00:00Z', '2099-01-01T00:00:00Z');

INSERT INTO idempotency_keys (id, tenant_id, key, request_hash, response_status, response_body, created_at, expires_at)
VALUES ('acc7f000-0000-4000-8000-00000000000a', 'acc70000-0000-4000-8000-0000000000a1', 'acc7f000-0000-4000-8000-00000000000b', 'h', 200, '\x7b7d'::bytea, '2026-01-01T00:00:00Z', '2099-01-01T00:00:00Z');

INSERT INTO embedding_usage (id, tenant_id, year_month, tokens_used, created_at, updated_at)
VALUES ('acc7f000-0000-4000-8000-00000000000c', 'acc70000-0000-4000-8000-0000000000a1', '1999-01', 10, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO retrieval_telemetry_events (id, tenant_id, source_path, hit_count, zero_hit, limit_requested, created_at)
VALUES ('acc7f000-0000-4000-8000-00000000000d', 'acc70000-0000-4000-8000-0000000000a1', 'chat', 1, false, 5, '2026-01-01T00:00:00Z');

INSERT INTO retrieval_leak_reports (id, tenant_id, period_start, period_end, total_retrievals, zero_hit_count, zero_hit_rate, created_at)
VALUES ('acc7f000-0000-4000-8000-00000000000e', 'acc70000-0000-4000-8000-0000000000a1', '2026-01-01T00:00:00Z', '2026-01-08T00:00:00Z', 10, 1, 0.1, '2026-01-08T00:00:00Z');

-- Both spend-gate spellings (one in an odd case: the erase compares lower()),
-- plus a bucket of acct-fresh's that acct-del's deletion must not touch.
INSERT INTO workflow_spend_buckets (scope_key, period_kind, period_start, updated_at)
VALUES
  ('tenant:ACC70000-0000-4000-8000-0000000000A1', 'day', '2026-01-01', '2026-01-01T00:00:00Z'),
  ('FreeLane:Tenant:acc70000-0000-4000-8000-0000000000a1', 'day', '2026-01-01', '2026-01-01T00:00:00Z'),
  ('team:tenant:ACC70000-0000-4000-8000-0000000000A1', 'day', '2026-01-01', '2026-01-01T00:00:00Z'),
  ('tenant:ACC70000-0000-4000-8000-0000000000B2', 'day', '2026-01-01', '2026-01-01T00:00:00Z');

-- Billing records are kept with user_id nulled.
INSERT INTO billing_event_logs (id, event_id, event_type, user_id, processed_at)
VALUES
  ('acc7f000-0000-4000-8000-00000000000f', 'evt-acct-del', 'INITIAL_PURCHASE', 'acc70000-0000-4000-8000-0000000000a1', '2026-01-01T00:00:00Z'),
  ('acc7f000-0000-4000-8000-000000000010', 'evt-acct-fresh', 'INITIAL_PURCHASE', 'acc70000-0000-4000-8000-0000000000b2', '2026-01-01T00:00:00Z');

-- acct-del acted in alice's vault: the row stays (alice's feed) but loses the
-- name; acct-del's own vault feed cascades away with the vault.
INSERT INTO vault_activity_events (id, vault_id, actor_user_id, actor_name, action, target_type, created_at)
VALUES
  ('acc7f000-0000-4000-8000-000000000011', 'a11ce000-0000-4000-8000-000000000001', 'acc70000-0000-4000-8000-0000000000a1', 'acct-del-display-name', 'note.created', 'note', '2026-01-01T00:00:00Z'),
  ('acc7f000-0000-4000-8000-000000000012', 'acc70000-0000-4000-8000-0000000000a1', 'acc70000-0000-4000-8000-0000000000a1', 'acct-del-display-name', 'note.created', 'note', '2026-01-01T00:00:01Z');

-- ON DELETE SET NULL, left to the FK.
INSERT INTO provider_failover_events (id, tenant_id, provider, model, status_code, source, happened_at)
VALUES ('acc7f000-0000-4000-8000-000000000013', 'acc70000-0000-4000-8000-0000000000a1', 'anthropic', 'claude-haiku', 529, 'chat', '2026-01-01T00:00:00Z');

-- acct-owner owns a team: teams.owner_user_id is ON DELETE RESTRICT, so the
-- user delete fails and the whole transaction (eraser included) rolls back.
INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at)
VALUES ('acc7f000-0000-4000-8000-000000000014', 'Owner''s team', 'acc70000-0000-4000-8000-0000000000a7', 'acc70000-0000-4000-8000-0000000000a7', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO device_command_queue (id, tenant_id, kind, payload, created_at, expires_at)
VALUES ('acc7f000-0000-4000-8000-000000000015', 'acc70000-0000-4000-8000-0000000000a7', 'reminder_create', '{"title":"kept"}', '2026-01-01T00:00:00Z', '2099-01-01T00:00:00Z');
