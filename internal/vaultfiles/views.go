package vaultfiles

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MaxViews is how many saved views one vault keeps (E8.3).
const MaxViews = 200

// MaxViewColumns is how many columns one saved view keeps.
const MaxViewColumns = 100

// EnsureFolderView returns the saved view called name over folder, creating
// it with these columns when the vault has none: the Notion import's one view
// per database, which a re-import must not duplicate. The JSON is what
// /v1/vault/views stores: scope {"folder"}, columns [{"property"}], no
// filter, sort or group. created is false for a view that was already there.
// A vault at MaxViews gets uuid.Nil and no error.
func (s *Store) EnsureFolderView(ctx context.Context, tenant, actor uuid.UUID, name, folder string, columns []string) (id uuid.UUID, created bool, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT id FROM vault_views
		WHERE tenant_id = $1 AND name = $2 AND scope->>'folder' = $3
		ORDER BY created_at, id LIMIT 1`, tenant, name, folder).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, err
	}
	type column struct {
		Property string `json:"property"`
	}
	cols := make([]column, 0, len(columns))
	for _, c := range columns {
		cols = append(cols, column{Property: c})
	}
	scope, err := json.Marshal(map[string]string{"folder": folder})
	if err != nil {
		return uuid.Nil, false, err
	}
	encoded, err := json.Marshal(cols)
	if err != nil {
		return uuid.Nil, false, err
	}
	// The count check and the insert are one statement, as in the views route.
	err = s.pool.QueryRow(ctx, `
		INSERT INTO vault_views (tenant_id, name, scope, columns, created_by)
		SELECT $1, $2, $3, $4, $5
		WHERE (SELECT count(*) FROM vault_views WHERE tenant_id = $1) < $6
		RETURNING id`, tenant, name, scope, encoded, actor, MaxViews).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, true, nil
}
