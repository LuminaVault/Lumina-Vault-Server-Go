-- go-only: no Swift migration creates this table (backlog E3.1, trash with restore).
-- One row per file a delete moved into the tenant's raw root as `_deleted_*`.
-- It keeps what the vault_files row held, so a restore can rebuild it.
-- +goose Up
CREATE TABLE IF NOT EXISTS vault_trash (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES vaults(id) ON DELETE CASCADE,
    vault_file_id UUID NOT NULL,
    space_id UUID REFERENCES spaces(id) ON DELETE SET NULL,
    original_path TEXT NOT NULL,
    trash_name TEXT NOT NULL,
    content_type TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    sha256 TEXT NOT NULL,
    metadata JSONB,
    created_by_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    deleted_by_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    deleted_at TIMESTAMPTZ NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS vault_trash_tenant_trash_name_uq
ON vault_trash (tenant_id, trash_name);

CREATE INDEX IF NOT EXISTS vault_trash_tenant_deleted_at_idx
ON vault_trash (tenant_id, deleted_at DESC);

-- +goose Down
DROP TABLE vault_trash;
