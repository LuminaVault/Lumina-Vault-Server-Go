package llm

import (
	"log/slog"
	"lumina/internal/swiftstr"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"unicode"
)

// ProviderConfig is an env-loaded provider credential snapshot. A blank API key
// disables the provider.
type ProviderConfig struct {
	Kind    ProviderKind
	APIKey  string
	BaseURL *url.URL
}

// IsEnabled: a key that is all whitespace counts as missing.
func (c ProviderConfig) IsEnabled() bool { return swiftstr.TrimWhitespacesAndNewlines(c.APIKey) != "" }

// Registry maps a ProviderKind to its adapter and config. Written at boot, read
// on every call.
type Registry struct {
	mu       sync.RWMutex
	adapters map[ProviderKind]Adapter
	configs  map[ProviderKind]ProviderConfig
}

// NewRegistry seeds the registry. Disabled configs are dropped.
func NewRegistry(configs []ProviderConfig, adapters ...Adapter) *Registry {
	r := &Registry{adapters: map[ProviderKind]Adapter{}, configs: map[ProviderKind]ProviderConfig{}}
	for _, c := range configs {
		if c.IsEnabled() {
			r.configs[c.Kind] = c
			slog.Info("provider config enabled", "provider", c.Kind)
		}
	}
	for _, a := range adapters {
		r.Register(a)
	}
	return r
}

// Register overwrites any adapter of the same kind.
func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.Kind()] = a
	slog.Info("provider registered", "provider", a.Kind())
}

func (r *Registry) Adapter(kind ProviderKind) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[kind]
	return a, ok
}

func (r *Registry) Config(kind ProviderKind) (ProviderConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.configs[kind]
	return c, ok
}

// IsEnabled: the gateway is on when an adapter is registered, since it needs no
// key. Everything else must have a configured key to be eligible for routing.
func (r *Registry) IsEnabled(kind ProviderKind) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if kind == HermesGateway {
		_, ok := r.adapters[kind]
		return ok
	}
	c, ok := r.configs[kind]
	return ok && c.IsEnabled()
}

// Registered returns the kinds with an adapter, sorted.
func (r *Registry) Registered() []ProviderKind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Sorted(maps.Keys(r.adapters))
}

// EnabledProviders returns the kinds with a config, sorted.
func (r *Registry) EnabledProviders() []ProviderKind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Sorted(maps.Keys(r.configs))
}

// envKey is swift-configuration's EnvironmentKeyEncoder: components split on
// ".", camelCase boundaries become "_", everything is uppercased. So
// llm.provider.openRouter.apiKey reads LLM_PROVIDER_OPEN_ROUTER_API_KEY.
func envKey(dotted string) string {
	var b strings.Builder
	var prev rune
	for _, r := range dotted {
		switch {
		case r == '.':
			b.WriteRune('_')
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			b.WriteRune('_')
			b.WriteRune(r)
		default:
			b.WriteRune(unicode.ToUpper(r))
		}
		prev = r
	}
	return b.String()
}

func apiKeyConfigKey(key string) string { return "llm.provider." + key + ".apiKey" }

// legacyAPIKeyConfigKey is the pre-fix spelling every .env and compose file
// shipped, kept working: LLM_PROVIDER_OPENROUTER_APIKEY.
func legacyAPIKeyConfigKey(key string) string {
	return "llm.provider." + strings.ToLower(key) + ".apikey"
}

func baseURLConfigKeys(key string) []string {
	return []string{"llm.provider." + key + ".baseURL", "llm.provider." + strings.ToLower(key) + ".baseurl"}
}

func firstTrimmed(values ...string) string {
	for _, v := range values {
		if t := swiftstr.TrimWhitespacesAndNewlines(v); t != "" {
			return t
		}
	}
	return ""
}

// LoadProviderConfigs is ProviderRegistry.loadConfigs. A key resolves canonical,
// then legacy spelling, then aliases; a blank value is absent, so a key sealed
// as a stray space cannot shadow a usable alias.
//
// ANTHROPIC_API_KEY is deliberately not read here. Enabling Anthropic in the
// registry would put Sonnet and Opus at the front of pro routing.
func LoadProviderConfigs(getenv func(string) string) []ProviderConfig {
	load := func(kind ProviderKind, key string, aliases ...string) *ProviderConfig {
		candidates := []string{getenv(envKey(apiKeyConfigKey(key))), getenv(envKey(legacyAPIKeyConfigKey(key)))}
		for _, alias := range aliases {
			candidates = append(candidates, getenv(envKey(alias)))
		}
		apiKey := firstTrimmed(candidates...)
		if apiKey == "" {
			return nil
		}
		urls := baseURLConfigKeys(key)
		raw := firstTrimmed(getenv(envKey(urls[0])), getenv(envKey(urls[1])))
		return &ProviderConfig{Kind: kind, APIKey: apiKey, BaseURL: parseOptionalURL(&raw)}
	}
	all := []*ProviderConfig{
		load(Anthropic, "anthropic"),
		load(OpenAI, "openai"),
		load(OpenRouter, "openRouter", "openrouter.api_key", "openrouter.fallback_api_key"),
		load(Nvidia, "nvidia", "nvidia.api_key"),
		load(Gemini, "gemini"),
		load(Together, "together"),
		load(Groq, "groq"),
		load(Fireworks, "fireworks"),
		load(DeepInfra, "deepInfra"),
		load(DeepseekDirect, "deepseekDirect"),
	}
	var out []ProviderConfig
	for _, c := range all {
		if c != nil {
			out = append(out, *c)
		}
	}
	return out
}
