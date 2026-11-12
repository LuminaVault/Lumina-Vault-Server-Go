-- Hermes mirror fixtures (parity/scenarios/hermes-mirror,
-- parity/scenarios-switched/agents/hermes-mirror). One user per scenario file so
-- settingsByUser (30 a minute) never fills.
--   hm_amy  base reads, sync and the error paths on the managed transport
--   hm_bea  stored artifacts: filters, search, paging, get
--   hm_cy   a mirrored job with stored runs
--   hm_eve  webhook reads and the public push route
--   hm_dee  BYO gateway on the LLM stub (state agents, seed-switched/agents.sql)
INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at) VALUES
  ('4e770000-0000-4000-8000-0000000000a1', 'hm-amy@parity.test', 'x', true, 'hm_amy', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4e770000-0000-4000-8000-0000000000b2', 'hm-bea@parity.test', 'x', true, 'hm_bea', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4e770000-0000-4000-8000-0000000000c3', 'hm-cy@parity.test',  'x', true, 'hm_cy',  'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4e770000-0000-4000-8000-0000000000d4', 'hm-dee@parity.test', 'x', true, 'hm_dee', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4e770000-0000-4000-8000-0000000000e5', 'hm-eve@parity.test', 'x', true, 'hm_eve', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES
  ('4e770000-0000-4000-8000-0000000000a1', '4e770000-0000-4000-8000-0000000000a1', 'hm_amy''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4e770000-0000-4000-8000-0000000000b2', '4e770000-0000-4000-8000-0000000000b2', 'hm_bea''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4e770000-0000-4000-8000-0000000000c3', '4e770000-0000-4000-8000-0000000000c3', 'hm_cy''s Vault',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4e770000-0000-4000-8000-0000000000d4', '4e770000-0000-4000-8000-0000000000d4', 'hm_dee''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('4e770000-0000-4000-8000-0000000000e5', '4e770000-0000-4000-8000-0000000000e5', 'hm_eve''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- hm_bea: five artifacts over two sessions; one label carries LIKE wildcards.
INSERT INTO hermes_artifacts (id, tenant_id, kind, value, href, label, session_id, session_title, occurred_at, content_hash, collected_at) VALUES
  ('4e771000-0000-4000-8000-000000000001', '4e770000-0000-4000-8000-0000000000b2', 'link',  'https://x.test/one',  'https://x.test/one',  'one',        's1', 'Research', '2026-09-01T12:00:00Z', 'hm-hash-1', '2026-09-02T00:00:00Z'),
  ('4e771000-0000-4000-8000-000000000002', '4e770000-0000-4000-8000-0000000000b2', 'image', 'https://x.test/a.png', 'https://x.test/a.png', 'a.png',      's1', 'Research', '2026-09-01T12:01:00Z', 'hm-hash-2', '2026-09-02T00:00:00Z'),
  ('4e771000-0000-4000-8000-000000000003', '4e770000-0000-4000-8000-0000000000b2', 'file',  '/kb/100%_done.md',    '/kb/100%_done.md',    '100%_done.md', 's2', 'Export',   '2026-09-01T12:02:00Z', 'hm-hash-3', '2026-09-02T00:00:00Z'),
  ('4e771000-0000-4000-8000-000000000004', '4e770000-0000-4000-8000-0000000000b2', 'file',  '/kb/1000 done.md',    '/kb/1000 done.md',    '1000 done.md', 's2', 'Export',   '2026-09-01T12:03:00Z', 'hm-hash-4', '2026-09-02T00:00:00Z'),
  ('4e771000-0000-4000-8000-000000000005', '4e770000-0000-4000-8000-0000000000b2', 'link',  'https://x.test/two',  'https://x.test/two',  'two',        's2', 'Export',   '2026-09-01T12:04:00Z', 'hm-hash-5', '2026-09-02T00:00:00Z');

-- hm_cy: one mirrored job (snapshot row) and three stored runs, one filed to the vault.
INSERT INTO hermes_mirrored_jobs (id, tenant_id, hermes_job_id, name, schedule, prompt, paused, last_run_at, next_run_at, raw, updated_at, runs_high_water_at, runs_collected_at) VALUES
  ('4e772000-0000-4000-8000-000000000001', '4e770000-0000-4000-8000-0000000000c3', 'digest', 'Daily Digest', '0 9 * * *', 'summarise', false,
   '2026-09-02T09:00:00Z', '2026-09-03T09:00:00Z', '{"id":"digest","name":"Daily Digest","enabled":true}', '2026-09-02T09:05:00Z', '2026-09-02T09:00:00Z', '2026-09-02T09:05:00Z');

INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at) VALUES
  ('4e773000-0000-4000-8000-000000000001', '4e770000-0000-4000-8000-0000000000c3', 'raw/jobs/daily-digest/2026-09-02T09-00-00Z.md', 'text/markdown', 10, 'hm-seed-1', '2026-09-02T09:05:00Z', '2026-09-02T09:05:00Z');

INSERT INTO hermes_job_runs (id, tenant_id, hermes_job_id, hermes_run_key, status, started_at, finished_at, output, error, tokens, vault_file_id, collected_at) VALUES
  ('4e774000-0000-4000-8000-000000000001', '4e770000-0000-4000-8000-0000000000c3', 'digest', 'run-3', 'ok',    '2026-09-02T09:00:00Z', '2026-09-02T09:01:00Z', '# Tuesday', NULL, '{"input":120,"output":340}', '4e773000-0000-4000-8000-000000000001', '2026-09-02T09:05:00Z'),
  ('4e774000-0000-4000-8000-000000000002', '4e770000-0000-4000-8000-0000000000c3', 'digest', 'run-2', 'error', '2026-09-01T09:00:00Z', NULL, NULL, 'provider timeout', NULL, NULL, '2026-09-01T09:05:00Z'),
  ('4e774000-0000-4000-8000-000000000003', '4e770000-0000-4000-8000-0000000000c3', 'digest', 'run-1', 'ok',    '2026-08-31T09:00:00Z', '2026-08-31T09:01:00Z', '# Sunday', NULL, NULL, NULL, '2026-08-31T09:05:00Z');

-- The rows without their random ids or clock columns.
CREATE VIEW parity_hm_state AS
  SELECT tenant_id, last_status, last_error, skills_count, jobs_count, vault_files_count, vault_path, vault_state,
         sessions_imported, compile_job_id, last_sync_at IS NOT NULL AS synced
    FROM hermes_mirror_state
   WHERE tenant_id IN ('4e770000-0000-4000-8000-0000000000a1', '4e770000-0000-4000-8000-0000000000c3',
                       '4e770000-0000-4000-8000-0000000000d4', '4e770000-0000-4000-8000-0000000000e5');

CREATE VIEW parity_hm_skills AS
  SELECT tenant_id, name, description, enabled, source, content_hash IS NOT NULL AS hashed
    FROM hermes_mirrored_skills
   WHERE tenant_id IN ('4e770000-0000-4000-8000-0000000000a1', '4e770000-0000-4000-8000-0000000000d4');

CREATE VIEW parity_hm_jobs AS
  SELECT tenant_id, hermes_job_id, name, schedule, prompt, paused, last_run_at, next_run_at, raw,
         runs_high_water_at, runs_collected_at IS NOT NULL AS collected
    FROM hermes_mirrored_jobs
   WHERE tenant_id IN ('4e770000-0000-4000-8000-0000000000a1', '4e770000-0000-4000-8000-0000000000c3',
                       '4e770000-0000-4000-8000-0000000000d4');

CREATE VIEW parity_hm_artifacts AS
  SELECT tenant_id, kind, value, href, label, session_id, session_title, occurred_at, content_hash
    FROM hermes_artifacts
   WHERE tenant_id IN ('4e770000-0000-4000-8000-0000000000b2', '4e770000-0000-4000-8000-0000000000d4');
