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
	known := map[string]bool{
		"LV_GO_JOBS": true,
		// Go-only: /v1/tts's switch, for a self-hosted service (internal/modules/tts).
		"TTS_BASE_URL": true,
		// Go-only until wave 6C: the Swift API escalated chat turns start their run on
		// (owner decision 1, option A; internal/modules/chat/runs.go).
		"CHAT_HERMES_RUNS_BRIDGE_URL": true,
		// TranscribeProviderRegistry.loadConfig builds these with key = "openai".
		"TRANSCRIBE_RATECARD_OPENAI_MTOK_PER_SECOND":              true,
		"TRANSCRIBE_RATECARD_OPENAI_IMPUTED_USD_PER_AUDIO_MINUTE": true,
		// ProviderRegistry.loadConfig builds these as llm.provider.\(key).apiKey
		// and llm.provider.\(key.lowercased()).apikey with key = "openRouter".
		"LLM_PROVIDER_OPEN_ROUTER_API_KEY": true,
		"LLM_PROVIDER_OPENROUTER_APIKEY":   true,
	}
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

func TestRequestPathDefaults(t *testing.T) {
	cfg, err := config.LoadFrom(map[string]string{})
	require.NoError(t, err)
	require.Equal(t, 3, cfg.MaxConcurrentStreamsPerUser)
	require.Equal(t, "filesystem", cfg.HermesGatewayKind)
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

func TestLoadUsageAndSelfImprovementDefaults(t *testing.T) {
	cfg, err := config.LoadFrom(map[string]string{})
	require.NoError(t, err)
	require.InDelta(t, 1.0, cfg.UsageFreeMtokDaily, 0)
	require.InDelta(t, 0.2, cfg.UsagePerSkillMtokDaily, 0)
	require.Equal(t, "hermes-3-small", cfg.UsageDegradeModel)
	require.True(t, cfg.SelfImprovementEnabled)
}

func TestSelfImprovementEconomyModel(t *testing.T) {
	for _, c := range []struct {
		name    string
		environ map[string]string
		want    string
	}{
		{"unset is the degrade model's default", map[string]string{}, "hermes-3-small"},
		{"unset follows the degrade model", map[string]string{"USAGE_DEGRADE_MODEL": "cheap"}, "cheap"},
		{"set wins", map[string]string{"USAGE_DEGRADE_MODEL": "cheap", "SELF_IMPROVEMENT_ECONOMY_MODEL": "eco"}, "eco"},
		{"set but empty stays empty", map[string]string{"USAGE_DEGRADE_MODEL": "cheap", "SELF_IMPROVEMENT_ECONOMY_MODEL": ""}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := config.LoadFrom(c.environ)
			require.NoError(t, err)
			require.Equal(t, c.want, cfg.SelfImprovementEconomyModel)
		})
	}
}

func TestLoadEmptyOriginsMeansNone(t *testing.T) {
	cfg, err := config.LoadFrom(map[string]string{})
	require.NoError(t, err)
	require.Empty(t, cfg.CORSAllowedOrigins)
}

func TestBYOHermesRequireHTTPS(t *testing.T) {
	cases := []struct {
		env, raw string
		want     bool
	}{
		{"", "", false}, // LV_ENVIRONMENT defaults to dev in Swift
		{"dev", "", false},
		{"production", "", true},
		{"staging", "", true},
		{"production", "false", false},
		{"dev", "TRUE", true},
		{"dev", "1", false},
	}
	for _, tc := range cases {
		c := config.Config{LVEnvironment: tc.env, BYOHermesRequireHTTPSRaw: tc.raw}
		if got := c.BYOHermesRequireHTTPS(); got != tc.want {
			t.Errorf("env %q raw %q: got %v", tc.env, tc.raw, got)
		}
	}
}

// Ports the env half of LLM/Transcribe/TranscribeProviderConfigTests.swift.
func TestTranscribeEnvSpellings(t *testing.T) {
	cfg, err := config.LoadFrom(map[string]string{
		"TRANSCRIBE_PROVIDER_OPENAI_BASE_URL":                     "https://api.example.com/v1",
		"TRANSCRIBE_PROVIDER_OPENAI_API_KEY":                      "sk-test",
		"TRANSCRIBE_PROVIDER_OPENAI_MODEL":                        "whisper-1",
		"TRANSCRIBE_RATECARD_OPENAI_IMPUTED_USD_PER_AUDIO_MINUTE": "0.006",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TranscribeOpenAIBaseURL != "https://api.example.com/v1" || cfg.TranscribeOpenAIAPIKey != "sk-test" ||
		cfg.TranscribeOpenAIModel != "whisper-1" || cfg.TranscribeOpenAIImputedPerMin != 0.006 {
		t.Errorf("canonical spellings didn't load: %+v", cfg)
	}

	// Silent if it breaks: the variable is set, nothing reads it, and the
	// deployment keeps using the in-cluster default while its .env says otherwise.
	cfg, err = config.LoadFrom(map[string]string{
		"TRANSCRIBE_PROVIDER_OPENAI_BASEURL": "https://api.example.com/v1",
		"TRANSCRIBE_PROVIDER_OPENAI_APIKEY":  "sk-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TranscribeOpenAIBaseURL != "" || cfg.TranscribeOpenAIAPIKey != "" {
		t.Errorf("flattened spellings loaded: %q %q", cfg.TranscribeOpenAIBaseURL, cfg.TranscribeOpenAIAPIKey)
	}
}
