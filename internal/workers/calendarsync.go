package workers

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/jobs"
	"lumina/internal/modules/calendar"
	"time"
)

// CalendarSyncWorker is CalendarSyncWorker (Sources/App/Workers/CalendarSyncWorker.swift):
// at boot and then every 15 minutes, an incremental Google Calendar sync of every
// connected account. Swift starts it only with a SecretBox and all three Google
// OAuth settings, so the caller registers it only then.
func CalendarSyncWorker(google *calendar.Google, logger *slog.Logger) jobs.Job {
	if logger == nil {
		logger = slog.Default()
	}
	return jobs.Job{
		Name:     "CalendarSyncWorker",
		Schedule: jobs.Every(15*time.Minute, 0),
		Run: func(ctx context.Context) error {
			synced, err := google.SyncConnected(ctx, 4)
			if err == nil && synced > 0 {
				logger.Info("calendar.sync.worker tick", "accounts", fmt.Sprint(synced))
			}
			return err
		},
	}
}
