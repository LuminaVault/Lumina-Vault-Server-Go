-- go-only: no Swift migration (backlog E8.2, note properties). Spec:
-- docs/superpowers/specs/2026-11-16-e8-properties-and-views.md, D2 and D3.
--
-- vault_files.properties holds a Markdown note's frontmatter, normalized for
-- queries (strings, numbers, booleans, ISO dates, lists). It is filled only
-- while VAULT_PROPERTIES_ENABLED is on; the default '{}' keeps every existing
-- row valid. properties_error is the parser's message when the frontmatter is
-- invalid; the file itself is never rewritten.
-- +goose Up
ALTER TABLE vault_files ADD COLUMN IF NOT EXISTS properties JSONB NOT NULL DEFAULT '{}';
ALTER TABLE vault_files ADD COLUMN IF NOT EXISTS properties_error TEXT;

CREATE INDEX IF NOT EXISTS vault_files_properties_gin
ON vault_files USING GIN (properties jsonb_path_ops);

-- One declared type per property name per vault. Names compare without case,
-- as Obsidian's types.json does in practice.
CREATE TABLE IF NOT EXISTS vault_property_types (
    tenant_id UUID NOT NULL REFERENCES vaults(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    type TEXT NOT NULL CHECK (type IN
        ('text', 'number', 'select', 'multiselect', 'date', 'datetime', 'checkbox', 'url')),
    options JSONB NOT NULL DEFAULT '[]',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS vault_property_types_tenant_name_uq
ON vault_property_types (tenant_id, lower(name));

-- +goose Down
DROP TABLE IF EXISTS vault_property_types;
DROP INDEX IF EXISTS vault_files_properties_gin;
ALTER TABLE vault_files DROP COLUMN IF EXISTS properties_error;
ALTER TABLE vault_files DROP COLUMN IF EXISTS properties;
