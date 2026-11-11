package hermescontainers

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test temp dir
	require.NoError(t, err)
	return string(b)
}

func mtime(t *testing.T, path string) time.Time {
	t.Helper()
	st, err := os.Stat(path)
	require.NoError(t, err)
	return st.ModTime()
}

// Ported from HermesTenantConfigSeedTests (6 cases).
func TestHermesTenantConfigSeed(t *testing.T) {
	t.Run("seed writes config and env at volume root", func(t *testing.T) {
		v := t.TempDir()
		require.NoError(t, Seed(v, "deadbeef-test-key", "hermes-3", nil, true, nil))
		cfg, env := readFile(t, v+"/config.yaml"), readFile(t, v+"/.env")
		for _, s := range []string{"api_server:", "enabled: true", "host: 0.0.0.0", "hermes-3"} {
			require.Contains(t, cfg, s)
		}
		for _, s := range []string{"telegram:", "discord:", "whatsapp:"} {
			require.NotContains(t, cfg, s)
		}
		require.Contains(t, env, "API_SERVER_KEY=deadbeef-test-key")
		require.Contains(t, env, "API_SERVER_HOST=0.0.0.0")
	})
	t.Run("seed is idempotent on identical re-call", func(t *testing.T) {
		v := t.TempDir()
		require.NoError(t, Seed(v, "stable", "hermes-3", nil, true, nil))
		first := mtime(t, v+"/config.yaml")
		time.Sleep(1100 * time.Millisecond)
		require.NoError(t, Seed(v, "stable", "hermes-3", nil, true, nil))
		require.Equal(t, first, mtime(t, v+"/config.yaml"), "identical content must not trigger a rewrite")
	})
	t.Run("seed rewrites on drift", func(t *testing.T) {
		v := t.TempDir()
		require.NoError(t, Seed(v, "k1", "hermes-3", nil, true, nil))
		require.NoError(t, os.WriteFile(v+"/config.yaml", []byte("tampered"), 0o600))
		first := mtime(t, v+"/config.yaml")
		time.Sleep(1100 * time.Millisecond)
		require.NoError(t, Seed(v, "k1", "hermes-3", nil, true, nil))
		require.NotEqual(t, first, mtime(t, v+"/config.yaml"), "drifted content must trigger a rewrite")
		require.Contains(t, readFile(t, v+"/config.yaml"), "api_server:")
	})
	t.Run("seed rewrites when api key rotates", func(t *testing.T) {
		v := t.TempDir()
		require.NoError(t, Seed(v, "old-key", "hermes-3", nil, true, nil))
		require.Contains(t, readFile(t, v+"/.env"), "API_SERVER_KEY=old-key")
		require.NoError(t, Seed(v, "new-key", "hermes-3", nil, true, nil))
		require.Contains(t, readFile(t, v+"/.env"), "API_SERVER_KEY=new-key")
		require.NotContains(t, readFile(t, v+"/.env"), "old-key")
	})
	t.Run("configYAML wires mnemosyne and disables native memory when enabled", func(t *testing.T) {
		y := ConfigYAML("hermes-3", true, nil)
		for _, s := range []string{
			"mcp_servers:", "mnemosyne:", "MNEMOSYNE_DATA_DIR: /opt/data/mnemosyne",
			"FASTEMBED_CACHE_PATH: /opt/data/mnemosyne/cache", "memory_enabled: false", "user_profile_enabled: false",
		} {
			require.Contains(t, y, s)
		}
	})
	t.Run("configYAML omits mnemosyne and leaves native memory when disabled", func(t *testing.T) {
		y := ConfigYAML("hermes-3", false, nil)
		require.NotContains(t, y, "mnemosyne")
		require.NotContains(t, y, "memory_enabled: false")
		require.Contains(t, y, "api_server:")
	})
}

// Ported from HermesAudioProxySeedTests (10 cases).
func TestHermesAudioProxySeed(t *testing.T) {
	proxy := NewAudioProxySeed("https://api.luminavault.app/v1", "scoped-audio-token")
	t.Run("emitsSTTBlockPointedAtTheProxy", func(t *testing.T) {
		y := ConfigYAML("hermes-3", true, proxy)
		require.Contains(t, y, "stt:")
		require.Contains(t, y, "provider: openai")
		require.Contains(t, y, `base_url: "https://api.luminavault.app/v1"`)
		require.Contains(t, y, "enabled: true")
	})
	t.Run("pinsTTSProviderSoEdgeTTSCanNeverBeSelected", func(t *testing.T) {
		y := ConfigYAML("hermes-3", true, proxy)
		require.Contains(t, y, "tts:")
		require.NotContains(t, y, "provider: edge")
		require.Equal(t, 2, strings.Count(y, "provider: openai"))
	})
	t.Run("configYAMLNeverContainsTheCredential", func(t *testing.T) {
		require.NotContains(t, ConfigYAML("hermes-3", true, proxy), proxy.Token)
	})
	t.Run("rendersIdenticallyToBeforeWhenNoProxyIsConfigured", func(t *testing.T) {
		y := ConfigYAML("hermes-3", true, nil)
		require.NotContains(t, y, "stt:")
		require.NotContains(t, y, "tts:")
		require.Contains(t, y, "model:")
		require.Contains(t, y, "allow_all_users: false")
		require.Contains(t, y, "mcp_servers:")
	})
	t.Run("audioAndMnemosyneBlocksCoexist", func(t *testing.T) {
		y := ConfigYAML("hermes-3", true, proxy)
		require.Contains(t, y, "stt:")
		require.Contains(t, y, "mcp_servers:")
		require.Contains(t, y, "memory_enabled: false")
	})
	t.Run("audioIsEmittedEvenWhenMnemosyneIsOff", func(t *testing.T) {
		y := ConfigYAML("hermes-3", false, proxy)
		require.Contains(t, y, "stt:")
		require.NotContains(t, y, "mcp_servers:")
	})
	t.Run("renderingIsDeterministic", func(t *testing.T) {
		require.Equal(t, ConfigYAML("hermes-3", true, proxy), ConfigYAML("hermes-3", true, proxy))
	})
	t.Run("envCarriesScopedKeyAndBaseURL", func(t *testing.T) {
		env := EnvFile("api-key", nil, proxy)
		require.Contains(t, env, "VOICE_TOOLS_OPENAI_KEY='scoped-audio-token'")
		require.Contains(t, env, "STT_OPENAI_BASE_URL='https://api.luminavault.app/v1'")
	})
	t.Run("envDoesNotSetAGeneralOpenAIKey", func(t *testing.T) {
		for _, l := range strings.Split(EnvFile("api-key", nil, proxy), "\n") {
			require.False(t, strings.HasPrefix(l, "OPENAI_API_KEY="))
		}
	})
	t.Run("envOmitsVoiceEntirelyWhenNoProxyIsConfigured", func(t *testing.T) {
		env := EnvFile("api-key", nil, nil)
		require.NotContains(t, env, "VOICE_TOOLS_OPENAI_KEY")
		require.NotContains(t, env, "STT_OPENAI_BASE_URL")
	})
}

// The exact bytes matter: the seed is SHA-256 drift-compared against what Swift
// wrote, so a tenant flipping servers must not see a rewrite.
func TestConfigYAMLBytes(t *testing.T) {
	y := ConfigYAML("hermes-3", true, nil)
	require.True(t, strings.HasPrefix(y, "# Auto-generated by HermesContainerManager (HER-254). Do not edit in-place;\n"))
	require.Contains(t, y, "logging:\n  level: INFO\n\n# HER-XXX — Mnemosyne is the default memory layer")
	require.True(t, strings.HasSuffix(y, "      FASTEMBED_CACHE_PATH: /opt/data/mnemosyne/cache"))
	env := EnvFile("k", []GatewaySeed{
		{GatewayID: "telegram", Config: map[string]string{"bot_token": " t'ok ", "allowed_users": "1"}},
		{GatewayID: "whatsapp", Config: map[string]string{"x": "y"}},
	}, nil)
	require.Equal(t, "# Auto-generated by HermesContainerManager (HER-254).\nAPI_SERVER_KEY=k\nAPI_SERVER_ENABLED=true\n"+
		"API_SERVER_HOST=0.0.0.0\nAPI_SERVER_PORT=8642\n\n# Messaging gateways (token presence activates the platform).\n"+
		"TELEGRAM_ALLOWED_USERS='1'\nTELEGRAM_BOT_TOKEN='tok'", env)
}
