package llm

// Ports Tests/AppTests/LLM/ModelOriginTests.swift and
// Tests/AppTests/LLM/ProviderKindSpendableTests.swift.
//
// ModelOriginRegistry.isCNOrigin is IsCNOrigin (router.go). Swift's
// ModelOriginRegistry.filter has no standalone Go function: TableRouter.Pick
// applies the same predicate inline, so the filter cases run through Pick with
// a stub table of candidates fed by a privacy-on and a privacy-off user.

import (
	"net/url"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestAuditFClassifiesKnownCNOriginModelIdentifiers(t *testing.T) {
	for _, id := range []string{
		"deepseek-r1",
		"DeepSeek-V3",
		"together_ai/deepseek-ai/DeepSeek-V3",
		"qwen2.5-72b-instruct",
		"Qwen/Qwen3-Coder",
		"moonshot-v1-8k",
		"kimi-k2",
		"01-ai/yi-large",
	} {
		assert.True(t, IsCNOrigin(id), "expected CN: %s", id)
	}
}

func TestAuditFDoesNotFlagUSOrEUOriginModels(t *testing.T) {
	for _, id := range []string{
		"gpt-5-mini",
		"gpt-4o",
		"gemini-2.5-flash",
		"claude-opus-4-7",
		"claude-haiku-4-5-20251001",
		"mistral-large-latest", // EU
		"llama-3.3-70b-versatile",
	} {
		assert.False(t, IsCNOrigin(id), "expected non-CN: %s", id)
	}
}

// filterCNOrigin restates ModelOriginRegistry.filter in terms of IsCNOrigin,
// exactly as TableRouter.Pick applies it: skip when the toggle is on and the id
// is CN-origin, otherwise keep, in order.
func filterCNOrigin(ids []string, privacyNoCNOrigin bool) []string {
	kept := []string{}
	for _, id := range ids {
		if privacyNoCNOrigin && IsCNOrigin(id) {
			continue
		}
		kept = append(kept, id)
	}
	return kept
}

func TestAuditFFilterIsNoOpWhenToggleOff(t *testing.T) {
	candidates := []string{"gpt-5-mini", "deepseek-r1", "qwen2.5-72b"}
	assert.Equal(t, candidates, filterCNOrigin(candidates, false))
}

func TestAuditFFilterExcludesCNOriginWhenToggleOn(t *testing.T) {
	candidates := []string{"gpt-5-mini", "deepseek-r1", "qwen2.5-72b", "gemini-2.5-flash", "kimi-k2"}
	assert.Equal(t, []string{"gpt-5-mini", "gemini-2.5-flash"}, filterCNOrigin(candidates, true))
}

func TestAuditFFilterOnEmptyCandidatesReturnsEmpty(t *testing.T) {
	assert.Empty(t, filterCNOrigin(nil, true))
}

// The production filter: TableRouter never hands a privacy-on user a CN-origin
// route, and the gateway fallback is always appended so the decision is never
// empty. (The current table holds no CN-origin ids, so this guards the wiring,
// not the predicate.)
func TestAuditFTableRouterHonoursPrivacyToggle(t *testing.T) {
	reg := NewRegistry([]ProviderConfig{
		{Kind: OpenRouter, APIKey: "k"},
		{Kind: Nvidia, APIKey: "k"},
		{Kind: Anthropic, APIKey: "k"},
		{Kind: Gemini, APIKey: "k"},
		{Kind: OpenAI, APIKey: "k"},
	})
	r := &TableRouter{Registry: reg, HermesDefaultModel: "hermes"}
	for _, tier := range []string{"free", "pro"} {
		for _, cap := range []Capability{CapabilityMedium, CapabilityHigh} {
			on := r.Pick(t.Context(), "", cap, &User{ID: uuid.New(), Tier: tier, PrivacyNoCNOrigin: true})
			off := r.Pick(t.Context(), "", cap, &User{ID: uuid.New(), Tier: tier})
			for _, route := range on.Candidates() {
				assert.False(t, IsCNOrigin(route.ModelID), "%s/%s: %s", tier, cap, route.ModelID)
			}
			assert.Equal(t, HermesGateway, on.Candidates()[len(on.Candidates())-1].Provider)
			// Toggle off is a no-op relative to the CN-free table.
			wantOff := slices.DeleteFunc(slices.Clone(off.Candidates()), func(r Route) bool { return IsCNOrigin(r.ModelID) })
			assert.Equal(t, wantOff, on.Candidates(), "%s/%s", tier, cap)
		}
	}
}

// --- ProviderKindSpendableTests ---

// keylessCapable is ProviderKind.keylessCapable. Go hard-codes the set inside
// IsSpendable rather than exporting it.
var keylessCapable = map[ProviderKind]bool{Ollama: true, Custom: true}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return u
}

func TestAuditFANonEmptyKeyIsSpendableForAnyProvider(t *testing.T) {
	for kind := range userCredentialTargets {
		assert.True(t, kind.IsSpendable(strPtr("sk-live"), nil), "%s with a key", kind)
	}
}

func TestAuditFAnEmptyOrAbsentKeyIsNotSpendable(t *testing.T) {
	assert.False(t, OpenAI.IsSpendable(strPtr(""), nil))
	assert.False(t, OpenAI.IsSpendable(nil, nil))
}

func TestAuditFABareBaseURLDoesNotUnlockAProviderThatAuthenticates(t *testing.T) {
	u := mustURL(t, "https://api.openai.com/v1")
	checked := 0
	for kind := range userCredentialTargets {
		if keylessCapable[kind] {
			continue
		}
		checked++
		assert.False(t, kind.IsSpendable(nil, u), "%s has no keyless mode — a base URL alone proves nothing", kind)
		assert.False(t, kind.IsSpendable(strPtr(""), u), "%s with an empty key", kind)
	}
	assert.Equal(t, len(userCredentialTargets)-len(keylessCapable), checked)
}

func TestAuditFABaseURLIsTheWholeCredentialForASelfHostedEndpoint(t *testing.T) {
	u := mustURL(t, "http://100.105.117.67:11434")
	assert.True(t, Ollama.IsSpendable(nil, u))
	assert.True(t, Custom.IsSpendable(nil, u))
}

// Swift's URL(string: "not a url") is nil; Go's url.Parse accepts it as a bare
// path. Either way it carries no scheme and no host, so it is not a credential.
func TestAuditFAMalformedBaseURLIsNotACredential(t *testing.T) {
	assert.False(t, Ollama.IsSpendable(nil, mustURL(t, "not a url")))
	assert.False(t, Ollama.IsSpendable(nil, mustURL(t, "/just/a/path")))
	assert.False(t, Ollama.IsSpendable(nil, nil))
	// Scheme with no host, and host with no scheme, are both rejected.
	assert.False(t, Ollama.IsSpendable(nil, &url.URL{Scheme: "http"}))
	assert.False(t, Ollama.IsSpendable(nil, &url.URL{Host: "100.105.117.67:11434"}))
}
