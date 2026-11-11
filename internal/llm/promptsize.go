package llm

import (
	"lumina/internal/swiftstr"
	"strings"
)

// promptCharactersPerToken is PromptSizeEstimator.charactersPerToken: no tokenizer
// on this path, four characters per token is close enough for a gauge.
const promptCharactersPerToken = 4

// EstimatePromptTokens is PromptSizeEstimator.estimateTokens(of:): characters / 4
// plus four tokens of role framing per message. Characters are Swift Characters.
func EstimatePromptTokens(messages []ChatMessage) int {
	characters := 0
	for _, m := range messages {
		characters += swiftstr.Count(m.Content)
	}
	return characters/promptCharactersPerToken + len(messages)*4
}

// ContextWindowForModel is PromptSizeEstimator.contextWindow(forModel:): the first
// catalogue entry with that id, over providers in ProviderID.allCases order, then
// again with any "provider/" prefix stripped. ok is false when the catalogue
// doesn't know the model; inventing a default would put a wrong gauge on screen.
func ContextWindowForModel(model string) (window int, ok bool) {
	if model == "" {
		return 0, false
	}
	for _, provider := range allProviderIDs {
		for _, m := range ModelsFor(provider) {
			if m.ID == model {
				// The first match decides, even one with no window (0 here, nil in
				// Swift): the search stops there.
				return m.ContextWindow, m.ContextWindow != 0
			}
		}
	}
	if slash := strings.LastIndex(model, "/"); slash >= 0 {
		return ContextWindowForModel(model[slash+1:])
	}
	return 0, false
}
