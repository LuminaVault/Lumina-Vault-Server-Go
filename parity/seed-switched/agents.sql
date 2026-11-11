-- State "agents" (scripts/parity-switches.sh): BYO Hermes on the LLM stub, which the
-- SSRF guard admits only because the state allows private ranges over plain HTTP.
--   agents_fay, agents_jon  the stub's gateway routes
--   agents_gus              a path the stub doesn't serve: an older Hermes, 404s with no error code
--   agents_hal              a port nothing listens on
INSERT INTO user_hermes_config (id, tenant_id, base_url, verified_at, created_at, updated_at)
VALUES
  ('a9e79000-0000-4000-8000-000000000001', 'a9e70000-0000-4000-8000-0000000000a6', 'http://llmstub:8080', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a9e79000-0000-4000-8000-000000000002', 'a9e70000-0000-4000-8000-0000000000a7', 'http://llmstub:8080/old-hermes', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a9e79000-0000-4000-8000-000000000003', 'a9e70000-0000-4000-8000-0000000000a8', 'http://llmstub:9', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a9e79000-0000-4000-8000-000000000004', 'a9e70000-0000-4000-8000-0000000000aa', 'http://llmstub:8080/', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
