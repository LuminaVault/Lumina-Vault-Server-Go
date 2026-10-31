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

	// VaultRootPath holds tenants/<UUID>/{raw,wiki,memories} on the shared PVC.
	VaultRootPath string `env:"VAULT_ROOT_PATH" envDefault:"/tmp/luminavault"`

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
