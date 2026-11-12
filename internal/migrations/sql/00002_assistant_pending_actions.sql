-- go-only: no Swift migration creates this table (go-plan owner question 1)
-- DDL is M151_CreateAssistantPendingActions, verbatim.
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
DROP TABLE assistant_pending_actions;
