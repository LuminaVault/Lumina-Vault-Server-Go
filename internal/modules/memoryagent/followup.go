package memoryagent

import (
	"context"
	"encoding/json"
	"log/slog"
	"lumina/internal/llm"
	"lumina/internal/swiftstr"
	"strconv"
	"strings"
)

const (
	defaultMaxFollowUps = 4
	followUpSnippet     = 120
)

// FollowUpGenerator is FollowUpGenerator: one bounded model call for the chips under an
// answer. Defensive: any failure is an empty list, so it can never abort a parent answer.
type FollowUpGenerator struct {
	Transport    llm.ChatTransport
	DefaultModel string
	MaxFollowUps int // 0 means 4
}

type followUpBody struct {
	Model          string            `json:"model"`
	Messages       []llm.ChatMessage `json:"messages"`
	Temperature    float64           `json:"temperature"`
	ResponseFormat map[string]string `json:"response_format"`
	Stream         bool              `json:"stream"`
}

func (g *FollowUpGenerator) limit() int {
	if g.MaxFollowUps == 0 {
		return defaultMaxFollowUps
	}
	return g.MaxFollowUps
}

// Generate is generate. It returns 0 to MaxFollowUps entries and never fails.
func (g *FollowUpGenerator) Generate(ctx context.Context, sessionKey, sessionID, summary string, sources []HitDTO) []string {
	trimmed := swiftstr.TrimWhitespacesAndNewlines(summary)
	if trimmed == "" && len(sources) == 0 {
		return []string{}
	}
	payload, err := json.Marshal(followUpBody{
		Model: g.DefaultModel, Messages: BuildFollowUpPrompt(trimmed, sources, g.limit()),
		Temperature: 0.5, ResponseFormat: map[string]string{"type": "json_object"},
	})
	if err != nil {
		return []string{}
	}
	response, err := g.Transport.ChatCompletions(ctx, payload, sessionKey, sessionID)
	if err != nil {
		slog.WarnContext(ctx, "follow-up generator upstream failed", "error", err)
		return []string{}
	}
	parsed := ParseFollowUps(response.Data)
	if parsed == nil {
		return []string{}
	}
	return parsed[:min(len(parsed), g.limit())]
}

// BuildFollowUpPrompt is buildPrompt(summary:sources:max:).
func BuildFollowUpPrompt(summary string, sources []HitDTO, maxFollowUps int) []llm.ChatMessage {
	sourceLines := "(no source notes were used)"
	if len(sources) > 0 {
		lines := make([]string, len(sources))
		for i, hit := range sources {
			snippet := swiftstr.TrimWhitespacesAndNewlines(swiftstr.Prefix(hit.Content, followUpSnippet))
			lines[i] = "[" + strconv.Itoa(i+1) + "] " + snippet
		}
		sourceLines = strings.Join(lines, "\n")
	}
	answer := summary
	if answer == "" {
		answer = "(empty)"
	}
	user := `Lumina just answered the user. Suggest ` + strconv.Itoa(maxFollowUps) + ` short follow-up
prompts they might ask next, in their voice (first person, present
tense). Each must be ≤ 60 characters and end with a question mark
OR an imperative verb (e.g. "Go deeper", "Compare with last month").
Reply ONLY with JSON of the shape:

    {"follow_ups": ["...", "..."]}

Do not add commentary, markdown, or extra keys.

Assistant's answer:
` + answer + `

Notes Lumina used:
` + sourceLines
	return []llm.ChatMessage{
		{Role: "system", Content: "You generate short, on-brand follow-up prompts for the Think\ntab. You never invent facts; only riff on what was just said."},
		{Role: "user", Content: user},
	}
}

// ParseFollowUps is parse(response:): nil on any structural mismatch or when nothing
// is left after trimming.
func ParseFollowUps(response []byte) []string {
	var outer struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(response, &outer) != nil || len(outer.Choices) == 0 || outer.Choices[0].Message.Content == nil {
		return nil
	}
	var inner struct {
		FollowUps *[]string `json:"follow_ups"`
	}
	if json.Unmarshal([]byte(*outer.Choices[0].Message.Content), &inner) != nil || inner.FollowUps == nil {
		return nil
	}
	var cleaned []string
	for _, f := range *inner.FollowUps {
		if t := swiftstr.TrimWhitespacesAndNewlines(f); t != "" {
			cleaned = append(cleaned, t)
		}
	}
	return cleaned
}
