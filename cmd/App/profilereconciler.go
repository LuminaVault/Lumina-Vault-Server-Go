package main

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/hermesprofile"
	"lumina/internal/jobs"
)

// profileReconcilerJob is HermesProfileReconcilerService
// (Admin/HermesProfileReconcilerService.swift): reconcile() every day at 04:00 UTC,
// an hour after the lapse archiver. Swift registers it when Fluent is on and not in
// the test environment; a failed pass is logged and the next day retries.
//
// Swift also logs the gateway's health once when the service starts. The jobs
// package has no start hook, so that line isn't kept: /health/deep and the admin
// health route answer the same question.
func (a *agent) profileReconcilerJob() []jobs.Job {
	if a.cfg.LVEnvironment == "test" {
		return nil
	}
	cfg := a.cfg
	service := hermesprofile.New(a.pool, hermesprofile.NewGateway(cfg.HermesGatewayKind, cfg.HermesDataRoot), a.paths)
	reconciler := hermesprofile.NewReconciler(a.pool, service, cfg.HermesDataRoot, cfg.HermesGatewayURL, a.gatewayProbe)
	return []jobs.Job{reconcilerJob(reconciler, slog.Default().With("label", "lv.admin"))}
}

type profileReconciler interface {
	Reconcile(ctx context.Context) (hermesprofile.ReconcileSummary, error)
}

func reconcilerJob(r profileReconciler, log *slog.Logger) jobs.Job {
	return jobs.Job{
		Name:     "HermesProfileReconcilerService",
		Schedule: jobs.DailyUTC(4),
		Run: func(ctx context.Context) error {
			s, err := r.Reconcile(ctx)
			if err != nil {
				log.Warn(fmt.Sprintf("hermes.reconciler.service error %v", err))
				return err
			}
			log.Info(fmt.Sprintf("hermes.reconciler.service ran scanned=%d created=%d recovered=%d ok=%d failures=%d",
				s.UsersScanned, s.ProfilesCreated, s.ProfilesRecovered, s.ProfilesAlreadyOK, len(s.Failures)))
			return nil
		},
	}
}
