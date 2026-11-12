package main

import (
	"context"
	"lumina/internal/hermescontainers"
	"lumina/internal/hermesdash"
	"lumina/internal/modules/dashboard"
	"lumina/internal/modules/skillscron"
	"lumina/internal/ssrfguard"
	"strings"

	"github.com/google/uuid"
)

// skillsCronModule is /v1/me/hermes/cron (CronBridgeController). Swift builds
// CronBridgeService only in the SecretBox branch, where the container manager,
// docker and the byoHermes SSRF guard live, and only after the routed transport
// exists (its NL → spec fallback classifies through it). No key, no routes.
func (a *agent) skillsCronModule() *skillscron.Module {
	if a.box == nil {
		return nil
	}
	return skillscron.New(&skillscron.Service{
		Pool: a.pool,
		Box:  a.box,
		Guard: ssrfguard.Guard{
			AllowPrivateRanges: strings.EqualFold(a.cfg.BYOHermesAllowPrivate, "true"),
			RequireHTTPS:       a.cfg.BYOHermesRequireHTTPS(),
			AllowTailnetHTTP:   strings.EqualFold(a.cfg.BYOHermesAllowTailnet, "true"),
		},
		Containers: hermescontainers.New(a.pool, a.box),
		Docker:     hermescontainers.ProcessDocker{BinaryPath: a.cfg.DockerBinaryPath},
		HTTP:       hermesdash.NewClient(),
		Transport:  a.stack.Transport,
		Model:      a.cfg.HermesDefaultModel,
	})
}

// dashboardCronPreview is CronBridgePreview: the bridge's jobs as
// DashboardCronJobDTOs.
func dashboardCronPreview(svc *skillscron.Service) dashboard.CronPreview {
	return func(ctx context.Context, tenant uuid.UUID) []dashboard.CronJobDTO {
		jobs := svc.CronPreview(ctx, tenant)
		out := make([]dashboard.CronJobDTO, 0, len(jobs))
		for _, j := range jobs {
			out = append(out, dashboard.CronJobDTO{ID: j.ID, Name: j.Name, Schedule: j.Schedule, LastRun: j.LastRun, Status: j.Status})
		}
		return out
	}
}
