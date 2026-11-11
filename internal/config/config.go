// Package config loads the server configuration from the same environment
// variables the Swift server reads, so the sealed api-env-* secrets and the
// infra values files serve both images unchanged. Every env tag here must be
// KeyToEnv of a key in docs/migration/config-keys.txt (enforced by a test);
// LV_GO_JOBS and TTS_BASE_URL are the only Go-only names.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	PostgresHost     string `env:"POSTGRES_HOST" envDefault:"localhost"`
	PostgresPort     int    `env:"POSTGRES_PORT" envDefault:"5432"`
	PostgresUser     string `env:"POSTGRES_USER"`
	PostgresPassword string `env:"POSTGRES_PASSWORD"`
	PostgresDatabase string `env:"POSTGRES_DATABASE"`
	FluentEnabled    bool   `env:"FLUENT_ENABLED" envDefault:"true"`

	JWTHMACSecret  string `env:"JWT_HMAC_SECRET"`
	JWTKID         string `env:"JWT_KID"`
	JWTHMACSecrets string `env:"JWT_HMAC_SECRETS"`

	LVSecretMasterKey string `env:"LV_SECRET_MASTER_KEY"`
	LVEnvironment     string `env:"LV_ENVIRONMENT"`

	CORSAllowedOrigins []string `env:"CORS_ALLOWED_ORIGINS" envSeparator:","`

	OTelEnabled     bool   `env:"OTEL_ENABLED"`
	OTelServiceName string `env:"OTEL_SERVICE_NAME" envDefault:"luminavault-api"`
	LogLevel        string `env:"LOG_LEVEL" envDefault:"info"`

	RedisURL string `env:"REDIS_URL"`

	// Wave 2 apple/devices/reminders.
	APNSBundleID                    string `env:"APNS_BUNDLE_ID"`
	EmbeddingProvider               string `env:"EMBEDDING_PROVIDER" envDefault:"deterministic"`
	EmbeddingMonthlyTokenCapDefault int64  `env:"EMBEDDING_MONTHLY_TOKEN_CAP_DEFAULT" envDefault:"2000000"`
	HermesDefaultModel              string `env:"HERMES_DEFAULT_MODEL" envDefault:"hermes-3"`
	// Embedding chain (internal/embedding). The OpenAI and OpenRouter keys are the
	// LLM routing's own, so there is no second secret to seal. Note the sealed
	// LLM_PROVIDER_OPENAI_APIKEY is not LLM_PROVIDER_OPENAI_API_KEY: Swift's registry
	// reads only the latter for embeddings, so Go does too.
	EmbeddingFallbackChain     string `env:"EMBEDDING_FALLBACK_CHAIN"`
	EmbeddingNomicAPIKey       string `env:"EMBEDDING_NOMIC_API_KEY"`
	EmbeddingOpenRouterBaseURL string `env:"EMBEDDING_OPEN_ROUTER_BASE_URL"`
	EmbeddingOpenRouterModel   string `env:"EMBEDDING_OPEN_ROUTER_MODEL"`
	LLMOpenAIAPIKey            string `env:"LLM_PROVIDER_OPENAI_API_KEY"`
	LLMOpenAIBaseURL           string `env:"LLM_PROVIDER_OPENAI_BASE_URL"`
	LLMOpenRouterAPIKey        string `env:"LLM_PROVIDER_OPEN_ROUTER_API_KEY"`
	LLMOpenRouterAPIKeyLegacy  string `env:"LLM_PROVIDER_OPENROUTER_APIKEY"`
	OpenRouterAPIKey           string `env:"OPENROUTER_API_KEY"`
	OpenRouterFallbackAPIKey   string `env:"OPENROUTER_FALLBACK_API_KEY"`
	// Calendar and Gmail (calendar, mail modules): oauth.googleCalendar.* and meeting.brief.maxDistance.
	GoogleCalendarClientID     string  `env:"OAUTH_GOOGLE_CALENDAR_CLIENT_ID"`
	GoogleCalendarClientSecret string  `env:"OAUTH_GOOGLE_CALENDAR_CLIENT_SECRET"`
	GoogleCalendarRedirectURI  string  `env:"OAUTH_GOOGLE_CALENDAR_REDIRECT_URI"`
	MeetingBriefMaxDistance    float64 `env:"MEETING_BRIEF_MAX_DISTANCE" envDefault:"0.4"`
	// Shared feed aggregator for the news ticker (news module). Raw strings:
	// the module splits FEEDS_DEFAULT_FEEDS exactly as Swift does.
	FeedsBaseURL      string `env:"FEEDS_BASE_URL"`
	FeedsDefaultFeeds string `env:"FEEDS_DEFAULT_FEEDS"`

	// Image embeddings (vision module): vision.embed.*. The sealed api-env spells the
	// key VISION_EMBED_PROVIDER_COHERE_APIKEY. Swift's encoder reads only _API_KEY, and
	// so does Go: until someone seals that name, both answer 503 (see cutover-log).
	// Speech to text (internal/modules/voice). An empty base URL means the in-cluster
	// whisper; there is deliberately no default here, so the URL lives in one place.
	TranscribeProvider            string  `env:"TRANSCRIBE_PROVIDER" envDefault:"openai_compatible"`
	TranscribeOpenAIBaseURL       string  `env:"TRANSCRIBE_PROVIDER_OPENAI_BASE_URL"`
	TranscribeOpenAIAPIKey        string  `env:"TRANSCRIBE_PROVIDER_OPENAI_API_KEY"`
	TranscribeOpenAIModel         string  `env:"TRANSCRIBE_PROVIDER_OPENAI_MODEL"`
	TranscribeOpenAIMtokPerSecond float64 `env:"TRANSCRIBE_RATECARD_OPENAI_MTOK_PER_SECOND"`
	TranscribeOpenAIImputedPerMin float64 `env:"TRANSCRIBE_RATECARD_OPENAI_IMPUTED_USD_PER_AUDIO_MINUTE"`
	TranscribeStubText            string  `env:"TRANSCRIBE_STUB_TEXT" envDefault:"stub transcript"`
	TranscribeStubLanguage        string  `env:"TRANSCRIBE_STUB_LANGUAGE" envDefault:"en"`
	TranscribeStubConfidence      float64 `env:"TRANSCRIBE_STUB_CONFIDENCE" envDefault:"0.95"`
	TranscribeStubDurationSeconds float64 `env:"TRANSCRIBE_STUB_DURATION_SECONDS" envDefault:"30"`
	// TTSBaseURL is Go-only and turns /v1/tts on: a self-hosted, OpenAI-shaped speech
	// service, base URL with its version prefix. See cmd/App/main.go.
	TTSBaseURL                     string  `env:"TTS_BASE_URL"`
	TTSDefaultModel                string  `env:"TTS_DEFAULT_MODEL" envDefault:"tts-1"`
	VisionEmbedProvider            string  `env:"VISION_EMBED_PROVIDER" envDefault:"cohere"`
	VisionEmbedProviderCohereKey   string  `env:"VISION_EMBED_PROVIDER_COHERE_API_KEY"`
	VisionEmbedProviderCohereURL   string  `env:"VISION_EMBED_PROVIDER_COHERE_BASE_URL" envDefault:"https://api.cohere.com"`
	VisionEmbedProviderCohereModel string  `env:"VISION_EMBED_PROVIDER_COHERE_MODEL" envDefault:"embed-image-v3.0"`
	VisionEmbedStubDim             int     `env:"VISION_EMBED_STUB_DIM" envDefault:"1024"`
	VisionEmbedStubFill            float64 `env:"VISION_EMBED_STUB_FILL" envDefault:"0.1"`
	VisionEmbedStubModel           string  `env:"VISION_EMBED_STUB_MODEL" envDefault:"stub-clip"`
	VisionEmbedStubSourceWidth     int     `env:"VISION_EMBED_STUB_SOURCE_WIDTH" envDefault:"768"`
	VisionEmbedStubSourceHeight    int     `env:"VISION_EMBED_STUB_SOURCE_HEIGHT" envDefault:"768"`

	// Team invitations (teams module): makeTeamInvitationSender's inputs.
	EmailKind         string `env:"EMAIL_KIND" envDefault:"logging"`
	EmailResendAPIKey string `env:"EMAIL_RESEND_API_KEY"`
	EmailFromAddress  string `env:"EMAIL_FROM_ADDRESS"`
	EmailReplyTo      string `env:"EMAIL_REPLY_TO"`
	TeamInviteBaseURL string `env:"TEAM_INVITE_BASE_URL" envDefault:"http://localhost:5173"`
	// MCPPublicBaseURL is mcp.publicBaseUrl, the base of the /v1/mcp URL in agent
	// connection setup snippets. Empty falls back to IngestionPublicBaseURL, then
	// to https://api.luminavault.com (App+build).
	MCPPublicBaseURL       string `env:"MCP_PUBLIC_BASE_URL"`
	IngestionPublicBaseURL string `env:"INGESTION_PUBLIC_BASE_URL"`
	// RecapThemesEnabled is recap.themesEnabled: the model-written card of
	// /v1/me/recap/period (me module). Off reports it as disabled.
	RecapThemesEnabled bool `env:"RECAP_THEMES_ENABLED" envDefault:"true"`

	// Community board (community module). Raw strings: the module reads them
	// with Swift's flag() rules, so "0" and "off" are off too.
	CommunityEnabled        string `env:"COMMUNITY_ENABLED"`
	CommunityPostingEnabled string `env:"COMMUNITY_POSTING_ENABLED" envDefault:"true"`
	AdminToken              string `env:"ADMIN_TOKEN"`

	// WASM plugin runner and artifact store (marketplace module).
	PluginRunnerURL          string `env:"PLUGIN_RUNNER_URL" envDefault:"http://plugin-runner:8090"`
	PluginRunnerToken        string `env:"PLUGIN_RUNNER_TOKEN"`
	PluginArtifactRoot       string `env:"PLUGIN_ARTIFACT_ROOT" envDefault:"/app/data/plugin-artifacts"`
	PluginArtifactSigningKey string `env:"PLUGIN_ARTIFACT_SIGNING_KEY"`

	// VaultRootPath holds tenants/<UUID>/{raw,wiki,memories} on the shared PVC.
	VaultRootPath string `env:"VAULT_ROOT_PATH" envDefault:"/tmp/luminavault"`
	// storage.minFreeBytes and storage.minFreeRatio: internal/diskguard's floors
	// and Swift's defaults (3 GiB, 10%). Zero switches a floor off.
	StorageMinFreeBytes int64   `env:"STORAGE_MIN_FREE_BYTES" envDefault:"3221225472"`
	StorageMinFreeRatio float64 `env:"STORAGE_MIN_FREE_RATIO" envDefault:"0.10"`
	// storage.quota.*: per-tenant ceilings on ingestion, Swift's defaults (5 GiB,
	// 100 GiB, 1 TiB, 1 GiB). 0 denies growth; a negative value is unlimited.
	StorageQuotaEnabled       bool  `env:"STORAGE_QUOTA_ENABLED" envDefault:"true"`
	StorageQuotaTrialBytes    int64 `env:"STORAGE_QUOTA_TRIAL_BYTES" envDefault:"5368709120"`
	StorageQuotaProBytes      int64 `env:"STORAGE_QUOTA_PRO_BYTES" envDefault:"107374182400"`
	StorageQuotaUltimateBytes int64 `env:"STORAGE_QUOTA_ULTIMATE_BYTES" envDefault:"1099511627776"`
	StorageQuotaFreeBytes     int64 `env:"STORAGE_QUOTA_FREE_BYTES" envDefault:"1073741824"`

	// Wave 3 account/health. HermesDataRoot holds profiles/<id>/ (account
	// deletion soft-renames it). BillingEnforcementEnabled stays a string:
	// Swift enables it only when it lowercases to "true" (entitlement package).
	HermesDataRoot            string `env:"HERMES_DATA_ROOT" envDefault:"/app/data/hermes"`
	BillingEnforcementEnabled string `env:"BILLING_ENFORCEMENT_ENABLED" envDefault:"false"`

	// Wave 7A billing. An empty webhook secret makes the RevenueCat webhook a 500,
	// as in Swift. BillingTierOverrideEmails is TierOverrideAllowlist's
	// `email=tier,...` (read by auth when token minting moves). Cold storage
	// defaults to <VAULT_ROOT_PATH>/cold-storage (ColdStoragePath). The lapse
	// archiver only logs what it would do until LV_LAPSE_DRY_RUN is false.
	RevenueCatWebhookSecret   string `env:"REVENUECAT_WEBHOOK_SECRET"`
	BillingTierOverrideEmails string `env:"BILLING_TIER_OVERRIDE_EMAILS"`
	BillingColdStoragePath    string `env:"BILLING_COLD_STORAGE_PATH"`
	LapseDryRun               bool   `env:"LV_LAPSE_DRY_RUN" envDefault:"true"`

	// Job J1 (internal/workers). AccountDeletionPurgeAfterDays is
	// account.deletion.purgeAfterDays, the recovery window before DeletedDataPurgeService
	// erases a deleted account's folders. RetrievalLeakReportSurfaceInsight is
	// retrieval.leakReport.surfaceInsight; only "true", in any case, turns it on.
	AccountDeletionPurgeAfterDays     int    `env:"ACCOUNT_DELETION_PURGE_AFTER_DAYS" envDefault:"30"`
	RetrievalLeakReportSurfaceInsight string `env:"RETRIEVAL_LEAK_REPORT_SURFACE_INSIGHT" envDefault:"false"`

	// Daily caps in million tokens, and the model the trial tier's soft cap degrades
	// to (usagemeter.Budgets). SelfImprovementEnabled is the operator's switch
	// (selfimprove.Activity).
	UsageFreeMtokDaily     float64 `env:"USAGE_FREE_MTOK_DAILY" envDefault:"1.0"`
	UsagePerSkillMtokDaily float64 `env:"USAGE_PER_SKILL_MTOK_DAILY" envDefault:"0.2"`
	UsageDegradeModel      string  `env:"USAGE_DEGRADE_MODEL" envDefault:"hermes-3-small"`
	SelfImprovementEnabled bool    `env:"SELF_IMPROVEMENT_ENABLED" envDefault:"true"`
	// SelfImprovementEconomyModel is selfImprovement.economyModel. Unset means
	// UsageDegradeModel, Swift's default for it; set but empty stays empty, which
	// the status route reports as no economy model. See LoadFrom.
	SelfImprovementEconomyModel string `env:"SELF_IMPROVEMENT_ECONOMY_MODEL"`
	// SkillsBuiltinScanEnabled is skills.builtinScan.enabled. Only "false", in any
	// case, turns the bundled skills off (skills.NewVaultCatalog).
	SkillsBuiltinScanEnabled string `env:"SKILLS_BUILTIN_SCAN_ENABLED" envDefault:"true"`

	// CerberusStudioEnabled is raw: workflows.Enabled reads it as Swift's reader.bool
	// does. Off unmounts /v1/workflows, /v1/workflow-templates and /v1/workflow-hooks.
	CerberusStudioEnabled string `env:"CERBERUS_STUDIO_ENABLED" envDefault:"true"`

	// Hermes tenant containers (wave 6D/6E). DockerBinaryPath is forked for every
	// container operation; in a pod with no docker each call fails as in Swift.
	DockerBinaryPath              string `env:"DOCKER_BINARY_PATH" envDefault:"/usr/bin/docker"`
	HermesPerTenantNetwork        string `env:"HERMES_PER_TENANT_NETWORK" envDefault:"luminavault-hermes-net"`
	HermesPerTenantImage          string `env:"HERMES_PER_TENANT_IMAGE" envDefault:"luminavault-hermes:local"`
	HermesPerTenantDataRootBase   string `env:"HERMES_PER_TENANT_DATA_ROOT_BASE" envDefault:"/app/data/hermes-tenants"`
	HermesPerTenantPortRangeStart int    `env:"HERMES_PER_TENANT_PORT_RANGE_START" envDefault:"9000"`
	HermesPerTenantPortRangeEnd   int    `env:"HERMES_PER_TENANT_PORT_RANGE_END" envDefault:"9500"`
	HermesPerTenantIdleTTLSeconds int    `env:"HERMES_PER_TENANT_IDLE_TTL_SECONDS" envDefault:"1800"`
	HermesPerTenantMemoryLimit    string `env:"HERMES_PER_TENANT_MEMORY_LIMIT" envDefault:"512m"`
	HermesPerTenantCPULimit       string `env:"HERMES_PER_TENANT_CPU_LIMIT" envDefault:"1.0"`
	// HermesAudioProxyBaseURL includes /v1; empty seeds no voice into containers.
	HermesAudioProxyBaseURL string `env:"HERMES_AUDIO_PROXY_BASE_URL"`
	// MnemosyneEnabled is raw: on when it lowercases to "true", as Swift reads it.
	MnemosyneEnabled string `env:"MNEMOSYNE_ENABLED" envDefault:"true"`
	// PhotonSidecarURL and PhotonSidecarToken reach the central iMessage sidecar.
	// The token also gates POST /v1/gateways/photon/inbound; empty disables it.
	PhotonSidecarURL   string `env:"PHOTON_SIDECAR_URL" envDefault:"http://photon-sidecar:8789"`
	PhotonSidecarToken string `env:"PHOTON_SIDECAR_TOKEN"`

	// JinaAPIKey is Jina's reader, tier-2 of link enrichment. Unset still calls it,
	// without the header, as in Swift.
	JinaAPIKey string `env:"JINA_API_KEY"`

	// APNS unlock pushes (apns package). APNSEnabled stays a string: Swift
	// enables it only when it lowercases to "true".
	APNSEnabled        string `env:"APNS_ENABLED" envDefault:"false"`
	APNSTeamID         string `env:"APNS_TEAM_ID"`
	APNSKeyID          string `env:"APNS_KEY_ID"`
	APNSPrivateKeyPath string `env:"APNS_PRIVATE_KEY_PATH"`
	APNSEnvironment    string `env:"APNS_ENVIRONMENT" envDefault:"development"`

	// BYO Hermes resolution (hermesresolve, ssrfguard). Strings, read with
	// Swift's lowercased() == "true". BYOHermesRequireHTTPS has no default here
	// because Swift's depends on LV_ENVIRONMENT; use its method.
	HermesGatewayURL string `env:"HERMES_GATEWAY_URL" envDefault:"http://hermes:8642"`
	// Wave 4F. Strings where Swift reads lowercased() != "false" or == "true".
	HermesAPIKey              string `env:"HERMES_API_KEY"`
	HermesStreamIdleTimeout   int    `env:"HERMES_STREAM_IDLE_TIMEOUT" envDefault:"60"`
	HermesToolVaultMapEnabled string `env:"HERMES_TOOL_VAULT_MAP_ENABLED" envDefault:"true"`
	RetrievalTelemetryEnabled string `env:"RETRIEVAL_TELEMETRY_ENABLED" envDefault:"true"`
	// capture.autoFile.enabled. Swift's key encodes as CAPTURE_AUTO_FILE_ENABLED, whatever
	// its comment says about CAPTURE_AUTOFILE_ENABLED, which it never reads.
	CaptureAutoFileEnabled string `env:"CAPTURE_AUTO_FILE_ENABLED"`
	// HermesGatewayKind picks hermesprofile.NewGateway: "filesystem" or "logging".
	HermesGatewayKind        string `env:"HERMES_GATEWAY_KIND" envDefault:"filesystem"`
	BYOHermesAllowPrivate    string `env:"BYO_HERMES_ALLOW_PRIVATE" envDefault:"false"`
	BYOHermesRequireHTTPSRaw string `env:"BYO_HERMES_REQUIRE_HTTPS"`
	BYOHermesAllowTailnet    string `env:"BYO_HERMES_ALLOW_TAILNET_HTTP" envDefault:"true"`

	// MaxConcurrentStreamsPerUser is the ceiling on ratelimit.InFlightLimit for /v1/llm and /v1/query.
	MaxConcurrentStreamsPerUser int `env:"HTTP_MAX_CONCURRENT_STREAMS_PER_USER" envDefault:"3"`

	// Memory routes. HybridExecutionEnabled stays a string, read with Swift's
	// lowercased() == "true"; it mounts /v1/memory/local-sync.
	HybridExecutionEnabled   string  `env:"HYBRID_EXECUTION_ENABLED" envDefault:"true"`
	MemoryRelatedMaxDistance float64 `env:"MEMORY_RELATED_MAX_DISTANCE" envDefault:"0.45"`

	// Chat (wave 5C). LocalExecutionToolBrokerEnabled is localExecution.toolBroker.enabled
	// and AutoSaveLinksEnabled autoSaveLinks.enabled, both read with lowercased() == "true".
	// ChatRetrievalMaxDistance is chat.retrieval.maxDistance (ChatGrounding's floor).
	// ChatHermesRunsBridgeURL is Go only: the Swift API that starts an escalated turn's
	// Hermes run (owner decision 1, option A) until wave 6C ports HermesRunsService.
	// Empty means no runs service, so chat never escalates, as Swift without one.
	LocalExecutionToolBrokerEnabled string  `env:"LOCAL_EXECUTION_TOOL_BROKER_ENABLED" envDefault:"true"`
	AutoSaveLinksEnabled            string  `env:"AUTO_SAVE_LINKS_ENABLED" envDefault:"true"`
	ChatRetrievalMaxDistance        float64 `env:"CHAT_RETRIEVAL_MAX_DISTANCE" envDefault:"0.6"`
	ChatHermesRunsBridgeURL         string  `env:"CHAT_HERMES_RUNS_BRIDGE_URL"`

	// Product analytics (posthog package). Off unless both are set, as in Swift.
	PostHogProjectToken string `env:"POSTHOG_PROJECT_TOKEN"`
	PostHogHost         string `env:"POSTHOG_HOST"`

	// LVGoJobs lists the background jobs this Go process runs. It is set only
	// on api-go-* (extraEnv), so a Swift job flag inherited from api-* never
	// starts a Go job by accident. See the roadmap's background job rule. Names are
	// the Swift type names, and internal/jobs fails boot on one it doesn't know.
	LVGoJobs []string `env:"LV_GO_JOBS" envSeparator:","`
}

// Load reads Config from the process environment.
func Load() (Config, error) {
	return LoadFrom(nil)
}

// LoadFrom reads Config from environ (KEY=VALUE pairs). A nil environ reads the
// process environment.
func LoadFrom(environ map[string]string) (Config, error) {
	opts := env.Options{}
	if environ != nil {
		opts.Environment = environ
	}
	cfg, err := env.ParseAsWithOptions[Config](opts)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if !isSet(environ, "SELF_IMPROVEMENT_ECONOMY_MODEL") {
		cfg.SelfImprovementEconomyModel = cfg.UsageDegradeModel
	}
	cfg.CORSAllowedOrigins = cleanCSV(cfg.CORSAllowedOrigins)
	cfg.LVGoJobs = cleanCSV(cfg.LVGoJobs)
	return cfg, nil
}

// isSet tells unset from set but empty, which an envDefault can't: env applies the
// default to both, and a default taken from another key can't be written as a tag.
func isSet(environ map[string]string, key string) bool {
	if environ != nil {
		_, set := environ[key]
		return set
	}
	_, set := os.LookupEnv(key)
	return set
}

// cleanCSV trims every entry and drops empty ones, so "a, b,," is [a b].
func cleanCSV(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// BYOHermesRequireHTTPS is byoHermes.requireHttps: when unset, false in "dev"
// and true everywhere else, with LV_ENVIRONMENT itself defaulting to "dev". A
// set value is on only if it lowercases to "true".
func (c Config) BYOHermesRequireHTTPS() bool {
	if c.BYOHermesRequireHTTPSRaw == "" {
		return c.LVEnvironment != "" && c.LVEnvironment != "dev"
	}
	return strings.EqualFold(c.BYOHermesRequireHTTPSRaw, "true")
}

// JobEnabled reports whether this process should run the named background job.
func (c Config) JobEnabled(name string) bool {
	for _, j := range c.LVGoJobs {
		if j == name {
			return true
		}
	}
	return false
}

// ColdStoragePath is billing.coldStoragePath: BILLING_COLD_STORAGE_PATH, or
// <VAULT_ROOT_PATH>/cold-storage.
func (c Config) ColdStoragePath() string {
	if c.BillingColdStoragePath != "" {
		return c.BillingColdStoragePath
	}
	return filepath.Join(c.VaultRootPath, "cold-storage")
}
