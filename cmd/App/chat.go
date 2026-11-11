package main

import (
	"context"
	"lumina/internal/apns"
	linkcapture "lumina/internal/capture"
	"lumina/internal/chunkindex"
	"lumina/internal/entitlement"
	"lumina/internal/hermesprofile"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/modules/chat"
	"lumina/internal/modules/llmchat"
	"lumina/internal/modules/memoryagent"
	"lumina/internal/onboardlatch"
	"lumina/internal/posthog"
	"lumina/internal/selfimprove"
	"lumina/internal/skills"
	"lumina/internal/soul"
	"lumina/internal/usagemeter"
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
	// Owner decision 1, option A: while CHAT_HERMES_RUNS_BRIDGE_URL is set, escalated
	// turns start their run on the Swift API, whose watcher follows it. Unset, they
	// start on this process's HermesRunsService (6C), as Swift's chat does; the 6C
	// flip removes the bridge URL (decisions.md D-6c-runs).
	if bridge := chat.NewBridge(cfg.ChatHermesRunsBridgeURL); bridge != nil {
		opts.Runs = bridge
	} else if a.runs != nil {
		opts.Runs = runsStarter{svc: a.runs}
	}

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
	return []httpserver.Module{chat.New(a.pool, opts), llmRoute}
}
