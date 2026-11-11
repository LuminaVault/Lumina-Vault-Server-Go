package main

import (
	"log/slog"
	"lumina/internal/apns"
	"lumina/internal/config"
	"lumina/internal/diskguard"
	"lumina/internal/jobs"
	"lumina/internal/modules/calendar"
	"lumina/internal/secretbox"
	"lumina/internal/workers"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// j1Jobs builds group J1's jobs (wave-6-8-plan.md "J1, J2") under Swift's own start
// conditions, so naming one in LV_GO_JOBS where Swift wouldn't run it fails boot.
// None runs unless LV_GO_JOBS names it. MultimodalIngestionWorker isn't here: it
// needs the Hermes capabilities service, which 6A ports.
func j1Jobs(pool *pgxpool.Pool, cfg config.Config, disk *diskguard.Guard) []jobs.Job {
	// Always, in Swift.
	out := []jobs.Job{
		workers.DiskSpaceMonitor(disk),
		workers.NewDeletedDataPurge(workers.PurgeRoots(cfg.VaultRootPath, cfg.HermesDataRoot),
			cfg.AccountDeletionPurgeAfterDays, slog.Default().With("label", "lv.account.purge")).Job(),
	}
	if cfg.LVEnvironment != "test" {
		out = append(out,
			workers.AnalyticsMaintenance(pool),
			(&workers.GraphLayoutWorker{Pool: pool, Logger: slog.Default().With("label", "lv.graph.layout.worker")}).Job(),
			(&workers.RetrievalLeakReportWorker{
				Pool: pool, SurfaceInsight: strings.EqualFold(cfg.RetrievalLeakReportSurfaceInsight, "true"),
				Logger: slog.Default().With("label", "lv.retrieval.leakreport"),
			}).Job(),
			(&workers.ReminderScheduler{
				Pool: pool,
				Push: apns.New(pool, apns.Config{
					Enabled: cfg.APNSEnabled, BundleID: cfg.APNSBundleID, TeamID: cfg.APNSTeamID, KeyID: cfg.APNSKeyID,
					PrivateKeyPath: cfg.APNSPrivateKeyPath, Environment: cfg.APNSEnvironment,
				}),
				Logger: slog.Default().With("label", "lv.reminders.scheduler"),
			}).Job(),
		)
	}
	// Swift: a SecretBox and all three oauth.googleCalendar settings; not gated on test.
	// Its own Google side, so its in-flight refresh dedupe isn't shared with the
	// calendar routes' (Swift shares one CalendarTokenStore).
	if box, err := secretbox.New(cfg.LVSecretMasterKey); err == nil {
		google := calendar.NewGoogle(pool, calendar.GoogleOptions{
			Box: box, ClientID: cfg.GoogleCalendarClientID, ClientSecret: cfg.GoogleCalendarClientSecret,
			RedirectURI: cfg.GoogleCalendarRedirectURI, WebReturnOrigins: cfg.CORSAllowedOrigins,
		})
		if google.Configured() {
			out = append(out, workers.CalendarSyncWorker(google, slog.Default().With("label", "lv.calendar")))
		}
	}
	return out
}
