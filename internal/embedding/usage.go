package embedding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/embedding/db"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// usageTracker is EmbeddingUsageTracker: refuse when this month's usage has
// reached the cap (a cap of 0 disables the check), embed, then add an estimate to
// embedding_usage. A failed read counts as 0 used; a failed write is logged, never
// returned. Providers' real token counts are not used: the registry wires no
// usage callback.
type usageTracker struct {
	inner Service
	q     *db.Queries
	cap   int64
	now   func() time.Time
}

func (u *usageTracker) Embed(ctx context.Context, text string, tenant uuid.UUID) ([]float32, error) {
	if u.cap > 0 {
		used := int64(0)
		if row, err := u.q.EmbeddingUsageFor(ctx, db.EmbeddingUsageForParams{TenantID: tenant, YearMonth: yearMonth(u.now())}); err == nil {
			used = row.TokensUsed
		}
		if used >= u.cap {
			slog.WarnContext(ctx, fmt.Sprintf("embedding cap exceeded: tenant=%s used=%d cap=%d", tenant, used, u.cap))
			return nil, &Error{Kind: FailureCapExceeded, Tenant: tenant, Used: used, Cap: u.cap}
		}
	}
	vec, err := u.inner.Embed(ctx, text, tenant)
	if err != nil {
		return nil, err
	}
	if err := u.increment(ctx, tenant, EstimateTokens(text)); err != nil {
		slog.WarnContext(ctx, fmt.Sprintf("embedding usage increment failed: %v", err))
	}
	return vec, nil
}

// increment is EmbeddingUsageRepository.increment: update the month's row, or
// insert it, retrying once if a concurrent insert won. It is a read then a write,
// not an atomic add, so concurrent embeds can lose an update exactly as in Swift.
func (u *usageTracker) increment(ctx context.Context, tenant uuid.UUID, tokens int64) error {
	if tokens <= 0 {
		return nil
	}
	now := u.now()
	ym := yearMonth(now)
	for attempt := range 2 {
		row, err := u.q.EmbeddingUsageFor(ctx, db.EmbeddingUsageForParams{TenantID: tenant, YearMonth: ym})
		switch {
		case err == nil:
			return u.q.SetEmbeddingUsage(ctx, db.SetEmbeddingUsageParams{ID: row.ID, TokensUsed: row.TokensUsed + tokens, UpdatedAt: &now})
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		err = u.q.InsertEmbeddingUsage(ctx, db.InsertEmbeddingUsageParams{ID: uuid.New(), TenantID: tenant, YearMonth: ym, TokensUsed: tokens, CreatedAt: &now})
		if err == nil || attempt == 1 {
			return err
		}
	}
	return nil
}

// yearMonth is EmbeddingUsage.yearMonth: "YYYY-MM" in UTC.
func yearMonth(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%04d-%02d", t.Year(), int(t.Month()))
}
