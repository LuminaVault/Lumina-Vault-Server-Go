-- /v1/system/hermes fixtures (parity/scenarios/system). hermes_update_jobs is
-- global, not per user: sy_amy only carries the bearer the routes need before
-- the admin token. One succeeded job (lastUpdatedAt), one failed job with a
-- recorded previous version (manual rollback), one failed with none.
INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at)
VALUES
  ('5a570000-0000-4000-8000-0000000000a1', 'sy_amy@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'sy_amy', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z');

INSERT INTO hermes_update_jobs (id, state, steps_json, from_version, to_version, error_message, created_at, updated_at)
VALUES
  ('5a57a000-0000-4000-8000-000000000001', 'succeeded',
   '[{"id":"preflight","state":"succeeded","detail":"luminavault-hermes","startedAt":"2026-02-01T10:00:00Z","finishedAt":"2026-02-01T10:00:01Z"},{"id":"promote","state":"succeeded","detail":"ghcr.io\/luminavault\/luminavault-hermes:v1"}]',
   'ghcr.io/luminavault/luminavault-hermes:v0', 'ghcr.io/luminavault/luminavault-hermes:v1', NULL,
   '2026-02-01T10:00:00Z', '2026-02-01T10:05:00Z'),
  ('5a57a000-0000-4000-8000-000000000002', 'failed',
   '[{"id":"preflight","state":"succeeded","startedAt":"2026-02-02T10:00:00Z","finishedAt":"2026-02-02T10:00:01Z"},{"id":"pullImage","state":"failed","detail":"Couldn''t download the new version.","startedAt":"2026-02-02T10:00:01Z","finishedAt":"2026-02-02T10:01:00Z"},{"id":"verifyImage","state":"pending"}]',
   'ghcr.io/luminavault/luminavault-hermes:v1', 'ghcr.io/luminavault/luminavault-hermes:v2', 'Couldn''t download the new version.',
   '2026-02-02T10:00:00Z', '2026-02-02T10:01:00Z'),
  ('5a57a000-0000-4000-8000-000000000003', 'failed', 'not json', NULL, 'ghcr.io/luminavault/luminavault-hermes:v3', NULL,
   '2026-02-03T10:00:00Z', '2026-02-03T10:00:00Z');

-- What the update engine still owes: the scenario waits for it to finish.
CREATE VIEW parity_sy_running AS SELECT id FROM hermes_update_jobs WHERE state = 'running';
-- Every job, without the random ids and clock times of the ones the scenario starts.
CREATE VIEW parity_sy_jobs AS
  SELECT state, from_version, to_version,
         CASE WHEN created_at < '2026-03-01' THEN id::text END AS seeded_id,
         CASE WHEN created_at < '2026-03-01' THEN steps_json END AS seeded_steps,
         CASE WHEN created_at < '2026-03-01' THEN error_message END AS seeded_error
  FROM hermes_update_jobs ORDER BY to_version, state;
