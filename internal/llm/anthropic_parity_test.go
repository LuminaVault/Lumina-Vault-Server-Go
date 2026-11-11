package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Assertions present in the Swift suites (AnthropicAdapterTranslationTests,
// AnthropicManagedTrialRouterTests, AnthropicTrialKeyWiringTests) that the
// first port dropped or weakened.

// Swift: `haiku is priced for the meters but not routable`.
func TestAuditAHaikuPricedButNotRoutable(t *testing.T) {
	priced, ok := catalogPricing(ProviderID(Anthropic), "claude-haiku-5-5")
	require.True(t, ok, "the meters can price the trial model")
	assert.EqualValues(t, 100_000, priced.InputPerMillionUsdMicros)
	assert.EqualValues(t, 500_000, priced.OutputPerMillionUsdMicros)

	_, routable := CatalogEntry(ProviderID(Anthropic), "claude-haiku-5-5")
	assert.False(t, routable, "no tenant may route to the trial model by choice")
	for _, e := range RouterCatalogEntries() {
		assert.NotEqual(t, "claude-haiku-5-5", e.Model)
	}
	assert.Equal(t, int64(600_000), CatalogCost(ProviderID(Anthropic), "claude-haiku-5-5", 1_000_000, 1_000_000, nil))
}

// Swift: the json_object line is appended to the system prompt (hasSuffix),
// not merely somewhere in it.
func TestAuditAJSONOnlyInstructionIsTheSuffix(t *testing.T) {
	_, body := translate(t, obj{"response_format": obj{"type": "json_object"}, "messages": []any{
		obj{"role": "system", "content": "Extract."}, obj{"role": "user", "content": "x"},
	}}, testManaged)
	system, _ := body["system"].(string)
	assert.True(t, strings.HasSuffix(system, JSONOnlyInstruction), "system = %q", system)
	assert.True(t, strings.HasPrefix(system, "Extract."), "the caller's system prompt comes first")
}

// Swift checks the merged user turn holds exactly ["one", "two"] and every
// tool_result block's type, not only the first.
func TestAuditATurnShapesExactly(t *testing.T) {
	t.Run("merged user turn holds exactly the two texts", func(t *testing.T) {
		_, body := translate(t, obj{"messages": []any{
			obj{"role": "user", "content": "one"},
			obj{"role": "user", "content": "two"},
			obj{"role": "assistant", "content": "Sure"},
		}}, testManaged)
		turns := turnsOf(body)
		require.Len(t, turns, 3)
		var texts []string
		for _, b := range blocksOf(turns[0]) {
			if s, ok := b["text"].(string); ok {
				texts = append(texts, s)
			}
		}
		assert.Equal(t, []string{"one", "two"}, texts)
		assert.Equal(t, ContinuePrompt, blocksOf(turns[2])[0]["text"])
	})
	t.Run("every merged tool result is a tool_result", func(t *testing.T) {
		_, body := translate(t, obj{
			"tools": lookupTool,
			"messages": []any{
				obj{"role": "user", "content": "go"},
				obj{"role": "assistant", "tool_calls": []any{
					obj{"id": "call_1", "type": "function", "function": obj{"name": "lookup", "arguments": "{}"}},
					obj{"id": "call_2", "type": "function", "function": obj{"name": "lookup", "arguments": "{}"}},
				}},
				obj{"role": "tool", "tool_call_id": "call_1", "content": "a"},
				obj{"role": "tool", "tool_call_id": "call_2", "content": "b"},
			},
		}, testManaged)
		turns := turnsOf(body)
		require.Equal(t, []string{"user", "assistant", "user"}, rolesOf(turns))
		results := blocksOf(turns[2])
		require.Len(t, results, 2)
		for _, r := range results {
			assert.Equal(t, "tool_result", r["type"])
		}
	})
}

// Swift: `the trial key alias does not enable anthropic in the registry` also
// asserts loadConfigs carries no anthropic entry at all.
func TestAuditATrialKeyNotInProviderConfigs(t *testing.T) {
	env := envOf(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-trial", "ANTHROPIC_FIRST": "true"})
	for _, c := range LoadProviderConfigs(env) {
		if c.Kind == Anthropic {
			assert.False(t, c.IsEnabled(), "the trial alias must not produce an enabled anthropic config")
		}
	}
	assert.False(t, NewRegistry(LoadProviderConfigs(env)).IsEnabled(Anthropic))
}

// Swift registers the real AnthropicAdapter (empty registry key, trial loaded
// from the same env) before asking the table router. The first port only
// registered a hermes adapter, so an adapter-presence bug would not show.
func TestAuditAProTableIgnoresTrialAdapter(t *testing.T) {
	env := envOf(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-trial", "ANTHROPIC_FIRST": "true"})
	trial := LoadTrialConfig(env)
	require.NotNil(t, trial)
	registry := NewRegistry(LoadProviderConfigs(env), &AnthropicAdapter{APIKey: "", Trial: trial}, namedAdapter{HermesGateway})
	assert.False(t, registry.IsEnabled(Anthropic), "registering the trial adapter must not enable anthropic")
	r := &TableRouter{Registry: registry, HermesDefaultModel: "hermes-3"}
	for _, capability := range []Capability{CapabilityLow, CapabilityMedium, CapabilityHigh} {
		d := r.Pick(context.Background(), "", capability, &User{ID: uuid.New(), Tier: "pro"})
		assert.NotContains(t, providersOf(d.Candidates()), Anthropic, capability)
	}
}
