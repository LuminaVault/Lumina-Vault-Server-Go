package main

import (
	"context"
	"lumina/internal/config"
	"lumina/internal/jobs"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A typo in LV_GO_JOBS must stop the boot, not silently disable a job.
func TestServeFailsOnUnknownJob(t *testing.T) {
	cfg := config.Config{LVGoJobs: []string{"NoSuchWorker"}}
	err := serve(t.Context(), cfg, Command{Name: "serve", HTTPHost: "127.0.0.1", HTTPPort: 0})
	require.ErrorContains(t, err, `unknown job "NoSuchWorker"`)
}

// LV_GO_JOBS unset: the server starts and stops with no job, as it did before the runner.
func TestServeStartsAndStopsWithNoJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, config.Config{}, Command{Name: "serve", HTTPHost: "127.0.0.1", HTTPPort: 0})
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return")
	}
}

// serve can end without a signal when the listener fails, so stop must not wait on the
// parent context.
func TestStartJobsStopWorksWithoutParentCancel(t *testing.T) {
	started := make(chan struct{})
	stop, err := startJobs(t.Context(), config.Config{LVGoJobs: []string{"A"}}, []jobs.Job{{
		Name:     "A",
		Schedule: jobs.Every(time.Hour, 0),
		Run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	}})
	require.NoError(t, err)
	<-started
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return")
	}
}
