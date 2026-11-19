-- go-only: no Swift migration (backlog E8.3, saved views). Spec:
-- docs/superpowers/specs/2026-11-16-e8-properties-and-views.md, D6.
--
-- A saved view is a stored POST /v1/vault/query (scope, filter, sort, group)
-- plus its column order and widths. Used only while VAULT_PROPERTIES_ENABLED
-- is on.
-- +goose Up
CREATE TABLE IF NOT EXISTS vault_views (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL REFERENCES vaults(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    scope JSONB NOT NULL,
    columns JSONB NOT NULL DEFAULT '[]',
    filter JSONB NOT NULL DEFAULT '[]',
    sort JSONB NOT NULL DEFAULT '[]',
    group_by TEXT,
    created_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS vault_views_tenant ON vault_views (tenant_id, lower(name));

-- +goose Down
DROP TABLE IF EXISTS vault_views;
