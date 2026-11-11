// Command App is the LuminaVault API server, Go edition. It honours the Swift
// image contract so the infra chart runs it unchanged.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/achievementsink"
	"lumina/internal/apns"
	"lumina/internal/auth"
	linkcapture "lumina/internal/capture"
	"lumina/internal/chunkindex"
	"lumina/internal/config"
	"lumina/internal/db"
	"lumina/internal/devicerpc"
	"lumina/internal/diskguard"
	"lumina/internal/embedding"
	"lumina/internal/entitlement"
	"lumina/internal/hermescontainers"
	"lumina/internal/hermesresolve"
	"lumina/internal/httpserver"
	"lumina/internal/jobs"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/modules/account"
	"lumina/internal/modules/achievements"
	"lumina/internal/modules/apple"
	"lumina/internal/modules/billing"
	"lumina/internal/modules/brain"
	"lumina/internal/modules/calendar"
	"lumina/internal/modules/capture"
	"lumina/internal/modules/community"
	"lumina/internal/modules/dashboard"
	"lumina/internal/modules/devices"
	"lumina/internal/modules/grok"
	"lumina/internal/modules/health"
	"lumina/internal/modules/ingestion"
	"lumina/internal/modules/kanban"
	"lumina/internal/modules/kb"
	"lumina/internal/modules/knowledge"
	"lumina/internal/modules/mail"
	"lumina/internal/modules/marketplace"
	"lumina/internal/modules/me"
	"lumina/internal/modules/memory"
	"lumina/internal/modules/news"
	"lumina/internal/modules/onboarding"
	"lumina/internal/modules/profiles"
	"lumina/internal/modules/projects"
	"lumina/internal/modules/reminders"
	"lumina/internal/modules/selfimprovement"
	"lumina/internal/modules/sessions"
	"lumina/internal/modules/spaces"
	"lumina/internal/modules/system"
	"lumina/internal/modules/teams"
	"lumina/internal/modules/todos"
	"lumina/internal/modules/tts"
	"lumina/internal/modules/vault"
	"lumina/internal/modules/vision"
	"lumina/internal/modules/voice"
	"lumina/internal/modules/workflows"
	wsroute "lumina/internal/modules/ws"
	"lumina/internal/onboardlatch"
	"lumina/internal/posthog"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/selfimprove"
	"lumina/internal/skills"
	"lumina/internal/soul"
	"lumina/internal/ssrfguard"
	"lumina/internal/telemetry"
	"lumina/internal/usagemeter"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"lumina/internal/workers"
	"lumina/internal/ws"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("exit", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd, err := parseArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdown, err := telemetry.Setup(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(flushCtx); err != nil {
			slog.Warn("telemetry shutdown", "error", err)
		}
	}()

	switch cmd.Name {
	case "serve":
		return serve(ctx, cfg, cmd)
	case "migrate":
		return migrate(ctx, cfg, cmd)
	default:
		return fmt.Errorf("%s: not implemented", cmd.Name)
	}
}

// migrate runs goose. Until Phase 12 the chart's migrate job still runs the
// Swift image, so on a Fluent database this refuses unless --adopt-fluent.
func migrate(ctx context.Context, cfg config.Config, cmd Command) error {
	pool, err := db.Open(ctx, db.DSN(cfg))
	if err != nil {
		return err
	}
	defer pool.Close()
	if cmd.Flags["adopt-fluent"] == "true" {
		manifest, err := migrations.FluentManifest()
		if err != nil {
			return err
		}
		return migrations.AdoptFluent(ctx, pool, migrations.SQL(), manifest)
	}
	return migrations.Up(ctx, pool, migrations.SQL())
}

func serve(ctx context.Context, cfg config.Config, cmd Command) error {
	pluginOptions := marketplace.Options{
		RunnerURL: cfg.PluginRunnerURL, RunnerToken: cfg.PluginRunnerToken,
		ArtifactRoot: cfg.PluginArtifactRoot, ArtifactSigningKey: cfg.PluginArtifactSigningKey,
	}
	if err := marketplace.CheckConfig(cfg.LVEnvironment, pluginOptions); err != nil {
		return err
	}
	// Analytics outlive the HTTP server: the deferred cancel runs after Start
	// returns, so requests finishing in graceful shutdown still get flushed.
	analytics := posthog.New(cfg.PostHogProjectToken, cfg.PostHogHost, posthog.Options{})
	analyticsCtx, stopAnalytics := context.WithCancel(context.WithoutCancel(ctx))
	analyticsDone := make(chan struct{})
	go func() {
		defer close(analyticsDone)
		analytics.Run(analyticsCtx)
	}()
	defer func() {
		stopAnalytics()
		<-analyticsDone
	}()
	deps := httpserver.Deps{}
	// Background jobs a port has registered. A job that needs the database goes in the
	// FluentEnabled block, so naming it in LV_GO_JOBS without a database fails boot.
	var registered []jobs.Job
	if cfg.FluentEnabled {
		pool, err := db.Open(ctx, db.DSN(cfg))
		if err != nil {
			return err
		}
		defer pool.Close()
		keys, err := auth.LoadKeys(cfg)
		if err != nil {
			return err
		}
		deps.Auth = auth.Middleware(keys, auth.PGUsers(pool))
		// One rate-limit store per process: Swift's buckets are shared by
		// every route group that uses the same policy name.
		limits := ratelimit.NewStore()
		// makeRateLimitStorage: Go has only the per-process store. The owner keeps
		// rate-limit storage in memory (decisions.md D-ratelimit-memory).
		if ratelimit.ParseStorageKind(cfg.RateLimitStorageKind) == ratelimit.StorageRedis {
			slog.Error("rate-limit storage: RATE_LIMIT_STORAGE_KIND=redis but Go has no Valkey driver; using per-process memory")
		} else {
			slog.Info("rate-limit storage: memory (per-process)", "config", "RATE_LIMIT_STORAGE_KIND=memory")
		}
		// Identify: vault-scoped routes check X-Vault-ID before identity (decisions.md D-identify).
		identify := auth.Identify(keys, auth.PGUsers(pool))
		paths := vaultpaths.Paths{Root: cfg.VaultRootPath}
		// SecretBox: nil without LV_SECRET_MASTER_KEY, as in Swift.
		box, _ := secretbox.New(cfg.LVSecretMasterKey)
		embeddings := embedding.New(pool, embeddingConfig(cfg), box)
		meModule := me.New(pool, identify, limits, me.Options{RecapThemesEnabled: cfg.RecapThemesEnabled})
		// One achievements inbox per process, and MeTodayCache is its one real subscriber.
		// The sink stops after the HTTP server, so a request finishing in graceful shutdown
		// can still enqueue.
		sink, stopSink := startAchievementSink(ctx, pool, cfg, func(tenant uuid.UUID, _ achievements.Sub) { meModule.InvalidateToday(tenant) })
		defer stopSink()
		// One socket map and device-RPC broker per process (P5); 5J mounts /v1/ws on sockets.
		sockets := ws.NewManager()
		deviceRPC := devicerpc.New(sockets, 0)
		// Migrated modules (modules.yaml status ≥ parity). Traffic only reaches
		// them through the api ingress.extraPaths that name them.
		deps.Modules = []httpserver.Module{
			projects.New(pool),
			kanban.New(pool, identify, limits, paths),
			spaces.New(pool, paths, identify),
			profiles.New(pool),
			sessions.New(pool),
			// apple, devices and reminders flip together (D-shared-prefixes).
			apple.New(pool, apple.Options{Embedder: embeddings, Analytics: analytics, Devices: deviceRPC}),
			devices.New(pool, cfg.APNSBundleID),
			reminders.New(pool, nil, cfg.HermesDefaultModel),
			achievements.New(pool, identify, limits),
			teams.New(pool, identify, limits, paths, teams.NewSender(teams.SenderConfig{
				Kind: cfg.EmailKind, APIKey: cfg.EmailResendAPIKey, FromAddress: cfg.EmailFromAddress,
				ReplyTo: cfg.EmailReplyTo, BaseURL: cfg.TeamInviteBaseURL,
			}, slog.Default().With("module", "teams"))),
			meModule,
			dashboard.New(pool, identify),
			ingestion.New(pool, identify, limits, paths,
				diskguard.New(cfg.VaultRootPath, cfg.StorageMinFreeBytes, cfg.StorageMinFreeRatio, diskguard.Options{}),
				ingestion.QuotaFromConfig(cfg)),
		}
		// Wave 3: account, onboarding, health. Health ingest sits behind the
		// billing gate; the BYO-key exemption needs the SecretBox (nil without
		// LV_SECRET_MASTER_KEY, as Swift's UserCredentialStore).
		healthGate := entitlement.Middleware(pool, entitlement.HealthIngest,
			entitlement.EnforcementEnabled(cfg.BillingEnforcementEnabled), entitlement.PGCredentials(pool, box))
		deps.Modules = append(deps.Modules,
			account.New(pool, keys, paths, cfg.HermesDataRoot),
			onboarding.New(pool),
			health.New(pool, healthGate, analytics),
		)
		deps.Modules = append(deps.Modules, memoryModule(pool, cfg, box, identify, limits, paths, embeddings, sink, meModule))
		deps.Modules = append(deps.Modules, selfImprovementModule(pool, cfg, box, identify, paths))
		deps.Modules = append(deps.Modules, soulModule(pool, cfg, box, identify, limits, paths, sink))
		if m := settingsModule(pool, cfg, box, identify, limits); m != nil { // 6A: needs the secret box, as in Swift
			deps.Modules = append(deps.Modules, m)
		}
		// Wave 4: knowledge, vision and the compile probe. /v1/vision spends a platform
		// key, so it has its own gate. Both groups read billing.enforcementEnabled.
		enforcement := entitlement.EnforcementEnabled(cfg.BillingEnforcementEnabled)
		deps.Modules = append(deps.Modules,
			knowledge.New(pool, identify, entitlement.Middleware(pool, entitlement.MemoryQuery, enforcement, entitlement.PGCredentials(pool, box)), limits),
			vision.New(memorystore.New(pool, nil), usagemeter.New(pool), identify,
				entitlement.PlatformFundedMiddleware(pool, entitlement.MemoryQuery, enforcement), limits, visionConfig(cfg)),
			kb.New(pool),
			brain.New(pool, identify, limits),
		)
		// Wave 5: speech to text, on the cluster's own whisper, and the 501 for spoken replies.
		deps.Modules = append(deps.Modules, voice.New(pool, usagemeter.New(pool), identify, auth.AudioIdentify(keys, auth.PGAudioUsers(pool)),
			entitlement.PlatformFundedMiddleware(pool, entitlement.Chat, enforcement),
			entitlement.Middleware(pool, entitlement.Chat, enforcement, nil), limits, voiceConfig(cfg)))
		// /v1/tts exists only for a self-hosted, OpenAI-shaped service. Unset, it isn't
		// mounted, which matches production's 404. Open question 12: owner to confirm.
		if strings.TrimSpace(cfg.TTSBaseURL) != "" {
			deps.Modules = append(deps.Modules, tts.New(usagemeter.New(pool), identify,
				entitlement.PlatformFundedMiddleware(pool, entitlement.Chat, enforcement), limits,
				tts.Config{BaseURL: cfg.TTSBaseURL, DefaultModel: cfg.TTSDefaultModel}))
		}
		deps.Modules = append(deps.Modules, calendarModules(pool, cfg, identify, limits, embeddings, analytics)...)
		if m := news.New(pool, cfg, identify, limits); m != nil { // nil without LV_SECRET_MASTER_KEY, as in Swift
			deps.Modules = append(deps.Modules, m)
		}
		deps.Modules = append(deps.Modules, community.New(pool, community.Config{
			Enabled: community.ParseFlag(cfg.CommunityEnabled), PostingEnabled: community.ParseFlag(cfg.CommunityPostingEnabled),
			AdminToken: cfg.AdminToken,
		}, identify, limits, nil))
		// Wave 5: Swift builds grokController in the SecretBox branch, so no key, no routes.
		if box != nil {
			deps.Modules = append(deps.Modules, grok.New(hermescontainers.New(pool, box), entitlement.PremiumGuardMiddleware(pool), nil))
			deps.Modules = append(deps.Modules, system.New(pool, system.Options{
				Identify: identify, AdminToken: cfg.AdminToken, PerTenant: true,
				ContainerName: cfg.HermesCentralContainerName, RegistryImage: cfg.HermesCentralRegistryImage,
				ChannelTag: cfg.HermesCentralChannelTag, TempName: cfg.HermesCentralTempName,
				Network: cfg.HermesCentralNetwork, VolumePath: cfg.HermesCentralVolumePath,
				Port: cfg.HermesCentralPort, TempPort: cfg.HermesCentralTempPort, APIKey: cfg.HermesAPIKey,
			}))
		}
		deps.Modules = append(deps.Modules, billing.New(pool, billing.Options{
			WebhookSecret: cfg.RevenueCatWebhookSecret, AdminToken: cfg.AdminToken, EnforcementEnabled: entitlement.EnforcementEnabled(cfg.BillingEnforcementEnabled),
		}))
		if m := marketplace.New(pool, cfg.LVSecretMasterKey, pluginOptions, identify, limits, paths); m != nil { // nil without LV_SECRET_MASTER_KEY, as in Swift
			deps.Modules = append(deps.Modules, m)
		}
		// One disk guard for wave 4's writers.
		disk := diskguard.New(paths.Root, cfg.StorageMinFreeBytes, cfg.StorageMinFreeRatio, diskguard.Options{})
		memories := memorystore.New(pool, nil)
		registered = append(registered, j1Jobs(pool, cfg, disk)...)
		// LapseArchiverService (7A): dry run unless LV_LAPSE_DRY_RUN=false; off unless LV_GO_JOBS names it.
		registered = append(registered, (&workers.LapseArchiver{
			Pool: pool, Paths: paths, ColdStorageRoot: cfg.ColdStoragePath(), DryRun: cfg.LapseDryRun,
		}).Job())
		agent, stopAgent := newAgent(ctx, pool, cfg, box, paths, identify, limits, embeddings, sink, meModule, disk)
		agent.sockets = sockets
		defer stopAgent()
		links, enrichment := linkCaptureService(pool, cfg, paths, embeddings, memories, disk, sink)
		deps.Modules = append(deps.Modules,
			vault.New(pool, vault.Options{
				Paths: paths, HermesDataRoot: cfg.HermesDataRoot, Identify: identify, Limits: limits,
				Spaces: spaces.New(pool, paths, identify), Embeddings: embeddings,
				Guard:   disk,
				Sink:    sink,
				Latches: onboardlatch.New(pool), Analytics: analytics,
				CaptureFiling: agent.captureFiling(),
			}),
			todos.New(pool, paths, todos.Options{Disk: disk, Memories: memories, Embeddings: embeddings}),
			captureModule(pool, cfg, box, limits, links, analytics),
		)
		if workflows.Enabled(cfg.CerberusStudioEnabled) {
			deps.Modules = append(deps.Modules, workflowsModule(pool, cfg, box, identify, limits))
		}
		deps.Modules = append(deps.Modules, agent.modules(links, enrichment)...)
		deps.Modules = append(deps.Modules, agent.containerModules(keys)...)
		// /v1/ws (5J) flips with kb-compile, the device result route, mcp and skills.
		deps.Modules = append(deps.Modules, wsroute.New(pool, sockets, identify))
		deps.Modules = append(deps.Modules, agent.agentsModule())
		deps.Modules = append(deps.Modules, agent.mcpModule(deviceRPC))
		deps.Modules = append(deps.Modules, agent.routerModules()...)
		deps.Modules = append(deps.Modules, agent.chatModules(links, enrichment, analytics)...)
		deps.Modules = append(deps.Modules, agent.skillsModule(deviceRPC))
		if m := agent.pluginsModule(); m != nil { // nil without LV_SECRET_MASTER_KEY, as in Swift
			deps.Modules = append(deps.Modules, m)
		}
		if m := agent.hermesRunsModule(); m != nil {
			deps.Modules = append(deps.Modules, m)
		}
		registered = append(registered, agent.hermesRunsJob()...)
	}
	// Deferred after the sink and the pool, so jobs stop first and can still use both.
	stopJobs, err := startJobs(ctx, cfg, registered)
	if err != nil {
		return err
	}
	defer stopJobs()
	e := httpserver.New(cfg, deps)
	sc := echo.StartConfig{
		Address:         net.JoinHostPort(cmd.HTTPHost, strconv.Itoa(cmd.HTTPPort)),
		HideBanner:      true,
		HidePort:        true,
		GracefulTimeout: 10 * time.Second,
	}
	slog.Info("listening", "address", sc.Address)
	if err := sc.Start(ctx, httpserver.Handler(cfg, e)); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// workflowsModule builds the workflow routes, the "if workflowStudioEnabled" block of
// App+build.swift without its background services, which stay in Swift.
func workflowsModule(pool *pgxpool.Pool, cfg config.Config, box *secretbox.Box, identify echo.MiddlewareFunc, limits *ratelimit.Store) httpserver.Module {
	var isUserOverride func(context.Context, uuid.UUID) bool
	if box != nil {
		if gateway, err := url.Parse(cfg.HermesGatewayURL); err == nil {
			isUserOverride = selfimprove.ResolverOverride(hermesresolve.New(pool, box, ssrfguard.Guard{
				AllowPrivateRanges: strings.EqualFold(cfg.BYOHermesAllowPrivate, "true"),
				RequireHTTPS:       cfg.BYOHermesRequireHTTPS(),
				AllowTailnetHTTP:   strings.EqualFold(cfg.BYOHermesAllowTailnet, "true"),
			}, gateway))
		}
	}
	return workflows.New(pool, workflows.Options{
		Identify: identify,
		Limits:   limits,
		Gate: entitlement.Middleware(pool, entitlement.WorkflowAutomation,
			entitlement.EnforcementEnabled(cfg.BillingEnforcementEnabled), entitlement.PGCredentials(pool, box)),
		Box:         box,
		Preferences: llm.NewPreferenceRepository(pool),
		Activity:    selfimprove.NewActivity(pool, cfg.SelfImprovementEnabled, isUserOverride),
		// providerAPIKey("openRouter", alias: "openrouter.api_key").
		ManagedInferenceAvailable: cfg.LLMOpenRouterAPIKey != "" || cfg.LLMOpenRouterAPIKeyLegacy != "" || cfg.OpenRouterAPIKey != "",
	})
}

// startJobs starts the jobs LV_GO_JOBS enables. It fails on a name no registered job
// has. The returned stop cancels them and waits, and the HTTP server can also end
// without a signal, so stop can't rely on ctx being canceled.
func startJobs(ctx context.Context, cfg config.Config, registered []jobs.Job) (stop func(), err error) {
	runner, err := jobs.New(cfg, registered, jobs.Options{})
	if err != nil {
		return nil, err
	}
	jobsCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.Run(jobsCtx)
	}()
	return func() {
		cancel()
		<-done
	}, nil
}

// linkCaptureService builds the one link-capture pipeline: POST /v1/capture/safari and
// /v1/import both stage through it. internal/capture holds the service because import
// shares it.
func linkCaptureService(pool *pgxpool.Pool, cfg config.Config, paths vaultpaths.Paths,
	embeddings embedding.Service, memories *memorystore.Repository, disk *diskguard.Guard, unlocks *achievementsink.Sink,
) (*linkcapture.Service, *linkcapture.EnrichmentService) {
	store := vaultfiles.NewStore(pool, paths)
	enrichment := linkcapture.NewEnrichmentService(linkcapture.EnrichmentOptions{
		Paths: paths, Store: store, Embeddings: embeddings, Memories: memories,
		Jina: &linkcapture.JinaEnricher{APIKey: cfg.JinaAPIKey},
		Hooks: &linkcapture.HookDispatcher{
			Registry:  linkcapture.NewHookRegistry(linkcapture.ReadingTimeHook{}),
			Installed: linkcapture.InstalledSlugsFrom(pool),
		},
	})
	return linkcapture.New(pool, linkcapture.Options{
		Paths: paths, Store: store, Disk: disk, Achievements: unlocks, Latches: onboardlatch.New(pool),
		Enrichment: enrichment,
	}), enrichment
}

// captureModule builds POST /v1/capture/safari over the shared pipeline.
func captureModule(pool *pgxpool.Pool, cfg config.Config, box *secretbox.Box, limits *ratelimit.Store, service *linkcapture.Service, analytics *posthog.Client) httpserver.Module {
	gate := entitlement.Middleware(pool, entitlement.Capture,
		entitlement.EnforcementEnabled(cfg.BillingEnforcementEnabled), entitlement.PGCredentials(pool, box))
	return capture.New(pool, service, gate, limits, analytics)
}

// startAchievementSink starts the sink that records achievements after a response.
// The returned stop drains it: the caller defers it inside the pool's lifetime,
// so it runs after the HTTP server stops and before the pool closes.
func startAchievementSink(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, onUnlock func(uuid.UUID, achievements.Sub)) (sink *achievementsink.Sink, stop func()) {
	pusher := apns.New(pool, apns.Config{
		Enabled: cfg.APNSEnabled, BundleID: cfg.APNSBundleID, TeamID: cfg.APNSTeamID, KeyID: cfg.APNSKeyID,
		PrivateKeyPath: cfg.APNSPrivateKeyPath, Environment: cfg.APNSEnvironment,
	})
	sink = achievementsink.New(achievements.NewRecorder(pool), pusher, achievementsink.Options{OnUnlock: onUnlock})
	sinkCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		sink.Run(sinkCtx)
	}()
	return sink, func() {
		cancel()
		<-done
	}
}

// calendarModules builds calendar and mail, which share the Google grant and
// flip together. Without a usable LV_SECRET_MASTER_KEY there is no Google side:
// Swift then registers only /v1/calendar/sync and the brief.
// voiceConfig maps the env onto the transcribe.* reads.
func voiceConfig(cfg config.Config) voice.Config {
	return voice.Config{
		Provider: cfg.TranscribeProvider, BaseURL: cfg.TranscribeOpenAIBaseURL, APIKey: cfg.TranscribeOpenAIAPIKey,
		Model: cfg.TranscribeOpenAIModel, MtokPerSecond: cfg.TranscribeOpenAIMtokPerSecond,
		ImputedUSDPerAudioMinute: cfg.TranscribeOpenAIImputedPerMin,
		StubText:                 cfg.TranscribeStubText, StubLanguage: cfg.TranscribeStubLanguage,
		StubConfidence: cfg.TranscribeStubConfidence, StubDurationSeconds: cfg.TranscribeStubDurationSeconds,
	}
}

// visionConfig maps the env onto App+build.swift's vision.embed.* reads.
func visionConfig(cfg config.Config) vision.Config {
	return vision.Config{
		Provider:         cfg.VisionEmbedProvider,
		CohereAPIKey:     cfg.VisionEmbedProviderCohereKey,
		CohereBaseURL:    cfg.VisionEmbedProviderCohereURL,
		CohereModel:      cfg.VisionEmbedProviderCohereModel,
		StubDim:          cfg.VisionEmbedStubDim,
		StubFill:         cfg.VisionEmbedStubFill,
		StubModel:        cfg.VisionEmbedStubModel,
		StubSourceWidth:  cfg.VisionEmbedStubSourceWidth,
		StubSourceHeight: cfg.VisionEmbedStubSourceHeight,
	}
}

// embeddingConfig maps the env onto EmbeddingProviderRegistry.bootstrap's reads.
func embeddingConfig(cfg config.Config) embedding.Config {
	return embedding.Config{
		Provider:      cfg.EmbeddingProvider,
		FallbackChain: cfg.EmbeddingFallbackChain,
		MonthlyCap:    cfg.EmbeddingMonthlyTokenCapDefault,
		OpenAIAPIKey:  cfg.LLMOpenAIAPIKey,
		OpenAIBaseURL: cfg.LLMOpenAIBaseURL,
		OpenRouterAPIKeys: []string{
			cfg.LLMOpenRouterAPIKey, cfg.LLMOpenRouterAPIKeyLegacy, cfg.OpenRouterAPIKey, cfg.OpenRouterFallbackAPIKey,
		},
		OpenRouterBaseURL: cfg.EmbeddingOpenRouterBaseURL,
		OpenRouterModel:   cfg.EmbeddingOpenRouterModel,
		NomicAPIKey:       cfg.EmbeddingNomicAPIKey,
	}
}

// memoryModule builds /v1/memory. POST /v1/memory/upsert sits behind BYO Hermes
// resolution (only with a SecretBox, as Swift's) and the capture gate.
func memoryModule(pool *pgxpool.Pool, cfg config.Config, box *secretbox.Box, identify echo.MiddlewareFunc, limits *ratelimit.Store, paths vaultpaths.Paths, embeddings embedding.Service, sink *achievementsink.Sink, today *me.Module) httpserver.Module {
	opts := memory.Options{
		Identify:   identify,
		Limits:     limits,
		Paths:      paths,
		Embeddings: embeddings,
		Indexer: chunkindex.NewIndexer(chunkindex.NewChunkRepository(pool), embeddings,
			chunkindex.NewLinkRepository(pool)),
		Preferences: llm.NewPreferenceRepository(pool),
		CaptureGate: entitlement.MiddlewareWithHermes(pool, entitlement.Capture,
			entitlement.EnforcementEnabled(cfg.BillingEnforcementEnabled), entitlement.PGCredentials(pool, box), hermesresolve.IsUserOverride),
		Achievements:           sink,
		OnUpserted:             today.InvalidateToday,
		HybridExecutionEnabled: strings.EqualFold(cfg.HybridExecutionEnabled, "true"),
		RelatedMaxDistance:     float32(cfg.MemoryRelatedMaxDistance),
	}
	opts.Hermes = byoHermes(pool, cfg, box)
	return memory.New(pool, memorystore.New(pool, nil), opts)
}

// hermesResolver is HermesEndpointResolver, which Swift mounts only with a SecretBox.
// Nil without one, or without a gateway URL that parses.
func hermesResolver(pool *pgxpool.Pool, cfg config.Config, box *secretbox.Box) *hermesresolve.Resolver {
	if box == nil {
		return nil
	}
	gateway, err := url.Parse(cfg.HermesGatewayURL)
	if err != nil {
		return nil
	}
	return hermesresolve.New(pool, box, ssrfguard.Guard{
		AllowPrivateRanges: strings.EqualFold(cfg.BYOHermesAllowPrivate, "true"),
		RequireHTTPS:       cfg.BYOHermesRequireHTTPS(),
		AllowTailnetHTTP:   strings.EqualFold(cfg.BYOHermesAllowTailnet, "true"),
	}, gateway)
}

// selfImprovementModule builds /v1/me/improvement. SelfImprovementScheduler stays in
// Swift and picks up the rows these routes queue.
func selfImprovementModule(pool *pgxpool.Pool, cfg config.Config, box *secretbox.Box, identify echo.MiddlewareFunc, paths vaultpaths.Paths) httpserver.Module {
	catalog := skills.NewCatalog(paths)
	if strings.EqualFold(cfg.SkillsBuiltinScanEnabled, "false") {
		catalog = skills.NewVaultCatalog(paths)
	}
	opts := selfimprovement.Options{
		Identify:        identify,
		Paths:           paths,
		Soul:            soul.New(paths, cfg.HermesDataRoot),
		Catalog:         catalog,
		EconomyModel:    cfg.SelfImprovementEconomyModel,
		GloballyEnabled: cfg.SelfImprovementEnabled,
	}
	if resolver := hermesResolver(pool, cfg, box); resolver != nil {
		opts.IsUserOverride = selfimprove.ResolverOverride(resolver)
	}
	return selfimprovement.New(pool, opts)
}

func calendarModules(pool *pgxpool.Pool, cfg config.Config, identify echo.MiddlewareFunc, limits *ratelimit.Store, embeddings embedding.Service, analytics *posthog.Client) []httpserver.Module {
	var google *calendar.Google
	if box, err := secretbox.New(cfg.LVSecretMasterKey); err == nil {
		google = calendar.NewGoogle(pool, calendar.GoogleOptions{
			Box: box, ClientID: cfg.GoogleCalendarClientID, ClientSecret: cfg.GoogleCalendarClientSecret,
			RedirectURI: cfg.GoogleCalendarRedirectURI, WebReturnOrigins: cfg.CORSAllowedOrigins,
		})
	}
	return []httpserver.Module{
		calendar.New(pool, identify, limits, calendar.Options{
			Google:      google,
			MaxDistance: float32(cfg.MeetingBriefMaxDistance),
			Embeddings:  embeddings,
			Analytics:   analytics,
		}),
		mail.New(google, analytics),
	}
}
