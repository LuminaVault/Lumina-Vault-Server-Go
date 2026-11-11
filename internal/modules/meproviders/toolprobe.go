package meproviders

import (
	"encoding/json"
	"lumina/internal/llm"
	"lumina/internal/swiftstr"
	"strings"

	"github.com/google/uuid"
)

// verdict is ProviderToolProbe.Verdict. Three-valued by necessity: unknown is
// a network failure, an unexpected shape or a refusal, and reads as permissive.
type verdict int

const (
	verdictUnknown verdict = iota
	verdictSupported
	verdictNotSupported
)

// probeNonce is ProviderToolProbe.nonce: a token no model can know, fresh per
// probe so a cached completion upstream cannot satisfy it.
func probeNonce() string {
	return "lv-" + strings.ToLower(uuid.NewString()[:8])
}

// probePayload is ProviderToolProbe.payload: one tool, one question that needs
// it, and a tight token budget, since it runs on the user's own key.
func probePayload(model, nonce string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 64,
		"messages": []map[string]any{{
			"role":    "user",
			"content": "What is the status of job " + nonce + "? Use the tool; do not guess.",
		}},
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "lookup_job_status",
				"description": "Look up the status of a job by its opaque identifier.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"id": map[string]any{"type": "string"}},
					"required":   []string{"id"},
				},
			},
		}},
		"tool_choice": "auto",
	})
	return b
}

// probeVerdict is ProviderToolProbe.verdict(from:). Only an actual tool call
// counts as support: prose that mentions the tool is the failure being detected.
func probeVerdict(data []byte) verdict {
	message, ok := llm.AssistantMessage(data)
	if !ok {
		return verdictUnknown
	}
	if len(llm.ToolCallNames(message)) > 0 {
		return verdictSupported
	}
	if content, ok := message["content"].(string); ok && swiftstr.TrimWhitespacesAndNewlines(content) != "" {
		return verdictNotSupported
	}
	return verdictUnknown
}

// allowsTools is ProviderToolProbe.allowsTools: no verdict is permissive.
func allowsTools(stored *bool) bool {
	return stored == nil || *stored
}
