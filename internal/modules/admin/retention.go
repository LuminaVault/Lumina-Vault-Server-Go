package admin

import (
	"context"
	"log/slog"
	"lumina/internal/wire"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// DefaultRetentionBatchSize is UsageEventsRetentionPolicy.defaultBatchSize.
	DefaultRetentionBatchSize = 5000
	// MaxRetentionBatchSize is UsageEventsRetentionPolicy.maxBatchSize.
	MaxRetentionBatchSize = 10000
	// maxRetentionBatches is UsageEventsRetentionService.maxBatches.
	maxRetentionBatches = 20
)

// RetentionPolicy is UsageEventsRetentionPolicy.
type RetentionPolicy struct {
	RetentionDays int
	BatchSize     int
}

// NewRetentionPolicy clamps the batch size to 1...MaxRetentionBatchSize.
func NewRetentionPolicy(days, batchSize int) RetentionPolicy {
	return RetentionPolicy{RetentionDays: days, BatchSize: min(max(1, batchSize), MaxRetentionBatchSize)}
}

// Enabled is isEnabled: a positive window.
func (p RetentionPolicy) Enabled() bool { return p.RetentionDays > 0 }

// Cutoff is cutoff(now:): nil when disabled.
func (p RetentionPolicy) Cutoff(now time.Time) *time.Time {
	if !p.Enabled() {
		return nil
	}
	c := now.Add(-time.Duration(p.RetentionDays) * 24 * time.Hour)
	return &c
}

// RetentionSummary is UsageEventsRetentionService.Summary; cutoff is left out
// when nil.
type RetentionSummary struct {
	Enabled       bool       `json:"enabled"`
	RetentionDays int        `json:"retentionDays"`
	Cutoff        *wire.Time `json:"cutoff,omitempty"`
	Deleted       int        `json:"deleted"`
	MoreRemaining bool       `json:"moreRemaining"`
}

// Retention is UsageEventsRetentionService.
type Retention struct {
	Pool   *pgxpool.Pool
	Policy RetentionPolicy
}

// Sweep is sweep(now:): bounded batches by primary key, oldest first, at most
// maxRetentionBatches of them, so no single statement holds a long lock on the table
// the transcription path writes to.
func (r Retention) Sweep(ctx context.Context, now time.Time) (RetentionSummary, error) {
	cutoff := r.Policy.Cutoff(now)
	s := RetentionSummary{Enabled: cutoff != nil, RetentionDays: r.Policy.RetentionDays}
	if cutoff == nil {
		return s, nil
	}
	s.Cutoff = &wire.Time{Time: *cutoff}
	for batch := 1; batch <= maxRetentionBatches; batch++ {
		tag, err := r.Pool.Exec(ctx, `
			DELETE FROM usage_events
			WHERE id IN (
			    SELECT id FROM usage_events
			    WHERE occurred_at < $1
			    ORDER BY occurred_at
			    LIMIT $2
			)`, *cutoff, r.Policy.BatchSize)
		if err != nil {
			return RetentionSummary{}, err
		}
		n := int(tag.RowsAffected())
		s.Deleted += n
		if n < r.Policy.BatchSize {
			break
		}
		if batch == maxRetentionBatches {
			s.MoreRemaining = true
		}
	}
	if s.Deleted > 0 || s.MoreRemaining {
		slog.InfoContext(ctx, "usage_events retention sweep", "deleted", s.Deleted,
			"cutoff", cutoff.UTC().Format(time.RFC3339), "more_remaining", s.MoreRemaining)
	}
	return s, nil
}
