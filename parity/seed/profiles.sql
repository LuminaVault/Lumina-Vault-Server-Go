-- Profiles (Swift: Sources/App/Profiles/ProfilesController.swift). Loaded
-- after base.sql. alice starts with three personas so list order, activeSlug
-- and the NULL-timestamp fallback (`createdAt ?? Date()`) are exercised; bob
-- starts with none so "the first profile becomes default" runs on his tenant.
INSERT INTO user_hermes_profiles (id, tenant_id, slug, label, system_prompt, is_default, skills_enabled, created_at, updated_at)
VALUES
  ('9f000000-0000-4000-8000-0000000000a1', 'a11ce000-0000-4000-8000-000000000001', 'assistant', 'Assistant', 'You are the seeded assistant.', true, '{kb-ask,kb-compile}', '2026-01-05T00:00:00Z', '2026-01-05T10:00:00.75Z'),
  ('9f000000-0000-4000-8000-0000000000a2', 'a11ce000-0000-4000-8000-000000000001', 'research', 'Research', '', false, '{}', '2026-01-06T00:00:00Z', '2026-01-06T00:00:00Z'),
  -- Rows written before timestamps were set: Swift falls back to Date().
  ('9f000000-0000-4000-8000-0000000000a3', 'a11ce000-0000-4000-8000-000000000001', 'legacy', '  Legacy  ', 'old', false, '{kb-ask}', NULL, NULL);
