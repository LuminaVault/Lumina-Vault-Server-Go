package llm

import (
	"context"
	"encoding/base64"
	"lumina/internal/secretbox"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ports ModelRouterTests with Swift's exact registries and its trial-tier user:
// TestTableRouter covers the same matrix, but with every provider enabled and a
// "free" tier.
func TestAuditETableRouterSwiftRegistries(t *testing.T) {
	ctx := context.Background()
	trial := func(privacy bool) *User {
		return &User{ID: uuid.New(), Tier: "trial", PrivacyNoCNOrigin: privacy}
	}
	pick := func(enabled []ProviderKind, c Capability, u *User) Decision {
		return (&TableRouter{Registry: enabledRegistry(enabled...), HermesDefaultModel: "hermes-3"}).Pick(ctx, "", c, u)
	}
	pro := &User{ID: uuid.New(), Tier: "pro"}

	t.Run("pro high routes sonnet then opus then gpt5", func(t *testing.T) {
		d := pick([]ProviderKind{Anthropic, OpenAI}, CapabilityHigh, pro)
		assert.Equal(t, Route{Anthropic, "claude-sonnet-4-6"}, d.Primary)
		assert.Contains(t, d.Fallbacks, Route{Anthropic, "claude-opus-4-7"})
		assert.Contains(t, d.Fallbacks, Route{OpenAI, "gpt-5"})
		require.NotEmpty(t, d.Fallbacks)
		assert.Equal(t, hermesRoute, d.Fallbacks[len(d.Fallbacks)-1])
	})
	t.Run("pro medium routes sonnet then gemini 25 pro", func(t *testing.T) {
		d := pick([]ProviderKind{Anthropic, Gemini}, CapabilityMedium, pro)
		assert.Equal(t, Route{Anthropic, "claude-sonnet-4-6"}, d.Primary)
		assert.Contains(t, d.Fallbacks, Route{Gemini, "gemini-2.5-pro"})
	})
	t.Run("free tier routes the openrouter free leg then nvidia", func(t *testing.T) {
		d := pick([]ProviderKind{OpenRouter, Nvidia}, CapabilityHigh, trial(false))
		assert.Equal(t, freeLead, d.Primary)
		assert.Contains(t, d.Fallbacks, freeNim)
	})
	t.Run("the free lane is the same at every capability level", func(t *testing.T) {
		for _, c := range []Capability{CapabilityHigh, CapabilityMedium, CapabilityLow} {
			assert.Equal(t, freeLead, pick([]ProviderKind{OpenRouter, Nvidia}, c, trial(false)).Primary, c)
		}
	})
	t.Run("free tier falls back to hermes when neither free leg is configured", func(t *testing.T) {
		assert.Equal(t, hermesRoute, pick(nil, CapabilityHigh, trial(false)).Primary)
	})
	t.Run("privacy no CN origin leaves the free lane intact", func(t *testing.T) {
		d := pick([]ProviderKind{OpenRouter, Nvidia}, CapabilityHigh, trial(true))
		assert.Equal(t, freeLead, d.Primary)
		for _, r := range d.Candidates() {
			assert.False(t, IsCNOrigin(r.ModelID), r.ModelID)
		}
	})
	t.Run("privacy no CN origin keeps non CN routes intact for pro", func(t *testing.T) {
		u := &User{ID: uuid.New(), Tier: "pro", PrivacyNoCNOrigin: true}
		assert.Equal(t, Anthropic, pick([]ProviderKind{Anthropic, OpenAI}, CapabilityHigh, u).Primary.Provider)
	})
	t.Run("disabled providers fall through to hermes", func(t *testing.T) {
		d := pick(nil, CapabilityHigh, pro)
		assert.Equal(t, hermesRoute, d.Primary)
		assert.Empty(t, d.Fallbacks)
	})
	t.Run("ultimate tier uses pro routing table", func(t *testing.T) {
		assert.Equal(t, Anthropic, pick([]ProviderKind{Anthropic}, CapabilityMedium, &User{ID: uuid.New(), Tier: "ultimate"}).Primary.Provider)
	})
	t.Run("ultimate override on lapsed user still routes pro", func(t *testing.T) {
		u := &User{ID: uuid.New(), Tier: "lapsed", TierOverride: "ultimate"}
		assert.Equal(t, Anthropic, pick([]ProviderKind{Anthropic}, CapabilityHigh, u).Primary.Provider)
	})
	t.Run("nil user defaults to free routing", func(t *testing.T) {
		assert.Equal(t, freeLead, pick([]ProviderKind{OpenRouter, Nvidia}, CapabilityMedium, nil).Primary)
	})
}

// Ports ProviderCredentialWiringTests against the real assembly: Swift lists the
// adapters as App+build constructs them, so here NewStack does the constructing.
// Every kind the UI collects a key for must land on an adapter that can spend
// it, and without a Box (no store) none may claim to.
func TestAuditECredentialWiringInStack(t *testing.T) {
	box, err := secretbox.New(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	require.NoError(t, err)
	env := envOf(nil)
	withStore := NewStack(StackConfig{Getenv: env, Box: box, HermesURL: mustParseURL("http://hermes.test")})
	withoutStore := NewStack(StackConfig{Getenv: env, HermesURL: mustParseURL("http://hermes.test")})
	require.NotNil(t, withStore.Credentials)

	for kind := range userCredentialTargets {
		t.Run("every user-credential target has a BYO-capable adapter: "+string(kind), func(t *testing.T) {
			adapter, ok := withStore.Registry.Adapter(kind)
			require.True(t, ok, "%s accepts a user key with no adapter to spend it", kind)
			assert.True(t, adapter.AcceptsUserCredentials(), kind)
		})
		t.Run("an adapter with no store does not claim to accept user credentials: "+string(kind), func(t *testing.T) {
			adapter, ok := withoutStore.Registry.Adapter(kind)
			require.True(t, ok)
			assert.False(t, adapter.AcceptsUserCredentials(), kind)
		})
	}
}
