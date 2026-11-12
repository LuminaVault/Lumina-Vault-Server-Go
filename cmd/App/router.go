package main

import (
	"lumina/internal/chunkindex"
	"lumina/internal/hermesprofile"
	"lumina/internal/httpserver"
	"lumina/internal/modules/admin"
	"lumina/internal/modules/llmadmin"
	"lumina/internal/modules/llmrouter"
	"lumina/internal/modules/meproviders"
)

// routerModules builds wave 5A's modules on the agent's one LLM stack, so a
// credential or preference write drops the cache the router reads.
func (a *agent) routerModules() []httpserver.Module {
	return []httpserver.Module{
		meproviders.New(a.pool, meproviders.Options{Identify: a.identify, Limits: a.limits, Stack: a.stack, Box: a.box}),
		llmrouter.New(a.pool, llmrouter.Options{
			Identify: a.identify, Limits: a.limits, Stack: a.stack, Memories: a.memories, Embeddings: a.embeddings,
		}),
		llmadmin.New(a.stack.Registry, a.cfg.AdminToken, nil),
		a.adminModule(),
	}
}

// adminModule is wave 7B: the admin-token routes and /internal/metrics. Health
// correlation calls the routed transport, as Swift's routedTransport.
func (a *agent) adminModule() httpserver.Module {
	cfg := a.cfg
	chunks := chunkindex.NewChunkRepository(a.pool)
	indexer := chunkindex.NewIndexer(chunks, a.embeddings, chunkindex.NewLinkRepository(a.pool))
	service := hermesprofile.New(a.pool, hermesprofile.NewGateway(cfg.HermesGatewayKind, cfg.HermesDataRoot), a.paths)
	return admin.New(a.pool, admin.Options{
		AdminToken:         cfg.AdminToken,
		MetricsSecret:      cfg.MetricsSecret,
		Reconciler:         hermesprofile.NewReconciler(a.pool, service, cfg.HermesDataRoot, cfg.HermesGatewayURL, a.gatewayProbe),
		Backfill:           chunkindex.NewBackfiller(a.pool, a.paths, indexer),
		Transport:          a.stack.Transport,
		Embeddings:         a.embeddings,
		Memories:           a.memories,
		DefaultModel:       cfg.HermesDefaultModel,
		RetentionDays:      cfg.UsageEventsRetentionDays,
		RetentionBatchSize: cfg.UsageEventsRetentionBatchSize,
	})
}
