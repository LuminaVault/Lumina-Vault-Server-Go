package llm

import (
	"log/slog"
	"lumina/internal/secretbox"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

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
}

// Stack is the assembled routing layer.
type Stack struct {
	Registry    *Registry
	Router      Router
	Transport   *Transport
	Credentials *CredentialStore // nil without a Box
	Preferences *PreferenceRepository
	Profiles    *RouterProfileRepository
	Telemetry   *RouterTelemetryService
	FreeLane    *FreeLaneRuntime // nil when freelane.enabled is off
	// ParallelEnabled is cerberus.parallelEnabled, which also gates the
	// conversation, router and parallel controllers.
	ParallelEnabled bool
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

// defaultManagedModel is hermes.defaultManagedModel's default.
const defaultManagedModel = "deepseek/deepseek-v4-flash"

// NewStack is the LLM half of App+build.swift. Adapters: the gateway; Together,
// Groq, Fireworks, DeepInfra and DeepSeek-direct when they have a key; xAI, OpenAI,
// OpenRouter, Nous, NVIDIA, custom, Anthropic, Gemini and Ollama always, so a tenant
// can attach a key at any time.
//
// The router chain is Trial(Cerberus(UserPreference(Table))). Cerberus is present
// only when cerberus.executionMode is "active", its default; the trial only when
// configured. The transport gets router telemetry in either mode.
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
	adapters = append(adapters, &GeminiContentsAdapter{
		APIKey:      getenv(envKey("gemini.apiKey")),
		Client:      cfg.Client,
		Credentials: credentials,
	})
	ollamaBase := mustParseURL("http://localhost:11434")
	if raw := envProviderBaseURL(getenv, "ollama"); raw != "" {
		if u := parseOptionalURL(&raw); u != nil {
			ollamaBase = u
		}
	}
	adapters = append(adapters, &OllamaAdapter{DefaultBaseURL: ollamaBase, Client: cfg.Client, Credentials: credentials})
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

	legacy := &UserPreferenceRouter{
		Preferences: stack.Preferences,
		Fallback:    &TableRouter{Registry: stack.Registry, HermesDefaultModel: cfg.HermesDefaultModel},
	}
	stack.Telemetry = NewRouterTelemetryService(cfg.Pool)
	stack.Profiles = NewRouterProfileRepository(cfg.Pool, stack.Preferences, envString(getenv, "hermes.defaultManagedModel", defaultManagedModel))
	executionMode := strings.ToLower(envString(getenv, "cerberus.executionMode", "active"))
	// The new name covers every parallel strategy; the old flag is its default.
	stack.ParallelEnabled = envBool(getenv, "cerberus.parallelEnabled", envBool(getenv, "cerberus.ensemblesEnabled", false))
	if stack.ParallelEnabled {
		slog.Warn("cerberus.parallelEnabled is on but ParallelExecutor is not ported: ensemble actions run sequentially and explicit parallel requests fail with parallel_quorum_failed")
	}
	stack.FreeLane = LoadFreeLaneRuntime(getenv, cfg.Pool)
	if warning := FreeLaneStartupWarning(stack.FreeLane != nil, stack.Registry.IsEnabled(OpenRouter), stack.Registry.IsEnabled(Nvidia)); warning != "" {
		slog.Warn(warning)
	}
	var router Router = legacy
	if executionMode == "active" {
		router = &CerberusRouter{
			Profiles:         stack.Profiles,
			Fallback:         legacy,
			Budget:           stack.Telemetry,
			EnsemblesEnabled: stack.ParallelEnabled,
			Credentials:      credentials,
			Registry:         stack.Registry,
			FreeLane:         stack.FreeLane,
		}
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
		Registry:        stack.Registry,
		Router:          router,
		RouterTelemetry: stack.Telemetry,
		UsageMeter:      meters.UsageMeter(),
		CostLedger:      meters.CostLedger(),
		TraceRecorder:   meters.TraceRecorder(),
		FailoverLogger:  meters.FailoverLogger(),
	}
	return stack
}

// DefaultAnthropicBaseURL is https://api.anthropic.com.
func DefaultAnthropicBaseURL() *url.URL { return mustParseURL("https://api.anthropic.com") }

// GatewayConfig is the central Hermes gateway the managed stream falls back to:
// hermes.gatewayUrl, hermes.apiKey, hermes.defaultModel and
// hermes.streamIdleTimeout (HERMES_STREAM_IDLE_TIMEOUT, 60 s when zero).
type GatewayConfig struct {
	BaseURL      *url.URL
	APIKey       string
	DefaultModel string
	IdleTimeout  time.Duration
}

// StreamService is App+build's conversationStreamService: the routed stream
// (parallel, BYOK, free lane, managed Auto) over this stack, falling back to the
// gateway. Build it once and share it: /v1/query/stream and chat both use it.
func (s *Stack) StreamService(gw GatewayConfig) *RoutedStreamService {
	return &RoutedStreamService{
		Fallback: &GatewayStreamService{
			BaseURL: gw.BaseURL, DefaultModel: gw.DefaultModel, APIKey: gw.APIKey, IdleTimeout: gw.IdleTimeout,
		},
		Transport: s.Transport, Preferences: s.Preferences, Router: s.Router, Telemetry: s.Telemetry,
	}
}

// ChatService is App+build's llmService: RoutedHermesLLMService over this stack's
// transport, for POST /v1/llm/chat.
func (s *Stack) ChatService(defaultModel string) *RoutedService {
	return &RoutedService{Transport: s.Transport, DefaultModel: defaultModel, Preferences: s.Preferences}
}
