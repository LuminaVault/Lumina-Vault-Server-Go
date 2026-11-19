-- go-only: no Swift migration (owner decision 2026-11-18 on the refresh reuse scope left open
-- by #162). A refresh token belongs to the login chain it was rotated from: sign-in starts a
-- family, each rotation carries it. Reuse of a rotated token revokes that family only, not
-- every session of the tenant. Rows without a family (issued before this column, or by the
-- Swift rollback image, which never writes it) keep the tenant-wide revocation.
-- Expand-only: a nullable column and an index the Swift image never reads.
-- +goose Up
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS family_id UUID;
CREATE INDEX IF NOT EXISTS refresh_tokens_family_live_idx ON refresh_tokens (family_id) WHERE revoked_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS refresh_tokens_family_live_idx;
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS family_id;
