package meproviders

import (
	"encoding/json"
	"lumina/internal/llm"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MARK: - ProviderCatalogTests

func TestCatalogEveryProviderHasAnEntry(t *testing.T) {
	entries := catalogAll(func(llm.ProviderID) bool { return true })
	require.Len(t, entries, len(providerIDs))
	for i, e := range entries {
		assert.Equal(t, providerIDs[i], e.Provider)
		assert.NotEmpty(t, e.DisplayName, "%s has no display name", e.Provider)
	}
}

func TestCatalogProvidersThatAuthenticateRequireAKey(t *testing.T) {
	for _, id := range []llm.ProviderID{"xai", "anthropic", "openai", "openRouter", "nvidia", "gemini", "nous"} {
		e := catalogEntry(id)
		assert.True(t, e.RequiresAPIKey, "%s authenticates and must ask for a key", id)
		assert.NotNil(t, e.DefaultBaseURL, "%s has a known endpoint", id)
	}
}

func TestCatalogSelfHostedProvidersRequireABaseURLInsteadOfAKey(t *testing.T) {
	for _, id := range []llm.ProviderID{"ollama", "custom"} {
		e := catalogEntry(id)
		assert.True(t, e.RequiresBaseURL, "%s needs a base URL", id)
		assert.False(t, e.RequiresAPIKey, "%s commonly has no auth", id)
	}
}

// The keyless set must match the one the router treats as keyless
// (ProviderKind.keylessCapable, which IsSpendable encodes): a base URL alone
// is spendable exactly there.
func TestCatalogKeylessProvidersMatchTheRouter(t *testing.T) {
	base, _ := url.Parse("http://127.0.0.1:9")
	for _, id := range providerIDs {
		kind, ok := llm.FromShared(id)
		require.True(t, ok, id)
		assert.Equal(t, !catalogEntry(id).RequiresAPIKey, kind.IsSpendable(nil, base), id)
	}
}

func TestCatalogAvailabilityReflectsTheDeployment(t *testing.T) {
	entries := catalogAll(func(p llm.ProviderID) bool { return p == "openai" })
	for _, e := range entries {
		switch e.Provider {
		case "openai":
			assert.True(t, e.Available)
		case "anthropic":
			assert.False(t, e.Available)
		}
	}
}

func TestCatalogKeyHintsOnlyWhereAKeyIsTaken(t *testing.T) {
	for _, e := range catalogAll(func(llm.ProviderID) bool { return true }) {
		if e.KeyHint != nil {
			assert.True(t, e.RequiresAPIKey, "%s hints at a key it does not take", e.Provider)
		}
	}
}

// MARK: - ProviderModelListingTests

func TestModelListingEveryProviderIDIsACredentialTarget(t *testing.T) {
	for _, id := range providerIDs {
		kind, ok := llm.FromShared(id)
		assert.True(t, ok, "ProviderID.%s has no ProviderKind twin", id)
		assert.True(t, llm.UserCredentialTarget(kind), "%s missing from userCredentialTargets", kind)
	}
}

func TestModelListingOpenAIShapeFallsBackToDisplayName(t *testing.T) {
	models, ok := parseModels([]byte(`{"data":[
		{"id":"claude-sonnet-4-5","display_name":"Claude Sonnet 4.5"},
		{"id":"claude-opus-4-1"}
	]}`))
	require.True(t, ok)
	require.Len(t, models, 2)
	assert.Equal(t, "Claude Sonnet 4.5", models[0].DisplayName)
	assert.Equal(t, "claude-opus-4-1", models[1].DisplayName)
}

func TestModelListingGeminiStripsPrefixAndFiltersNonChatModels(t *testing.T) {
	models, ok := parseGeminiModels([]byte(`{"models":[
		{"name":"models/gemini-2.5-flash","displayName":"Gemini 2.5 Flash","supportedGenerationMethods":["generateContent","countTokens"]},
		{"name":"models/text-embedding-004","displayName":"Embedding","supportedGenerationMethods":["embedContent"]},
		{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent"]}
	]}`))
	require.True(t, ok)
	require.Len(t, models, 2)
	assert.Equal(t, "gemini-2.5-flash", models[0].ID)
	assert.Equal(t, "gemini-2.5-pro", models[1].ID)
	assert.Equal(t, "Gemini 2.5 Flash", models[0].DisplayName)
	assert.Equal(t, "gemini-2.5-pro", models[1].DisplayName)
}

func TestModelListingOllamaReadsTagNames(t *testing.T) {
	models, ok := parseOllamaModels([]byte(`{"models":[{"name":"llama3:8b","size":123},{"name":"qwen2.5:14b"}]}`))
	require.True(t, ok)
	require.Len(t, models, 2)
	assert.Equal(t, "llama3:8b", models[0].ID)
	assert.Equal(t, "qwen2.5:14b", models[1].ID)
}

func TestModelListingParsersRejectUnexpectedShapes(t *testing.T) {
	garbage := []byte(`{"unexpected":true}`)
	_, ok := parseGeminiModels(garbage)
	assert.False(t, ok)
	_, ok = parseOllamaModels(garbage)
	assert.False(t, ok)
	_, ok = parseModels(garbage)
	assert.False(t, ok)
}

// MARK: - ProviderToolProbeTests

func completion(t *testing.T, message map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": message}}})
	require.NoError(t, err)
	return b
}

func TestToolProbeAToolCallIsSupport(t *testing.T) {
	data := completion(t, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
		"id": "call_1", "type": "function",
		"function": map[string]any{"name": "lookup_job_status", "arguments": `{"id":"lv-abc"}`},
	}}})
	assert.Equal(t, verdictSupported, probeVerdict(data))
}

func TestToolProbeLegacyFunctionCallIsSupport(t *testing.T) {
	data := completion(t, map[string]any{"role": "assistant", "function_call": map[string]any{"name": "lookup_job_status", "arguments": "{}"}})
	assert.Equal(t, verdictSupported, probeVerdict(data))
}

func TestToolProbeProseInsteadOfAToolCallIsNotSupport(t *testing.T) {
	data := completion(t, map[string]any{"role": "assistant", "content": "Job lv-abc completed successfully at 14:03."})
	assert.Equal(t, verdictNotSupported, probeVerdict(data))
}

func TestToolProbeMentioningTheToolInProseIsNotSupport(t *testing.T) {
	data := completion(t, map[string]any{"role": "assistant", "content": "I would call lookup_job_status with that id."})
	assert.Equal(t, verdictNotSupported, probeVerdict(data))
}

func TestToolProbeUnreadableOrEmptyResponseYieldsNoVerdict(t *testing.T) {
	assert.Equal(t, verdictUnknown, probeVerdict(nil))
	assert.Equal(t, verdictUnknown, probeVerdict([]byte("not json")))
	assert.Equal(t, verdictUnknown, probeVerdict(completion(t, map[string]any{"role": "assistant"})))
	assert.Equal(t, verdictUnknown, probeVerdict(completion(t, map[string]any{"role": "assistant", "content": "  "})))
}

func TestToolProbeAnAbsentVerdictPermitsTools(t *testing.T) {
	assert.True(t, allowsTools(nil))
}

func TestToolProbeAStoredVerdictIsHonouredInBothDirections(t *testing.T) {
	yes, no := true, false
	assert.True(t, allowsTools(&yes))
	assert.False(t, allowsTools(&no))
}

func TestToolProbeBindsOneToolAndAsksSomethingOnlyItCanAnswer(t *testing.T) {
	nonce := probeNonce()
	var payload struct {
		Tools     []any `json:"tools"`
		MaxTokens int   `json:"max_tokens"`
		Messages  []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(probePayload("gpt-4o-mini", nonce), &payload))
	assert.Len(t, payload.Tools, 1)
	require.NotEmpty(t, payload.Messages)
	assert.True(t, strings.Contains(payload.Messages[0].Content, nonce))
	assert.LessOrEqual(t, payload.MaxTokens, 64)
}

func TestToolProbeEachProbeUsesAFreshNonce(t *testing.T) {
	assert.NotEqual(t, probeNonce(), probeNonce())
}

// MARK: - OpenAICompletionReaderTests (the ProviderToolProbe half)

// The probe and the trace reader must not disagree about what a tool call is.
func TestToolProbeAgreesWithTheReaderOnEveryShape(t *testing.T) {
	withTools := completion(t, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
		"type": "function", "function": map[string]any{"name": "t", "arguments": "{}"},
	}}})
	withProse := completion(t, map[string]any{"role": "assistant", "content": "no tools here"})
	assert.Equal(t, verdictSupported, probeVerdict(withTools))
	assert.NotEmpty(t, llm.ToolCallNamesFrom(withTools))
	assert.Equal(t, verdictNotSupported, probeVerdict(withProse))
	assert.Empty(t, llm.ToolCallNamesFrom(withProse))
}
