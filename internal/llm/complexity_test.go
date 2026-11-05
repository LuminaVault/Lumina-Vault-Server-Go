package llm

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func wireRoute(provider ProviderKind, model string) WireRoute {
	return WireRoute{Provider: ProviderID(provider), Model: model}
}

func providerSet(kinds ...ProviderKind) map[ProviderID]bool {
	out := map[ProviderID]bool{}
	for _, k := range kinds {
		out[ProviderID(k)] = true
	}
	return out
}

func allOn(pool []WireRoute, provider ProviderKind) bool {
	return !slices.ContainsFunc(pool, func(r WireRoute) bool { return r.Provider != ProviderID(provider) })
}

func hasModel(pool []WireRoute, model string) bool {
	return slices.ContainsFunc(pool, func(r WireRoute) bool { return r.Model == model })
}

// ManagedModelCatalogTests.
func TestManagedModelCatalog(t *testing.T) {
	t.Run("managed default has an OpenRouter catalog entry", func(t *testing.T) {
		entry, ok := CatalogEntry(ManagedProvider, ManagedModel)
		require.True(t, ok)
		assert.Equal(t, "DeepSeek V4 Flash", entry.DisplayName)
		assert.Equal(t, int64(90000), entry.InputPerMillionUsdMicros)
		assert.Equal(t, int64(180_000), entry.OutputPerMillionUsdMicros)
		assert.Contains(t, entry.Capabilities, "tools")
		assert.Contains(t, entry.Capabilities, "reasoning")
	})
	t.Run("the nvidia catalog carries the paid Nemotron entries", func(t *testing.T) {
		super120, ok := CatalogEntry(ProviderID(Nvidia), FreeLaneNvidiaModel)
		require.True(t, ok)
		assert.Equal(t, TierBalanced, super120.Tier)
		ultra, ok := CatalogEntry(ProviderID(Nvidia), "nvidia/nemotron-3-ultra-550b-a55b")
		require.True(t, ok)
		assert.Equal(t, TierMax, ultra.Tier)
	})
	t.Run("zero-cost free slugs stay out of the router catalog", func(t *testing.T) {
		_, ok := CatalogEntry(ProviderID(OpenRouter), FreeLaneOpenRouterModel)
		assert.False(t, ok)
		for _, e := range RouterCatalogEntries() {
			assert.False(t, strings.HasSuffix(e.Model, ":free"))
		}
	})
}

// ComplexityClassifierTests.
func TestComplexityClassifier(t *testing.T) {
	t.Run("simple greeting is low", func(t *testing.T) {
		assert.Equal(t, ComplexityLow, ClassifyComplexity("hi there", SurfaceChat))
	})
	t.Run("simple math is low", func(t *testing.T) {
		assert.Equal(t, ComplexityLow, ClassifyComplexity("what is 2+2?", SurfaceChat))
	})
	t.Run("short summary is low or medium", func(t *testing.T) {
		assert.Contains(t, []RouterComplexity{ComplexityLow, ComplexityMedium}, ClassifyComplexity("summarize this: the cat sat on the mat", SurfaceChat))
	})
	t.Run("complex debug is high", func(t *testing.T) {
		prompt := "Please debug and refactor this distributed lock-free queue. Prove correctness\n" +
			"and optimize the algorithm for multi-step concurrent access.\n\n```swift\n// large code block\n" +
			strings.Repeat("func f() {}\n", 50) + "\n```"
		assert.Equal(t, ComplexityHigh, ClassifyComplexity(prompt, SurfaceChat))
	})
	t.Run("job surface floors at medium", func(t *testing.T) {
		assert.GreaterOrEqual(t, ClassifyComplexity("hi", SurfaceJob).rank(), ComplexityMedium.rank())
	})
	t.Run("implement plan is at least medium", func(t *testing.T) {
		got := ClassifyComplexity("Please plan and implement a migration for the auth service API", SurfaceChat)
		assert.GreaterOrEqual(t, got.rank(), ComplexityMedium.rank())
	})
}

// AvailableModelPoolBuilderTests.
func TestModelPoolBuilder(t *testing.T) {
	opus, gpt4o := wireRoute(Anthropic, "claude-opus-4-1"), wireRoute(OpenAI, "gpt-4o")
	t.Run("byok only uses credentialed providers", func(t *testing.T) {
		pool := BuildModelPool(PoolInput{
			Mode: ModeBYOK, Policy: PolicyBalanced, ProfileRoutes: []WireRoute{opus, gpt4o},
			CredentialedProviders: providerSet(Anthropic), DeploymentEnabledProviders: providerSet(OpenRouter), MinTier: TierFast,
		})
		assert.True(t, allOn(pool, Anthropic))
		assert.True(t, slices.ContainsFunc(pool, func(r WireRoute) bool {
			return strings.Contains(r.Model, "haiku") || RouteTier(r) == TierFast
		}))
	})
	t.Run("byok pool can be empty when profile excludes credentialed providers", func(t *testing.T) {
		pool := BuildModelPool(PoolInput{
			Mode: ModeBYOK, Policy: PolicyBalanced, ProfileRoutes: []WireRoute{opus, gpt4o},
			AllowedProviders:      []ProviderID{ProviderID(OpenRouter)},
			CredentialedProviders: providerSet(Anthropic), DeploymentEnabledProviders: providerSet(OpenRouter), MinTier: TierFast,
		})
		assert.Empty(t, pool)
	})
	t.Run("tier floor drops cheap when high", func(t *testing.T) {
		pool := BuildModelPool(PoolInput{
			Mode: ModeBYOK, Policy: PolicyBalanced, ProfileRoutes: []WireRoute{wireRoute(Anthropic, "claude-3-5-haiku-20241022"), opus},
			CredentialedProviders: providerSet(Anthropic), MinTier: TierMax,
		})
		for _, r := range pool {
			assert.Equal(t, TierMax, RouteTier(r), r.Model)
		}
	})
	t.Run("reason strings are non-empty", func(t *testing.T) {
		route := wireRoute(Gemini, "gemini-2.5-flash")
		assert.Contains(t, RoutingReason(PolicyAutoSmart, ComplexityLow, TaskGeneral, route, false), "Auto")
		assert.Contains(t, RoutingReason(PolicyLocked, ComplexityLow, TaskGeneral, route, true), "Hermes")
	})
}

// AutoSmartPoolTests.
func TestAutoSmartPool(t *testing.T) {
	t.Run("managed auto pool is OpenRouter only", func(t *testing.T) {
		pool := BuildModelPool(PoolInput{
			Mode: ModeManaged, Policy: PolicyAutoSmart,
			ProfileRoutes:         []WireRoute{wireRoute(Anthropic, "claude-opus-4-1"), {Provider: ManagedProvider, Model: ManagedModel}},
			CredentialedProviders: providerSet(Anthropic, Gemini), DeploymentEnabledProviders: providerSet(OpenAI, Gemini), MinTier: TierFast,
		})
		assert.NotEmpty(t, pool)
		assert.True(t, allOn(pool, OpenRouter))
	})
	t.Run("managed auto works with no deployment keys", func(t *testing.T) {
		pool := BuildModelPool(PoolInput{Mode: ModeManaged, Policy: PolicyAutoSmart, MinTier: TierFast})
		assert.NotEmpty(t, pool)
		assert.True(t, allOn(pool, OpenRouter))
	})
	t.Run("managed auto high complexity reaches frontier", func(t *testing.T) {
		pool := BuildModelPool(PoolInput{Mode: ModeManaged, Policy: PolicyAutoSmart, MinTier: TierMax})
		assert.True(t, allOn(pool, OpenRouter))
		for _, model := range []string{"x-ai/grok-4.5", "openai/gpt-5", "anthropic/claude-opus-4.1"} {
			assert.True(t, hasModel(pool, model), model)
		}
	})
	t.Run("byok auto requires OpenRouter credential", func(t *testing.T) {
		without := BuildModelPool(PoolInput{
			Mode: ModeBYOK, Policy: PolicyAutoSmart, ProfileRoutes: []WireRoute{wireRoute(Anthropic, "claude-opus-4-1")},
			CredentialedProviders: providerSet(Anthropic), DeploymentEnabledProviders: providerSet(OpenRouter), MinTier: TierFast,
		})
		assert.Empty(t, without)
		with := BuildModelPool(PoolInput{Mode: ModeBYOK, Policy: PolicyAutoSmart, CredentialedProviders: providerSet(OpenRouter, Anthropic), MinTier: TierFast})
		assert.NotEmpty(t, with)
		assert.True(t, allOn(with, OpenRouter))
	})
	t.Run("non-auto policies keep legacy pool shape", func(t *testing.T) {
		pool := BuildModelPool(PoolInput{
			Mode: ModeBYOK, Policy: PolicyBalanced, ProfileRoutes: []WireRoute{wireRoute(Anthropic, "claude-opus-4-1")},
			CredentialedProviders: providerSet(Anthropic), MinTier: TierFast,
		})
		assert.NotEmpty(t, pool)
		assert.True(t, allOn(pool, Anthropic))
	})
}

// ManagedAutoGatewayMappingTests.
func TestManagedAutoGatewayMapping(t *testing.T) {
	t.Run("managed auto routes ride the gateway", func(t *testing.T) {
		route := wireRoute(OpenRouter, "x-ai/grok-4.5")
		via, ok := ToModelRoute(route, true)
		require.True(t, ok)
		assert.Equal(t, Route{HermesGateway, "x-ai/grok-4.5"}, via)
		direct, ok := ToModelRoute(route, false)
		require.True(t, ok)
		assert.Equal(t, OpenRouter, direct.Provider)
	})
	t.Run("gateway mapping leaves non-OpenRouter alone", func(t *testing.T) {
		mapped, ok := ToModelRoute(wireRoute(Anthropic, "claude-opus-4-1"), true)
		require.True(t, ok)
		assert.Equal(t, Anthropic, mapped.Provider)
	})
}

// RouterTaskClassifierTests.
func TestRouterTaskClassifier(t *testing.T) {
	t.Run("classifies representative prompts", func(t *testing.T) {
		assert.Equal(t, TaskCoding, ClassifyTask("Debug this Swift function", SurfaceChat))
		assert.Equal(t, TaskSummarization, ClassifyTask("Summarize these meeting notes", SurfaceChat))
		assert.Equal(t, TaskReasoning, ClassifyTask("Compare the trade-offs", SurfaceChat))
		assert.Equal(t, TaskSearch, ClassifyTask("What is the latest news?", SurfaceChat))
		assert.Equal(t, TaskGeneral, ClassifyTask("Hello there", SurfaceChat))
	})
	t.Run("automation surfaces take precedence over prompt keywords", func(t *testing.T) {
		assert.Equal(t, TaskAutomation, ClassifyTask("Summarize this", SurfaceJob))
		assert.Equal(t, TaskAutomation, ClassifyTask("Write Swift code", SurfaceSkill))
	})
}
