package main

import (
	"log/slog"
	"lumina/internal/hermescontainers"
	"lumina/internal/httpserver"
	"lumina/internal/modules/integrations"
	"lumina/internal/modules/megateways"
)

// containerModules is /v1/me/hermes-gateways, /v1/gateways/photon/inbound (6D)
// and /v1/integrations/{xai,nous} (6E). Swift builds all of them in the SecretBox
// branch, so no key, no routes. Their streams, OAuth sessions and parked CLI
// processes are in-memory: each group flips as one (flip-blockers.md).
func (a *agent) containerModules() []httpserver.Module {
	if a.box == nil {
		return nil
	}
	cfg := a.cfg
	docker := hermescontainers.ProcessDocker{BinaryPath: cfg.DockerBinaryPath}
	containers := &hermescontainers.Lifecycle{
		Manager: hermescontainers.New(a.pool, a.box), Docker: docker, Network: cfg.HermesPerTenantNetwork,
	}
	if cfg.PhotonSidecarURL != "" && cfg.PhotonSidecarToken != "" {
		slog.Warn("photon sidecar is configured but not ported to Go: inbound answers 503 and provisioning skips activation")
	}
	return []httpserver.Module{
		megateways.New(a.pool, megateways.Options{
			Box: a.box, Limits: a.limits, Containers: containers, Docker: docker,
			Resolver: hermesResolver(a.pool, cfg, a.box), SidecarToken: cfg.PhotonSidecarToken,
		}),
		integrations.New(a.pool, integrations.Options{
			Containers: containers, Xai: integrations.NewLiveXaiBackend(docker),
			Nous: integrations.NewLiveNousBackend(docker), Credentials: a.stack.Credentials,
		}),
	}
}
