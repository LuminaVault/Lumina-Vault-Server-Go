package main

import (
	"context"
	"lumina/internal/achievementsink"
	"lumina/internal/capture"
	"lumina/internal/chunkindex"
	"lumina/internal/config"
	"lumina/internal/diskguard"
	"lumina/internal/embedding"
	"lumina/internal/entitlement"
	"lumina/internal/hermesresolve"
	"lumina/internal/httpserver"
	"lumina/internal/idempotency"
	"lumina/internal/llm"
	"lumina/internal/memorycompile"
	"lumina/internal/memorystore"
	"lumina/internal/modules/imports"
	"lumina/internal/modules/kbcompile"
	"lumina/internal/modules/knowledge"
	"lumina/internal/modules/me"
	"lumina/internal/modules/memoryagent"
	"lumina/internal/modules/spaces"
	"lumina/internal/onboardlatch"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// byoHermes builds the BYO Hermes middleware; nil without a secret box or a gateway URL
// that parses, as in Swift.
func byoHermes(pool *pgxpool.Pool, cfg config.Config, box *secretbox.Box) echo.MiddlewareFunc {
	resolver := hermesResolver(pool, cfg, box)
	if resolver == nil {
		return nil
	}
	return resolver.Middleware()
}

// agent holds the singletons the model-calling routes share: one LLM stack and one
// hit-count queue, whose Run stops after the HTTP server, like the achievement sink.
type agent struct {
	pool       *pgxpool.Pool
	cfg        config.Config
	box        *secretbox.Box
	paths      vaultpaths.Paths
	identify   echo.MiddlewareFunc
	limits     *ratelimit.Store
	embeddings embedding.Service
	sink       *achievementsink.Sink
	today      *me.Module
	disk       *diskguard.Guard
	stack      *llm.Stack
	memories   *memorystore.Repository
}

func newAgent(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, box *secretbox.Box, paths vaultpaths.Paths, identify echo.MiddlewareFunc,
	limits *ratelimit.Store, embeddings embedding.Service, sink *achievementsink.Sink, today *me.Module, disk *diskguard.Guard,
) (a *agent, stop func()) {
	gateway, err := url.Parse(cfg.HermesGatewayURL)
	if err != nil {
		gateway = nil
	}
	stack := llm.NewStack(llm.StackConfig{
		Getenv: os.Getenv, Pool: pool, Box: box, HermesURL: gateway,
		HermesAPIKey: cfg.HermesAPIKey, HermesDefaultModel: cfg.HermesDefaultModel,
	})
	hits := memorystore.NewHitCountQueue(pool)
	hitsCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = hits.Run(hitsCtx)
	}()
	a = &agent{
		pool: pool, cfg: cfg, box: box, paths: paths, identify: identify, limits: limits,
		embeddings: embeddings, sink: sink, today: today, disk: disk, stack: stack,
		memories: memorystore.New(pool, hits),
	}
	return a, func() {
		cancel()
		<-done
		stack.Transport.Wait()
	}
}

// captureFiling is the note upload's Space suggestion; nil while capture.autoFile.enabled
// is off, which it is in production. It runs detached: the upload never waits on the model.
func (a *agent) captureFiling() func(tenant, file uuid.UUID) {
	if !strings.EqualFold(a.cfg.CaptureAutoFileEnabled, "true") {
		return nil
	}
	filing := &capture.FilingService{
		Pool: a.pool, Transport: a.stack.Transport, Paths: a.paths,
		Store: vaultfiles.NewStore(a.pool, a.paths), DefaultModel: a.cfg.HermesDefaultModel,
	}
	return func(tenant, file uuid.UUID) {
		go filing.SuggestBestEffort(context.Background(), tenant, file)
	}
}

func (a *agent) gate(c entitlement.Capability) echo.MiddlewareFunc {
	return entitlement.MiddlewareWithHermes(a.pool, c, entitlement.EnforcementEnabled(a.cfg.BillingEnforcementEnabled),
		entitlement.PGCredentials(a.pool, a.box), hermesresolve.IsUserOverride)
}

// modules builds the four 4F modules. links is the link-capture service the capture
// route uses, enrichment its enrichment service.
func (a *agent) modules(links *capture.Service, enrichment *capture.EnrichmentService) []httpserver.Module {
	chunks := chunkindex.NewChunkRepository(a.pool)
	hybrid := chunkindex.NewHybrid(a.memories, chunks)
	var telemetry memoryagent.Telemetry
	if a.cfg.RetrievalTelemetryEnabled != "false" {
		telemetry = memoryagent.PGTelemetry{Pool: a.pool}
	}
	transport := a.stack.Transport
	store := vaultfiles.NewStore(a.pool, a.paths)
	byo := byoHermes(a.pool, a.cfg, a.box)
	service := memoryagent.NewService(memoryagent.ServiceOptions{
		Transport: transport, Memories: a.memories, Embeddings: a.embeddings, DefaultModel: a.cfg.HermesDefaultModel,
		VaultMapEnabled: a.cfg.HermesToolVaultMapEnabled != "false", Telemetry: telemetry, Hybrid: hybrid,
		OnUpserted: a.today.InvalidateToday,
	})
	var gateway *url.URL
	if parsed, err := url.Parse(a.cfg.HermesGatewayURL); err == nil {
		gateway = parsed
	}
	stream := a.stack.StreamService(llm.GatewayConfig{
		BaseURL: gateway, DefaultModel: a.cfg.HermesDefaultModel, APIKey: a.cfg.HermesAPIKey,
		IdleTimeout: time.Duration(a.cfg.HermesStreamIdleTimeout) * time.Second,
	})
	agentRoutes := memoryagent.New(a.pool, memoryagent.Options{
		Identify: a.identify, Hermes: byo, SearchGate: a.gate(entitlement.MemoryQuery), QueryGate: a.gate(entitlement.MemoryQuery),
		MemoGate: a.gate(entitlement.MemoGenerator), Limits: a.limits, MaxStreams: a.cfg.MaxConcurrentStreamsPerUser,
		Service: service,
		Memo: memoryagent.NewMemoService(memoryagent.MemoOptions{
			Transport: transport, Memories: a.memories, Embeddings: a.embeddings, Paths: a.paths, Store: store,
			DefaultModel: a.cfg.HermesDefaultModel,
		}),
		FollowUps: &memoryagent.FollowUpGenerator{Transport: transport, DefaultModel: a.cfg.HermesDefaultModel},
		Stream:    stream, Memories: a.memories, Embeddings: a.embeddings, Hybrid: hybrid, Telemetry: telemetry,
		Preferences: a.stack.Preferences, Achievements: a.sink, DefaultModel: a.cfg.HermesDefaultModel,
	})

	// The progress publisher stays a no-op until /v1/ws moves: the sockets are Swift's.
	compile := memorycompile.New(a.pool, memorycompile.Options{
		Paths: a.paths, Transport: transport, Memories: a.memories, Embeddings: a.embeddings,
		DefaultModel: a.cfg.HermesDefaultModel,
	})
	compileRoute := kbcompile.New(a.pool, kbcompile.Options{
		Identify: a.identify, Idempotency: idempotency.New(a.pool, idempotency.Options{}), Hermes: byo,
		Gate: a.gate(entitlement.MemoryCompile), Limits: a.limits, Service: compile,
		Latches: onboardlatch.New(a.pool), Achievements: a.sink,
	})

	// Knowledge's gate has no BYO middleware before it, so the override check never fires.
	reason := knowledge.NewReason(knowledge.New(a.pool, a.identify, a.gate(entitlement.MemoryQuery), a.limits), transport, a.cfg.HermesDefaultModel)

	importService := imports.NewService(a.pool, links, spaces.New(a.pool, a.paths, a.identify), a.paths, compile, enrichment)
	importRoutes := imports.New(importService,
		&imports.Categorizer{Service: importService, Transport: transport, DefaultModel: a.cfg.HermesDefaultModel},
		imports.NewIngest(a.pool, importService, a.memories, a.embeddings,
			chunkindex.NewIndexer(chunks, a.embeddings, chunkindex.NewLinkRepository(a.pool)), a.disk),
		a.identify)
	return []httpserver.Module{agentRoutes, compileRoute, reason, importRoutes}
}
