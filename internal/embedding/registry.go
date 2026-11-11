package embedding

import (
	"fmt"
	"log/slog"
	"lumina/internal/embedding/db"
	"lumina/internal/secretbox"
	"lumina/internal/swiftstr"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config is what EmbeddingProviderRegistry.bootstrap reads. Empty means unset.
type Config struct {
	Provider      string // embedding.provider; unset or unknown is deterministic
	FallbackChain string // embedding.fallbackChain, comma separated
	MonthlyCap    int64  // embedding.monthlyTokenCapDefault; 0 disables the cap

	OpenAIAPIKey  string // llm.provider.openai.apiKey
	OpenAIBaseURL string // llm.provider.openai.baseURL

	// OpenRouterAPIKeys is every spelling the LLM routing accepts, in its order:
	// llm.provider.openRouter.apiKey, llm.provider.openrouter.apikey,
	// openrouter.api_key, openrouter.fallback_api_key. First non-blank wins.
	OpenRouterAPIKeys []string
	OpenRouterBaseURL string // embedding.openRouter.baseURL
	OpenRouterModel   string // embedding.openRouter.model

	NomicAPIKey string // embedding.nomic.apiKey

	// HermesResolver backs the hermesLocal kind. Nil answers endpointMissing for
	// every tenant; Go has no container manager to resolve against.
	HermesResolver HandleResolver

	// HTTPClient is for tests. Nil is a client with URLSession's 60 s timeout.
	HTTPClient *http.Client
}

// Registry is EmbeddingProviderRegistry. Active is already wrapped by the usage
// tracker and the fallback chain.
type Registry struct {
	Active        Service
	ActiveKind    Kind
	FallbackKinds []Kind
}

// New is the service callers use: the registry's active service, wrapped so a
// tenant with an enabled byok-embeddings install is routed through their own key.
// box nil (no LV_SECRET_MASTER_KEY) skips the wrapper, as in App+build.swift.
func New(pool *pgxpool.Pool, cfg Config, box *secretbox.Box) Service {
	svc, _ := NewWithKind(pool, cfg, box)
	return svc
}

// NewWithKind is New plus the registry's resolved active kind, which
// EmbeddingReembedService reads (embeddingRegistry.activeKind in App+build.swift).
func NewWithKind(pool *pgxpool.Pool, cfg Config, box *secretbox.Box) (Service, Kind) {
	registry := Bootstrap(pool, cfg)
	if box == nil {
		return registry.Active, registry.ActiveKind
	}
	return &tenantAware{
		global:   registry.Active,
		resolver: newPerTenantResolver(db.New(pool), box, keyedServiceFactory(registry.ActiveKind, cfg)),
	}, registry.ActiveKind
}

// Bootstrap is EmbeddingProviderRegistry.bootstrap. It never fails: a provider
// missing its credential logs a warning and the server runs on deterministic
// vectors, so the app still boots.
func Bootstrap(pool *pgxpool.Pool, cfg Config) Registry {
	client := httpClient(cfg)
	requested, ok := ParseKind(cfg.Provider)
	if !ok {
		requested = KindDeterministic
	}

	var fallbackKinds []Kind
	for raw := range strings.SplitSeq(cfg.FallbackChain, ",") {
		// Compared with the requested kind, not the resolved one: a chain naming
		// deterministic keeps it even when the primary fell back to deterministic.
		if kind, ok := ParseKind(raw); ok && kind != requested {
			fallbackKinds = append(fallbackKinds, kind)
		}
	}

	built := map[Kind]Service{
		KindDeterministic: Deterministic{},
		// No static credential: it resolves a per-tenant handle on each call.
		KindHermesLocal: &hermesLocalService{resolve: cfg.HermesResolver, model: hermesDefaultModel, client: client},
	}
	if cfg.OpenAIAPIKey != "" {
		built[KindOpenAI] = newOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, client)
	}
	if key := openRouterAPIKey(cfg.OpenRouterAPIKeys); key != "" {
		model := swiftstr.TrimWhitespacesAndNewlines(cfg.OpenRouterModel)
		if model == "" {
			model = openRouterModel
		}
		built[KindOpenRouter] = &openAIService{
			apiKey:   key,
			baseURL:  parseBaseURL(cfg.OpenRouterBaseURL, openRouterBaseURL),
			model:    model,
			label:    "openrouter",
			maxInput: defaultMaxInputCharacters,
			client:   client,
		}
	}
	if cfg.NomicAPIKey != "" {
		built[KindNomic] = newNomic(cfg.NomicAPIKey, client)
	}

	active := requested
	if _, ok := built[active]; !ok {
		slog.Warn(fmt.Sprintf("embedding provider %s requested but not configured (%s); falling back to deterministic — semantic search, recall and related memories run on hash-based fake vectors", requested, missingCredentialHint(requested)))
		active = KindDeterministic
	}

	chain := []namedService{{active, built[active]}}
	resolvedFallbacks := []Kind{}
	names := []string{}
	for _, kind := range fallbackKinds {
		svc, ok := built[kind]
		if !ok {
			slog.Warn(fmt.Sprintf("embedding fallback %s not configured; dropping from chain", kind))
			continue
		}
		chain = append(chain, namedService{kind, svc})
		resolvedFallbacks = append(resolvedFallbacks, kind)
		names = append(names, string(kind))
	}

	slog.Info(fmt.Sprintf("embedding registry active=%s fallbacks=%s cap=%d", active, strings.Join(names, ","), cfg.MonthlyCap))
	return Registry{
		Active: &usageTracker{
			inner: &fallbackChain{chain: chain},
			q:     db.New(pool),
			cap:   cfg.MonthlyCap,
			now:   time.Now,
		},
		ActiveKind:    active,
		FallbackKinds: resolvedFallbacks,
	}
}

func httpClient(cfg Config) *http.Client {
	if cfg.HTTPClient != nil {
		return cfg.HTTPClient
	}
	return defaultHTTPClient()
}

func newOpenAI(apiKey, rawBaseURL string, client *http.Client) *openAIService {
	return &openAIService{
		apiKey:   apiKey,
		baseURL:  parseBaseURL(rawBaseURL, openAIDefaultBaseURL),
		model:    openAIDefaultModel,
		label:    "openai",
		maxInput: defaultMaxInputCharacters,
		client:   client,
	}
}

func newNomic(apiKey string, client *http.Client) *nomicService {
	return &nomicService{
		apiKey:  apiKey,
		baseURL: parseBaseURL("", nomicDefaultBaseURL),
		model:   nomicDefaultModel,
		client:  client,
	}
}

// openRouterAPIKey is ProviderRegistry.loadConfig's resolution: trimmed, first
// non-blank, so a key sealed as a stray space doesn't shadow a usable alias.
func openRouterAPIKey(keys []string) string {
	for _, key := range keys {
		if key = swiftstr.TrimWhitespacesAndNewlines(key); key != "" {
			return key
		}
	}
	return ""
}

// missingCredentialHint names the env var an operator has to set.
func missingCredentialHint(kind Kind) string {
	switch kind {
	case KindOpenAI:
		return "LLM_PROVIDER_OPENAI_API_KEY is empty"
	case KindOpenRouter:
		return "no OpenRouter key: LLM_PROVIDER_OPEN_ROUTER_API_KEY / OPENROUTER_API_KEY / OPENROUTER_FALLBACK_API_KEY are all empty"
	case KindNomic:
		return "EMBEDDING_NOMIC_API_KEY is empty"
	default:
		return "adapter not registered"
	}
}

// keyedServiceFactory builds a tenant's service of the active kind with their
// key, so the vector space stays the same. Only openai and nomic are keyable: the
// plugin asks for an OpenAI or Nomic key, and an OpenAI key sent to OpenRouter is a
// 401 on every embed, so OpenRouter tenants stay on the platform key.
func keyedServiceFactory(active Kind, cfg Config) func(apiKey string) Service {
	client := httpClient(cfg)
	return func(apiKey string) Service {
		switch active {
		case KindOpenAI:
			return newOpenAI(apiKey, cfg.OpenAIBaseURL, client)
		case KindNomic:
			return newNomic(apiKey, client)
		default:
			return nil
		}
	}
}
