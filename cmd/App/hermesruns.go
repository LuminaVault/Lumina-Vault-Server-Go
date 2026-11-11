package main

import (
	"context"
	"log/slog"
	"lumina/internal/apns"
	"lumina/internal/config"
	"lumina/internal/entitlement"
	"lumina/internal/hermesprofile"
	"lumina/internal/hermesresolve"
	"lumina/internal/hermesruns"
	"lumina/internal/httpserver"
	"lumina/internal/jobs"
	"lumina/internal/modules/chat"
	hermesrunsroute "lumina/internal/modules/hermesruns"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newHermesRuns builds the one HermesRunsService chat and /v1/hermes/runs share
// (two would mean two watcher budgets). Nil without the endpoint resolver, which
// needs LV_SECRET_MASTER_KEY, as in Swift.
func newHermesRuns(pool *pgxpool.Pool, cfg config.Config, resolver *hermesresolve.Resolver) *hermesruns.Service {
	if resolver == nil {
		return nil
	}
	log := slog.Default().With("label", "lv.hermes.runs")
	push := apns.New(pool, apns.Config{
		Enabled: cfg.APNSEnabled, BundleID: cfg.APNSBundleID, TeamID: cfg.APNSTeamID, KeyID: cfg.APNSKeyID,
		PrivateKeyPath: cfg.APNSPrivateKeyPath, Environment: cfg.APNSEnvironment,
	})
	httpClient := hermesruns.NoRedirectClient()
	apiKey := cfg.HermesAPIKey
	return hermesruns.NewService(hermesruns.Options{
		Pool:     pool,
		Notifier: hermesruns.APNSNotifier{Push: push, Log: log},
		Resolve:  resolver.Resolve,
		MakeClient: func(res hermesresolve.Resolution, sessionKey string) *hermesruns.Client {
			c := hermesruns.MakeClient(res, apiKey, sessionKey, httpClient)
			c.Log = log
			return c
		},
		Log: log,
	})
}

// hermesRunsModule is /v1/hermes/runs (6C), mounted on the same SecretBox gate.
func (a *agent) hermesRunsModule() httpserver.Module {
	if a.runs == nil {
		return nil
	}
	return hermesrunsroute.New(hermesrunsroute.Options{
		Identify: a.identify,
		Hermes:   byoHermes(a.pool, a.cfg, a.box),
		Profile:  hermesprofile.New(a.pool, hermesprofile.NewGateway(a.cfg.HermesGatewayKind, a.cfg.HermesDataRoot), a.paths).Middleware(),
		Limits:   a.limits,
		Gate:     a.gate(entitlement.Chat),
		Service:  a.runs,
	})
}

// hermesRunsJob is HermesRunsService's run(): re-attach watchers to the runs that
// were active when the process last stopped, once at boot, then hold until
// shutdown and stop every watcher. Swift starts it with Fluent outside test; both
// servers re-attaching the same runs duplicates watchers (jobs.md), so Go does it
// only when LV_GO_JOBS names it.
func (a *agent) hermesRunsJob() []jobs.Job {
	if a.runs == nil || a.cfg.LVEnvironment == "test" {
		return nil
	}
	runs := a.runs
	return []jobs.Job{{
		Name:     "HermesRunsService",
		Schedule: jobs.Every(24*time.Hour, 0),
		Run: func(ctx context.Context) error {
			runs.Reattach(ctx)
			<-ctx.Done()
			slog.Default().Info("hermes runs service stopped", "watchers_cancelled", runs.StopAll())
			return nil
		},
	}}
}

// runsStarter adapts the service to chat's HermesRuns seam: Swift's chat calls
// the one in-process service with the vault id as the session key.
type runsStarter struct{ svc *hermesruns.Service }

func (r runsStarter) Start(ctx context.Context, req chat.RunStart) (chat.RunRef, error) {
	conversation := req.ConversationID
	start := hermesruns.StartRequest{Prompt: req.Prompt}
	if conversation != uuid.Nil {
		start.ConversationID = &conversation
	}
	run, err := r.svc.Start(ctx, req.Tenant, start, req.SessionKey)
	if err != nil {
		return chat.RunRef{}, err
	}
	return chat.RunRef{ID: run.ID.UUID, SessionID: run.SessionID, StartedAt: run.StartedAt.Time}, nil
}
