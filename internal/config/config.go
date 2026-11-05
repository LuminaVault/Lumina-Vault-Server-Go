// Package config loads the server configuration from the same environment
// variables the Swift server reads, so the sealed api-env-* secrets and the
// infra values files serve both images unchanged. Every env tag here must be
// KeyToEnv of a key in docs/migration/config-keys.txt (enforced by a test);
// LV_GO_JOBS is the one Go-only name.
package config

import (
	"fmt"
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
	HermesGatewayURL         string `env:"HERMES_GATEWAY_URL" envDefault:"http://hermes:8642"`
	BYOHermesAllowPrivate    string `env:"BYO_HERMES_ALLOW_PRIVATE" envDefault:"false"`
	BYOHermesRequireHTTPSRaw string `env:"BYO_HERMES_REQUIRE_HTTPS"`
	BYOHermesAllowTailnet    string `env:"BYO_HERMES_ALLOW_TAILNET_HTTP" envDefault:"true"`

	// Memory routes. HybridExecutionEnabled stays a string, read with Swift's
	// lowercased() == "true"; it mounts /v1/memory/local-sync.
	HybridExecutionEnabled   string  `env:"HYBRID_EXECUTION_ENABLED" envDefault:"true"`
	MemoryRelatedMaxDistance float64 `env:"MEMORY_RELATED_MAX_DISTANCE" envDefault:"0.45"`

	// Product analytics (posthog package). Off unless both are set, as in Swift.
	PostHogProjectToken string `env:"POSTHOG_PROJECT_TOKEN"`
	PostHogHost         string `env:"POSTHOG_HOST"`

	// LVGoJobs lists the background jobs this Go process runs. It is set only
	// on api-go-* (extraEnv), so a Swift job flag inherited from api-* never
	// starts a Go job by accident. See the roadmap's background job rule.
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
	cfg.CORSAllowedOrigins = cleanCSV(cfg.CORSAllowedOrigins)
	cfg.LVGoJobs = cleanCSV(cfg.LVGoJobs)
	return cfg, nil
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
