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

	// Wave 3 account/health. HermesDataRoot holds profiles/<id>/ (account
	// deletion soft-renames it). BillingEnforcementEnabled stays a string:
	// Swift enables it only when it lowercases to "true" (entitlement package).
	HermesDataRoot            string `env:"HERMES_DATA_ROOT" envDefault:"/app/data/hermes"`
	BillingEnforcementEnabled string `env:"BILLING_ENFORCEMENT_ENABLED" envDefault:"false"`

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

// JobEnabled reports whether this process should run the named background job.
func (c Config) JobEnabled(name string) bool {
	for _, j := range c.LVGoJobs {
		if j == name {
			return true
		}
	}
	return false
}
