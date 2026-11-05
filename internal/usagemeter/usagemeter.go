// Package usagemeter is the one insert wave 4 needs from Swift's
// UsageMeterService (Sources/App/Billing/UsageMeterService.swift): the
// usage_meter row VisionEmbedService writes per embed. The rest of the service
// (budgets, rollups) stays with billing and LLM.
package usagemeter

import (
	"context"
	"log/slog"
	"lumina/internal/ids"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const insertTimeout = 10 * time.Second

const upsertUsage = `INSERT INTO usage_meter (tenant_id, day, model, mtok_in, mtok_out)
VALUES ($1, CURRENT_DATE, $2, $3, $4)
ON CONFLICT (tenant_id, day, model) DO UPDATE
    SET mtok_in  = usage_meter.mtok_in  + EXCLUDED.mtok_in,
        mtok_out = usage_meter.mtok_out + EXCLUDED.mtok_out`

// Meter writes usage_meter rows. A nil *Meter does nothing.
type Meter struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Meter {
	return &Meter{pool: pool}
}

// Record adds to today's row for (tenant, model). It does nothing when both
// counts are zero or negative, and logs failures rather than returning them:
// metering must never break the hot path.
func (m *Meter) Record(ctx context.Context, tenant uuid.UUID, model string, tokensIn, tokensOut int64) {
	if m == nil || (tokensIn <= 0 && tokensOut <= 0) {
		return
	}
	if _, err := m.pool.Exec(ctx, upsertUsage, tenant, model, tokensIn, tokensOut); err != nil {
		slog.ErrorContext(ctx, "usage_meter record failed", "tenant_id", ids.Upper(tenant), "error", err)
	}
}

// VisionEmbedModel is the usage_meter.model value for an image embed under the
// active provider kind.
func VisionEmbedModel(kind string) string {
	return "vision.embed:" + kind
}

// RecordVisionEmbed is VisionEmbedService's fire-and-forget usage row: one
// "call" as a constant token equivalent (tokensIn 1), on its own goroutine, so
// the embed response never waits on it. done, if set, runs when the write ends.
func (m *Meter) RecordVisionEmbed(tenant uuid.UUID, kind string, done func()) {
	if m == nil {
		if done != nil {
			done()
		}
		return
	}
	go func() {
		if done != nil {
			defer done()
		}
		ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
		defer cancel()
		m.Record(ctx, tenant, VisionEmbedModel(kind), 1, 0)
	}()
}
