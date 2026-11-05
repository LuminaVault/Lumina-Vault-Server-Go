package llm

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cacheTTL is the 600 s both Swift stores cache rows for. A credential or
// preference changed through the Swift process stays stale here for up to this
// long, as it does across Swift replicas (coupling.md).
const cacheTTL = 600 * time.Second

// PreferenceStep is one rung of a tenant's fallback chain.
type PreferenceStep struct {
	Provider ProviderKind
	Model    string
}

// Preference is UserLLMPreferenceRepository.Snapshot. An empty Allowed means all
// providers.
type Preference struct {
	Mode            CredentialMode
	PrimaryProvider ProviderKind
	PrimaryModel    string
	FallbackChain   []PreferenceStep
	Allowed         []ProviderKind
	Blocked         []ProviderKind
}

// PreferenceSource reads a tenant's LLM preference. Nil with a nil error means
// no row.
type PreferenceSource interface {
	Get(ctx context.Context, tenant uuid.UUID) (*Preference, error)
}

// PreferenceRepository is the read half of UserLLMPreferenceRepository, with its
// TTL cache. The write half moves with me-providers in wave 5.
type PreferenceRepository struct {
	pool  *pgxpool.Pool
	ttl   time.Duration
	now   func() time.Time
	mu    sync.Mutex
	cache map[uuid.UUID]preferenceEntry
}

type preferenceEntry struct {
	snapshot *Preference
	expires  time.Time
}

func NewPreferenceRepository(pool *pgxpool.Pool) *PreferenceRepository {
	return &PreferenceRepository{pool: pool, ttl: cacheTTL, now: time.Now, cache: map[uuid.UUID]preferenceEntry{}}
}

// Invalidate drops one tenant's cached row.
func (r *PreferenceRepository) Invalidate(tenant uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, tenant)
}

func (r *PreferenceRepository) Get(ctx context.Context, tenant uuid.UUID) (*Preference, error) {
	r.mu.Lock()
	cached, ok := r.cache[tenant]
	r.mu.Unlock()
	if ok && cached.expires.After(r.now()) {
		return cached.snapshot, nil
	}
	snapshot, err := r.load(ctx, tenant)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.cache[tenant] = preferenceEntry{snapshot, r.now().Add(r.ttl)}
	r.mu.Unlock()
	return snapshot, nil
}

func (r *PreferenceRepository) load(ctx context.Context, tenant uuid.UUID) (*Preference, error) {
	var mode, primaryProvider, primaryModel string
	var chainRaw, allowedRaw, blockedRaw []byte
	err := r.pool.QueryRow(ctx, `SELECT mode, primary_provider, primary_model, fallback_chain, allowed_providers, blocked_providers
		FROM user_llm_preferences WHERE tenant_id = $1 LIMIT 1`, tenant).
		Scan(&mode, &primaryProvider, &primaryModel, &chainRaw, &allowedRaw, &blockedRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodePreference(mode, primaryProvider, primaryModel, chainRaw, allowedRaw, blockedRaw)
}

func decodePreference(mode, primaryProvider, primaryModel string, chainRaw, allowedRaw, blockedRaw []byte) (*Preference, error) {
	p := &Preference{Mode: ModeManaged, PrimaryProvider: HermesGateway, PrimaryModel: primaryModel}
	if mode == string(ModeBYOK) {
		p.Mode = ModeBYOK
	}
	if knownProvider(primaryProvider) {
		p.PrimaryProvider = ProviderKind(primaryProvider)
	}
	var chain struct {
		Steps []struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(chainRaw, &chain); err != nil {
		return nil, err
	}
	for _, s := range chain.Steps {
		if knownProvider(s.Provider) {
			p.FallbackChain = append(p.FallbackChain, PreferenceStep{ProviderKind(s.Provider), s.Model})
		}
	}
	var err error
	if p.Allowed, err = decodeProviderList(allowedRaw); err != nil {
		return nil, err
	}
	if p.Blocked, err = decodeProviderList(blockedRaw); err != nil {
		return nil, err
	}
	return p, nil
}

func decodeProviderList(raw []byte) ([]ProviderKind, error) {
	if raw == nil {
		return nil, nil
	}
	var list struct {
		Providers []string `json:"providers"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	var out []ProviderKind
	for _, s := range list.Providers {
		if knownProvider(s) {
			out = append(out, ProviderKind(s))
		}
	}
	return out, nil
}

func knownProvider(s string) bool {
	switch ProviderKind(s) {
	case HermesGateway, Anthropic, OpenAI, Gemini, Together, Groq, Fireworks, DeepInfra, DeepseekDirect,
		OpenRouter, Deepseek, Kimi, Ollama, XAI, Nvidia, Nous, Custom:
		return true
	}
	return false
}

// UserPreferenceRouter is UserPreferenceModelRouter: a tenant's primary and
// fallback chain first, with the table stitched on as the last-resort cascade.
type UserPreferenceRouter struct {
	Preferences PreferenceSource
	Fallback    Router
}

func (r *UserPreferenceRouter) Pick(ctx context.Context, model string, capability Capability, user *User) Decision {
	table := r.Fallback.Pick(ctx, model, capability, user)
	if user == nil {
		return table
	}
	pref, err := r.Preferences.Get(ctx, user.ID)
	if err != nil {
		slog.ErrorContext(ctx, "user llm preference lookup failed", "error", err)
		return table
	}
	if pref == nil {
		return table
	}
	if pref.Mode == ModeManaged {
		// Managed users opted out of BYOK routing. The row's primary model is
		// kept for the UI but does not steer routing.
		return Decision{Primary: table.Primary, Fallbacks: table.Fallbacks, Cerberus: table.Cerberus, CredentialMode: ModeManaged}
	}
	isAllowed := func(p ProviderKind) bool {
		if slices.Contains(pref.Blocked, p) {
			return false
		}
		return len(pref.Allowed) == 0 || slices.Contains(pref.Allowed, p)
	}
	chain := []Route{{pref.PrimaryProvider, pref.PrimaryModel}}
	for _, s := range pref.FallbackChain {
		chain = append(chain, Route{s.Provider, s.Model})
	}
	chain = append(chain, table.Candidates()...)
	allowed := make([]Route, 0, len(chain))
	for _, route := range chain {
		if isAllowed(route.Provider) && !slices.Contains(allowed, route) {
			allowed = append(allowed, route)
		}
	}
	if len(allowed) == 0 {
		// Never an empty candidate list. Still BYOK, so the adapters fail closed
		// rather than spend the deployment key.
		slog.WarnContext(ctx, "llm allow/block lists filtered out all routes; using table cascade")
		return Decision{Primary: table.Primary, Fallbacks: table.Fallbacks, Cerberus: table.Cerberus, CredentialMode: ModeBYOK}
	}
	return Decision{Primary: allowed[0], Fallbacks: allowed[1:], CredentialMode: ModeBYOK}
}
