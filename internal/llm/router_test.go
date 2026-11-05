package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func enabledRegistry(kinds ...ProviderKind) *Registry {
	configs := make([]ProviderConfig, 0, len(kinds))
	for _, k := range kinds {
		configs = append(configs, ProviderConfig{Kind: k, APIKey: "k"})
	}
	return NewRegistry(configs, namedAdapter{HermesGateway})
}

var (
	hermesRoute = Route{HermesGateway, "hermes-3"}
	freeLead    = Route{OpenRouter, FreeLaneOpenRouterModel}
	freeNim     = Route{Nvidia, FreeLaneNvidiaModel}
)

// ModelRouterTests.
func TestTableRouter(t *testing.T) {
	ctx := context.Background()
	pro := &User{ID: uuid.New(), Tier: "pro"}
	free := &User{ID: uuid.New(), Tier: "free"}
	all := enabledRegistry(Anthropic, OpenAI, Gemini, OpenRouter, Nvidia)
	router := &TableRouter{Registry: all, HermesDefaultModel: "hermes-3"}

	high := router.Pick(ctx, "", CapabilityHigh, pro)
	assert.Equal(t, Route{Anthropic, "claude-sonnet-4-6"}, high.Primary)
	assert.Contains(t, high.Fallbacks, Route{Anthropic, "claude-opus-4-7"})
	assert.Contains(t, high.Fallbacks, Route{OpenAI, "gpt-5"})
	assert.Equal(t, hermesRoute, high.Fallbacks[len(high.Fallbacks)-1])

	medium := router.Pick(ctx, "", CapabilityMedium, pro)
	assert.Equal(t, Route{Anthropic, "claude-sonnet-4-6"}, medium.Primary)
	assert.Contains(t, medium.Fallbacks, Route{Gemini, "gemini-2.5-pro"})

	lane := router.Pick(ctx, "", CapabilityMedium, free)
	assert.Equal(t, freeLead, lane.Primary)
	assert.Contains(t, lane.Fallbacks, freeNim)
	for _, c := range []Capability{CapabilityLow, CapabilityMedium, CapabilityHigh} {
		assert.Equal(t, freeLead, router.Pick(ctx, "", c, free).Primary, c)
	}
	assert.Equal(t, freeLead, router.Pick(ctx, "", CapabilityHigh, nil).Primary, "a nil user routes free")

	none := &TableRouter{Registry: enabledRegistry(), HermesDefaultModel: "hermes-3"}
	assert.Equal(t, hermesRoute, none.Pick(ctx, "", CapabilityMedium, free).Primary)
	disabled := none.Pick(ctx, "", CapabilityMedium, pro)
	assert.Equal(t, hermesRoute, disabled.Primary)
	assert.Empty(t, disabled.Fallbacks)

	private := &User{ID: uuid.New(), Tier: "free", PrivacyNoCNOrigin: true}
	privateLane := router.Pick(ctx, "", CapabilityMedium, private)
	assert.Equal(t, freeLead, privateLane.Primary)
	for _, route := range privateLane.Candidates() {
		assert.False(t, IsCNOrigin(route.ModelID), route.ModelID)
	}
	privatePro := &User{ID: uuid.New(), Tier: "pro", PrivacyNoCNOrigin: true}
	assert.Equal(t, Anthropic, router.Pick(ctx, "", CapabilityMedium, privatePro).Primary.Provider)

	assert.Equal(t, Anthropic, router.Pick(ctx, "", CapabilityMedium, &User{Tier: "ultimate"}).Primary.Provider)
	assert.Equal(t, Anthropic, router.Pick(ctx, "", CapabilityMedium, &User{Tier: "lapsed", TierOverride: "ultimate"}).Primary.Provider)
}

func TestModelOrigin(t *testing.T) {
	for _, id := range []string{"deepseek/deepseek-r1-0528", "together_ai/deepseek-ai/DeepSeek-V3", "qwen/qwen-2.5", "Moonshot-v1", "yi-large"} {
		assert.True(t, IsCNOrigin(id), id)
	}
	assert.False(t, IsCNOrigin("claude-sonnet-4-6"))
}

func TestSingleGatewayRouter(t *testing.T) {
	d := SingleGatewayRouter{HermesDefaultModel: "hermes-3"}.Pick(context.Background(), "ignored", CapabilityHigh, nil)
	assert.Equal(t, hermesRoute, d.Primary)
	assert.Empty(t, d.Fallbacks)
}

// RoutingModelRouterTests.
func TestRoutingModelRouter(t *testing.T) {
	ctx := context.Background()
	r := RoutingModelRouter{HermesDefaultModel: "hermes-3"}
	assert.Equal(t, HermesGateway, r.Pick(ctx, "", CapabilityLow, nil).Primary.Provider)
	assert.Empty(t, r.Pick(ctx, "", CapabilityLow, nil).Fallbacks)
	for _, model := range []string{"gemini-2.5-pro", "gemini-2.5-flash", "gemini", "GEMINI-2.5-PRO"} {
		d := r.Pick(ctx, model, CapabilityLow, nil)
		assert.Equal(t, Route{Gemini, model}, d.Primary)
		assert.Equal(t, []Route{hermesRoute}, d.Fallbacks)
	}
	for _, model := range []string{"gpt-5", "claude-sonnet-4-6"} {
		d := r.Pick(ctx, model, CapabilityLow, nil)
		assert.Equal(t, Route{HermesGateway, model}, d.Primary)
		assert.Empty(t, d.Fallbacks)
	}
	custom := RoutingModelRouter{HermesDefaultModel: "hermes-3", FallbackKinds: []ProviderKind{OpenRouter, Groq}}
	assert.Equal(t, []ProviderKind{HermesGateway, OpenRouter, Groq}, providersOf(custom.Pick(ctx, "gemini-x", CapabilityLow, nil).Fallbacks))
}

func providersOf(routes []Route) []ProviderKind {
	out := make([]ProviderKind, 0, len(routes))
	for _, r := range routes {
		out = append(out, r.Provider)
	}
	return out
}

type fixedRouter struct{ decision Decision }

func (r fixedRouter) Pick(context.Context, string, Capability, *User) Decision { return r.decision }

type stubPreferences struct {
	pref *Preference
	err  error
}

func (s stubPreferences) Get(context.Context, uuid.UUID) (*Preference, error) { return s.pref, s.err }

// UserPreferenceModelRouter has no dedicated Swift suite; these pin its code.
func TestUserPreferenceRouter(t *testing.T) {
	ctx := context.Background()
	table := Decision{Primary: Route{OpenRouter, "t1"}, Fallbacks: []Route{hermesRoute}}
	user := &User{ID: uuid.New()}
	pick := func(s stubPreferences, u *User) Decision {
		r := &UserPreferenceRouter{Preferences: s, Fallback: fixedRouter{table}}
		return r.Pick(ctx, "", CapabilityMedium, u)
	}

	assert.Equal(t, table, pick(stubPreferences{}, nil), "no user")
	assert.Equal(t, table, pick(stubPreferences{}, user), "no row")
	assert.Equal(t, table, pick(stubPreferences{err: errors.New("db")}, user), "a failed lookup falls through")

	managed := pick(stubPreferences{pref: &Preference{Mode: ModeManaged, PrimaryProvider: XAI, PrimaryModel: "grok"}}, user)
	assert.Equal(t, table.Primary, managed.Primary, "the row's model does not steer managed routing")
	assert.Equal(t, ModeManaged, managed.CredentialMode)

	byok := pick(stubPreferences{pref: &Preference{
		Mode: ModeBYOK, PrimaryProvider: XAI, PrimaryModel: "grok",
		FallbackChain: []PreferenceStep{{OpenAI, "gpt"}, {OpenRouter, "t1"}},
	}}, user)
	assert.Equal(t, []Route{{XAI, "grok"}, {OpenAI, "gpt"}, {OpenRouter, "t1"}, hermesRoute}, byok.Candidates(), "deduped, table stitched on last")
	assert.Equal(t, ModeBYOK, byok.CredentialMode)

	blocked := pick(stubPreferences{pref: &Preference{
		Mode: ModeBYOK, PrimaryProvider: XAI, PrimaryModel: "grok",
		FallbackChain: []PreferenceStep{{OpenAI, "gpt"}}, Blocked: []ProviderKind{XAI, HermesGateway},
	}}, user)
	assert.Equal(t, []Route{{OpenAI, "gpt"}, {OpenRouter, "t1"}}, blocked.Candidates())

	allowed := pick(stubPreferences{pref: &Preference{
		Mode: ModeBYOK, PrimaryProvider: XAI, PrimaryModel: "grok", Allowed: []ProviderKind{OpenRouter},
	}}, user)
	assert.Equal(t, []Route{{OpenRouter, "t1"}}, allowed.Candidates())

	empty := pick(stubPreferences{pref: &Preference{
		Mode: ModeBYOK, PrimaryProvider: XAI, PrimaryModel: "grok", Allowed: []ProviderKind{Nous},
	}}, user)
	assert.Equal(t, table.Candidates(), empty.Candidates(), "never an empty candidate list")
	assert.Equal(t, ModeBYOK, empty.CredentialMode, "still fails closed")
}

func TestDecodePreference(t *testing.T) {
	p, err := decodePreference("byok", "openRouter", "m",
		[]byte(`{"steps":[{"provider":"xai","model":"g"},{"provider":"nope","model":"x"}]}`),
		[]byte(`{"providers":["xai","nope"]}`), nil)
	assert.NoError(t, err)
	assert.Equal(t, &Preference{
		Mode: ModeBYOK, PrimaryProvider: OpenRouter, PrimaryModel: "m",
		FallbackChain: []PreferenceStep{{XAI, "g"}}, Allowed: []ProviderKind{XAI},
	}, p)

	unknown, err := decodePreference("weird", "nope", "m", []byte(`{"steps":[]}`), nil, nil)
	assert.NoError(t, err)
	assert.Equal(t, ModeManaged, unknown.Mode, "an unknown mode is managed")
	assert.Equal(t, HermesGateway, unknown.PrimaryProvider)

	_, err = decodePreference("byok", "xai", "m", []byte(`[]`), nil, nil)
	assert.Error(t, err, "a chain of the wrong shape fails the lookup")
}
