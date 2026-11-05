-- Memory fixture (parity/scenarios/memory, internal/modules/memory tests). Dedicated
-- users so other modules' scenarios stay untouched and each write scenario snapshots
-- only its own rows. Every timestamp is fixed and over 90 days old, so graph activity
-- (recency decays with the request's clock) is pinned to the score term.

CREATE FUNCTION pg_temp.mem_vec(a float8, b float8, c float8) RETURNS vector AS $$
  SELECT ('[' || a || ',' || b || ',' || c || ',' || array_to_string(array_fill(0, ARRAY[1533]), ',') || ']')::vector
$$ LANGUAGE sql IMMUTABLE;

INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
SELECT u.id::uuid, u.name || '@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, u.name, u.tier, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'
FROM (VALUES
  ('3e300000-0000-4000-8000-0000000000a1', 'mem_ann', 'trial'),
  ('3e300000-0000-4000-8000-0000000000a2', 'mem_bob', 'trial'),
  ('3e300000-0000-4000-8000-0000000000a3', 'mem_cat', 'trial'),
  ('3e300000-0000-4000-8000-0000000000a4', 'mem_gus', 'trial'),
  ('3e300000-0000-4000-8000-0000000000a5', 'mem_gra', 'trial'),
  ('3e300000-0000-4000-8000-0000000000a6', 'mem_cur', 'trial'),
  ('3e300000-0000-4000-8000-0000000000a7', 'mem_rel', 'trial'),
  ('3e300000-0000-4000-8000-0000000000a8', 'mem_new', 'trial'),
  ('3e300000-0000-4000-8000-0000000000a9', 'mem_wri', 'trial'),
  ('3e300000-0000-4000-8000-0000000000aa', 'mem_vie', 'trial'),
  ('3e300000-0000-4000-8000-0000000000ab', 'mem_nov', 'trial'),
  ('3e300000-0000-4000-8000-0000000000ac', 'mem_lim', 'trial'),
  ('3e300000-0000-4000-8000-0000000000ad', 'mem_sug', 'trial'),
  ('3e300000-0000-4000-8000-0000000000ae', 'mem_arc', 'archived'),
  ('3e300000-0000-4000-8000-0000000000af', 'mem_ups', 'trial')
) AS u(id, name, tier);

-- Everyone but mem_nov has a personal vault.
INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
SELECT id, id, username || '''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'
FROM users WHERE username LIKE 'mem\_%' AND username <> 'mem_nov';

-- Team vault: ann admin, vie viewer, cat viewer (a BYOK reader of a managed vault).
INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at)
VALUES ('3e3c0000-0000-4000-8000-0000000000c0', 'Mem Team', '3e300000-0000-4000-8000-0000000000a1', '3e300000-0000-4000-8000-0000000000a1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO vaults (id, team_id, name, created_at, updated_at)
VALUES ('3e3c0000-0000-4000-8000-0000000000c1', '3e3c0000-0000-4000-8000-0000000000c0', 'Mem Team Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
VALUES
  ('3e3c0000-0000-4000-8000-0000000000d1', '3e3c0000-0000-4000-8000-0000000000c1', '3e300000-0000-4000-8000-0000000000a1', 'admin', true, '3e300000-0000-4000-8000-0000000000a1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e3c0000-0000-4000-8000-0000000000d2', '3e3c0000-0000-4000-8000-0000000000c1', '3e300000-0000-4000-8000-0000000000aa', 'viewer', false, '3e300000-0000-4000-8000-0000000000a1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e3c0000-0000-4000-8000-0000000000d3', '3e3c0000-0000-4000-8000-0000000000c1', '3e300000-0000-4000-8000-0000000000a3', 'viewer', false, '3e300000-0000-4000-8000-0000000000a1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO team_memberships (id, team_id, user_id, role, created_at, updated_at)
VALUES
  ('3e3c0000-0000-4000-8000-0000000000e1', '3e3c0000-0000-4000-8000-0000000000c0', '3e300000-0000-4000-8000-0000000000a1', 'owner', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e3c0000-0000-4000-8000-0000000000e2', '3e3c0000-0000-4000-8000-0000000000c0', '3e300000-0000-4000-8000-0000000000aa', 'member', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('3e3c0000-0000-4000-8000-0000000000e3', '3e3c0000-0000-4000-8000-0000000000c0', '3e300000-0000-4000-8000-0000000000a3', 'member', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- cat reads model identity (BYOK). gus has a BYO Hermes at a private address, which
-- the capture group's resolution refuses before the handler runs.
INSERT INTO user_llm_preferences (id, tenant_id, primary_provider, primary_model, fallback_chain, mode, created_at, updated_at)
VALUES ('3e3e0000-0000-4000-8000-000000000001', '3e300000-0000-4000-8000-0000000000a3', 'openai', 'gpt-4o', '{"steps": []}', 'byok', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO user_hermes_config (id, tenant_id, base_url, verified_at, created_at, updated_at)
VALUES ('3e3e0000-0000-4000-8000-000000000002', '3e300000-0000-4000-8000-0000000000a4', 'http://10.20.30.40:8642', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at)
VALUES
  ('3e355000-0000-4000-8000-0000000000a1', '3e300000-0000-4000-8000-0000000000a1', 'Stocks', 'stocks', 0, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('3e355000-0000-4000-8000-0000000000a5', '3e300000-0000-4000-8000-0000000000a5', 'Graph Space', 'graph-space', 0, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('3e355000-0000-4000-8000-0000000000a9', '3e300000-0000-4000-8000-0000000000a9', 'Work', 'work', 0, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('3e355000-0000-4000-8000-0000000000b9', '3e300000-0000-4000-8000-0000000000a9', 'Home', 'home', 0, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('3e355000-0000-4000-8000-0000000000af', '3e300000-0000-4000-8000-0000000000af', 'Ups', 'ups', 0, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, space_id, created_at, updated_at, processed_at, metadata)
VALUES
  ('3e3f0000-0000-4000-8000-0000000000a1', '3e300000-0000-4000-8000-0000000000a1', 'stocks/source note.md', 'text/markdown', 5, 'mem1', '3e355000-0000-4000-8000-0000000000a1', '2026-01-03T12:00:00Z', '2026-01-03T12:00:00Z', '2026-01-03T12:00:00Z', '{}'),
  ('3e3f0000-0000-4000-8000-0000000000a2', '3e300000-0000-4000-8000-0000000000a1', 'inbox/undated.md', 'text/markdown', 5, 'mem2', NULL, NULL, NULL, NULL, '{}'),
  -- Graph: two wiki pages, an image, a PDF that is not a node, a title in metadata.
  ('3e3f0000-0000-4000-8000-0000000000b1', '3e300000-0000-4000-8000-0000000000a5', 'wiki/Linked Page.md', 'text/markdown', 5, 'mem3', NULL, '2026-01-05T08:00:00Z', '2026-01-05T08:00:00Z', '2026-01-05T08:00:00Z', '{}'),
  ('3e3f0000-0000-4000-8000-0000000000b2', '3e300000-0000-4000-8000-0000000000a5', 'wiki/titled.md', 'text/plain', 5, 'mem4', '3e355000-0000-4000-8000-0000000000a5', '2026-01-06T08:00:00Z', '2026-01-06T08:00:00Z', '2026-01-06T08:00:00Z', '{"title": "From Metadata"}'),
  ('3e3f0000-0000-4000-8000-0000000000b3', '3e300000-0000-4000-8000-0000000000a5', 'wiki/.hidden', 'image/png', 5, 'mem5', NULL, '2026-01-07T08:00:00Z', '2026-01-07T08:00:00Z', '2026-01-07T08:00:00Z', '{"title": "   "}'),
  ('3e3f0000-0000-4000-8000-0000000000b4', '3e300000-0000-4000-8000-0000000000a5', 'wiki/report.pdf', 'application/pdf', 5, 'mem6', NULL, '2026-01-08T08:00:00Z', '2026-01-08T08:00:00Z', '2026-01-08T08:00:00Z', '{}'),
  ('3e3f0000-0000-4000-8000-0000000000b5', '3e300000-0000-4000-8000-0000000000a5', 'nodate.md', 'text/markdown', 5, 'mem7', NULL, NULL, NULL, NULL, '{}'),
  -- Writer: a memory's source note, moved with it. The row has no bytes on disk.
  ('3e3f0000-0000-4000-8000-0000000000c1', '3e300000-0000-4000-8000-0000000000a9', 'inbox/moved.md', 'text/markdown', 5, 'mem8', NULL, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '{}'),
  ('3e3f0000-0000-4000-8000-0000000000c2', '3e300000-0000-4000-8000-0000000000a9', 'inbox/rejected.md', 'text/markdown', 5, 'mem9', NULL, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '{}');

-- ann: every list filter, DTO field and provenance shape.
INSERT INTO memories (id, tenant_id, content, created_at, updated_at, tags, score, last_accessed_at, source_vault_file_id, lat, lng, accuracy_m, place_name, review_state, space_id, created_by_user_id, updated_by_user_id, origin_kind, origin_source_id, origin_provider, origin_model, kind, next_review_at, resurface_muted, review_count, last_reviewed_at, access_count, query_hit_count)
VALUES
  ('3e3a0000-0000-4000-8000-000000000001', '3e300000-0000-4000-8000-0000000000a1', 'Alpha note about stocks', '2026-01-10T09:00:00Z', '2026-01-10T09:30:00.123456Z', '{finance,stocks}', 7.5, '2026-01-20T00:00:00Z', NULL, 37.7749, -122.4194, 12.5, 'San Francisco', 'auto', '3e355000-0000-4000-8000-0000000000a1', '3e300000-0000-4000-8000-0000000000a1', '3e300000-0000-4000-8000-0000000000a1', 'manual', NULL, NULL, NULL, 'fact', NULL, false, 0, NULL, 3, 2),
  ('3e3a0000-0000-4000-8000-000000000002', '3e300000-0000-4000-8000-0000000000a1', 'Pending decision from a skill', '2026-01-11T09:00:00Z', '2026-01-11T09:00:00Z', NULL, 3.25, NULL, NULL, NULL, NULL, NULL, NULL, 'pending', NULL, NULL, NULL, 'skill', 'skill-run-1', 'mcp:codex', 'gpt-5', 'decision', NULL, false, 0, NULL, 0, 0),
  ('3e3a0000-0000-4000-8000-000000000003', '3e300000-0000-4000-8000-0000000000a1', 'Approved with no creation date', NULL, NULL, '{}', 0.1, NULL, NULL, NULL, 0, NULL, NULL, 'approved', NULL, NULL, NULL, 'legacy', NULL, NULL, NULL, 'preference', NULL, false, 1, '2026-01-15T00:00:00Z', 0, 0),
  ('3e3a0000-0000-4000-8000-000000000004', '3e300000-0000-4000-8000-0000000000a1', 'Rejected and overdue', '2026-01-12T09:00:00Z', '2026-01-12T09:00:00Z', '{finance}', 1, NULL, NULL, NULL, NULL, NULL, NULL, 'rejected', NULL, NULL, NULL, 'chat', 'msg-1', 'openrouter', 'anthropic/claude-3', 'note', '2026-01-13T00:00:00Z', false, 2, '2026-01-12T00:00:00Z', 0, 0),
  ('3e3a0000-0000-4000-8000-000000000005', '3e300000-0000-4000-8000-0000000000a1', 'Compiled from a note', '2026-01-13T09:00:00Z', '2026-01-13T09:00:00Z', '{}', 0, NULL, '3e3f0000-0000-4000-8000-0000000000a1', NULL, NULL, NULL, NULL, 'auto', '3e355000-0000-4000-8000-0000000000a1', NULL, NULL, 'knowledge_compile', 'compile-1', 'openrouter', 'google/gemini', NULL, NULL, true, 0, NULL, 0, 0),
  ('3e3a0000-0000-4000-8000-000000000006', '3e300000-0000-4000-8000-0000000000a1', 'Source note has no date', '2026-01-14T09:00:00Z', '2026-01-14T09:00:00Z', '{}', 0, NULL, '3e3f0000-0000-4000-8000-0000000000a2', NULL, NULL, NULL, NULL, 'auto', NULL, NULL, NULL, 'vault', 'file-1', NULL, NULL, NULL, NULL, false, 0, NULL, 0, 0),
  ('3e3a0000-0000-4000-8000-000000000007', '3e300000-0000-4000-8000-0000000000a1', 'Unknown origin kind reads as legacy', '2026-01-15T09:00:00Z', '2026-01-15T09:00:00Z', '{unused}', 0, '2025-01-01T00:00:00Z', NULL, NULL, NULL, NULL, NULL, 'auto', NULL, NULL, NULL, 'not-a-kind', NULL, NULL, 'lonely-model', NULL, NULL, false, 0, NULL, 0, 0),
  -- bob's: the cross-tenant target.
  ('3e3a0000-0000-4000-8000-0000000000b1', '3e300000-0000-4000-8000-0000000000a2', 'Bob private', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{finance}', 1, NULL, NULL, NULL, NULL, NULL, NULL, 'pending', NULL, NULL, NULL, 'manual', NULL, NULL, NULL, NULL, NULL, false, 0, NULL, 0, 0),
  -- cat's: same provenance shapes, read as BYOK.
  ('3e3a0000-0000-4000-8000-0000000000c1', '3e300000-0000-4000-8000-0000000000a3', 'Cat sees models', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 1, NULL, NULL, NULL, NULL, NULL, NULL, 'auto', NULL, NULL, NULL, 'chat', 'msg-2', 'openrouter', 'anthropic/claude-3', NULL, NULL, false, 0, NULL, 0, 0),
  -- team vault.
  ('3e3a0000-0000-4000-8000-0000000000d1', '3e3c0000-0000-4000-8000-0000000000c1', 'Team memory', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 1, NULL, NULL, NULL, NULL, NULL, NULL, 'pending', NULL, '3e300000-0000-4000-8000-0000000000a1', NULL, 'chat', 'msg-3', 'openrouter', 'anthropic/claude-3', 'fact', NULL, false, 0, NULL, 0, 0);

INSERT INTO memory_contributions (id, tenant_id, memory_id, operation, actor_kind, source_kind, provider, model, source_reference, created_at)
VALUES
  ('3e3b0000-0000-4000-8000-000000000001', '3e300000-0000-4000-8000-0000000000a1', '3e3a0000-0000-4000-8000-000000000001', 'create', 'user', 'manual', NULL, NULL, NULL, '2026-01-10T09:00:00Z'),
  ('3e3b0000-0000-4000-8000-000000000002', '3e300000-0000-4000-8000-0000000000a1', '3e3a0000-0000-4000-8000-000000000001', 'update', 'model', 'chat', 'openrouter', 'anthropic/claude-3', 'msg-9', '2026-01-10T09:10:00Z'),
  ('3e3b0000-0000-4000-8000-000000000003', '3e300000-0000-4000-8000-0000000000a1', '3e3a0000-0000-4000-8000-000000000001', 'update', 'model', 'skill', 'mcp:codex', 'gpt-5', NULL, '2026-01-10T09:20:00Z'),
  ('3e3b0000-0000-4000-8000-000000000004', '3e300000-0000-4000-8000-0000000000a1', '3e3a0000-0000-4000-8000-000000000001', 'update', 'model', 'chat', 'openrouter', 'anthropic/claude-3', NULL, '2026-01-10T09:30:00Z'),
  ('3e3b0000-0000-4000-8000-000000000005', '3e300000-0000-4000-8000-0000000000a1', '3e3a0000-0000-4000-8000-000000000005', 'create', 'model', 'knowledge_compile', 'openrouter', 'google/gemini', 'compile-1', '2026-01-13T09:00:00Z'),
  ('3e3b0000-0000-4000-8000-000000000006', '3e300000-0000-4000-8000-0000000000a3', '3e3a0000-0000-4000-8000-0000000000c1', 'create', 'model', 'chat', 'openrouter', 'anthropic/claude-3', 'msg-2', '2026-01-10T09:00:00Z');

-- writer: one row per mutation so each step's outcome stands alone.
INSERT INTO memories (id, tenant_id, content, created_at, updated_at, tags, score, source_vault_file_id, review_state, space_id, origin_kind, origin_source_id, kind, embedding)
VALUES
  ('3e3a1000-0000-4000-8000-000000000001', '3e300000-0000-4000-8000-0000000000a9', 'Patch my content', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 0, NULL, 'auto', NULL, 'manual', NULL, NULL, pg_temp.mem_vec(1, 0, 0)),
  ('3e3a1000-0000-4000-8000-000000000002', '3e300000-0000-4000-8000-0000000000a9', 'Patch my tags', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{old}', 0, NULL, 'auto', NULL, 'manual', NULL, NULL, NULL),
  ('3e3a1000-0000-4000-8000-000000000003', '3e300000-0000-4000-8000-0000000000a9', 'Pending to approve', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 0, NULL, 'pending', NULL, 'manual', NULL, NULL, NULL),
  ('3e3a1000-0000-4000-8000-000000000004', '3e300000-0000-4000-8000-0000000000a9', 'Pending to reject', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 0, '3e3f0000-0000-4000-8000-0000000000c2', 'pending', NULL, 'manual', NULL, NULL, NULL),
  ('3e3a1000-0000-4000-8000-000000000005', '3e300000-0000-4000-8000-0000000000a9', 'Already rejected', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 0, NULL, 'rejected', NULL, 'manual', NULL, NULL, NULL),
  ('3e3a1000-0000-4000-8000-000000000006', '3e300000-0000-4000-8000-0000000000a9', 'Auto review state', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 0, NULL, 'auto', NULL, 'manual', NULL, NULL, NULL),
  ('3e3a1000-0000-4000-8000-000000000007', '3e300000-0000-4000-8000-0000000000a9', 'Kind only', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 0, NULL, 'auto', NULL, 'manual', NULL, 'note', NULL),
  ('3e3a1000-0000-4000-8000-000000000008', '3e300000-0000-4000-8000-0000000000a9', 'Move without a note', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 0, NULL, 'auto', NULL, 'manual', NULL, NULL, NULL),
  ('3e3a1000-0000-4000-8000-000000000009', '3e300000-0000-4000-8000-0000000000a9', 'Move with a note', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 0, '3e3f0000-0000-4000-8000-0000000000c1', 'auto', NULL, 'manual', NULL, NULL, NULL),
  ('3e3a1000-0000-4000-8000-00000000000a', '3e300000-0000-4000-8000-0000000000a9', 'Already filed', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 0, NULL, 'auto', '3e355000-0000-4000-8000-0000000000a9', 'manual', NULL, NULL, NULL),
  ('3e3a1000-0000-4000-8000-00000000000b', '3e300000-0000-4000-8000-0000000000a9', 'Everything at once', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 0, NULL, 'pending', NULL, 'manual', NULL, NULL, NULL),
  ('3e3a1000-0000-4000-8000-00000000000c', '3e300000-0000-4000-8000-0000000000a9', 'Delete me', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 0, NULL, 'auto', NULL, 'chat', 'wri-msg', NULL, NULL),
  ('3e3a1000-0000-4000-8000-00000000000d', '3e300000-0000-4000-8000-0000000000a9', 'Delete me without a job', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 0, NULL, 'auto', NULL, 'manual', NULL, NULL, NULL),
  ('3e3a1000-0000-4000-8000-00000000000e', '3e300000-0000-4000-8000-0000000000a9', 'Review me', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 0, NULL, 'auto', NULL, 'manual', NULL, NULL, NULL),
  ('3e3a1000-0000-4000-8000-00000000000f', '3e300000-0000-4000-8000-0000000000a9', E'# Heading\n\nA longer corrected body.\n', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '{}', 0, NULL, 'auto', NULL, 'manual', NULL, NULL, NULL);
INSERT INTO memory_index_jobs (id, tenant_id, source_kind, source_id, content, status, created_at, updated_at, next_attempt_at)
VALUES ('3e3d0000-0000-4000-8000-000000000001', '3e300000-0000-4000-8000-0000000000a9', 'chat', 'wri-msg', 'Delete me', 'completed', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z', '2026-01-10T09:00:00Z');

-- graph: tags, near and far vectors, a position, a Space hub with a wiki member, one
-- source link, two days of timeline, and a node with no created_at.
INSERT INTO memories (id, tenant_id, content, created_at, tags, score, last_accessed_at, source_vault_file_id, review_state, space_id, graph_x, graph_y, graph_z, origin_kind, origin_source_id, origin_provider, origin_model, embedding)
VALUES
  ('3e3a2000-0000-4000-8000-000000000001', '3e300000-0000-4000-8000-0000000000a5', E'First line title\nSecond line', '2026-01-10T09:00:00Z', '{alpha,beta}', 9.5, '2025-06-01T00:00:00Z', '3e3f0000-0000-4000-8000-0000000000b1', 'auto', '3e355000-0000-4000-8000-0000000000a5', 1.5, -2.25, 3, 'chat', 'gra-1', 'openrouter', 'anthropic/claude-3', pg_temp.mem_vec(1, 0, 0)),
  ('3e3a2000-0000-4000-8000-000000000002', '3e300000-0000-4000-8000-0000000000a5', '   Trimmed title   ', '2026-01-10T10:00:00Z', '{alpha}', 7, NULL, NULL, 'auto', '3e355000-0000-4000-8000-0000000000a5', 1.5, NULL, 3, 'manual', NULL, NULL, NULL, pg_temp.mem_vec(0.9, 0.1, 0)),
  ('3e3a2000-0000-4000-8000-000000000003', '3e300000-0000-4000-8000-0000000000a5', 'A very long first line that keeps going so the title must be cut somewhere well before the end of all of this text', '2026-01-10T11:00:00Z', '{beta}', 4.25, NULL, NULL, 'approved', NULL, NULL, NULL, NULL, 'skill', 'gra-2', 'mcp:codex', 'gpt-5', pg_temp.mem_vec(0, 1, 0)),
  ('3e3a2000-0000-4000-8000-000000000004', '3e300000-0000-4000-8000-0000000000a5', 'Next day node', '2026-01-11T09:00:00Z', '{}', 2, '2025-01-01T00:00:00Z', NULL, 'auto', NULL, NULL, NULL, NULL, 'vault', 'gra-3', NULL, NULL, pg_temp.mem_vec(0, 0.2, 1)),
  ('3e3a2000-0000-4000-8000-000000000005', '3e300000-0000-4000-8000-0000000000a5', 'Same day as next day', '2026-01-11T09:00:00Z', NULL, 1, NULL, NULL, 'auto', NULL, NULL, NULL, NULL, 'knowledge_compile', 'gra-4', 'openrouter', 'google/gemini', NULL),
  ('3e3a2000-0000-4000-8000-000000000006', '3e300000-0000-4000-8000-0000000000a5', 'No created at', NULL, '{alpha}', 0.5, NULL, NULL, 'auto', NULL, NULL, NULL, NULL, 'legacy', NULL, NULL, NULL, NULL);

-- local sync: ties on updated_at broken by id, microseconds a cursor round-trips,
-- rows and tombstones dropped for a NULL time, and a tombstone tying a memory.
INSERT INTO memories (id, tenant_id, content, created_at, updated_at, tags, score, review_state, origin_kind)
VALUES
  ('3e3a3000-0000-4000-8000-000000000001', '3e300000-0000-4000-8000-0000000000a6', 'One', '2026-01-10T00:00:00Z', '2026-01-10T10:00:00Z', '{}', 0, 'auto', 'chat'),
  ('3e3a3000-0000-4000-8000-000000000002', '3e300000-0000-4000-8000-0000000000a6', 'Two', NULL, '2026-01-10T10:00:00Z', '{}', 0, 'auto', 'manual'),
  ('3e3a3000-0000-4000-8000-000000000003', '3e300000-0000-4000-8000-0000000000a6', 'Three', '2026-01-10T00:00:00Z', '2026-01-10T10:00:00.123456Z', '{}', 0, 'auto', 'not-a-kind'),
  ('3e3a3000-0000-4000-8000-000000000004', '3e300000-0000-4000-8000-0000000000a6', 'Four', '2026-01-10T00:00:00Z', '2026-01-10T11:00:00Z', '{}', 0, 'auto', 'vault'),
  ('3e3a3000-0000-4000-8000-000000000005', '3e300000-0000-4000-8000-0000000000a6', 'No update time', '2026-01-10T00:00:00Z', NULL, '{}', 0, 'auto', 'vault'),
  ('3e3a3000-0000-4000-8000-000000000006', '3e300000-0000-4000-8000-0000000000a6', 'Six', '2026-01-10T00:00:00Z', '2026-01-10T12:00:00Z', '{}', 0, 'auto', 'skill');
INSERT INTO memory_sync_tombstones (id, tenant_id, memory_id, deleted_at)
VALUES
  ('3e3a3000-0000-4000-8000-000000000004', '3e300000-0000-4000-8000-0000000000a6', '3e3a3fff-0000-4000-8000-000000000001', '2026-01-10T11:00:00Z'),
  ('3e3a3f00-0000-4000-8000-000000000001', '3e300000-0000-4000-8000-0000000000a6', '3e3a3fff-0000-4000-8000-000000000002', '2026-01-10T11:30:00Z'),
  ('3e3a3f00-0000-4000-8000-000000000002', '3e300000-0000-4000-8000-0000000000a6', '3e3a3fff-0000-4000-8000-000000000003', NULL),
  ('3e3a3f00-0000-4000-8000-000000000003', '3e300000-0000-4000-8000-0000000000a2', '3e3a3fff-0000-4000-8000-000000000004', '2026-01-10T11:30:00Z');

-- related: neighbours at known distances. r5 is orthogonal and over the cutoff, r6
-- has no vector, r8 is another tenant's twin of r1.
INSERT INTO memories (id, tenant_id, content, created_at, tags, score, review_state, space_id, kind, origin_kind, query_hit_count, embedding)
VALUES
  ('3e3a4000-0000-4000-8000-000000000001', '3e300000-0000-4000-8000-0000000000a7', 'Related target', '2026-01-10T09:00:00Z', '{}', 0, 'auto', NULL, NULL, 'manual', 0, pg_temp.mem_vec(1, 0, 0)),
  ('3e3a4000-0000-4000-8000-000000000002', '3e300000-0000-4000-8000-0000000000a7', E'Closest   neighbour\n\twith mixed whitespace', '2026-01-10T09:00:00Z', '{}', 0, 'auto', NULL, 'fact', 'manual', 0, pg_temp.mem_vec(1, 0.1, 0)),
  ('3e3a4000-0000-4000-8000-000000000003', '3e300000-0000-4000-8000-0000000000a7', repeat('Long neighbour text ', 12), '2026-01-10T09:00:00Z', '{}', 0, 'auto', NULL, 'note', 'manual', 0, pg_temp.mem_vec(1, 0.5, 0)),
  ('3e3a4000-0000-4000-8000-000000000004', '3e300000-0000-4000-8000-0000000000a7', 'Neighbour with no date', NULL, '{}', 0, 'auto', NULL, NULL, 'manual', 0, pg_temp.mem_vec(1, 0.7, 0)),
  ('3e3a4000-0000-4000-8000-000000000005', '3e300000-0000-4000-8000-0000000000a7', 'Orthogonal, over the cutoff', '2026-01-10T09:00:00Z', '{}', 0, 'auto', NULL, NULL, 'manual', 0, pg_temp.mem_vec(0, 1, 0)),
  ('3e3a4000-0000-4000-8000-000000000006', '3e300000-0000-4000-8000-0000000000a7', 'No vector', '2026-01-10T09:00:00Z', '{}', 0, 'auto', NULL, NULL, 'manual', 0, NULL),
  ('3e3a4000-0000-4000-8000-000000000008', '3e300000-0000-4000-8000-0000000000a2', 'Twin in another tenant', '2026-01-10T09:00:00Z', '{}', 0, 'auto', NULL, NULL, 'manual', 0, pg_temp.mem_vec(1, 0, 0));
