package config_test

import (
	"lumina/internal/config"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKeyToEnv(t *testing.T) {
	for key, want := range map[string]string{
		// Pairs confirmed by platform/infra apps/api/values-production.yaml and
		// the ProviderRegistry.swift comment.
		"cors.allowedOrigins":                  "CORS_ALLOWED_ORIGINS",
		"feeds.baseUrl":                        "FEEDS_BASE_URL",
		"feeds.defaultFeeds":                   "FEEDS_DEFAULT_FEEDS",
		"oauth.google.clientId":                "OAUTH_GOOGLE_CLIENT_ID",
		"webauthn.relyingPartyId":              "WEBAUTHN_RELYING_PARTY_ID",
		"billing.tierOverrideEmails":           "BILLING_TIER_OVERRIDE_EMAILS",
		"selfImprovement.economyModel":         "SELF_IMPROVEMENT_ECONOMY_MODEL",
		"llm.provider.openRouter.apiKey":       "LLM_PROVIDER_OPEN_ROUTER_API_KEY",
		"llm.provider.openrouter.apikey":       "LLM_PROVIDER_OPENROUTER_APIKEY",
		"metrics.secret":                       "METRICS_SECRET",
		"fluent.enabled":                       "FLUENT_ENABLED",
		"lv.secretMasterKey":                   "LV_SECRET_MASTER_KEY",
		"jwt.hmac.secrets":                     "JWT_HMAC_SECRETS",
		"community.dailyPost.hourUtc":          "COMMUNITY_DAILY_POST_HOUR_UTC",
		"cerberus.studio.globalDailyUsdMicros": "CERBERUS_STUDIO_GLOBAL_DAILY_USD_MICROS",
		"hermes.gatewayURL":                    "HERMES_GATEWAY_URL",
		"nvidia.api_key":                       "NVIDIA_API_KEY",
	} {
		require.Equal(t, want, config.KeyToEnv(key), key)
	}
}

// Every env tag on Config must be the env name of a key Swift reads, so Go
// never invents a variable the sealed secret does not carry.
func TestConfigEnvNamesComeFromSwiftKeys(t *testing.T) {
	known := map[string]bool{"LV_GO_JOBS": true}
	raw, err := os.ReadFile("../../docs/migration/config-keys.txt")
	require.NoError(t, err)
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.Contains(line, `\(`) {
			continue
		}
		known[config.KeyToEnv(line)] = true
	}

	typ := reflect.TypeFor[config.Config]()
	for i := range typ.NumField() {
		name := typ.Field(i).Tag.Get("env")
		require.True(t, known[name], "env %s is not read by the Swift server", name)
	}
}

func TestLoadCSVAndDefaults(t *testing.T) {
	cfg, err := config.LoadFrom(map[string]string{
		"CORS_ALLOWED_ORIGINS": "https://app.luminavault.fyi, https://luminavault.fyi,,",
		"LV_GO_JOBS":           "achievements",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"https://app.luminavault.fyi", "https://luminavault.fyi"}, cfg.CORSAllowedOrigins)
	require.Equal(t, 5432, cfg.PostgresPort)
	require.True(t, cfg.FluentEnabled)
	require.True(t, cfg.JobEnabled("achievements"))
	require.False(t, cfg.JobEnabled("calendar-sync"))
}

func TestLoadEmptyOriginsMeansNone(t *testing.T) {
	cfg, err := config.LoadFrom(map[string]string{})
	require.NoError(t, err)
	require.Empty(t, cfg.CORSAllowedOrigins)
}
