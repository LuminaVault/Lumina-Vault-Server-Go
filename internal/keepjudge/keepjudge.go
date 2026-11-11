// Package keepjudge is Workers/MemoryKeepJudge.swift: whether an assistant reply is
// worth keeping as a memory, and as which kind. MemoryIndexWorker calls it when
// memory.keepGate.enabled is set; that worker is a W5 job and is wired there, so
// nothing in Go calls Judge yet.
package keepjudge

import (
	"context"
	"encoding/json"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/swiftstr"
	"strings"

	"github.com/google/uuid"
)

// MaxChars is LLMMemoryKeepJudge.maxChars: the reply is cut to this many Characters.
const MaxChars = 4000

// Decision is MemoryKeepDecision. Kind is "" for none.
type Decision struct {
	Keep bool
	Kind string
}

// KeepUnclassified is MemoryKeepDecision.keepUnclassified.
var KeepUnclassified = Decision{Keep: true}

// kinds is MemoryKindDTO's raw values.
var kinds = map[string]bool{"preference": true, "decision": true, "fact": true, "note": true}

const systemPrompt = `You read one answer an assistant gave its user, and decide whether it ` +
	`records something durable about the user worth recalling in a later ` +
	`session: a preference, a decision they made, or a lasting fact about ` +
	`them or their work. General knowledge, explanations, how-to answers, ` +
	`greetings, and anything only useful in this conversation are not ` +
	`durable. Return ONLY JSON: {"keep": true|false, "kind": ` +
	`"preference"|"decision"|"fact"|null}.`

// Judge is LLMMemoryKeepJudge on the managed chain. No temperature is sent: Haiku
// 5.5 rejects non-default sampling parameters.
type Judge struct {
	Transport llm.ChatTransport
	Model     string
	// IsBYOK reports a tenant whose preference mode is byok; they're never judged,
	// so their replies don't reach the managed model. A failed read is false, as
	// Swift's try?. Nil means no preference store.
	IsBYOK func(ctx context.Context, tenant uuid.UUID) bool
}

type payload struct {
	Model    string            `json:"model"`
	Messages []llm.ChatMessage `json:"messages"`
	Stream   bool              `json:"stream"`
}

// Judge is judge(content:tenantID:). An unusable verdict keeps the reply; a failed
// call or an undecodable body is an error, which the worker treats as keep.
func (j Judge) Judge(ctx context.Context, content string, tenant uuid.UUID) (Decision, error) {
	if j.IsBYOK != nil && j.IsBYOK(ctx, tenant) {
		return KeepUnclassified, nil
	}
	body, err := json.Marshal(payload{Model: j.Model, Messages: []llm.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: swiftstr.Prefix(content, MaxChars)},
	}})
	if err != nil {
		return Decision{}, err
	}
	raw, err := j.Transport.ChatCompletions(ctx, body, ids.Upper(tenant), "")
	if err != nil {
		return Decision{}, err
	}
	var resp struct {
		Choices *[]struct {
			Message *struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw.Data, &resp); err != nil || resp.Choices == nil {
		return Decision{}, errBody
	}
	text := ""
	for i, c := range *resp.Choices {
		if c.Message == nil {
			return Decision{}, errBody
		}
		if i == 0 && c.Message.Content != nil {
			text = *c.Message.Content
		}
	}
	if d, ok := Parse(text); ok {
		return d, nil
	}
	return KeepUnclassified, nil
}

type bodyError struct{}

func (bodyError) Error() string { return "keepjudge: completion body did not decode" }

var errBody = bodyError{}

// Parse is LLMMemoryKeepJudge.parse: lenient, false when there's no usable verdict.
// It takes the text from the first "{" to the last "}", needs a JSON object with a
// Bool "keep" (NSNumber 0 or 1 bridges, as JSONSerialization's does), and drops the
// kind of a reply it doesn't keep.
func Parse(raw string) (Decision, bool) {
	s := swiftstr.TrimWhitespacesAndNewlines(raw)
	if lo, hi := strings.Index(s, "{"), strings.LastIndex(s, "}"); lo >= 0 && hi >= 0 && lo < hi {
		s = s[lo : hi+1]
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &object); err != nil || object == nil {
		return Decision{}, false
	}
	keep, ok := bridgeBool(object["keep"])
	if !ok {
		return Decision{}, false
	}
	var kind string
	if err := json.Unmarshal(object["kind"], &kind); err != nil || !kinds[kind] || !keep {
		kind = ""
	}
	return Decision{Keep: keep, Kind: kind}, true
}

func bridgeBool(raw json.RawMessage) (bool, bool) {
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, true
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil && (n == 0 || n == 1) {
		return n == 1, true
	}
	return false, false
}
