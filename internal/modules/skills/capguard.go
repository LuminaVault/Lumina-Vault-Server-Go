package skills

import (
	"context"
	"errors"
	"time"

	"lumina/internal/skills"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// capGuard is SkillRunCapGuard: per-skill daily run caps from the manifest's
// metadata.daily_run_cap, counted on skills_state and reset at the next local
// midnight in the server's zone (Swift's TimeZone.current).
type capGuard struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// capExceededError is SkillRunCapExceededError. Swift's controller never maps it
// (capExceededHTTPError has no caller), so it reaches the client as a 500.
type capExceededError struct{ retryAfter time.Duration }

func (e *capExceededError) Error() string { return "SkillRunCapExceededError" }

// checkAndIncrement allows the run and counts it, or denies it with the time
// left until the reset.
func (g capGuard) checkAndIncrement(ctx context.Context, tenant uuid.UUID, tier string, mf skills.Manifest) error {
	limit := 0
	if mf.DailyRunCap != nil {
		limit = mf.DailyRunCap.Value(tier)
	}
	if limit <= 0 {
		return nil
	}
	now := g.now()
	nextReset := nextLocalMidnight(now)
	var count int
	var resetAt *time.Time
	err := g.pool.QueryRow(ctx, `SELECT daily_run_count, daily_run_reset_at FROM skills_state
		WHERE tenant_id = $1 AND source = $2 AND name = $3`, tenant, string(mf.Source), mf.Name).Scan(&count, &resetAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if resetAt == nil || !resetAt.After(now) {
		count = 0
		resetAt = &nextReset
	}
	if count >= limit {
		return &capExceededError{retryAfter: max(time.Second, resetAt.Sub(now))}
	}
	_, err = g.pool.Exec(ctx, `
		INSERT INTO skills_state (tenant_id, source, name, enabled, daily_run_count, daily_run_reset_at)
		VALUES ($1, $2, $3, TRUE, $4, $5)
		ON CONFLICT (tenant_id, source, name) DO UPDATE
		    SET daily_run_count = EXCLUDED.daily_run_count,
		        daily_run_reset_at = EXCLUDED.daily_run_reset_at`,
		tenant, string(mf.Source), mf.Name, count+1, *resetAt)
	return err
}

// recordFailure refunds the slot a failed run took.
func (g capGuard) recordFailure(ctx context.Context, tenant uuid.UUID, mf skills.Manifest) error {
	_, err := g.pool.Exec(ctx, `UPDATE skills_state SET daily_run_count = GREATEST(daily_run_count - 1, 0)
		WHERE tenant_id = $1 AND source = $2 AND name = $3`, tenant, string(mf.Source), mf.Name)
	return err
}

func nextLocalMidnight(t time.Time) time.Time {
	tomorrow := t.AddDate(0, 0, 1)
	return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, t.Location())
}
