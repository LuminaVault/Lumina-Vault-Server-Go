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
	"lumina/internal/chunkindex"
	"lumina/internal/config"
	"lumina/internal/db"
	"lumina/internal/embedding"
	"lumina/internal/entitlement"
	"lumina/internal/hermesresolve"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/modules/account"
	"lumina/internal/modules/achievements"
	"lumina/internal/modules/apple"
	"lumina/internal/modules/calendar"
	"lumina/internal/modules/community"
	"lumina/internal/modules/dashboard"
	"lumina/internal/modules/devices"
	"lumina/internal/modules/health"
	"lumina/internal/modules/kanban"
	"lumina/internal/modules/mail"
	"lumina/internal/modules/marketplace"
	"lumina/internal/modules/me"
	"lumina/internal/modules/memory"
	"lumina/internal/modules/news"
	"lumina/internal/modules/onboarding"
	"lumina/internal/modules/profiles"
	"lumina/internal/modules/projects"
	"lumina/internal/modules/reminders"
	"lumina/internal/modules/sessions"
	"lumina/internal/modules/spaces"
	"lumina/internal/modules/teams"
	"lumina/internal/posthog"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/ssrfguard"
	"lumina/internal/telemetry"
	"lumina/internal/vaultpaths"
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
		// Identify: vault-scoped routes check X-Vault-ID before identity (decisions.md D-identify).
		identify := auth.Identify(keys, auth.PGUsers(pool))
		paths := vaultpaths.Paths{Root: cfg.VaultRootPath}
		// SecretBox: nil without LV_SECRET_MASTER_KEY, as in Swift.
		box, _ := secretbox.New(cfg.LVSecretMasterKey)
		embeddings := embedding.New(pool, embeddingConfig(cfg), box)
		meModule := me.New(pool, identify, limits, me.Options{RecapThemesEnabled: cfg.RecapThemesEnabled})
		// One achievements inbox per process. It stops after the HTTP server and
		// before the pool closes: this defer runs before pool.Close's.
		sink := achievementsink.New(achievements.NewRecorder(pool), apns.New(pool, apns.Config{
			Enabled: cfg.APNSEnabled, BundleID: cfg.APNSBundleID, TeamID: cfg.APNSTeamID, KeyID: cfg.APNSKeyID,
			PrivateKeyPath: cfg.APNSPrivateKeyPath, Environment: cfg.APNSEnvironment,
		}), achievementsink.Options{OnUnlock: func(tenant uuid.UUID, _ achievements.Sub) { meModule.InvalidateToday(tenant) }})
		sinkCtx, stopSink := context.WithCancel(context.WithoutCancel(ctx))
		sinkDone := make(chan struct{})
		go func() {
			defer close(sinkDone)
			sink.Run(sinkCtx)
		}()
		defer func() {
			stopSink()
			<-sinkDone
		}()
		// Migrated modules (modules.yaml status ≥ parity). Traffic only reaches
		// them through the api ingress.extraPaths that name them.
		deps.Modules = []httpserver.Module{
			projects.New(pool),
			kanban.New(pool, identify, limits, paths),
			spaces.New(pool, paths, identify),
			profiles.New(pool),
			sessions.New(pool),
			// apple, devices and reminders flip together (D-shared-prefixes).
			apple.New(pool, apple.Options{Embedder: embeddings, Analytics: analytics}),
			devices.New(pool, cfg.APNSBundleID),
			reminders.New(pool, nil, cfg.HermesDefaultModel),
			achievements.New(pool, identify, limits),
			teams.New(pool, identify, limits, paths, teams.NewSender(teams.SenderConfig{
				Kind: cfg.EmailKind, APIKey: cfg.EmailResendAPIKey, FromAddress: cfg.EmailFromAddress,
				ReplyTo: cfg.EmailReplyTo, BaseURL: cfg.TeamInviteBaseURL,
			}, slog.Default().With("module", "teams"))),
			meModule,
			dashboard.New(pool, identify),
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
		deps.Modules = append(deps.Modules, calendarModules(pool, cfg, identify, limits, embeddings, analytics)...)
		if m := news.New(pool, cfg, identify, limits); m != nil { // nil without LV_SECRET_MASTER_KEY, as in Swift
			deps.Modules = append(deps.Modules, m)
		}
		deps.Modules = append(deps.Modules, community.New(pool, community.Config{
			Enabled: community.ParseFlag(cfg.CommunityEnabled), PostingEnabled: community.ParseFlag(cfg.CommunityPostingEnabled),
			AdminToken: cfg.AdminToken,
		}, identify, limits, nil))
		if m := marketplace.New(pool, cfg.LVSecretMasterKey, pluginOptions, identify, limits, paths); m != nil { // nil without LV_SECRET_MASTER_KEY, as in Swift
			deps.Modules = append(deps.Modules, m)
		}
	}
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

// calendarModules builds calendar and mail, which share the Google grant and
// flip together. Without a usable LV_SECRET_MASTER_KEY there is no Google side:
// Swift then registers only /v1/calendar/sync and the brief.
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
	if box != nil {
		if gateway, err := url.Parse(cfg.HermesGatewayURL); err == nil {
			opts.Hermes = hermesresolve.New(pool, box, ssrfguard.Guard{
				AllowPrivateRanges: strings.EqualFold(cfg.BYOHermesAllowPrivate, "true"),
				RequireHTTPS:       cfg.BYOHermesRequireHTTPS(),
				AllowTailnetHTTP:   strings.EqualFold(cfg.BYOHermesAllowTailnet, "true"),
			}, gateway).Middleware()
		}
	}
	return memory.New(pool, memorystore.New(pool, nil), opts)
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
