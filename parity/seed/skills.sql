-- Skills fixtures (parity/scenarios/skills). skillRunByUser allows 10 a minute, so each
-- scenario file uses its own users. Times relative to NOW() are read by both servers
-- from the same rows, so they compare equal.
INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at) VALUES
  ('5c100000-0000-4000-8000-0000000000a1', 'sk-ann@parity.test', 'x', true, 'sk_ann', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5c100000-0000-4000-8000-0000000000b2', 'sk-ben@parity.test', 'x', true, 'sk_ben', 'pro', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5c100000-0000-4000-8000-0000000000c3', 'sk-cy@parity.test', 'x', true, 'sk_cy', 'ultimate', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5c100000-0000-4000-8000-0000000000d4', 'sk-dee@parity.test', 'x', true, 'sk_dee', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5c100000-0000-4000-8000-0000000000e5', 'sk-eve@parity.test', 'x', true, 'sk_eve', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5c100000-0000-4000-8000-0000000000f6', 'sk-fay@parity.test', 'x', true, 'sk_fay', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5c100000-0000-4000-8000-0000000000a7', 'sk-gus@parity.test', 'x', true, 'sk_gus', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5c100000-0000-4000-8000-0000000000b8', 'sk-hal@parity.test', 'x', true, 'sk_hal', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- sk_ann: state rows for built-ins, one with every optional set and invalid enum values
-- elsewhere, plus a mirrored Hermes skill.
INSERT INTO skills_state (tenant_id, source, name, enabled, schedule_override, last_run_at, last_status, last_error, apns_category, origin_kind, origin_id) VALUES
  ('5c100000-0000-4000-8000-0000000000a1', 'builtin', 'daily-brief', false, '0 6 * * *', '2026-02-01T08:00:00Z', 'success', NULL, 'digest', NULL, NULL),
  ('5c100000-0000-4000-8000-0000000000a1', 'builtin', 'pattern-detector', true, NULL, '2026-02-02T08:00:00.250Z', 'exploded', 'boom', 'shouty', 'card', '5c10c000-0000-4000-8000-000000000001'),
  ('5c100000-0000-4000-8000-0000000000a1', 'builtin', 'not-in-catalog', true, NULL, NULL, NULL, NULL, NULL, 'mystery', '5c10c000-0000-4000-8000-000000000002');

INSERT INTO hermes_mirrored_skills (id, tenant_id, name, description, enabled, source, updated_at) VALUES
  ('5c10a000-0000-4000-8000-000000000001', '5c100000-0000-4000-8000-0000000000a1', 'web-search', 'Searches the web for the latest on a topic', true, 'builtin', '2026-02-01T00:00:00Z'),
  ('5c10a000-0000-4000-8000-000000000002', '5c100000-0000-4000-8000-0000000000a1', 'arxiv', '', false, 'hub', NULL);

-- Runs: today's runs feed dailyRunCount; older ones the history and sparkline.
INSERT INTO skill_run_log (id, tenant_id, source, name, started_at, ended_at, status, error, model_used, mtok_in, mtok_out, markdown, blocks) VALUES
  ('5c10b000-0000-4000-8000-000000000001', '5c100000-0000-4000-8000-0000000000a1', 'builtin', 'daily-brief', date_trunc('day', NOW()) + INTERVAL '1 minute', date_trunc('day', NOW()) + INTERVAL '2 minutes', 'success', NULL, 'parity-model', 120, 45, '# Morning\nAll quiet.', $b$[{"type":"heading","text":"Morning","level":1},{"type":"statCard","label":"Steps","value":"9,001","delta":"+3%","trend":"up"},{"type":"list","items":[]},{"type":"lineChart","series":[{"name":"s","points":[{"x":"mon","y":1},{"x":"tue","y":2.5}]}]},{"type":"keyValue","pairs":[{"key":"a","value":"b"}],"extra":true}]$b$),
  ('5c10b000-0000-4000-8000-000000000002', '5c100000-0000-4000-8000-0000000000a1', 'builtin', 'daily-brief', '2026-01-10T07:00:00Z', NULL, 'running', NULL, NULL, 0, 0, NULL, $b$[{"text":"no type"}]$b$),
  ('5c10b000-0000-4000-8000-000000000003', '5c100000-0000-4000-8000-0000000000a1', 'builtin', 'daily-brief', '2026-01-09T07:00:00Z', '2026-01-09T07:00:03Z', 'weird', 'it broke', 'm2', 1, 2, '', $b$[]$b$),
  ('5c10b000-0000-4000-8000-000000000004', '5c100000-0000-4000-8000-0000000000a1', 'builtin', 'daily-brief', '2026-01-08T07:00:00Z', '2026-01-08T07:00:03Z', 'error', 'timeout', NULL, 0, 0, NULL, $b$null$b$),
  ('5c10b000-0000-4000-8000-000000000005', '5c100000-0000-4000-8000-0000000000a1', 'builtin', 'pattern-detector', NOW() - INTERVAL '3 days', NOW() - INTERVAL '3 days', 'success', NULL, NULL, 0, 0, 'memo', NULL),
  ('5c10b000-0000-4000-8000-000000000006', '5c100000-0000-4000-8000-0000000000a1', 'hermes', 'pattern-detector', date_trunc('day', NOW()) + INTERVAL '3 minutes', NULL, 'success', NULL, NULL, 0, 0, 'from hermes', NULL);

-- sk_dee: the Today feed. Local runs (with and without markdown, a Hermes-sourced row
-- that must not show) unioned with Hermes job runs; a streak of yesterday and the day
-- before; a pending local run makes activeRun true.
INSERT INTO skill_run_log (id, tenant_id, source, name, started_at, ended_at, status, error, model_used, mtok_in, mtok_out, markdown, blocks) VALUES
  ('5c10d000-0000-4000-8000-000000000001', '5c100000-0000-4000-8000-0000000000d4', 'builtin', 'daily-brief', date_trunc('day', NOW()) - INTERVAL '1 day' + INTERVAL '8 hours', NULL, 'success', NULL, NULL, 0, 0, E'\n\n  ## Good morning, Dee  \nRest of the brief', NULL),
  ('5c10d000-0000-4000-8000-000000000002', '5c100000-0000-4000-8000-0000000000d4', 'vault', 'job-pattern-watch', date_trunc('day', NOW()) - INTERVAL '2 days' + INTERVAL '8 hours', NULL, 'error', 'model refused', NULL, 0, 0, 'partial output', NULL),
  ('5c10d000-0000-4000-8000-000000000003', '5c100000-0000-4000-8000-0000000000d4', 'builtin', 'contradiction-scan', '2026-01-20T10:00:00.123Z', NULL, 'success', NULL, NULL, 0, 0, 'A very long headline that goes on and on and on and on and on and on and on and on and on and on and on and on and on and on and ends here', NULL),
  ('5c10d000-0000-4000-8000-000000000004', '5c100000-0000-4000-8000-0000000000d4', 'builtin', 'capture-enrich', '2026-01-19T10:00:00Z', NULL, 'success', NULL, NULL, 0, 0, '', NULL),
  ('5c10d000-0000-4000-8000-000000000005', '5c100000-0000-4000-8000-0000000000d4', 'hermes', 'hermes-mirror', '2026-01-18T10:00:00Z', NULL, 'success', NULL, NULL, 0, 0, 'mirrored row', NULL),
  ('5c10d000-0000-4000-8000-000000000006', '5c100000-0000-4000-8000-0000000000d4', 'builtin', 'correlation-finder', '2026-01-17T10:00:00Z', NULL, 'success', NULL, NULL, 0, 0, E'####\n#\n   \n', NULL),
  ('5c10d000-0000-4000-8000-000000000007', '5c100000-0000-4000-8000-0000000000d4', 'builtin', 'pending-one', NOW() - INTERVAL '5 minutes', NULL, 'pending', NULL, NULL, 0, 0, NULL, NULL);

INSERT INTO hermes_mirrored_jobs (id, tenant_id, hermes_job_id, name, schedule, prompt, paused, raw, updated_at) VALUES
  ('5c10e000-0000-4000-8000-000000000001', '5c100000-0000-4000-8000-0000000000d4', 'hj-1', 'Weekly digest', '0 9 * * 1', 'p', false, '{}', '2026-01-01T00:00:00Z'),
  ('5c10e000-0000-4000-8000-000000000002', '5c100000-0000-4000-8000-0000000000d4', 'hj-2', '', '0 9 * * 1', 'p', false, '{}', '2026-01-01T00:00:00Z');

INSERT INTO vault_files (id, tenant_id, space_id, path, content_type, size_bytes, sha256, created_at, updated_at) VALUES
  ('5c10f000-0000-4000-8000-000000000001', '5c100000-0000-4000-8000-0000000000d4', NULL, 'hermes/weekly-digest.md', 'text/markdown', 1, 'h', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO hermes_job_runs (id, tenant_id, hermes_job_id, hermes_run_key, status, started_at, finished_at, output, error, vault_file_id) VALUES
  ('5c10e100-0000-4000-8000-000000000001', '5c100000-0000-4000-8000-0000000000d4', 'hj-1', 'k1', 'ok', '2026-01-21T09:00:00Z', '2026-01-21T09:01:00Z', E'Digest\nbody', NULL, '5c10f000-0000-4000-8000-000000000001'),
  ('5c10e100-0000-4000-8000-000000000002', '5c100000-0000-4000-8000-0000000000d4', 'hj-2', 'k2', 'error', '2026-01-16T09:00:00Z', NULL, NULL, 'job failed upstream', NULL),
  ('5c10e100-0000-4000-8000-000000000003', '5c100000-0000-4000-8000-0000000000d4', 'hj-unknown', 'k3', 'ok', '2026-01-15T09:00:00Z', NULL, 'orphan output', NULL, NULL),
  ('5c10e100-0000-4000-8000-000000000004', '5c100000-0000-4000-8000-0000000000d4', 'hj-1', 'k4', 'running', '2026-01-22T09:00:00Z', NULL, 'partial', NULL, NULL),
  ('5c10e100-0000-4000-8000-000000000005', '5c100000-0000-4000-8000-0000000000d4', 'hj-1', 'k5', 'ok', '2026-01-14T09:00:00Z', NULL, NULL, NULL, NULL);

-- sk_eve: a Hermes job still running makes activeRun true with no local run.
INSERT INTO hermes_job_runs (id, tenant_id, hermes_job_id, hermes_run_key, status, started_at) VALUES
  ('5c10e100-0000-4000-8000-000000000006', '5c100000-0000-4000-8000-0000000000e5', 'hj-9', 'k9', 'running', '2026-01-22T09:00:00Z');

-- sk_gus: a conversation a job can link back to.
INSERT INTO conversations (id, tenant_id, title, created_at, updated_at) VALUES
  ('5c10c100-0000-4000-8000-000000000001', '5c100000-0000-4000-8000-0000000000a7', 'Gus chats', '2026-02-01T00:00:00Z', '2026-02-01T00:00:00Z');
