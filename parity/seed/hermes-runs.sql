-- hermes runs fixtures (parity/scenarios/hermes, scenarios-switched/*/hermes, wave 6C).
-- One user per scenario file, so no conversationByUser bucket fills.
--   hr_ann  validation and error paths; owns a run nobody else may read
--   hr_ben  reads over seeded runs (list, get, events replay, stop on terminal)
--   hr_cy   the live flow through the stub (switched state "chat")
--   hr_dee  unmounted routes (switched state "no-master-key")
-- Only terminal runs are seeded: Swift's boot re-attach would mark an active one
-- lost, and Go doesn't re-attach unless LV_GO_JOBS names HermesRunsService.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
VALUES
  ('e6c00000-0000-4000-8000-0000000000a1', 'hr_ann@parity.test', 'x', true, 'hr_ann', 'pro', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('e6c00000-0000-4000-8000-0000000000b2', 'hr_ben@parity.test', 'x', true, 'hr_ben', 'pro', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('e6c00000-0000-4000-8000-0000000000c3', 'hr_cy@parity.test', 'x', true, 'hr_cy', 'pro', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('e6c00000-0000-4000-8000-0000000000d4', 'hr_dee@parity.test', 'x', true, 'hr_dee', 'pro', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
SELECT id, id, username || '''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'
FROM users WHERE id::text LIKE 'e6c00000-%';

INSERT INTO conversations (id, tenant_id, title, pinned_memory_ids, created_at, updated_at)
VALUES
  ('e6c00000-0000-4000-8000-0000000002b1', 'e6c00000-0000-4000-8000-0000000000b2', 'Agent', '{}', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('e6c00000-0000-4000-8000-0000000002c1', 'e6c00000-0000-4000-8000-0000000000c3', 'Agent', '{}', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

-- hr_ben: a completed run in a conversation with three events, a failed standalone
-- run, and a row whose status string no build knows (it reads as lost) carrying a
-- pending approval stored as Fluent writes it (requestedAt in reference-date seconds).
INSERT INTO hermes_runs (id, tenant_id, hermes_run_id, status, prompt, session_id, model, conversation_id,
  started_at, finished_at, last_event, last_seq, pending_approval, summary, error, created_at, updated_at)
VALUES
  ('e6c00000-0000-4000-8000-0000000001b1', 'e6c00000-0000-4000-8000-0000000000b2', 'run_seed_b1', 'completed',
   'audit the vault', 'sess-1', 'hermes-3', 'e6c00000-0000-4000-8000-0000000002b1',
   '2026-01-03T10:00:00Z', '2026-01-03T10:00:05Z', 'run.completed', 3, NULL, 'vault is clean', NULL,
   '2026-01-03T10:00:00Z', '2026-01-03T10:00:05Z'),
  ('e6c00000-0000-4000-8000-0000000001b2', 'e6c00000-0000-4000-8000-0000000000b2', 'run_seed_b2', 'failed',
   'delete the temp files', NULL, NULL, NULL,
   '2026-01-02T10:00:00Z', '2026-01-02T10:00:03Z', 'run.failed', 2, NULL, NULL, 'approval denied',
   '2026-01-02T10:00:00Z', '2026-01-02T10:00:03Z'),
  ('e6c00000-0000-4000-8000-0000000001b3', 'e6c00000-0000-4000-8000-0000000000b2', 'run_seed_b3', 'exploded',
   'a status from the future', NULL, NULL, NULL,
   '2026-01-01T10:00:00Z', NULL, 'approval.request', 1,
   '{"command":"rm -rf ***","choices":["once","deny"],"requestedAt":789048000,"extra":{"tool":"shell","depth":2}}',
   NULL, NULL, '2026-01-01T10:00:00Z', '2026-01-01T10:00:00Z'),
  ('e6c00000-0000-4000-8000-0000000001a1', 'e6c00000-0000-4000-8000-0000000000a1', 'run_seed_a1', 'completed',
   'mine alone', NULL, NULL, NULL,
   '2026-01-03T10:00:00Z', '2026-01-03T10:00:01Z', 'run.completed', 0, NULL, 'done', NULL,
   '2026-01-03T10:00:00Z', '2026-01-03T10:00:01Z');

INSERT INTO hermes_run_events (id, run_id, seq, event, payload, at)
VALUES
  ('e6c00000-0000-4000-8000-0000000003b1', 'e6c00000-0000-4000-8000-0000000001b1', 1, 'run.started',
   '{"event":"run.started","run_id":"run_seed_b1"}', '2026-01-03T10:00:01Z'),
  ('e6c00000-0000-4000-8000-0000000003b2', 'e6c00000-0000-4000-8000-0000000001b1', 2, 'tool.completed',
   '{"event":"tool.completed","tool":"shell","duration":1.5,"result":{"rows":[1,2.5],"ok":true}}', '2026-01-03T10:00:02Z'),
  ('e6c00000-0000-4000-8000-0000000003b3', 'e6c00000-0000-4000-8000-0000000001b1', 3, 'run.completed',
   '{"event":"run.completed","output":"vault is clean"}', '2026-01-03T10:00:05Z'),
  ('e6c00000-0000-4000-8000-0000000003b4', 'e6c00000-0000-4000-8000-0000000001b2', 1, 'run.started',
   '{"event":"run.started"}', '2026-01-02T10:00:01Z'),
  ('e6c00000-0000-4000-8000-0000000003b5', 'e6c00000-0000-4000-8000-0000000001b2', 2, 'run.failed',
   '{"event":"run.failed","error":"approval denied"}', '2026-01-02T10:00:03Z');
