-- Onboarding (GET/PATCH /v1/onboarding). Two users with no onboarding_state
-- row: the scenarios create them (GET lazily inserts, PATCH too). Ids match
-- parity/seed/users.yaml.
INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at)
VALUES
  ('0b0a0000-0000-4000-8000-0000000000a1', 'onb-ann@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'onb-ann', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('0b0a0000-0000-4000-8000-0000000000b2', 'onb-bob@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'onb-bob', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
