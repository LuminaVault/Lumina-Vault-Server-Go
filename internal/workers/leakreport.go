package workers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/jobs"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RetrievalLeakReportWorker is RetrievalLeakReportWorker
// (Sources/App/Telemetry/RetrievalLeakReportWorker.swift): an hourly tick that, when
// it lands in Sunday's 05:00 UTC hour, rolls each tenant's retrieval telemetry over
// the trailing 7 days into one retrieval_leak_reports row. The table's unique window
// index makes it safe in two processes: the loser's insert fails and is dropped.
type RetrievalLeakReportWorker struct {
	Pool *pgxpool.Pool
	// SurfaceInsight is retrieval.leakReport.surfaceInsight: also write a patterns
	// Insight per report.
	SurfaceInsight bool
	Logger         *slog.Logger
}

// LeakPeriod is RetrievalLeakReportWorker.Period, [Start, End).
type LeakPeriod struct {
	Start, End time.Time
}

// Job ticks at boot and then every hour. A tick outside Sunday 05 UTC is a skipped
// run, so the gate metric's last success is the last weekly roll-up.
func (w *RetrievalLeakReportWorker) Job() jobs.Job {
	return jobs.Job{
		Name:     "RetrievalLeakReportWorker",
		Schedule: jobs.Every(time.Hour, 0),
		Run: func(ctx context.Context) error {
			_, err := w.Tick(ctx, time.Now())
			return err
		},
	}
}

func (w *RetrievalLeakReportWorker) logger() *slog.Logger {
	if w.Logger == nil {
		return slog.Default()
	}
	return w.Logger
}

// Tick is RetrievalLeakReportWorker.tick(at:). It returns how many reports it
// inserted, and errNotDue outside Sunday's 05:00 UTC hour.
func (w *RetrievalLeakReportWorker) Tick(ctx context.Context, now time.Time) (int, error) {
	utc := now.UTC()
	if utc.Hour() != 5 || utc.Weekday() != time.Sunday {
		return 0, errNotDue
	}
	return w.RunForAllTenants(ctx, now)
}

// RunForAllTenants is runForAllTenants: one report per user, a tenant's failure
// logged and skipped.
func (w *RetrievalLeakReportWorker) RunForAllTenants(ctx context.Context, now time.Time) (int, error) {
	window := WeeklyWindow(now)
	tenants, err := allUserIDs(ctx, w.Pool)
	if err != nil {
		return 0, err
	}
	inserted := 0
	for _, tenant := range tenants {
		ok, err := w.RunLeakJob(ctx, tenant, window)
		if err != nil {
			if ctx.Err() != nil {
				return inserted, ctx.Err()
			}
			w.logger().Warn(fmt.Sprintf("retrieval.leak_report tenant=%s error: %v", strings.ToUpper(tenant.String()), err))
			continue
		}
		if ok {
			inserted++
		}
	}
	return inserted, nil
}

// RunLeakJob is runLeakJob: true when a row was inserted, false when this window is
// already reported, the tenant has no telemetry in it, or another process won.
func (w *RetrievalLeakReportWorker) RunLeakJob(ctx context.Context, tenant uuid.UUID, window LeakPeriod) (bool, error) {
	var exists bool
	if err := w.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM retrieval_leak_reports
		WHERE tenant_id = $1 AND period_start = $2 AND period_end = $3)`,
		tenant, window.Start, window.End).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	var total, zeroHits int
	var meanTop *float64
	if err := w.Pool.QueryRow(ctx, `
		SELECT
			COUNT(*) AS total,
			COUNT(*) FILTER (WHERE zero_hit) AS zero_hits,
			AVG(top_distance) FILTER (WHERE NOT zero_hit) AS mean_top
		FROM retrieval_telemetry_events
		WHERE tenant_id = $1
		  AND created_at >= $2
		  AND created_at < $3`,
		tenant, window.Start, window.End).Scan(&total, &zeroHits, &meanTop); err != nil {
		return false, err
	}
	if total <= 0 {
		return false, nil
	}

	// Worst source = highest zero-hit rate among sources with ≥1 retrieval.
	var worst *string
	var source string
	var n, zeros int
	err := w.Pool.QueryRow(ctx, `
		SELECT source_path,
		       COUNT(*) AS n,
		       COUNT(*) FILTER (WHERE zero_hit) AS zeros
		FROM retrieval_telemetry_events
		WHERE tenant_id = $1
		  AND created_at >= $2
		  AND created_at < $3
		GROUP BY source_path
		ORDER BY (COUNT(*) FILTER (WHERE zero_hit))::float / COUNT(*) DESC, n DESC
		LIMIT 1`, tenant, window.Start, window.End).Scan(&source, &n, &zeros)
	switch {
	case err == nil:
		worst = &source
	case !errors.Is(err, pgx.ErrNoRows):
		return false, err
	}

	zeroRate := float64(zeroHits) / float64(total)
	if _, err := w.Pool.Exec(ctx, `
		INSERT INTO retrieval_leak_reports
			(id, tenant_id, period_start, period_end, total_retrievals, zero_hit_count, zero_hit_rate,
			 mean_top_distance, worst_source, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		uuid.New(), tenant, window.Start, window.End, total, zeroHits, zeroRate, meanTop, worst, time.Now()); err != nil {
		// Lost the idempotency race (concurrent run / restart): the unique window
		// index rejected the duplicate. Treat as already done.
		w.logger().Debug(fmt.Sprintf("retrieval.leak_report duplicate tenant=%s: %v", strings.ToUpper(tenant.String()), err))
		return false, nil
	}

	meanText, worstText := "n/a", "n/a"
	if meanTop != nil {
		meanText = fmt.Sprintf("%.3f", *meanTop)
	}
	if worst != nil {
		worstText = *worst
	}
	w.logger().Info(fmt.Sprintf("retrieval.leak_report tenant=%s total=%d zeroHitRate=%.3f meanTopDistance=%s worst=%s",
		strings.ToUpper(tenant.String()), total, zeroRate, meanText, worstText))

	if w.SurfaceInsight {
		// try? in Swift: a failed insight never fails the report.
		_, _ = w.Pool.Exec(ctx, `
			INSERT INTO insights (id, tenant_id, section, headline, summary, source_memory_ids, period_start, period_end, created_at)
			VALUES ($1, $2, 'patterns', 'Retrieval health this week', $3, '{}', $4, $5, $6)`,
			uuid.New(), tenant, LeakInsightSummary(total, zeroRate, meanTop, worst), window.Start, window.End, time.Now())
	}
	return true, nil
}

// LeakInsightSummary is RetrievalLeakReportWorker.insightSummary.
func LeakInsightSummary(total int, zeroRate float64, meanTop *float64, worst *string) string {
	pct := int(math.Round(zeroRate * 100))
	s := fmt.Sprintf("%d retrievals; %d%% found nothing", total, pct)
	if meanTop != nil {
		s += fmt.Sprintf("; avg best match %.2f", *meanTop)
	}
	if worst != nil && zeroRate > 0 {
		s += "; weakest path: " + *worst
	}
	return s + "."
}

// WeeklyWindow is RetrievalLeakReportWorker.weeklyWindow: the 7 UTC days before
// the start of now's UTC day.
func WeeklyWindow(now time.Time) LeakPeriod {
	utc := now.UTC()
	end := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	return LeakPeriod{Start: end.AddDate(0, 0, -7), End: end}
}
