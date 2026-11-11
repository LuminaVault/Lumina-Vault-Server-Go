package main

import (
	"lumina/internal/config"
	"lumina/internal/diskguard"
	"lumina/internal/jobs"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func j1Names(cfg config.Config) []string {
	var names []string
	for _, j := range j1Jobs(nil, cfg, diskguard.New(t0Dir, 0, 0, diskguard.Options{})) {
		names = append(names, j.Name)
	}
	slices.Sort(names)
	return names
}

const t0Dir = "/nonexistent"

// J1 registers each job under Swift's start conditions, with the Swift type name,
// so LV_GO_JOBS reads like jobs.md, and LV_GO_JOBS naming all of them boots.
func TestJ1JobsFollowSwiftStartConditions(t *testing.T) {
	all := config.Config{
		LVEnvironment:              "production",
		LVSecretMasterKey:          "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		GoogleCalendarClientID:     "id",
		GoogleCalendarClientSecret: "secret",
		GoogleCalendarRedirectURI:  "https://example.invalid/cb",
	}
	want := []string{
		"AnalyticsMaintenanceService", "CalendarSyncWorker", "DeletedDataPurgeService", "DiskSpaceMonitorService",
		"GraphLayoutWorker", "ReminderScheduler", "RetrievalLeakReportWorker",
	}
	require.Equal(t, want, j1Names(all))

	all.LVGoJobs = want
	_, err := jobs.New(all, j1Jobs(nil, all, diskguard.New(t0Dir, 0, 0, diskguard.Options{})), jobs.Options{})
	require.NoError(t, err)

	// No Google settings: no calendar worker, as Swift's calendarConfigured.
	noGoogle := all
	noGoogle.GoogleCalendarRedirectURI = ""
	require.NotContains(t, j1Names(noGoogle), "CalendarSyncWorker")
	noKey := all
	noKey.LVSecretMasterKey = ""
	require.NotContains(t, j1Names(noKey), "CalendarSyncWorker")

	// LV_ENVIRONMENT=test keeps only the ones Swift doesn't gate on it.
	test := all
	test.LVEnvironment = "test"
	require.Equal(t, []string{"CalendarSyncWorker", "DeletedDataPurgeService", "DiskSpaceMonitorService"}, j1Names(test))
}
