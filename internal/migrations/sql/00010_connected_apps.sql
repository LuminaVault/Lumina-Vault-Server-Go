-- go-only: connected apps (spec 2026-11-18)
-- +goose Up
CREATE TABLE IF NOT EXISTS connected_app_accounts (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    app_id TEXT NOT NULL,
    access_ciphertext BYTEA,
    access_nonce BYTEA,
    refresh_ciphertext BYTEA,
    refresh_nonce BYTEA,
    access_expires_at TIMESTAMPTZ,
    scope TEXT,
    status TEXT NOT NULL DEFAULT 'connected',
    last_failure_code TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, app_id)
);

CREATE TABLE IF NOT EXISTS connected_app_clients (
    app_id TEXT PRIMARY KEY,
    client_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE agent_connections
    ADD COLUMN IF NOT EXISTS connected_app_grants TEXT[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE agent_connections DROP COLUMN IF EXISTS connected_app_grants;
DROP TABLE IF EXISTS connected_app_clients;
DROP TABLE IF EXISTS connected_app_accounts;
