package scenario

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGSnapshotter reads whole tables as JSON rows, ordered by their text form
// so both databases list rows identically regardless of insertion order.
func PGSnapshotter(pool *pgxpool.Pool) Snapshotter {
	return func(ctx context.Context, table string) ([]json.RawMessage, error) {
		if !tableRe.MatchString(table) {
			return nil, fmt.Errorf("bad table %q", table)
		}
		// table is validated against tableRe above; identifiers cannot be bound.
		rows, err := pool.Query(ctx, fmt.Sprintf(`SELECT row_to_json(t)::text FROM %q t ORDER BY 1`, table))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []json.RawMessage
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return nil, err
			}
			out = append(out, json.RawMessage(s))
		}
		return out, rows.Err()
	}
}
