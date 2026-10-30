-- Achievements fixture (parity/scenarios/achievements). Rows shaped as
-- Swift's AchievementsService writes them (one row per tenant + key).
-- Unlock times are distinct so ORDER BY unlocked_at DESC is total.
INSERT INTO achievement_progress (id, tenant_id, achievement_key, progress_count, unlocked_at, created_at, updated_at)
VALUES
  -- alice: three catalog unlocks, one in-progress sub, and two keys that
  -- are no longer in the catalog (one unlocked: /recent falls back to the key
  -- as its label; neither shows in the catalog list).
  ('ac000000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001', 'lightbringer.first-spark',  3, '2026-02-01T10:00:00Z',     '2026-02-01T10:00:00Z', '2026-02-01T10:00:00Z'),
  ('ac000000-0000-4000-8000-000000000002', 'a11ce000-0000-4000-8000-000000000001', 'lightbringer.kindled-mind', 4, NULL,                       '2026-02-01T10:00:00Z', '2026-02-01T10:00:00Z'),
  ('ac000000-0000-4000-8000-000000000003', 'a11ce000-0000-4000-8000-000000000001', 'reignmaker.first-edict',    1, '2026-02-02T00:00:00Z',     '2026-02-02T00:00:00Z', '2026-02-02T00:00:00Z'),
  ('ac000000-0000-4000-8000-000000000004', 'a11ce000-0000-4000-8000-000000000001', 'shadowlord.shadow-touched', 1, '2026-02-03T08:30:00.750Z', '2026-02-03T08:30:00Z', '2026-02-03T08:30:00Z'),
  ('ac000000-0000-4000-8000-000000000005', 'a11ce000-0000-4000-8000-000000000001', 'legacy.retired-badge',      7, '2026-02-04T00:00:00Z',     '2026-02-04T00:00:00Z', '2026-02-04T00:00:00Z'),
  ('ac000000-0000-4000-8000-000000000006', 'a11ce000-0000-4000-8000-000000000001', 'legacy.never-unlocked',     2, NULL,                       '2026-02-04T00:00:00Z', '2026-02-04T00:00:00Z'),
  -- bob: one unlock, so isolation shows in both routes.
  ('ac000000-0000-4000-8000-000000000007', 'b0b00000-0000-4000-8000-000000000002', 'soulseeker.first-relic',    2, '2026-03-01T00:00:00Z',     '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z');
