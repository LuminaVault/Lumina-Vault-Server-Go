-- go-only: GitHub sync (spec 2026-11-19-e4-github-sync D11). The owner's
-- answer to "this branch already has files: merge them into the vault?" is
-- kept with the connection, so the first sync (which runs after the request
-- that asked) knows whether a merge with an unrelated history was agreed to.
-- +goose Up
ALTER TABLE github_sync_connections ADD COLUMN IF NOT EXISTS merge_existing BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE github_sync_connections DROP COLUMN IF EXISTS merge_existing;
