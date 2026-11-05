package llm

import (
	"log/slog"
	"lumina/internal/secretbox"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// StackConfig is what App+build.swift reads to assemble LLM routing.
type StackConfig struct {
	Getenv             func(string) string
	Pool               *pgxpool.Pool
	Box                *secretbox.Box // nil means no BYOK store, as in Swift
	HermesURL          *url.URL
	HermesAPIKey       string
	HermesDefaultModel string
	XAIResolver        XAIContainerResolver // nil until the container manager lands
	Client             *http.Client         // nil means http.DefaultClient; tests set it
	// WrapRouter sits where CerberusModelRouter does: around the preference
	// router and inside the trial router. Production Swift always has one there.
	WrapRouter func(Router) Router
}

// Stack is the assembled routing layer.
type Stack struct {
	Registry    *Registry
	Router      Router
	Transport   *Transport
	Credentials *CredentialStore // nil without a Box
	Preferences *PreferenceRepository
}

func envProviderKey(getenv func(string) string, key string) string {
	if v := getenv(envKey(apiKeyConfigKey(key))); v != "" {
		return v
	}
	return getenv(envKey(legacyAPIKeyConfigKey(key)))
}

func envProviderBaseURL(getenv func(string) string, key string) string {
	urls := baseURLConfigKeys(key)
	return firstTrimmed(getenv(envKey(urls[0])), getenv(envKey(urls[1])))
}

// NewStack is the LLM half of App+build.swift. Adapters: the gateway; Together,
// Groq, Fireworks, DeepInfra and DeepSeek-direct when they have a key; xAI, OpenAI,
// OpenRouter, Nous, NVIDIA, custom and Anthropic always, so a tenant can attach a
// key at any time. Gemini and Ollama are not ported, so a route naming one is
// skipped as unregistered.
func NewStack(cfg StackConfig) *Stack {
	getenv := cfg.Getenv
	stack := &Stack{Preferences: NewPreferenceRepository(cfg.Pool)}
	var credentials CredentialSource
	if cfg.Box != nil {
		stack.Credentials = NewCredentialStore(cfg.Pool, cfg.Box, cfg.XAIResolver)
		credentials = stack.Credentials
	}

	baseURLFor := func(kind ProviderKind) *url.URL {
		raw := envProviderBaseURL(getenv, string(kind))
		if raw == "" {
			return DefaultBaseURL(kind)
		}
		if u := parseOptionalURL(&raw); u != nil {
			return u
		}
		return DefaultBaseURL(kind)
	}

	gateway := &HermesGatewayAdapter{BaseURL: cfg.HermesURL}
	if cfg.HermesAPIKey != "" {
		gateway.DefaultAuthHeader = "Bearer " + cfg.HermesAPIKey
	}
	adapters := []Adapter{gateway}
	for _, kind := range []ProviderKind{Together, Groq, Fireworks, DeepInfra, DeepseekDirect} {
		// Only the canonical spelling here, as in Swift's loop.
		apiKey := getenv(envKey(apiKeyConfigKey(string(kind))))
		if apiKey == "" {
			continue
		}
		raw := firstTrimmed(getenv(envKey(baseURLConfigKeys(string(kind))[0])))
		base := DefaultBaseURL(kind)
		if u := parseOptionalURL(&raw); u != nil {
			base = u
		}
		adapters = append(adapters, &OpenAICompatibleAdapter{ProviderKind: kind, APIKey: apiKey, BaseURL: base, Client: cfg.Client})
	}
	for _, kind := range []ProviderKind{XAI, OpenAI, OpenRouter, Nous, Nvidia, Custom} {
		apiKey := envProviderKey(getenv, string(kind))
		if apiKey == "" {
			switch kind {
			case OpenRouter:
				apiKey = getenv(envKey("openrouter.api_key"))
			case Nvidia:
				apiKey = getenv(envKey("nvidia.api_key"))
			}
		}
		adapter := &OpenAICompatibleAdapter{ProviderKind: kind, APIKey: apiKey, BaseURL: baseURLFor(kind), Client: cfg.Client, Credentials: credentials}
		if kind == XAI {
			adapter.XAIResolver = cfg.XAIResolver
		}
		adapters = append(adapters, adapter)
	}
	trial := LoadTrialConfig(getenv)
	anthropicBase := DefaultAnthropicBaseURL()
	if raw := envProviderBaseURL(getenv, "anthropic"); raw != "" {
		if u := parseOptionalURL(&raw); u != nil {
			anthropicBase = u
		}
	}
	adapters = append(adapters, &AnthropicAdapter{
		APIKey:      envProviderKey(getenv, "anthropic"),
		BaseURL:     anthropicBase,
		Client:      cfg.Client,
		Credentials: credentials,
		Trial:       trial,
	})
	configs := LoadProviderConfigs(getenv)
	enabled := make([]string, 0, len(configs))
	for _, c := range configs {
		enabled = append(enabled, string(c.Kind))
	}
	slices.Sort(enabled)
	// The cheap way to notice a key that failed to load: a provider without one is
	// silently absent rather than failing the boot.
	slog.Info("llm providers enabled", "providers", strings.Join(enabled, ","))
	stack.Registry = NewRegistry(configs, adapters...)

	var router Router = &UserPreferenceRouter{
		Preferences: stack.Preferences,
		Fallback:    &TableRouter{Registry: stack.Registry, HermesDefaultModel: cfg.HermesDefaultModel},
	}
	if cfg.WrapRouter != nil {
		router = cfg.WrapRouter(router)
	}
	if trial != nil {
		router = &TrialRouter{Inner: router, Config: *trial}
		effort := trial.Effort
		if effort == "" {
			effort = "default"
		}
		slog.Info("anthropic managed trial enabled", "model", trial.Model, "effort", effort, "thinking", trial.Thinking, "scope", trial.Scope)
	}
	stack.Router = router

	meters := PGMeters{Pool: cfg.Pool}
	stack.Transport = &Transport{
		Registry:       stack.Registry,
		Router:         router,
		UsageMeter:     meters.UsageMeter(),
		CostLedger:     meters.CostLedger(),
		TraceRecorder:  meters.TraceRecorder(),
		FailoverLogger: meters.FailoverLogger(),
	}
	return stack
}

// DefaultAnthropicBaseURL is https://api.anthropic.com.
func DefaultAnthropicBaseURL() *url.URL { return mustParseURL("https://api.anthropic.com") }
