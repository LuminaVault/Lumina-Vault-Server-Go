-- go-only: no Swift migration adds this column (post-archive backlog, auth weaknesses kept
-- by #85: a password reset did not invalidate access tokens). Session tokens issued before
-- sessions_valid_after are refused. Expand-only: a nullable column the Swift rollback image
-- never reads.
-- +goose Up
ALTER TABLE users ADD COLUMN IF NOT EXISTS sessions_valid_after TIMESTAMPTZ;

-- +goose Down
ALTER TABLE users DROP COLUMN IF EXISTS sessions_valid_after;
