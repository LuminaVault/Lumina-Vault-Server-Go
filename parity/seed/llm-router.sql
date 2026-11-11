-- llm-router parity seed (parity/scenarios/llm-router). Ids use the 1a7e prefix.
-- One user per tier and per placeholder case, so no settingsByUser bucket fills.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, created_at, updated_at)
VALUES
  ('1a7e0000-0000-4000-8000-0000000000a1', 'lr_free@parity.test', 'x', true, 'lr_free', 'free',     'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a2', 'lr_pro@parity.test',  'x', true, 'lr_pro',  'pro',      'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a3', 'lr_ult@parity.test',  'x', true, 'lr_ult',  'ultimate', 'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a4', 'lr_byok@parity.test', 'x', true, 'lr_byok', 'pro',      'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a5', 'lr_none@parity.test', 'x', true, 'lr_none', 'pro',      'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a6', 'lr_dash@parity.test', 'x', true, 'lr_dash', 'trial',    'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a7', 'lr_bind@parity.test', 'x', true, 'lr_bind', 'pro',      'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a8', 'lr_val@parity.test',  'x', true, 'lr_val',  'pro',      'none',     '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a9', 'lr_over@parity.test', 'x', true, 'lr_over', 'free',     'ultimate', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Personal vaults: id = owner user id (spaces and other tenant rows reference vaults.id).
INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
VALUES
  ('1a7e0000-0000-4000-8000-0000000000a1', '1a7e0000-0000-4000-8000-0000000000a1', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a2', '1a7e0000-0000-4000-8000-0000000000a2', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a3', '1a7e0000-0000-4000-8000-0000000000a3', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a4', '1a7e0000-0000-4000-8000-0000000000a4', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a5', '1a7e0000-0000-4000-8000-0000000000a5', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a6', '1a7e0000-0000-4000-8000-0000000000a6', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a7', '1a7e0000-0000-4000-8000-0000000000a7', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a8', '1a7e0000-0000-4000-8000-0000000000a8', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a7e0000-0000-4000-8000-0000000000a9', '1a7e0000-0000-4000-8000-0000000000a9', 'Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- byok's saved BYOK chain: the primary, a fallback that is the brain label and one
-- that is the placeholder (both dropped), a duplicate of the primary and a real one.
INSERT INTO user_llm_preferences (id, tenant_id, mode, primary_provider, primary_model, fallback_chain, allowed_providers, blocked_providers, created_at, updated_at)
VALUES
  ('1a7e1000-0000-4000-8000-000000000001', '1a7e0000-0000-4000-8000-0000000000a4', 'byok', 'openai', 'gpt-4o',
   '{"steps":[{"provider":"openRouter","model":"LuminaVault Brain · Auto"},{"provider":"openRouter","model":"auto"},{"provider":"openai","model":"gpt-4o"},{"provider":"anthropic","model":"claude-sonnet-4-6"}]}',
   '{"providers":["openai","anthropic"]}', '{"providers":[]}', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

-- bind's scopes (a workflow's draft must decode: Swift loads the Fluent model): a space and a workflow of its own, a job by name, and another
-- tenant's space and workflow.
INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at)
VALUES
  ('1a7e2000-0000-4000-8000-000000000001', '1a7e0000-0000-4000-8000-0000000000a7', 'Research', 'research', 0, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('1a7e2000-0000-4000-8000-000000000002', '1a7e0000-0000-4000-8000-0000000000a2', 'Elsewhere', 'elsewhere', 0, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

INSERT INTO workflows (id, tenant_id, name, draft_definition, created_at, updated_at)
VALUES
  ('1a7e3000-0000-4000-8000-000000000001', '1a7e0000-0000-4000-8000-0000000000a7', 'Digest', '{"schemaVersion":1,"trigger":"manual","triggerConfiguration":{},"nodes":[{"id":"1A7ED000-0000-4000-8000-000000000001","kind":"trigger","name":"Start","x":0,"y":0,"configuration":{}}],"edges":[]}', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('1a7e3000-0000-4000-8000-000000000002', '1a7e0000-0000-4000-8000-0000000000a2', 'Other', '{"schemaVersion":1,"trigger":"manual","triggerConfiguration":{},"nodes":[{"id":"1A7ED000-0000-4000-8000-000000000001","kind":"trigger","name":"Start","x":0,"y":0,"configuration":{}}],"edges":[]}', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

INSERT INTO skills_state (tenant_id, source, name)
VALUES ('1a7e0000-0000-4000-8000-0000000000a7', 'builtin', 'morning-brief');

-- dash's routed calls this month (occurred_at is the template's seed time) and
-- one from a month that is always past. The dashboard sums the first two only.
INSERT INTO router_executions (id, tenant_id, surface, task_type, strategy, status, tokens_in, tokens_out, estimated_cost_usd_micros, latency_ms, fallback_count, occurred_at)
VALUES
  ('1a7e4000-0000-4000-8000-000000000001', '1a7e0000-0000-4000-8000-0000000000a6', 'chat', 'general', 'sequential', 'ok',    120, 40, 900, 300, 0, now()),
  ('1a7e4000-0000-4000-8000-000000000002', '1a7e0000-0000-4000-8000-0000000000a6', 'chat', 'coding',  'sequential', 'error', 80,  0,  100, 501, 2, now()),
  ('1a7e4000-0000-4000-8000-000000000003', '1a7e0000-0000-4000-8000-0000000000a6', 'chat', 'general', 'sequential', 'ok',    999, 999, 999, 999, 9, '2020-01-01T00:00:00Z');

-- ult's synthesis preset and parallel run: every parallel route is 403 while
-- cerberus.parallelEnabled is off, so these only prove nothing is read.
INSERT INTO router_synthesis_presets (id, tenant_id, name, prompt, created_at, updated_at)
VALUES ('1a7e5000-0000-4000-8000-000000000001', '1a7e0000-0000-4000-8000-0000000000a3', 'Judge', 'Pick the best answer.', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

INSERT INTO router_executions (id, tenant_id, surface, task_type, strategy, status, prompt, parallel_strategy, participant_count, occurred_at)
VALUES ('1a7e4000-0000-4000-8000-000000000010', '1a7e0000-0000-4000-8000-0000000000a3', 'query', 'general', 'ensemble', 'ok', 'Compare', 'debate', 2, '2026-01-03T00:00:00Z');

-- Snapshot views: stable columns first, server-minted ids last (snapshots sort on
-- the row's text).
CREATE VIEW parity_lr_profiles AS
SELECT tenant_id, name, mode, is_preset, revision, document, created_at, updated_at, id
FROM router_profiles WHERE tenant_id::text LIKE '1a7e0000-%';

CREATE VIEW parity_lr_bindings AS
SELECT tenant_id, scope, scope_id, profile_id, created_at, updated_at, id
FROM router_bindings WHERE tenant_id::text LIKE '1a7e0000-%';

-- Seeded rows only (the parallel routes are 403 in parity), so the seed ids lead.
CREATE VIEW parity_lr_parallel AS
SELECT id, tenant_id, 'execution' AS kind, prompt AS text FROM router_executions WHERE tenant_id::text LIKE '1a7e0000-%'
UNION ALL
SELECT id, tenant_id, 'preset', name || ': ' || prompt FROM router_synthesis_presets WHERE tenant_id::text LIKE '1a7e0000-%';
