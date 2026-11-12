package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"lumina/internal/config"
	"lumina/internal/hermesprofile"
	"lumina/internal/jobs"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeReconciler struct {
	summary hermesprofile.ReconcileSummary
	err     error
	calls   int
}

func (f *fakeReconciler) Reconcile(context.Context) (hermesprofile.ReconcileSummary, error) {
	f.calls++
	return f.summary, f.err
}

// HermesProfileReconcilerService: one reconcile() per run, Swift's summary line, and
// a failed pass logged as a warning and reported to the job loop.
func TestProfileReconcilerJobRunsReconcileAndLogsSummary(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	r := &fakeReconciler{summary: hermesprofile.ReconcileSummary{
		UsersScanned: 5, ProfilesCreated: 1, ProfilesRecovered: 2, ProfilesAlreadyOK: 2, Failures: []string{"u: boom"},
	}}
	job := reconcilerJob(r, log)
	require.Equal(t, "HermesProfileReconcilerService", job.Name)
	require.NoError(t, job.Run(t.Context()))
	require.Equal(t, 1, r.calls)
	require.Contains(t, buf.String(), "hermes.reconciler.service ran scanned=5 created=1 recovered=2 ok=2 failures=1")

	buf.Reset()
	r.err = errors.New("db down")
	require.Error(t, job.Run(t.Context()))
	require.Contains(t, buf.String(), "hermes.reconciler.service error db down")
}

// Registered as Swift does (Fluent on, not test), and LV_GO_JOBS may name it.
func TestProfileReconcilerJobStartConditions(t *testing.T) {
	prod := config.Config{LVEnvironment: "production", LVGoJobs: []string{"HermesProfileReconcilerService"}}
	a := &agent{cfg: prod}
	got := a.profileReconcilerJob()
	require.Len(t, got, 1)
	_, err := jobs.New(prod, got, jobs.Options{})
	require.NoError(t, err)

	a.cfg.LVEnvironment = "test"
	require.Empty(t, a.profileReconcilerJob())
}
