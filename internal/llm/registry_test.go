package llm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envOf(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

type namedAdapter struct{ kind ProviderKind }

func (a namedAdapter) Kind() ProviderKind           { return a.kind }
func (a namedAdapter) AcceptsUserCredentials() bool { return false }
func (a namedAdapter) ChatCompletions(context.Context, []byte, string, string) (Metadata, error) {
	return Metadata{}, nil
}

func (a namedAdapter) ChatStream(ctx context.Context, p []byte, k, id string) Stream {
	return BufferedChatStream(ctx, a, p, k, id)
}

func TestEnvKeyEncoding(t *testing.T) {
	for dotted, want := range map[string]string{
		"llm.provider.openRouter.apiKey":     "LLM_PROVIDER_OPEN_ROUTER_API_KEY",
		"llm.provider.openrouter.apikey":     "LLM_PROVIDER_OPENROUTER_APIKEY",
		"llm.provider.deepInfra.baseURL":     "LLM_PROVIDER_DEEP_INFRA_BASE_URL",
		"llm.provider.deepseekDirect.apiKey": "LLM_PROVIDER_DEEPSEEK_DIRECT_API_KEY",
		"openrouter.fallback_api_key":        "OPENROUTER_FALLBACK_API_KEY",
		"anthropic.apiKey":                   "ANTHROPIC_API_KEY",
	} {
		assert.Equal(t, want, envKey(dotted))
	}
}

// ProviderRegistryTests.
func TestProviderRegistry(t *testing.T) {
	registry := func(env map[string]string, adapters ...Adapter) *Registry {
		return NewRegistry(LoadProviderConfigs(envOf(env)), adapters...)
	}
	t.Run("every provider is loadable from env", func(t *testing.T) {
		r := registry(map[string]string{
			"LLM_PROVIDER_ANTHROPIC_API_KEY": "a", "LLM_PROVIDER_OPENAI_API_KEY": "b", "LLM_PROVIDER_GEMINI_API_KEY": "c",
			"LLM_PROVIDER_TOGETHER_API_KEY": "d", "LLM_PROVIDER_GROQ_API_KEY": "e", "LLM_PROVIDER_FIREWORKS_API_KEY": "f",
			"LLM_PROVIDER_DEEP_INFRA_API_KEY": "g", "LLM_PROVIDER_DEEPSEEK_DIRECT_API_KEY": "h",
		})
		assert.ElementsMatch(t, []ProviderKind{Anthropic, OpenAI, Gemini, Together, Groq, Fireworks, DeepInfra, DeepseekDirect}, r.EnabledProviders())
	})
	t.Run("openrouter fallback alias enables the provider", func(t *testing.T) {
		r := registry(map[string]string{"OPENROUTER_FALLBACK_API_KEY": "fallback-key"})
		assert.True(t, r.IsEnabled(OpenRouter))
		c, _ := r.Config(OpenRouter)
		assert.Equal(t, "fallback-key", c.APIKey)
	})
	t.Run("a blank canonical key does not shadow an alias", func(t *testing.T) {
		r := registry(map[string]string{"LLM_PROVIDER_OPEN_ROUTER_API_KEY": "   ", "OPENROUTER_FALLBACK_API_KEY": "fallback-key"})
		assert.True(t, r.IsEnabled(OpenRouter))
		c, ok := r.Config(OpenRouter)
		require.True(t, ok)
		assert.Equal(t, "fallback-key", c.APIKey)
	})
	t.Run("canonical wins over every alias", func(t *testing.T) {
		r := registry(map[string]string{
			"LLM_PROVIDER_OPEN_ROUTER_API_KEY": "canonical", "LLM_PROVIDER_OPENROUTER_APIKEY": "legacy",
			"OPENROUTER_API_KEY": "alias", "OPENROUTER_FALLBACK_API_KEY": "fallback",
		})
		c, _ := r.Config(OpenRouter)
		assert.Equal(t, "canonical", c.APIKey)
	})
	t.Run("legacy spelling still loads", func(t *testing.T) {
		c, ok := registry(map[string]string{"LLM_PROVIDER_OPENROUTER_APIKEY": "legacy"}).Config(OpenRouter)
		require.True(t, ok)
		assert.Equal(t, "legacy", c.APIKey)
	})
	t.Run("a missing key disables the provider", func(t *testing.T) {
		r := registry(map[string]string{"LLM_PROVIDER_ANTHROPIC_API_KEY": "a"})
		assert.True(t, r.IsEnabled(Anthropic))
		assert.False(t, r.IsEnabled(OpenAI))
	})
	t.Run("a whitespace key counts as missing", func(t *testing.T) {
		assert.False(t, registry(map[string]string{"LLM_PROVIDER_ANTHROPIC_API_KEY": " \n "}).IsEnabled(Anthropic))
	})
	t.Run("an optional base url is honoured", func(t *testing.T) {
		c, _ := registry(map[string]string{
			"LLM_PROVIDER_TOGETHER_API_KEY": "k", "LLM_PROVIDER_TOGETHER_BASE_URL": "https://example.together",
		}).Config(Together)
		assert.Equal(t, "https://example.together", c.BaseURL.String())
	})
	t.Run("the gateway is enabled iff an adapter is registered", func(t *testing.T) {
		assert.False(t, registry(nil).IsEnabled(HermesGateway))
		assert.True(t, registry(nil, namedAdapter{HermesGateway}).IsEnabled(HermesGateway))
	})
}
