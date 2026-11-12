package apple

import (
	"context"
	"lumina/internal/modules/apple/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConsentReader is AppleConsentController.isAllowed for callers outside this
// module, such as the Quick Assistant's personal tools.
type ConsentReader struct{ q *db.Queries }

func NewConsentReader(pool *pgxpool.Pool) *ConsentReader {
	return &ConsentReader{q: db.New(pool)}
}

// IsAllowed reports the tenant's consent for domain (an AppleDataDomain raw
// value). It fails closed: no row or any error is (false, false).
func (c *ConsentReader) IsAllowed(ctx context.Context, tenant uuid.UUID, domain string) (allowed, writes bool) {
	row, err := c.q.ConsentFor(ctx, db.ConsentForParams{TenantID: tenant, Domain: domain})
	if err != nil {
		return false, false
	}
	return row.Allowed, row.AllowWrites
}
