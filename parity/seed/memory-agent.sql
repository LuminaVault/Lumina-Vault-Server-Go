-- memory-agent fixtures (parity/scenarios/memory-agent, scenarios-switched/llm/memory-agent).
-- A memory holding a [[stub:reply:...]] directive ends the agent loop: the tool result
-- carrying it is the newest message with a directive, so the stub answers with its text.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
VALUES
  ('3a000000-0000-4000-8000-0000000000a1', 'ma_amy@parity.test', 'x', true, 'ma_amy', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a2', 'ma_ben@parity.test', 'x', true, 'ma_ben', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a3', 'ma_sat@parity.test', 'x', true, 'ma_sat', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a4', 'ma_fup@parity.test', 'x', true, 'ma_fup', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a5', 'ma_mem@parity.test', 'x', true, 'ma_mem', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a6', 'ma_str@parity.test', 'x', true, 'ma_str', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a7', 'ma_nil@parity.test', 'x', true, 'ma_nil', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a8', 'ma_cit@parity.test', 'x', true, 'ma_cit', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a9', 'ma_tag@parity.test', 'x', true, 'ma_tag', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000aa', 'ma_qry@parity.test', 'x', true, 'ma_qry', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000ab', 'ma_upd@parity.test', 'x', true, 'ma_upd', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
VALUES
  ('3a000000-0000-4000-8000-0000000000a1', '3a000000-0000-4000-8000-0000000000a1', 'ma_amy''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a2', '3a000000-0000-4000-8000-0000000000a2', 'ma_ben''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a3', '3a000000-0000-4000-8000-0000000000a3', 'ma_sat''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a4', '3a000000-0000-4000-8000-0000000000a4', 'ma_fup''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a5', '3a000000-0000-4000-8000-0000000000a5', 'ma_mem''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a6', '3a000000-0000-4000-8000-0000000000a6', 'ma_str''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a7', '3a000000-0000-4000-8000-0000000000a7', 'ma_nil''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a8', '3a000000-0000-4000-8000-0000000000a8', 'ma_cit''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000a9', '3a000000-0000-4000-8000-0000000000a9', 'ma_tag''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000aa', '3a000000-0000-4000-8000-0000000000aa', 'ma_qry''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3a000000-0000-4000-8000-0000000000ab', '3a000000-0000-4000-8000-0000000000ab', 'ma_upd''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO memories (id, tenant_id, content, created_at, updated_at, review_state, origin_kind)
VALUES
  ('3a100000-0000-4000-8000-000000000001', '3a000000-0000-4000-8000-0000000000a3', 'saturn has rings [[stub:reply:Saturn has rings.]]', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', 'auto', 'manual'),
  ('3a100000-0000-4000-8000-000000000002', '3a000000-0000-4000-8000-0000000000a4', 'jupiter [[stub:reply:{"follow_ups":["Why jupiter?","  ","Go deeper"]}]]', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', 'auto', 'manual'),
  ('3a100000-0000-4000-8000-000000000003', '3a000000-0000-4000-8000-0000000000a5', 'memo source neptune [[stub:reply:## Summary
Neptune is far.]]', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', 'auto', 'manual'),
  ('3a100000-0000-4000-8000-000000000004', '3a000000-0000-4000-8000-0000000000a6', 'uranus tilts', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', 'auto', 'manual'),
  ('3a100000-0000-4000-8000-000000000005', '3a000000-0000-4000-8000-0000000000a6', 'an uranus draft from a model', '2026-01-11T09:00:00Z', '2026-01-11T09:00:00Z', 'auto', 'query');
UPDATE memories SET origin_provider = 'openrouter', origin_model = 'gpt-5' WHERE id = '3a100000-0000-4000-8000-000000000005';

INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at)
VALUES
  ('3a200000-0000-4000-8000-000000000001', '3a000000-0000-4000-8000-0000000000a3', 'Planets [[stub:reply:Mapped the spaces.]]', 'planets', 2, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at, processed_at)
VALUES
  ('3a300000-0000-4000-8000-000000000001', '3a000000-0000-4000-8000-0000000000a1', 'memos/2026-01-01/first-memo.md', 'text/markdown', 5, 'ma1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', NULL),
  ('3a300000-0000-4000-8000-000000000002', '3a000000-0000-4000-8000-0000000000a1', 'memos/2026-01-02/second-memo-Ünï.md', 'text/markdown', 5, 'ma2', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z', NULL),
  ('3a300000-0000-4000-8000-000000000003', '3a000000-0000-4000-8000-0000000000a1', 'memosnot/2026-01-02/other.md', 'text/markdown', 5, 'ma3', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', NULL),
  ('3a300000-0000-4000-8000-000000000004', '3a000000-0000-4000-8000-0000000000a1', 'notes/memos/other.md', 'text/markdown', 5, 'ma4', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', NULL),
  ('3a300000-0000-4000-8000-000000000005', '3a000000-0000-4000-8000-0000000000a2', 'memos/2026-01-03/bens-memo.md', 'text/markdown', 5, 'ma5', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', NULL);

CREATE VIEW parity_ma_memories AS
SELECT id, tenant_id, content, tags, source_vault_file_id, review_state, space_id, origin_kind, origin_provider, origin_model, created_at
FROM memories WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'ma\_%');

CREATE VIEW parity_ma_analytics AS
SELECT vault_id, actor_user_id, event_name, source, dimensions
FROM analytics_events WHERE vault_id IN (SELECT id FROM users WHERE username LIKE 'ma\_%');

CREATE VIEW parity_ma_output_jobs AS
SELECT tenant_id, content, provider, model, source_kind, source_id, status
FROM memory_index_jobs WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'ma\_%');

CREATE VIEW parity_ma_files AS
SELECT id, tenant_id, space_id,
       regexp_replace(regexp_replace(path, '^memos/\d{4}-\d{2}-\d{2}/', 'memos/<date>/'), '-[0-9a-f]{8}\.md$', '-<suffix>.md') AS path,
       content_type, processed_at, created_at
FROM vault_files WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'ma\_%') AND sha256 NOT LIKE 'ma%';

CREATE VIEW parity_ma_autofile AS
SELECT tenant_id, path, space_id, metadata->>'suggestedSpaceID' IS NOT NULL AS suggested
FROM vault_files WHERE tenant_id IN (SELECT id FROM users WHERE username = 'ma_cit');

CREATE VIEW parity_ma_autofile_pending AS
SELECT id FROM vault_files
WHERE tenant_id IN (SELECT id FROM users WHERE username = 'ma_cit') AND path = 'note.md'
  AND metadata->>'suggestedSpaceID' IS NULL;
