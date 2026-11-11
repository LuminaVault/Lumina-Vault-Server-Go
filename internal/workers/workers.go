// Package workers holds the Go ports of Swift's background jobs that no wave owns
// (docs/migration/wave-6-8-plan.md, "J1, J2"). Each file is one Swift job and builds
// a jobs.Job named after the Swift type. A job runs only when LV_GO_JOBS names it,
// and the infra move rule decides when that happens, so no job here takes a lock
// that Swift's copy doesn't.
package workers

import (
	"context"
	"fmt"
	"lumina/internal/diskguard"
	"lumina/internal/jobs"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DiskSpaceMonitor is DiskSpaceMonitorService (Sources/App/Vault/DiskSpaceGuard.swift):
// sample the vault volume once a minute, starting at boot, so `storage.disk_low` is
// logged while nobody uploads. guard should be the writers' guard, so the log line's
// once-a-minute limit is shared with them as in Swift.
func DiskSpaceMonitor(guardrail *diskguard.Guard) jobs.Job {
	return jobs.Job{
		Name:     "DiskSpaceMonitorService",
		Schedule: jobs.Every(time.Minute, 0),
		Run: func(context.Context) error {
			guardrail.Sample()
			return nil
		},
	}
}

// AnalyticsMaintenance is AnalyticsMaintenanceService
// (Sources/App/Dashboard/AnalyticsMaintenanceService.swift): at boot and then daily,
// delete analytics and telemetry rows past retention. A failed DELETE stops the
// ones after it, as Swift's `try` does.
func AnalyticsMaintenance(pool *pgxpool.Pool) jobs.Job {
	return jobs.Job{
		Name:     "AnalyticsMaintenanceService",
		Schedule: jobs.Every(24*time.Hour, 0),
		Run: func(ctx context.Context) error {
			return RunAnalyticsMaintenance(ctx, pool)
		},
	}
}

// RunAnalyticsMaintenance is AnalyticsMaintenanceService.runOnce.
func RunAnalyticsMaintenance(ctx context.Context, pool *pgxpool.Pool) error {
	for _, stmt := range []string{
		`DELETE FROM analytics_events WHERE occurred_at < NOW() - interval '90 days'`,
		`DELETE FROM analytics_daily_rollups WHERE day < CURRENT_DATE - interval '13 months'`,
		// Retrieval telemetry is raw per-event; the weekly leak reports are the
		// durable roll-up, so events past 90 days can be dropped.
		`DELETE FROM retrieval_telemetry_events WHERE created_at < NOW() - interval '90 days'`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("analytics maintenance failed: %w", err)
		}
	}
	return nil
}

// errNotDue is what a weekly job returns outside its hour.
var errNotDue = fmt.Errorf("not due: %w", jobs.ErrSkipped)
