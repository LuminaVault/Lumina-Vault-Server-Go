-- go-only: no Swift migration (backlog E8.2 part 3b, the properties backfill).
--
-- properties_indexed_at is when a Markdown note's frontmatter was last read
-- into properties. NULL means never: the VaultPropertiesBackfill job indexes
-- those rows (notes written before VAULT_PROPERTIES_ENABLED was turned on).
-- +goose Up
ALTER TABLE vault_files ADD COLUMN IF NOT EXISTS properties_indexed_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS vault_files_properties_unindexed
ON vault_files (id) WHERE properties_indexed_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS vault_files_properties_unindexed;
ALTER TABLE vault_files DROP COLUMN IF EXISTS properties_indexed_at;
