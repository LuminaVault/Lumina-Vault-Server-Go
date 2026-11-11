-- chat fixtures (parity/scenarios/chat, scenarios-switched/*/chat, wave 5C).
-- One user per concern, so no conversationByUser or settingsByUser bucket fills.
--   chat_amy  CRUD and reads over seeded conversations
--   chat_ben  validation and error paths
--   chat_cid  chat preferences
--   chat_dee  hybrid-execution preferences
--   chat_eve  inbox
--   chat_fay  local execution (prepare, tools, commit, cancel)
--   chat_gus  the classic stream and escalation (switched state "chat")
--   chat_hal  isolation: owns a conversation nobody else may read
--   chat_ivy  POST /v1/llm/chat

INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
VALUES
  ('c4a70000-0000-4000-8000-0000000000a1', 'chat_amy@parity.test', 'x', true, 'chat_amy', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c4a70000-0000-4000-8000-0000000000a2', 'chat_ben@parity.test', 'x', true, 'chat_ben', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c4a70000-0000-4000-8000-0000000000a3', 'chat_cid@parity.test', 'x', true, 'chat_cid', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c4a70000-0000-4000-8000-0000000000a4', 'chat_dee@parity.test', 'x', true, 'chat_dee', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c4a70000-0000-4000-8000-0000000000a5', 'chat_eve@parity.test', 'x', true, 'chat_eve', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c4a70000-0000-4000-8000-0000000000a6', 'chat_fay@parity.test', 'x', true, 'chat_fay', 'pro', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c4a70000-0000-4000-8000-0000000000a7', 'chat_gus@parity.test', 'x', true, 'chat_gus', 'pro', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c4a70000-0000-4000-8000-0000000000a8', 'chat_hal@parity.test', 'x', true, 'chat_hal', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c4a70000-0000-4000-8000-0000000000a9', 'chat_ivy@parity.test', 'x', true, 'chat_ivy', 'pro', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
SELECT id, id, username || '''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'
FROM users WHERE id::text LIKE 'c4a70000-%';

INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at)
VALUES
  ('c4a70000-0000-4000-8000-0000000005a5', 'c4a70000-0000-4000-8000-0000000000a5', 'Work', 'work', 0, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

INSERT INTO memories (id, tenant_id, content, created_at, updated_at, review_state, origin_kind)
VALUES
  ('c4a70000-0000-4000-8000-0000000003a1', 'c4a70000-0000-4000-8000-0000000000a1', 'amy pinned memory', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', 'auto', 'manual'),
  ('c4a70000-0000-4000-8000-0000000003a8', 'c4a70000-0000-4000-8000-0000000000a8', 'hal private memory', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', 'auto', 'manual');

INSERT INTO conversations (id, tenant_id, title, space_id, created_at, updated_at, pinned_memory_ids, route_provider, route_model)
VALUES
  ('c4a70000-0000-4000-8000-0000000001a1', 'c4a70000-0000-4000-8000-0000000000a1', 'Trip planning', NULL, '2026-02-01T09:00:00Z', '2026-02-03T09:00:00Z', '{c4a70000-0000-4000-8000-0000000003a1}', NULL, NULL),
  ('c4a70000-0000-4000-8000-0000000001a2', 'c4a70000-0000-4000-8000-0000000000a1', 'Routed', NULL, '2026-02-01T09:00:00Z', '2026-02-02T09:00:00Z', '{}', 'openRouter', 'openai/gpt-4o-mini'),
  ('c4a70000-0000-4000-8000-0000000001a3', 'c4a70000-0000-4000-8000-0000000000a1', 'Unknown provider', NULL, '2026-02-01T09:00:00Z', '2026-02-01T09:00:00Z', '{}', 'nope', 'm'),
  ('c4a70000-0000-4000-8000-0000000001a4', 'c4a70000-0000-4000-8000-0000000000a1', 'To delete', NULL, '2026-01-20T09:00:00Z', '2026-01-20T09:00:00Z', '{}', NULL, NULL),
  ('c4a70000-0000-4000-8000-0000000001e1', 'c4a70000-0000-4000-8000-0000000000a5', 'Standup', 'c4a70000-0000-4000-8000-0000000005a5', '2026-02-01T09:00:00Z', '2026-02-05T09:00:00Z', '{}', NULL, NULL),
  ('c4a70000-0000-4000-8000-0000000001e2', 'c4a70000-0000-4000-8000-0000000000a5', 'Personal', NULL, '2026-02-01T09:00:00Z', '2026-02-04T09:00:00Z', '{}', NULL, NULL),
  ('c4a70000-0000-4000-8000-0000000001e3', 'c4a70000-0000-4000-8000-0000000000a5', 'Quiet', NULL, '2026-02-01T09:00:00Z', '2026-02-03T09:00:00Z', '{}', NULL, NULL),
  ('c4a70000-0000-4000-8000-0000000001f1', 'c4a70000-0000-4000-8000-0000000000a6', 'Local', NULL, '2026-02-01T09:00:00Z', '2026-02-01T09:00:00Z', '{}', NULL, NULL),
  ('c4a70000-0000-4000-8000-0000000001f2', 'c4a70000-0000-4000-8000-0000000000a6', 'Local two', NULL, '2026-02-01T09:00:00Z', '2026-02-01T09:00:00Z', '{}', NULL, NULL),
  ('c4a70000-0000-4000-8000-0000000001b1', 'c4a70000-0000-4000-8000-0000000000a2', 'Ben', NULL, '2026-02-01T09:00:00Z', '2026-02-01T09:00:00Z', '{}', NULL, NULL),
  ('c4a70000-0000-4000-8000-0000000001a8', 'c4a70000-0000-4000-8000-0000000000a8', 'Hal secret', NULL, '2026-02-01T09:00:00Z', '2026-02-01T09:00:00Z', '{}', NULL, NULL);

INSERT INTO conversation_messages (id, conversation_id, role, content, source_memory_ids, created_at, tool_call_count, origin, source_label)
VALUES
  ('c4a70000-0000-4000-8000-0000000002a1', 'c4a70000-0000-4000-8000-0000000001a1', 'user', 'where should we go?', '{}', '2026-02-03T08:00:00Z', 0, NULL, NULL),
  ('c4a70000-0000-4000-8000-0000000002a2', 'c4a70000-0000-4000-8000-0000000001a1', 'assistant', 'Lisbon [1]', '{c4a70000-0000-4000-8000-0000000003a1}', '2026-02-03T08:00:05Z', 2, NULL, NULL),
  ('c4a70000-0000-4000-8000-0000000002a3', 'c4a70000-0000-4000-8000-0000000001a1', 'assistant', 'A nudge from Muse', '{}', '2026-02-03T09:00:00Z', 0, 'proactive', 'Muse'),
  ('c4a70000-0000-4000-8000-0000000002e1', 'c4a70000-0000-4000-8000-0000000001e1', 'user', 'standup notes', '{}', '2026-02-05T08:00:00Z', 0, NULL, NULL),
  ('c4a70000-0000-4000-8000-0000000002e2', 'c4a70000-0000-4000-8000-0000000001e1', 'assistant', 'Here are the notes, with a fairly long preview line that keeps going past the cut so the inbox has to trim it to its preview length of one hundred and eighty characters, which is quite a lot of text.', '{}', '2026-02-05T08:00:05Z', 0, NULL, NULL),
  ('c4a70000-0000-4000-8000-0000000002e3', 'c4a70000-0000-4000-8000-0000000001e2', 'user', 'café ☕ notes', '{}', '2026-02-04T08:00:00Z', 0, NULL, NULL);

INSERT INTO agent_turn_traces (id, tenant_id, conversation_message_id, provider, model, tool_calls, failover_count, tokens_in, tokens_out, estimated_cost_usd_micros, latency_ms, credential_mode, occurred_at)
VALUES
  ('c4a70000-0000-4000-8000-0000000004a1', 'c4a70000-0000-4000-8000-0000000000a1', 'c4a70000-0000-4000-8000-0000000002a2', 'openRouter', 'openai/gpt-4o-mini',
   '{"names": ["memory_search", "web_fetch"], "count": 3}', 1, 120, 30, 450, 900, 'managed', '2026-02-03T08:00:05Z'),
  ('c4a70000-0000-4000-8000-0000000004a2', 'c4a70000-0000-4000-8000-0000000000a1', 'c4a70000-0000-4000-8000-0000000002a2', 'anthropic', 'claude-haiku-5-5',
   '{"names": ["memory_search"]}', 0, 10, 5, 0, 300, NULL, '2026-02-03T08:00:06Z'),
  ('c4a70000-0000-4000-8000-0000000004a3', 'c4a70000-0000-4000-8000-0000000000a1', 'c4a70000-0000-4000-8000-0000000002a3', 'openRouter', 'unknown',
   NULL, 0, 0, 0, 0, 0, 'byok', '2026-02-03T09:00:00Z');

INSERT INTO user_chat_preferences (id, tenant_id, auto_expand_thinking, send_on_return, created_at, updated_at, hybrid_profile, local_fallback_enabled, cloud_fallback_enabled, sync_local_conversations)
VALUES
  ('c4a70000-0000-4000-8000-0000000006a4', 'c4a70000-0000-4000-8000-0000000000a4', false, true, '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z', 'quality', true, false, true);
