-- go-only: GitHub sync (spec 2026-11-19-e4-github-sync D6), two facts about
-- attempts the row did not hold.
--
-- confirm_count: an answer GitHub documents ambiguously (a 422 or 403 minting
-- a token, a 403 on a push or a fetch) stops retries only when it is the
-- answer several attempts running. That run is counted here, apart from
-- retry_count, which counts every failure of any kind and drives the backoff.
--
-- reserved_at: an attempt writes its retry time before it pushes or fetches,
-- so that a process killed in the middle waits before it tries again. This
-- says next_retry_at is such a reservation and not the wait after a failure.
-- +goose Up
ALTER TABLE github_sync_connections ADD COLUMN IF NOT EXISTS confirm_count INT NOT NULL DEFAULT 0;
ALTER TABLE github_sync_connections ADD COLUMN IF NOT EXISTS reserved_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE github_sync_connections DROP COLUMN IF EXISTS reserved_at;
ALTER TABLE github_sync_connections DROP COLUMN IF EXISTS confirm_count;
