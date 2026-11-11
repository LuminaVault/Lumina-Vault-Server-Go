package main

import (
	"lumina/internal/auth"
	"lumina/internal/hermescontainers"
	"lumina/internal/httpserver"
	"lumina/internal/modules/integrations"
	"lumina/internal/modules/megateways"
	"strings"
)

// containerModules is /v1/me/hermes-gateways, /v1/gateways/photon/inbound (6D)
// and /v1/integrations/{xai,nous} (6E). Swift builds all of them in the SecretBox
// branch, so no key, no routes. Their streams, OAuth sessions and parked CLI
// processes are in-memory: each group flips as one (flip-blockers.md).
func (a *agent) containerModules(keys auth.Keyset) []httpserver.Module {
	if a.box == nil {
		return nil
	}
	cfg := a.cfg
	docker := hermescontainers.ProcessDocker{BinaryPath: cfg.DockerBinaryPath}
	containers := &hermescontainers.Lifecycle{
		Manager: hermescontainers.New(a.pool, a.box),
		Docker:  docker,
		Config: hermescontainers.Config{
			Image:             cfg.HermesPerTenantImage,
			Network:           cfg.HermesPerTenantNetwork,
			DataRootBase:      cfg.HermesPerTenantDataRootBase,
			PortRangeStart:    cfg.HermesPerTenantPortRangeStart,
			PortRangeEnd:      cfg.HermesPerTenantPortRangeEnd,
			IdleTTLSeconds:    cfg.HermesPerTenantIdleTTLSeconds,
			DefaultModel:      cfg.HermesDefaultModel,
			MnemosyneDefault:  strings.ToLower(cfg.MnemosyneEnabled) == "true",
			MemoryLimit:       cfg.HermesPerTenantMemoryLimit,
			CPULimit:          cfg.HermesPerTenantCPULimit,
			AudioProxyBaseURL: cfg.HermesAudioProxyBaseURL,
		},
		Audio: auth.AudioTokens{Keys: keys, Pool: a.pool},
	}
	// Swift builds the sidecar client only with both a URL and a token.
	var sidecar megateways.SidecarClient
	if cfg.PhotonSidecarURL != "" && cfg.PhotonSidecarToken != "" {
		sidecar = megateways.NewSidecarClient(cfg.PhotonSidecarURL, cfg.PhotonSidecarToken, nil)
	}
	return []httpserver.Module{
		megateways.New(a.pool, megateways.Options{
			Box: a.box, Limits: a.limits, Containers: containers, Docker: docker,
			Resolver: hermesResolver(a.pool, cfg, a.box), SidecarToken: cfg.PhotonSidecarToken, Sidecar: sidecar,
		}),
		integrations.New(a.pool, integrations.Options{
			Containers: containers, Xai: integrations.NewLiveXaiBackend(docker),
			Nous: integrations.NewLiveNousBackend(docker), Credentials: a.stack.Credentials,
		}),
	}
}
