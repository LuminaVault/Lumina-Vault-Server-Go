package main

import (
	"context"
	"lumina/internal/bundled"
	"lumina/internal/hermescaps"
	"lumina/internal/hermescontainers"
	"lumina/internal/hermesdash"
	"lumina/internal/hermesmirror"
	"lumina/internal/httpserver"
	hermesmirrormod "lumina/internal/modules/hermesmirror"
	"lumina/internal/modules/kbcompile"
	"lumina/internal/ssrfguard"
	"lumina/internal/wire"
	"strings"

	"github.com/google/uuid"
)

// kbCompileRunner is MemoryCompileControllerRunner over the kb-compile route.
type kbCompileRunner struct{ compile *kbcompile.Module }

func (r kbCompileRunner) Compile(ctx context.Context, tenant uuid.UUID, fileIDs []uuid.UUID) error {
	wrapped := make([]wire.UUID, 0, len(fileIDs))
	for _, id := range fileIDs {
		wrapped = append(wrapped, wire.UUID{UUID: id})
	}
	_, err := r.compile.Compile(ctx, ctx, tenant, wrapped, false)
	return err
}

// hermesMirrorModule is HermesMirrorWiring (6B): /v1/hermes/mirror,
// /v1/hermes/artifacts, /v1/hermes/workspace and the webhook push. Swift mounts
// it only with a secret box and an endpoint resolver. Call after modules():
// it needs the ingest service and the compile route. HermesMirrorRefreshWorker
// stays on Swift (O4).
func (a *agent) hermesMirrorModule() httpserver.Module {
	if a.box == nil || a.ingest == nil {
		return nil
	}
	resolver := hermesResolver(a.pool, a.cfg, a.box)
	if resolver == nil {
		return nil
	}
	guard := ssrfguard.Guard{
		AllowPrivateRanges: strings.EqualFold(a.cfg.BYOHermesAllowPrivate, "true"),
		RequireHTTPS:       a.cfg.BYOHermesRequireHTTPS(),
		AllowTailnetHTTP:   strings.EqualFold(a.cfg.BYOHermesAllowTailnet, "true"),
	}
	service := &hermesmirror.Service{
		Pool: a.pool,
		Transports: &hermesdash.Factory{
			Pool: a.pool, Box: a.box, Guard: guard, Resolver: resolver,
			Containers:            hermescontainers.New(a.pool, a.box),
			PerTenantDataRootBase: a.cfg.HermesPerTenantDataRootBase,
			ManagedHermesRoot:     a.cfg.HermesDataRoot,
			HTTP:                  hermesdash.NewClient(),
		},
		Capabilities:  hermescaps.New(a.pool, resolver, a.box, guard),
		Ingest:        a.ingest,
		BundledSkills: bundled.HermesSkills(),
		Limits:        hermesmirror.DefaultLimits(),
	}
	if a.compile != nil {
		service.Compile = kbCompileRunner{compile: a.compile}
	}
	return hermesmirrormod.New(a.pool, hermesmirrormod.Options{
		Identify: a.identify, Limits: a.limits, BYO: resolver.Middleware(), Box: a.box, Service: service,
	})
}
