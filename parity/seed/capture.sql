-- Capture fixtures (parity/scenarios/capture). Loaded after base.sql.
--
-- Own users: captureByUser allows 60 requests per 60 s per user, and enrichment
-- writes to the user's vault tree and memories after the response.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
VALUES
  ('ca97e000-0000-4000-8000-0000000000a1', 'capture-amy@parity.test', 'x', true, 'capture_amy', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca97e000-0000-4000-8000-0000000000b2', 'capture-ben@parity.test', 'x', true, 'capture_ben', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca97e000-0000-4000-8000-0000000000c3', 'capture-cid@parity.test', 'x', true, 'capture_cid', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca97e000-0000-4000-8000-0000000000d4', 'capture-hal@parity.test', 'x', true, 'capture_hal', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca97e000-0000-4000-8000-0000000000e5', 'capture-arc@parity.test', 'x', true, 'capture_arc', 'archived', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca97e000-0000-4000-8000-0000000000f6', 'capture-lap@parity.test', 'x', true, 'capture_lap', 'lapsed', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
VALUES
  ('ca97e000-0000-4000-8000-0000000000a1', 'ca97e000-0000-4000-8000-0000000000a1', 'capture_amy''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca97e000-0000-4000-8000-0000000000b2', 'ca97e000-0000-4000-8000-0000000000b2', 'capture_ben''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca97e000-0000-4000-8000-0000000000c3', 'ca97e000-0000-4000-8000-0000000000c3', 'capture_cid''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca97e000-0000-4000-8000-0000000000d4', 'ca97e000-0000-4000-8000-0000000000d4', 'capture_hal''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca97e000-0000-4000-8000-0000000000e5', 'ca97e000-0000-4000-8000-0000000000e5', 'capture_arc''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ca97e000-0000-4000-8000-0000000000f6', 'ca97e000-0000-4000-8000-0000000000f6', 'capture_lap''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at)
VALUES
  ('ca97e000-0000-4000-8000-0000000005a1', 'ca97e000-0000-4000-8000-0000000000a1', 'Reading', 'reading', 0, '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z'),
  ('ca97e000-0000-4000-8000-0000000005b2', 'ca97e000-0000-4000-8000-0000000000b2', 'Bens', 'bens', 0, '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z');

-- capture_hal has the reading-time hook enabled. capture_ben has it disabled, and a
-- connector that isn't a capture hook.
INSERT INTO plugin_installs (id, tenant_id, plugin_slug, status, config_ciphertext, config_nonce, created_at, updated_at)
VALUES
  ('ca97e000-0000-4000-8000-0000000006d4', 'ca97e000-0000-4000-8000-0000000000d4', 'reading-time', 'enabled', ''::bytea, ''::bytea, '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z'),
  ('ca97e000-0000-4000-8000-0000000006d5', 'ca97e000-0000-4000-8000-0000000000d4', 'readwise', 'enabled', ''::bytea, ''::bytea, '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z'),
  ('ca97e000-0000-4000-8000-0000000006b2', 'ca97e000-0000-4000-8000-0000000000b2', 'reading-time', 'disabled', ''::bytea, ''::bytea, '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z');

-- The path holds the time of the capture and a random suffix, which only these
-- views can mask. The rest of the row is compared as written.
CREATE VIEW parity_capture_files AS
SELECT id, tenant_id, created_by_user_id, updated_by_user_id, space_id,
       regexp_replace(path, '\d{4}-\d{2}-\d{2}-\d{6}-(.*)-[0-9a-f]{8}\.md$', '<time>-\1-<suffix>.md') AS path,
       content_type, size_bytes, sha256, processed_at, metadata, created_at, updated_at
FROM vault_files
WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'capture\_%');

-- Enrichment writes memories after the response, in whichever order the fetches
-- finish. The harness orders rows by created_at, so it's renamed here, and the row's
-- id and its file's id are left out or masked so rows with the same page are equal.
CREATE VIEW parity_capture_memories AS
SELECT m.tenant_id, m.content, m.created_at AS created, m.embedding, m.tags, m.score, m.access_count, m.query_hit_count,
       m.last_accessed_at,
       (SELECT regexp_replace(f.path, '\d{4}-\d{2}-\d{2}-\d{6}-(.*)-[0-9a-f]{8}\.md$', '<time>-\1-<suffix>.md')
        FROM vault_files f WHERE f.id = m.source_vault_file_id) AS source_path,
       m.lat, m.lng, m.accuracy_m, m.place_name, m.content_tsv, m.review_state, m.space_id, m.graph_x, m.graph_y, m.graph_z,
       m.graph_layout_at, m.created_by_user_id, m.updated_by_user_id, m.origin_kind, m.origin_source_id, m.origin_provider,
       m.origin_model, m.origin_conversation_message_id, m.last_reviewed_at, m.review_count, m.updated_at, m.kind,
       m.next_review_at, m.resurface_muted
FROM memories m
WHERE m.tenant_id IN (SELECT id FROM users WHERE username LIKE 'capture\_%');

CREATE VIEW parity_capture_contributions AS
SELECT * FROM memory_contributions
WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'capture\_%');

CREATE VIEW parity_capture_onboarding AS
SELECT * FROM onboarding_state
WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'capture\_%');

CREATE VIEW parity_capture_achievements AS
SELECT * FROM achievement_progress
WHERE tenant_id IN (SELECT id FROM users WHERE username LIKE 'capture\_%');

-- What enrichment and the achievement inbox still owe after the response. Both run
-- detached, so a step that compares their results waits for this to empty
-- (`waitEmpty`). A file over 100 bytes has content, so it gets a memory, and that
-- is written after the row's status.
CREATE VIEW parity_capture_pending AS
SELECT f.id
FROM vault_files f
WHERE f.tenant_id IN (SELECT id FROM users WHERE username LIKE 'capture\_%')
  AND (f.metadata->>'enrichmentStatus' = 'pending'
       OR (f.metadata->>'enrichmentStatus' = 'done' AND f.size_bytes > 100
           AND NOT EXISTS (SELECT 1 FROM memories m WHERE m.source_vault_file_id = f.id))
       OR (SELECT count(*) FROM vault_files c WHERE c.tenant_id = f.tenant_id)
          > COALESCE((SELECT p.progress_count FROM achievement_progress p
                      WHERE p.tenant_id = f.tenant_id AND p.achievement_key = 'soulseeker.first-relic'), 0));
