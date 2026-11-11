-- Wave 7B admin fixtures (parity/scenarios/admin). The admin routes run as the
-- operator, so these users never authenticate; they own the rows the per-user
-- routes touch, and nothing else touches them.
--   ad_mem   memories for recompute and prune: old and quiet (pruned), old and used
--            (kept), no created_at (scores 1, kept), and a future one (kept)
--   ad_two   an old, quiet memory a prune of ad_mem must not reach
--   ad_hc    no health events: skipped_insufficient_history
--   ad_old   30+ days of health history, none in the last 7: skipped_no_recent_events
--   ad_ran   a correlation memory from an hour before seeding: skipped_already_ran
-- Old rows are from 2024, so the recency term is under 1e-15 and the scores don't
-- depend on the second each server runs at.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at) VALUES
  ('ad000000-0000-4000-8000-0000000000a1', 'ad-mem@parity.test', 'x', true, 'ad_mem', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ad000000-0000-4000-8000-0000000000b2', 'ad-two@parity.test', 'x', true, 'ad_two', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ad000000-0000-4000-8000-0000000000c3', 'ad-hc@parity.test', 'x', true, 'ad_hc', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ad000000-0000-4000-8000-0000000000d4', 'ad-old@parity.test', 'x', true, 'ad_old', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ad000000-0000-4000-8000-0000000000e5', 'ad-ran@parity.test', 'x', true, 'ad_ran', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES
  ('ad000000-0000-4000-8000-0000000000a1', 'ad000000-0000-4000-8000-0000000000a1', 'ad_mem''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ad000000-0000-4000-8000-0000000000b2', 'ad000000-0000-4000-8000-0000000000b2', 'ad_two''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ad000000-0000-4000-8000-0000000000c3', 'ad000000-0000-4000-8000-0000000000c3', 'ad_hc''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ad000000-0000-4000-8000-0000000000d4', 'ad000000-0000-4000-8000-0000000000d4', 'ad_old''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('ad000000-0000-4000-8000-0000000000e5', 'ad000000-0000-4000-8000-0000000000e5', 'ad_ran''s Vault', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO memories (id, tenant_id, content, created_at, tags, score, access_count, query_hit_count, last_accessed_at) VALUES
  ('ad00a000-0000-4000-8000-000000000001', 'ad000000-0000-4000-8000-0000000000a1', 'old and quiet', '2024-01-01T00:00:00Z', '{old}', 0, 0, 0, NULL),
  ('ad00a000-0000-4000-8000-000000000002', 'ad000000-0000-4000-8000-0000000000a1', 'old and used', '2024-01-01T00:00:00Z', NULL, 0, 10, 5, '2024-02-01T00:00:00Z'),
  ('ad00a000-0000-4000-8000-000000000003', 'ad000000-0000-4000-8000-0000000000a1', 'no timestamp', NULL, NULL, 0, 0, 0, NULL),
  ('ad00a000-0000-4000-8000-000000000004', 'ad000000-0000-4000-8000-0000000000a1', 'from the future', '2099-01-01T00:00:00Z', NULL, 0, 0, 0, NULL),
  ('ad00a000-0000-4000-8000-000000000005', 'ad000000-0000-4000-8000-0000000000b2', 'old and quiet, other tenant', '2024-01-01T00:00:00Z', NULL, 0, 0, 0, NULL),
  ('ad00a000-0000-4000-8000-000000000006', 'ad000000-0000-4000-8000-0000000000e5', 'last week''s correlation', NOW() - interval '1 hour', '{correlation,weekly}', 0, 0, 0, NULL);

INSERT INTO health_events (id, tenant_id, event_type, value_numeric, unit, recorded_at, source, created_at) VALUES
  ('ad00e000-0000-4000-8000-000000000001', 'ad000000-0000-4000-8000-0000000000d4', 'steps', 4200, 'count', '2025-06-01T08:00:00Z', 'seed', '2025-06-01T08:00:00Z'),
  ('ad00e000-0000-4000-8000-000000000002', 'ad000000-0000-4000-8000-0000000000d4', 'steps', 5100, 'count', '2025-07-01T08:00:00Z', 'seed', '2025-07-01T08:00:00Z');

-- Snapshot views: only these fixtures' rows, scores rounded (ln and exp in float8
-- agree to far more places than 9), archived_at left out (NOW()).
CREATE VIEW parity_admin_memories AS
SELECT id, tenant_id, content, tags, round(score::numeric, 9) AS score, access_count, query_hit_count, created_at
FROM memories
WHERE tenant_id::text LIKE 'ad000000-%';

CREATE VIEW parity_admin_memories_archive AS
SELECT id, tenant_id, content, tags, round(score::numeric, 9) AS score, access_count, query_hit_count, last_accessed_at, created_at
FROM memories_archive
WHERE tenant_id::text LIKE 'ad000000-%';
