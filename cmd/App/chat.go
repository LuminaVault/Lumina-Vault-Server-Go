package main

import (
	"context"
	"log/slog"
	"lumina/internal/apns"
	linkcapture "lumina/internal/capture"
	"lumina/internal/chunkindex"
	"lumina/internal/entitlement"
	"lumina/internal/hermesprofile"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/modules/apple"
	"lumina/internal/modules/chat"
	"lumina/internal/modules/llmchat"
	"lumina/internal/modules/memoryagent"
	"lumina/internal/modules/quickassistant"
	"lumina/internal/modules/reminders"
	"lumina/internal/onboardlatch"
	"lumina/internal/posthog"
	"lumina/internal/selfimprove"
	"lumina/internal/skills"
	"lumina/internal/soul"
	"lumina/internal/usagemeter"
	"lumina/internal/vaultaccess"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// chatModules builds wave 5C: /v1/conversations and its siblings, and /v1/llm/chat.
// links is the shared link-capture pipeline, enrichment its enrichment service.
func (a *agent) chatModules(links *linkcapture.Service, enrichment *linkcapture.EnrichmentService, analytics *posthog.Client) []httpserver.Module {
	cfg := a.cfg
	byo := byoHermes(a.pool, cfg, a.box)
	var gateway *url.URL
	if parsed, err := url.Parse(cfg.HermesGatewayURL); err == nil {
		gateway = parsed
	}
	stream := a.stack.StreamService(llm.GatewayConfig{
		BaseURL: gateway, DefaultModel: cfg.HermesDefaultModel, APIKey: cfg.HermesAPIKey,
		IdleTimeout: time.Duration(cfg.HermesStreamIdleTimeout) * time.Second,
	})
	var telemetry memoryagent.Telemetry
	if cfg.RetrievalTelemetryEnabled != "false" {
		telemetry = memoryagent.PGTelemetry{Pool: a.pool}
	}
	var isUserOverride func(context.Context, uuid.UUID) bool
	if resolver := hermesResolver(a.pool, cfg, a.box); resolver != nil {
		isUserOverride = selfimprove.ResolverOverride(resolver)
	}
	opts := chat.Options{
		Identify: a.identify, Hermes: byo,
		Profile: hermesprofile.New(a.pool, hermesprofile.NewGateway(cfg.HermesGatewayKind, cfg.HermesDataRoot), a.paths).Middleware(),
		Gate:    a.gate(entitlement.MemoryQuery), Limits: a.limits,
		Stream:       stream,
		FollowUps:    &memoryagent.FollowUpGenerator{Transport: a.stack.Transport, DefaultModel: cfg.HermesDefaultModel},
		Memories:     a.memories,
		Embeddings:   a.embeddings,
		Hybrid:       chunkindex.NewHybrid(a.memories, chunkindex.NewChunkRepository(a.pool)),
		Telemetry:    telemetry,
		Preferences:  a.stack.Preferences,
		Traces:       llm.PGMeters{Pool: a.pool}.TraceRecorder(),
		Latches:      onboardlatch.New(a.pool),
		Analytics:    analytics,
		Activity:     selfimprove.NewActivity(a.pool, cfg.SelfImprovementEnabled, isUserOverride),
		Soul:         soul.New(a.paths, cfg.HermesDataRoot),
		Achievements: a.sink,

		DefaultModel:                    cfg.HermesDefaultModel,
		ParallelEnabled:                 a.stack.ParallelEnabled,
		HybridExecutionEnabled:          strings.EqualFold(cfg.HybridExecutionEnabled, "true"),
		LocalExecutionToolBrokerEnabled: strings.EqualFold(cfg.LocalExecutionToolBrokerEnabled, "true"),
		MaxGroundingDistance:            float32(cfg.ChatRetrievalMaxDistance),
	}
	if strings.EqualFold(cfg.AutoSaveLinksEnabled, "true") {
		opts.Links = links
	}
	// Owner decision 1, option A: escalated turns start their run on the Swift API
	// until wave 6C ports HermesRunsService.
	if bridge := chat.NewBridge(cfg.ChatHermesRunsBridgeURL); bridge != nil {
		opts.Runs = bridge
	}
	quick, quickRoutes := a.quickAssistant()
	opts.QuickAssistant = quick

	catalog := a.skillCatalog()
	selectorTransport := llmchat.SelectorTransport{Transport: a.stack.Transport}
	router := skills.NewContextRouter(catalog, func(tenant uuid.UUID, _ string) skills.Selector {
		return skills.DefaultSelector{Transport: selectorTransport, Model: cfg.HermesDefaultModel, SessionKey: ids.Upper(tenant)}
	}, llmchat.Entitled)

	llmRoute := llmchat.New(a.pool, llmchat.Options{
		Identify: a.identify, Hermes: byo, Gate: a.gate(entitlement.Chat), Limits: a.limits,
		MaxStreams: cfg.MaxConcurrentStreamsPerUser, ContextRouter: router,
		Chat:       a.stack.ChatService(cfg.HermesDefaultModel),
		Enrichment: enrichment,
		Usage:      usagemeter.New(a.pool),
		Push: apns.New(a.pool, apns.Config{
			Enabled: cfg.APNSEnabled, BundleID: cfg.APNSBundleID, TeamID: cfg.APNSTeamID, KeyID: cfg.APNSKeyID,
			PrivateKeyPath: cfg.APNSPrivateKeyPath, Environment: cfg.APNSEnvironment,
		}),
		Achievements: a.sink,
		Latches:      onboardlatch.New(a.pool),
	})
	modules := []httpserver.Module{chat.New(a.pool, opts), llmRoute}
	if quickRoutes != nil {
		modules = append(modules, quickRoutes)
	}
	return modules
}

// quickAssistant is Quick Assistant's half of the chat wiring: the loop the
// stream runs and the confirm/cancel routes. Both nil when
// QUICK_ASSISTANT_ENABLED is off, or when it is on but the pending-action
// table is missing, so chat stays exactly what it was.
func (a *agent) quickAssistant() (*chat.QuickAssistant, httpserver.Module) {
	if !strings.EqualFold(a.cfg.QuickAssistantEnabled, "true") {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready, err := quickassistant.Ready(ctx, a.pool)
	if err != nil {
		// Off until the next restart, but say it was the database, not the
		// migration.
		slog.Error("quick assistant disabled: assistant_pending_actions could not be checked", "error", err)
		return nil, nil
	}
	if !ready {
		slog.Error("quick assistant disabled: assistant_pending_actions missing")
		return nil, nil
	}
	if a.mcp == nil {
		slog.Error("quick assistant disabled: the /v1/mcp tool service is not built")
		return nil, nil
	}
	deps := quickassistant.Deps{
		Pool: a.pool, Access: vaultaccess.New(a.pool), MCP: a.mcp,
		Reminders: reminders.NewStore(a.pool), Consent: apple.NewConsentReader(a.pool),
		Store: quickassistant.NewStore(a.pool),
		// Devices stays nil until the /v1/ws cutover (P5/5J): no calendar_create, no Apple mirror.
		Devices: nil,
	}
	routes, err := quickassistant.New(a.pool, deps, quickassistant.Options{
		Identify: a.identify, Gate: a.gate(entitlement.MemoryQuery), Limits: a.limits,
	})
	if err != nil {
		slog.Error("quick assistant disabled: its routes cannot be guarded", "error", err)
		return nil, nil
	}
	return &chat.QuickAssistant{
		Service: &quickassistant.Service{
			Transport: a.stack.Transport, Model: a.cfg.HermesDefaultModel, Release: a.stack.Telemetry.Release,
			Tools: quickassistant.Definitions(quickassistant.DefinitionOptions{DeviceWrites: false}),
		},
		Deps: deps,
	}, routes
}
