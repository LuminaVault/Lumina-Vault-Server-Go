-- go-only: GitHub sync (spec 2026-11-19-e4-github-sync D2). No tokens are
-- stored: installation tokens are minted on demand and kept in memory.
-- +goose Up
CREATE TABLE IF NOT EXISTS github_sync_connections (
    vault_id               UUID PRIMARY KEY REFERENCES vaults(id) ON DELETE CASCADE,
    installation_id        BIGINT NOT NULL,
    account_login          TEXT NOT NULL,
    account_type           TEXT NOT NULL,
    repo_id                BIGINT NOT NULL UNIQUE,
    repo_full_name         TEXT NOT NULL,
    repo_private           BOOLEAN NOT NULL,
    branch                 TEXT NOT NULL,
    state                  TEXT NOT NULL,
    error_code             TEXT,
    error_at               TIMESTAMPTZ,
    retry_count            INT NOT NULL DEFAULT 0,
    next_retry_at          TIMESTAMPTZ,
    remote_sha             TEXT,
    last_push_at           TIMESTAMPTZ,
    last_pull_at           TIMESTAMPTZ,
    report                 JSONB NOT NULL DEFAULT '{}',
    connected_by_user_id   UUID REFERENCES users(id) ON DELETE SET NULL,
    connected_github_login TEXT NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS github_sync_connections_installation ON github_sync_connections (installation_id);

CREATE TABLE IF NOT EXISTS github_webhook_deliveries (
    delivery_id     TEXT PRIMARY KEY,
    event           TEXT NOT NULL,
    action          TEXT,
    installation_id BIGINT,
    repo_id         BIGINT,
    ref             TEXT,
    after_sha       TEXT,
    status          TEXT NOT NULL DEFAULT 'queued',
    received_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at    TIMESTAMPTZ
);

-- +goose Down
DROP TABLE IF EXISTS github_webhook_deliveries;
DROP TABLE IF EXISTS github_sync_connections;
