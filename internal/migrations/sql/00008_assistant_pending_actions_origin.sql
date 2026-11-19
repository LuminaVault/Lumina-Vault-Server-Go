-- go-only: connected apps (spec 2026-11-18). A pending action proposed in a
-- turn that ran a connected-app tool carries that turn's origin, so the line
-- its confirm or cancel leaves in the conversation is flagged the same way.
-- +goose Up
ALTER TABLE assistant_pending_actions ADD COLUMN IF NOT EXISTS origin TEXT;

-- +goose Down
ALTER TABLE assistant_pending_actions DROP COLUMN IF EXISTS origin;
