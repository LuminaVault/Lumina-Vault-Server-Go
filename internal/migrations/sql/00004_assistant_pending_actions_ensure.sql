-- go-only: repeats 00002 for databases goose adopted from Fluent.
-- Adoption marks every embedded version applied without running its DDL, so a
-- database adopted after 00002 was written has 00002 recorded and no table.
-- The statements are the ones in 00002; on a database that ran 00002 they do
-- nothing.
-- +goose Up
CREATE TABLE IF NOT EXISTS assistant_pending_actions (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    vault_id UUID NOT NULL REFERENCES vaults(id) ON DELETE CASCADE,
    conversation_id UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    tool TEXT NOT NULL,
    args JSONB NOT NULL,
    summary TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'confirmed', 'cancelled', 'failed')),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS assistant_pending_actions_tenant_status_idx
ON assistant_pending_actions (tenant_id, status);

-- +goose Down
-- 00002 owns the table; undoing this file leaves it in place.
SELECT 1;
