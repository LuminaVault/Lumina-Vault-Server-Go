-- Dashboard fixture (parity/scenarios/dashboard, internal/modules/dashboard tests).
-- Dedicated users so other modules' scenarios stay untouched.
--
-- Time windows are relative to now(), evaluated once when the template database
-- is built, so both servers read identical rows. Node activity on the home graph
-- preview decays with the request's clock, so graph rows only use last-access
-- times outside the 90-day ramp (far past or future), which pins recency to 0 or 1.

INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at)
SELECT u.id::uuid, u.name || '@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, u.name, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'
FROM (VALUES
  ('da5b0000-0000-4000-8000-0000000000a1', 'dash_ann'),
  ('da5b0000-0000-4000-8000-0000000000b2', 'dash_ben'),
  ('da5b0000-0000-4000-8000-0000000000c3', 'dash_cat'),
  ('da5b0000-0000-4000-8000-0000000000d4', 'dash_dan'),
  ('da5b0000-0000-4000-8000-0000000000e5', 'dash_eli'),
  ('da5b0000-0000-4000-8000-0000000000f6', 'dash_wri'),
  ('da5b0000-0000-4000-8000-0000000000a7', 'dash_ins'),
  ('da5b0000-0000-4000-8000-0000000000b8', 'dash_ovf')
) AS u(id, name);

-- Everyone but dash_dan has a personal vault.
INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
SELECT id, id, username || '''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'
FROM users WHERE username LIKE 'dash\_%' AND username <> 'dash_dan';

-- Team vault: ann admin, ben viewer (both team members), eli an editor without a
-- team membership. A second vault is archived.
INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at)
VALUES ('da5bc000-0000-4000-8000-0000000000c0', 'Dash Team', 'da5b0000-0000-4000-8000-0000000000a1', 'da5b0000-0000-4000-8000-0000000000a1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO vaults (id, team_id, name, archived_at, created_at, updated_at)
VALUES
  ('da5bc000-0000-4000-8000-0000000000c1', 'da5bc000-0000-4000-8000-0000000000c0', 'Dash Team Vault', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('da5bc000-0000-4000-8000-0000000000c2', 'da5bc000-0000-4000-8000-0000000000c0', 'Dash Archived', '2026-01-02T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
VALUES
  ('da5bc000-0000-4000-8000-0000000000d1', 'da5bc000-0000-4000-8000-0000000000c1', 'da5b0000-0000-4000-8000-0000000000a1', 'admin', true, 'da5b0000-0000-4000-8000-0000000000a1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('da5bc000-0000-4000-8000-0000000000d2', 'da5bc000-0000-4000-8000-0000000000c1', 'da5b0000-0000-4000-8000-0000000000b2', 'viewer', false, 'da5b0000-0000-4000-8000-0000000000a1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('da5bc000-0000-4000-8000-0000000000d3', 'da5bc000-0000-4000-8000-0000000000c1', 'da5b0000-0000-4000-8000-0000000000e5', 'editor', false, 'da5b0000-0000-4000-8000-0000000000a1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO team_memberships (id, team_id, user_id, role, created_at, updated_at)
VALUES
  ('da5bc000-0000-4000-8000-0000000000e1', 'da5bc000-0000-4000-8000-0000000000c0', 'da5b0000-0000-4000-8000-0000000000a1', 'owner', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('da5bc000-0000-4000-8000-0000000000e2', 'da5bc000-0000-4000-8000-0000000000c0', 'da5b0000-0000-4000-8000-0000000000b2', 'member', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- ann: spaces, vault files (two todos, one note), memories.
INSERT INTO spaces (id, tenant_id, name, slug, note_count, last_compiled_at, created_at, updated_at)
VALUES
  ('da5b5000-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', 'Compiled old', 'compiled-old', 0, now() - interval '9 days', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('da5b5000-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000a1', 'Compiled new', 'compiled-new', 0, now() - interval '2 days', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('da5b5000-0000-4000-8000-000000000003', 'da5b0000-0000-4000-8000-0000000000a1', 'Never compiled', 'never-compiled', 0, NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');
INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at, processed_at, metadata)
VALUES
  ('da5bf000-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', 'inbox/dash1.md', 'text/markdown', 5, 'dash1', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '{"done": false, "title": "One", "isTodo": true}'),
  ('da5bf000-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000a1', 'inbox/dash2.md', 'text/markdown', 5, 'dash2', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '{"done": true, "title": "Two", "isTodo": true}'),
  ('da5bf000-0000-4000-8000-000000000003', 'da5b0000-0000-4000-8000-0000000000a1', 'notes/dash3.md', 'text/markdown', 5, 'dash3', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', '{"title": "Note"}');

INSERT INTO memories (id, tenant_id, content, created_at, tags, score, access_count, query_hit_count, last_accessed_at, source_vault_file_id, review_state, space_id, graph_x, graph_y, graph_z, last_reviewed_at, created_by_user_id)
VALUES
  -- Five consecutive active days ending today (memories and conversations mixed),
  -- then a gap, so the streak is 5. Titles exercise titleFromContent.
  ('da5be000-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', E'# Morning plan\nDetails follow', date_trunc('day', now()) + interval '1 second', NULL, 1, 0, 0, NULL, NULL, 'auto', NULL, NULL, NULL, NULL, NULL, NULL),
  ('da5be000-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000a1', E'  > - * Quoted title  \nsecond line', date_trunc('day', now()) + interval '2 seconds', '{a,b}', 2, 1, 3, NULL, NULL, 'auto', 'da5b5000-0000-4000-8000-000000000001', NULL, NULL, NULL, NULL, NULL),
  ('da5be000-0000-4000-8000-000000000003', 'da5b0000-0000-4000-8000-0000000000a1', repeat('long title ', 8), now() - interval '1 day', NULL, 3, 5, 0, NULL, 'da5bf000-0000-4000-8000-000000000003', 'auto', NULL, NULL, NULL, NULL, NULL, NULL),
  ('da5be000-0000-4000-8000-000000000004', 'da5b0000-0000-4000-8000-0000000000a1', E'\n\n\r\nAfter blank lines\nsecond', now() - interval '2 days', NULL, 4, 0, 1, NULL, NULL, 'pending', 'da5b5000-0000-4000-8000-000000000002', NULL, NULL, NULL, NULL, NULL),
  ('da5be000-0000-4000-8000-000000000005', 'da5b0000-0000-4000-8000-0000000000a1', '###', now() - interval '4 days', NULL, 5, 0, 0, NULL, NULL, 'rejected', NULL, NULL, NULL, NULL, NULL, NULL),
  ('da5be000-0000-4000-8000-000000000006', 'da5b0000-0000-4000-8000-0000000000a1', repeat(E'é', 61), now() - interval '10 days', NULL, 6, 0, 0, NULL, NULL, 'auto', NULL, NULL, NULL, NULL, now() + interval '30 days', NULL),
  -- Graph preview: recency pinned to 1 (future) or 0 (past the 90-day ramp).
  ('da5be000-0000-4000-8000-000000000011', 'da5b0000-0000-4000-8000-0000000000a1', 'Hot node', now() - interval '200 days', NULL, 100, 0, 0, now() + interval '1 day', NULL, 'auto', NULL, 30, -6, 0.0006, NULL, NULL),
  ('da5be000-0000-4000-8000-000000000012', 'da5b0000-0000-4000-8000-0000000000a1', 'Warm node', now() - interval '201 days', NULL, 9, 0, 0, now() - interval '200 days', NULL, 'auto', NULL, 60, 45, -60, NULL, NULL),
  ('da5be000-0000-4000-8000-000000000013', 'da5b0000-0000-4000-8000-0000000000a1', 'Cold node', now() - interval '202 days', NULL, 0, 0, 0, NULL, NULL, 'auto', NULL, 1, 2, 3, NULL, NULL),
  ('da5be000-0000-4000-8000-000000000014', 'da5b0000-0000-4000-8000-0000000000a1', 'Partial layout, excluded', now() - interval '203 days', NULL, 0, 0, 0, NULL, NULL, 'auto', NULL, 1, 2, NULL, NULL, NULL);

INSERT INTO conversations (id, tenant_id, title, created_at, updated_at)
VALUES
  ('da5bd000-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', 'Trip planning', date_trunc('day', now()) + interval '3 seconds', now()),
  ('da5bd000-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000a1', '', now() - interval '1 day 1 hour', now()),
  ('da5bd000-0000-4000-8000-000000000003', 'da5b0000-0000-4000-8000-0000000000a1', 'Three days back', now() - interval '3 days', now()),
  ('da5bd000-0000-4000-8000-000000000004', 'da5b0000-0000-4000-8000-0000000000a1', 'Outside every window', now() - interval '120 days', now());

INSERT INTO skill_run_log (id, tenant_id, source, name, started_at, status)
VALUES
  ('da5b1000-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', 'builtin', 'digest', date_trunc('day', now()) + interval '4 seconds', 'OK'),
  ('da5b1000-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000a1', 'builtin', 'digest', date_trunc('day', now()) + interval '5 seconds', 'failed'),
  ('da5b1000-0000-4000-8000-000000000003', 'da5b0000-0000-4000-8000-0000000000a1', 'vault', 'scrape', date_trunc('day', now()) + interval '6 seconds', 'running'),
  ('da5b1000-0000-4000-8000-000000000004', 'da5b0000-0000-4000-8000-0000000000a1', 'builtin', 'digest', date_trunc('day', now()) - interval '1 day' + interval '7 seconds', 'completed'),
  ('da5b1000-0000-4000-8000-000000000005', 'da5b0000-0000-4000-8000-0000000000a1', 'builtin', 'digest', now() - interval '8 days', 'succeeded');

-- Nine enabled skills and tools (previews cap at 8) plus a disabled one.
INSERT INTO skills_state (tenant_id, source, name, enabled)
SELECT 'da5b0000-0000-4000-8000-0000000000a1', 'builtin', 'skill-' || n, n <= 9 FROM generate_series(1, 10) n;
INSERT INTO plugin_installs (id, tenant_id, plugin_slug, status, config_ciphertext, config_nonce, created_at, updated_at)
SELECT ('da5b2000-0000-4000-8000-0000000000' || lpad(n::text, 2, '0'))::uuid, 'da5b0000-0000-4000-8000-0000000000a1', 'tool-' || n,
       CASE WHEN n <= 9 THEN 'enabled' ELSE 'disabled' END, '\x00', '\x00', now(), now()
FROM generate_series(1, 10) n;

INSERT INTO reminders (id, tenant_id, title, body, fire_at, fired_at)
VALUES
  ('da5b3000-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', 'Pending', '', now() + interval '1 day', NULL),
  ('da5b3000-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000a1', 'Fired', '', now() - interval '1 day', now() - interval '1 day');
INSERT INTO projects (id, tenant_id, name, archived, created_at)
VALUES
  ('da5b4000-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', 'Active', false, now()),
  ('da5b4000-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000a1', 'Archived', true, now());
INSERT INTO user_hermes_profiles (id, tenant_id, slug, label, system_prompt, is_default, created_at, updated_at)
VALUES
  ('da5b6000-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', 'coach', 'Coach', 'p', true, now(), now()),
  ('da5b6000-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000a1', 'other', 'Other', 'p', false, now(), now());
INSERT INTO achievement_progress (id, tenant_id, achievement_key, progress_count, unlocked_at)
VALUES
  ('da5b7000-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', 'first_capture', 1, now() - interval '5 minutes'),
  ('da5b7000-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000a1', 'first_query', 1, now() - interval '7 days'),
  ('da5b7000-0000-4000-8000-000000000003', 'da5b0000-0000-4000-8000-0000000000a1', 'locked', 1, NULL);
INSERT INTO insights (id, tenant_id, section, headline, summary, source_memory_ids, period_start, period_end, dismissed_at, created_at)
VALUES
  ('da5b8000-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', 'patterns', 'Active one', 's', '{}', NULL, NULL, NULL, now() - interval '1 hour'),
  ('da5b8000-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000a1', 'patterns', 'Active two', 's', '{}', NULL, NULL, NULL, now() - interval '2 hours'),
  ('da5b8000-0000-4000-8000-000000000003', 'da5b0000-0000-4000-8000-0000000000a1', 'patterns', 'Dismissed', 's', '{}', NULL, NULL, now() - interval '1 hour', now() - interval '3 hours');

-- Tokens: today, yesterday, and one 40 days back (outside the 30-day summary).
INSERT INTO usage_meter (tenant_id, day, model, mtok_in, mtok_out)
VALUES
  ('da5b0000-0000-4000-8000-0000000000a1', CURRENT_DATE, 'm', 100, 50),
  ('da5b0000-0000-4000-8000-0000000000a1', CURRENT_DATE - 1, 'm', 20, 10),
  ('da5b0000-0000-4000-8000-0000000000a1', CURRENT_DATE - 40, 'm', 7000, 7000);
INSERT INTO embedding_usage (id, tenant_id, year_month, tokens_used, created_at, updated_at)
VALUES
  ('da5b9000-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM'), 1234, now(), now()),
  ('da5b9000-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000a1', '2020-01', 99999, now(), now());
-- Voice: three channels, a failed call, no-channel, an old row and another metric.
INSERT INTO usage_events (tenant_id, occurred_at, metric, amount, source, metadata)
VALUES
  ('da5b0000-0000-4000-8000-0000000000a1', now() - interval '1 day', 'voice_transcribe', 90000, 'whisper', '{"channel":"telegram","outcome":"ok","usdMicros":6000,"imputedUsdMicros":12000}'),
  ('da5b0000-0000-4000-8000-0000000000a1', now() - interval '2 days', 'voice_transcribe', 30000, 'whisper', '{"channel":"telegram","outcome":"error","usdMicros":6000,"imputedUsdMicros":9000}'),
  ('da5b0000-0000-4000-8000-0000000000a1', now() - interval '3 days', 'voice_transcribe', 9000, 'whisper', '{"channel":"app","outcome":"ok","usdMicros":25000}'),
  ('da5b0000-0000-4000-8000-0000000000a1', now() - interval '3 days', 'voice_transcribe', 1000, 'whisper', '{}'),
  ('da5b0000-0000-4000-8000-0000000000a1', now() - interval '45 days', 'voice_transcribe', 999000, 'whisper', '{"channel":"old","outcome":"ok"}'),
  ('da5b0000-0000-4000-8000-0000000000a1', now() - interval '1 day', 'voice_speech', 5000, 'whisper', '{"channel":"speech","outcome":"ok"}');

-- Retrieval: ann improving, ben declining, eli steady with a prior window,
-- ins has only a current window (and a row with no top_distance).
INSERT INTO retrieval_telemetry_events (tenant_id, source_path, hit_count, zero_hit, top_distance, mean_distance, limit_requested, created_at)
SELECT t.id::uuid, 'chat', 1, t.zero, t.dist, t.dist, 5, now() - t.age
FROM (VALUES
  ('da5b0000-0000-4000-8000-0000000000a1', false, 0.25::float8, interval '1 day'),
  ('da5b0000-0000-4000-8000-0000000000a1', false, 0.5, interval '2 days'),
  ('da5b0000-0000-4000-8000-0000000000a1', true, NULL, interval '3 days'),
  ('da5b0000-0000-4000-8000-0000000000a1', false, 0.3, interval '8 days'),
  ('da5b0000-0000-4000-8000-0000000000a1', true, NULL, interval '9 days'),
  ('da5b0000-0000-4000-8000-0000000000a1', false, 0.1, interval '20 days'),
  ('da5b0000-0000-4000-8000-0000000000b2', true, NULL, interval '1 day'),
  ('da5b0000-0000-4000-8000-0000000000b2', false, 0.4, interval '2 days'),
  ('da5b0000-0000-4000-8000-0000000000b2', false, 0.4, interval '8 days'),
  ('da5b0000-0000-4000-8000-0000000000e5', false, 0.2, interval '1 day'),
  ('da5b0000-0000-4000-8000-0000000000e5', true, NULL, interval '2 days'),
  ('da5b0000-0000-4000-8000-0000000000e5', false, 0.2, interval '8 days'),
  ('da5b0000-0000-4000-8000-0000000000e5', true, NULL, interval '9 days'),
  ('da5b0000-0000-4000-8000-0000000000a7', true, NULL, interval '1 day')
) AS t(id, zero, dist, age);
INSERT INTO retrieval_leak_reports (tenant_id, period_start, period_end, total_retrievals, zero_hit_count, zero_hit_rate)
VALUES
  ('da5b0000-0000-4000-8000-0000000000a1', now() - interval '14 days', now() - interval '7 days', 10, 4, 0.4),
  ('da5b0000-0000-4000-8000-0000000000a1', now() - interval '21 days', now() - interval '14 days', 10, 9, 0.9);

-- Router executions. ann (managed, personal vault): 12 runs, 3 failed, so the
-- model-reliability recommendation fires and two upstream models merge into one
-- generic row. ben (BYOK): three distinct rows, one with no provider or model.
INSERT INTO router_executions (id, tenant_id, actor_user_id, vault_id, surface, task_type, strategy, status, selected_provider, selected_model, tokens_in, tokens_out, estimated_cost_usd_micros, latency_ms, usage_estimated, fallback_count, occurred_at)
SELECT gen_random_uuid(), 'da5b0000-0000-4000-8000-0000000000a1', 'da5b0000-0000-4000-8000-0000000000a1', 'da5b0000-0000-4000-8000-0000000000a1', 'chat', 'general', 'single',
       CASE WHEN n <= 3 THEN 'error' ELSE 'ok' END, CASE WHEN n % 2 = 0 THEN 'openrouter' ELSE 'anthropic' END, CASE WHEN n % 2 = 0 THEN 'x/y' ELSE 'claude' END,
       100 * n, 10 * n, 12345 * n, 100 * n, false, CASE WHEN n = 12 THEN 1 ELSE 0 END, now() - (n || ' hours')::interval
FROM generate_series(1, 12) n;
INSERT INTO router_executions (id, tenant_id, actor_user_id, vault_id, surface, task_type, strategy, status, selected_provider, selected_model, tokens_in, tokens_out, estimated_cost_usd_micros, latency_ms, usage_estimated, fallback_count, occurred_at)
VALUES
  (gen_random_uuid(), 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'chat', 'general', 'single', 'ok', 'anthropic', 'claude-opus-4-1', 10, 5, 100, 200, false, 0, now() - interval '1 day'),
  (gen_random_uuid(), 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'chat', 'general', 'single', 'ok', 'anthropic', 'claude-opus-4-1', 20, 5, 200, 300, false, 1, now() - interval '2 days'),
  (gen_random_uuid(), 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'chat', 'general', 'single', 'error', 'anthropic', 'claude-opus-4-1', 30, 5, 300, 700, false, 0, now() - interval '3 days'),
  (gen_random_uuid(), 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'chat', 'general', 'single', 'ok', 'openai', 'gpt-test', 1, 1, 1, 50, false, 0, now() - interval '1 day'),
  (gen_random_uuid(), 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'chat', 'general', 'single', 'ok', 'openai', 'gpt-test', 1, 1, 1, 70, false, 0, now() - interval '40 days'),
  (gen_random_uuid(), 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'chat', 'general', 'single', 'ok', 'openai', 'gpt-test', 1, 1, 1, 80, false, 0, now() - interval '1 day 1 hour'),
  (gen_random_uuid(), 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'chat', 'general', 'single', 'failed', NULL, NULL, 0, 0, 0, 10, false, 0, now() - interval '1 day'),
  -- Team vault: ann and ben both act in it; ben only retrieves and requests.
  (gen_random_uuid(), 'da5b0000-0000-4000-8000-0000000000a1', 'da5b0000-0000-4000-8000-0000000000a1', 'da5bc000-0000-4000-8000-0000000000c1', 'chat', 'general', 'single', 'ok', 'anthropic', 'claude', 40, 2, 4000, 90, false, 0, now() - interval '1 day'),
  (gen_random_uuid(), 'da5b0000-0000-4000-8000-0000000000a1', 'da5b0000-0000-4000-8000-0000000000b2', 'da5bc000-0000-4000-8000-0000000000c1', 'chat', 'general', 'single', 'ok', 'anthropic', 'claude', 50, 3, 5000, 110, false, 0, now() - interval '2 days'),
  (gen_random_uuid(), 'da5b0000-0000-4000-8000-0000000000a1', 'da5b0000-0000-4000-8000-0000000000b2', 'da5bc000-0000-4000-8000-0000000000c1', 'chat', 'general', 'single', 'ok', 'anthropic', 'claude', 60, 4, 6000, 130, false, 0, now() - interval '3 days');
INSERT INTO user_llm_preferences (id, tenant_id, primary_provider, primary_model, fallback_chain, mode, created_at, updated_at)
VALUES
  ('da5bb000-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000b2', 'anthropic', 'claude-opus-4-1', '{"steps":[]}', 'byok', now(), now()),
  ('da5bb000-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000e5', 'openai', '', '{"steps":[]}', 'byok', now(), now()),
  ('da5bb000-0000-4000-8000-000000000003', 'da5b0000-0000-4000-8000-0000000000a7', 'openai', 'managed-model', '{"steps":[]}', 'managed', now(), now());
INSERT INTO analytics_events (id, vault_id, actor_user_id, event_name, source, occurred_at, dimensions)
VALUES
  ('da5ba000-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', 'da5b0000-0000-4000-8000-0000000000a1', 'model_feedback_positive', 'server', now() - interval '1 day', '{"provider":"openrouter","model":"x/y"}'),
  ('da5ba000-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000a1', 'da5b0000-0000-4000-8000-0000000000a1', 'model_feedback_negative', 'server', now() - interval '2 days', '{"provider":"anthropic","model":"claude"}'),
  ('da5ba000-0000-4000-8000-000000000003', 'da5b0000-0000-4000-8000-0000000000a1', 'da5b0000-0000-4000-8000-0000000000a1', 'memory_retrieved', 'server', now() - interval '1 day', '{}'),
  ('da5ba000-0000-4000-8000-000000000004', 'da5b0000-0000-4000-8000-0000000000a1', 'da5b0000-0000-4000-8000-0000000000a1', 'conversation_created', 'server', now() - interval '1 day', '{}'),
  ('da5ba000-0000-4000-8000-000000000005', 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'model_feedback_positive', 'server', now() - interval '1 day', '{"provider":"anthropic","model":"claude-opus-4-1"}'),
  ('da5ba000-0000-4000-8000-000000000006', 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'model_feedback_positive', 'server', now() - interval '2 days', '{"provider":"anthropic","model":"claude-opus-4-1"}'),
  ('da5ba000-0000-4000-8000-000000000007', 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'model_feedback_negative', 'server', now() - interval '3 days', '{"provider":"anthropic","model":"claude-opus-4-1"}'),
  ('da5ba000-0000-4000-8000-000000000008', 'da5b0000-0000-4000-8000-0000000000b2', 'da5b0000-0000-4000-8000-0000000000b2', 'model_feedback_negative', 'server', now() - interval '60 days', '{"provider":"openai","model":"gpt-test"}'),
  ('da5ba000-0000-4000-8000-000000000009', 'da5bc000-0000-4000-8000-0000000000c1', 'da5b0000-0000-4000-8000-0000000000b2', 'memory_retrieved', 'server', now() - interval '1 day', '{}'),
  ('da5ba000-0000-4000-8000-00000000000a', 'da5bc000-0000-4000-8000-0000000000c1', 'da5b0000-0000-4000-8000-0000000000b2', 'memory_retrieved', 'server', now() - interval '2 days', '{}'),
  ('da5ba000-0000-4000-8000-00000000000b', 'da5bc000-0000-4000-8000-0000000000c1', 'da5b0000-0000-4000-8000-0000000000a1', 'conversation_created', 'server', now() - interval '3 days', '{}');
INSERT INTO memories (id, tenant_id, content, created_at, review_state, created_by_user_id)
VALUES
  ('da5be000-0000-4000-8000-000000000021', 'da5bc000-0000-4000-8000-0000000000c1', 'Team capture one', now() - interval '1 day', 'auto', 'da5b0000-0000-4000-8000-0000000000a1'),
  ('da5be000-0000-4000-8000-000000000022', 'da5bc000-0000-4000-8000-0000000000c1', 'Team capture two', now() - interval '2 days', 'auto', 'da5b0000-0000-4000-8000-0000000000a1'),
  ('da5be000-0000-4000-8000-000000000023', 'da5bc000-0000-4000-8000-0000000000c1', 'Eli capture', now() - interval '2 days', 'auto', 'da5b0000-0000-4000-8000-0000000000e5');

-- Live work for ann: 5 running or waiting runs, 1 running gateway job (a unique index allows one per
-- tenant), finished rows that
-- never appear, and one run of ben's.
INSERT INTO workflows (id, tenant_id, name, draft_definition)
VALUES
  ('da5bc100-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', 'Daily digest', '{}'),
  ('da5bc100-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000b2', 'Bens flow', '{}');
INSERT INTO workflow_versions (id, tenant_id, workflow_id, version, definition)
VALUES
  ('da5bc200-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', 'da5bc100-0000-4000-8000-000000000001', 1, '{}'),
  ('da5bc200-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000b2', 'da5bc100-0000-4000-8000-000000000002', 1, '{}');
INSERT INTO workflow_runs (id, tenant_id, workflow_id, version_id, status, trigger_kind, started_at, error_message, created_at)
VALUES
  ('da5bc300-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', 'da5bc100-0000-4000-8000-000000000001', 'da5bc200-0000-4000-8000-000000000001', 'running', 'manual', now() - interval '2 hours', NULL, now() - interval '3 hours'),
  ('da5bc300-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000a1', 'da5bc100-0000-4000-8000-000000000001', 'da5bc200-0000-4000-8000-000000000001', 'running', 'manual', now() - interval '20 minutes', 'partial failure', now() - interval '3 hours'),
  ('da5bc300-0000-4000-8000-000000000003', 'da5b0000-0000-4000-8000-0000000000a1', 'da5bc100-0000-4000-8000-000000000001', 'da5bc200-0000-4000-8000-000000000001', 'queued', 'manual', NULL, NULL, now() - interval '1 hour'),
  ('da5bc300-0000-4000-8000-000000000004', 'da5b0000-0000-4000-8000-0000000000a1', 'da5bc100-0000-4000-8000-000000000001', 'da5bc200-0000-4000-8000-000000000001', 'waitingForApproval', 'manual', now() - interval '30 minutes', NULL, now() - interval '5 hours'),
  ('da5bc300-0000-4000-8000-000000000005', 'da5b0000-0000-4000-8000-0000000000a1', 'da5bc100-0000-4000-8000-000000000001', 'da5bc200-0000-4000-8000-000000000001', 'queued', 'manual', NULL, NULL, now() - interval '6 hours'),
  ('da5bc300-0000-4000-8000-000000000006', 'da5b0000-0000-4000-8000-0000000000a1', 'da5bc100-0000-4000-8000-000000000001', 'da5bc200-0000-4000-8000-000000000001', 'succeeded', 'manual', now() - interval '8 hours', NULL, now() - interval '8 hours'),
  ('da5bc300-0000-4000-8000-000000000007', 'da5b0000-0000-4000-8000-0000000000a1', 'da5bc100-0000-4000-8000-000000000001', 'da5bc200-0000-4000-8000-000000000001', 'paused', 'manual', now() - interval '9 hours', NULL, now() - interval '9 hours'),
  ('da5bc300-0000-4000-8000-000000000008', 'da5b0000-0000-4000-8000-0000000000b2', 'da5bc100-0000-4000-8000-000000000002', 'da5bc200-0000-4000-8000-000000000002', 'running', 'manual', now() - interval '1 hour', NULL, now() - interval '1 hour');
INSERT INTO hermes_gateway_apply_jobs (id, tenant_id, state, error_message, created_at)
VALUES
  ('da5bc400-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a1', 'running', 'retrying', now() - interval '10 minutes'),
  ('da5bc400-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000a1', 'failed', 'boom', now() - interval '4 hours'),
  ('da5bc400-0000-4000-8000-000000000003', 'da5b0000-0000-4000-8000-0000000000a1', 'succeeded', NULL, now() - interval '5 hours'),
  ('da5bc400-0000-4000-8000-000000000004', 'da5b0000-0000-4000-8000-0000000000a1', 'succeeded', NULL, now() - interval '1 minute');

-- dash_wri: six loose memories, enough to fire every memory recommendation, and
-- an expired snooze that must not hide its recommendation.
INSERT INTO memories (id, tenant_id, content, created_at, review_state)
SELECT ('da5be100-0000-4000-8000-0000000000' || lpad(n::text, 2, '0'))::uuid, 'da5b0000-0000-4000-8000-0000000000f6', 'Loose ' || n,
       now() - ((40 + n) || ' days')::interval, CASE WHEN n = 1 THEN 'pending' ELSE 'auto' END
FROM generate_series(1, 6) n;
INSERT INTO analytics_recommendation_states (vault_id, user_id, recommendation_id, snoozed_until)
VALUES ('da5b0000-0000-4000-8000-0000000000f6', 'da5b0000-0000-4000-8000-0000000000f6', 'memory-never-retrieved', now() - interval '1 day');

-- dash_ins: every section, an unknown section, a dismissed synthesis newer than
-- the active one, source memory ids and analytical periods.
INSERT INTO insights (id, tenant_id, section, headline, summary, source_memory_ids, period_start, period_end, dismissed_at, created_at)
VALUES
  ('da5b8100-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000a7', 'this_week', 'Week', 'wk', '{da5be000-0000-4000-8000-000000000001,da5be000-0000-4000-8000-000000000002}', now() - interval '7 days', now(), NULL, now() - interval '5 days'),
  ('da5b8100-0000-4000-8000-000000000002', 'da5b0000-0000-4000-8000-0000000000a7', 'this_month', 'Month dismissed', 'mo', '{}', now() - interval '30 days', now(), now() - interval '1 hour', now() - interval '1 day'),
  ('da5b8100-0000-4000-8000-000000000003', 'da5b0000-0000-4000-8000-0000000000a7', 'patterns', 'Pattern', 'p', '{}', NULL, NULL, NULL, now() - interval '2 days'),
  ('da5b8100-0000-4000-8000-000000000004', 'da5b0000-0000-4000-8000-0000000000a7', 'contradictions', 'Contradiction', 'c', '{}', NULL, NULL, NULL, now() - interval '3 days'),
  ('da5b8100-0000-4000-8000-000000000005', 'da5b0000-0000-4000-8000-0000000000a7', 'connections', 'Connection', 'n', '{}', NULL, NULL, NULL, now() - interval '4 days'),
  ('da5b8100-0000-4000-8000-000000000006', 'da5b0000-0000-4000-8000-0000000000a7', 'retired_section', 'Unknown section', 'u', '{}', NULL, NULL, NULL, now() - interval '6 days'),
  ('da5b8100-0000-4000-8000-000000000007', 'da5b0000-0000-4000-8000-0000000000b2', 'this_week', 'Bens week', 'b', '{}', NULL, NULL, NULL, now() - interval '1 day');

-- dash_ovf: tokens past Int32, so DashboardPeriodQuery.counts fails on ::int and
-- the home cockpit falls back to zeros (usage-summary uses ::bigint and works).
INSERT INTO usage_meter (tenant_id, day, model, mtok_in, mtok_out)
VALUES ('da5b0000-0000-4000-8000-0000000000b8', CURRENT_DATE, 'm', 3000000000, 1);
INSERT INTO memories (id, tenant_id, content, created_at)
VALUES ('da5be200-0000-4000-8000-000000000001', 'da5b0000-0000-4000-8000-0000000000b8', 'Overflow tenant', date_trunc('day', now()));

-- Parity-only view: the states table's timestamps are masked in snapshots, so
-- this exposes how many days a snooze runs and whether it is dismissed.
CREATE VIEW dashboard_recommendation_states_view AS
SELECT vault_id, user_id, recommendation_id, dismissed_at IS NOT NULL AS dismissed,
       round(extract(epoch FROM snoozed_until - updated_at) / 86400)::int AS snooze_days
FROM analytics_recommendation_states;
