package skills

import (
	"context"
	"encoding/json"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/swiftstr"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	// SelectorTimeout is the budget the middleware gives the selector.
	SelectorTimeout = 300 * time.Millisecond
	// MaxRoutingBodyBytes is ContextRouterMiddleware's maxBodyBytes. A caller
	// passes a larger body through unread rather than calling Rewrite.
	MaxRoutingBodyBytes = 256 * 1024
)

// ChatTransport is HermesChatTransport.chatCompletions: the response body, or an
// error. sessionID may be empty. Swift hands it the routed transport, which adapts
// to llm.Transport by returning Metadata.Data.
type ChatTransport interface {
	ChatCompletions(ctx context.Context, payload []byte, sessionKey, sessionID string) ([]byte, error)
}

// Selector is ContextRouterSelector: at most one skill for a message. It never
// fails, because chat is the hot path: a failure is no skill.
type Selector interface {
	SelectSkill(ctx context.Context, userMessage string, manifests []Manifest, timeout time.Duration) *Manifest
}

// DefaultSelector is DefaultContextRouterSelector. It sends temperature 0, as Swift
// does; the Anthropic adapter drops it (see platform/infra/docs/llm-haiku-trial.md).
type DefaultSelector struct {
	Transport ChatTransport
	Model     string
	// SessionKey is the tenant UUID string, uppercase as Swift's uuidString is.
	SessionKey string
}

const selectorSystemPrompt = `You route an incoming chat message to ONE of the user's skills,
or NONE. Respond with EXACTLY one token: the skill name from the
catalog below, or the literal word NONE. No prose, no
punctuation, no explanation.`

type selectorMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type selectorRequest struct {
	Model       string            `json:"model"`
	Messages    []selectorMessage `json:"messages"`
	Temperature float64           `json:"temperature"`
	Stream      bool              `json:"stream"`
}

// SelectSkill asks the model, and returns the catalog entry the reply's first word
// names. An empty catalog or message makes no call.
func (s DefaultSelector) SelectSkill(ctx context.Context, userMessage string, manifests []Manifest, timeout time.Duration) *Manifest {
	if len(manifests) == 0 || userMessage == "" {
		return nil
	}
	payload, err := selectorPayload(s.Model, userMessage, manifests)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		body, err := s.Transport.ChatCompletions(ctx, payload, s.SessionKey, "")
		done <- result{body, err}
	}()
	var raw []byte
	select {
	case r := <-done:
		if r.err != nil {
			slog.DebugContext(ctx, "context-routing selector: upstream failed, no-op", "error", r.err)
			return nil
		}
		raw = r.body
	case <-ctx.Done():
		slog.DebugContext(ctx, "context-routing selector: upstream failed, no-op", "error", ctx.Err())
		return nil
	}
	token, ok := selectorToken(raw)
	if !ok || token == "" || strings.EqualFold(token, "NONE") {
		return nil
	}
	key := swiftstr.CanonicalKey(token)
	for i := range manifests {
		if swiftstr.CanonicalKey(manifests[i].Name) == key {
			return &manifests[i]
		}
	}
	return nil
}

func selectorPayload(model, userMessage string, manifests []Manifest) ([]byte, error) {
	lines := make([]string, len(manifests))
	names := make([]string, len(manifests))
	for i, m := range manifests {
		lines[i] = m.Name + " — " + m.Description
		names[i] = m.Name
	}
	user := "Catalog:\n" + strings.Join(lines, "\n") +
		"\n\nMessage:\n" + userMessage +
		"\n\nReply with one of: " + strings.Join(names, ", ") + ", NONE."
	return json.Marshal(selectorRequest{
		Model: model,
		Messages: []selectorMessage{
			{Role: "system", Content: selectorSystemPrompt},
			{Role: "user", Content: user},
		},
	})
}

// selectorToken decodes OAIResponse, whose every choice needs a message with a
// string role, and gives the first word of the first choice's content.
func selectorToken(raw []byte) (string, bool) {
	response, ok := jsonObject(raw)
	if !ok {
		return "", false
	}
	var choices []json.RawMessage
	if !jsonField(response, "choices", &choices) {
		return "", false
	}
	var first string
	for i, c := range choices {
		choice, ok := jsonObject(c)
		if !ok {
			return "", false
		}
		var messageRaw json.RawMessage
		if !jsonField(choice, "message", &messageRaw) {
			return "", false
		}
		message, ok := jsonObject(messageRaw)
		if !ok {
			return "", false
		}
		var role string
		if !jsonField(message, "role", &role) {
			return "", false
		}
		var content *string
		if rawContent, present := message["content"]; present {
			if err := json.Unmarshal(rawContent, &content); err != nil {
				return "", false
			}
		}
		if i == 0 && content != nil {
			first = *content
		}
	}
	first = swiftstr.TrimWhitespacesAndNewlines(first)
	if i := strings.IndexFunc(first, swiftstr.IsWhitespaceOrNewline); i >= 0 {
		first = first[:i]
	}
	return first, true
}

// jsonObject decodes an object keeping Swift's exact, case-sensitive key match,
// which encoding/json's struct decoding doesn't have.
func jsonObject(raw []byte) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, false
	}
	return fields, true
}

// jsonField decodes a required, non-null field.
func jsonField(fields map[string]json.RawMessage, key string, to any) bool {
	raw, ok := fields[key]
	if !ok || string(raw) == "null" {
		return false
	}
	return json.Unmarshal(raw, to) == nil
}

// RoutingMessage is ChatRoutingBody.Message: role and content, both strings. A
// request whose content is anything else isn't routed.
type RoutingMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRoutingBody is the middleware's view of a chat body. Despite the doc comment
// on the Swift struct, re-encoding keeps only these three fields: when a skill
// matches, the downstream handler loses stream, tools and anything else, as it does
// in Swift.
type ChatRoutingBody struct {
	Messages    []RoutingMessage `json:"messages"`
	Model       *string          `json:"model,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
}

// DecodeChatRoutingBody is JSONDecoder's decode: messages is required, each message
// needs its two strings, and model and temperature may be absent or null.
func DecodeChatRoutingBody(data []byte) (ChatRoutingBody, bool) {
	fields, ok := jsonObject(data)
	if !ok || !utf8.Valid(data) {
		return ChatRoutingBody{}, false
	}
	var raws []json.RawMessage
	if !jsonField(fields, "messages", &raws) {
		return ChatRoutingBody{}, false
	}
	body := ChatRoutingBody{Messages: make([]RoutingMessage, len(raws))}
	for i, raw := range raws {
		message, ok := jsonObject(raw)
		if !ok || !jsonField(message, "role", &body.Messages[i].Role) || !jsonField(message, "content", &body.Messages[i].Content) {
			return ChatRoutingBody{}, false
		}
	}
	if raw, present := fields["model"]; present && json.Unmarshal(raw, &body.Model) != nil {
		return ChatRoutingBody{}, false
	}
	if raw, present := fields["temperature"]; present && json.Unmarshal(raw, &body.Temperature) != nil {
		return ChatRoutingBody{}, false
	}
	return body, true
}

// LatestUserContent is the content of the last user message.
func (b ChatRoutingBody) LatestUserContent() (string, bool) {
	for i := len(b.Messages) - 1; i >= 0; i-- {
		if b.Messages[i].Role == "user" {
			return b.Messages[i].Content, true
		}
	}
	return "", false
}

// PrependingSystem returns a copy with content in front of the system prompt: an
// existing first system message becomes "<content>\n\n<previous>", otherwise a new
// one goes first.
func (b ChatRoutingBody) PrependingSystem(content string) ChatRoutingBody {
	messages := make([]RoutingMessage, 0, len(b.Messages)+1)
	if len(b.Messages) > 0 && b.Messages[0].Role == "system" {
		messages = append(messages, RoutingMessage{Role: "system", Content: content + "\n\n" + b.Messages[0].Content})
		messages = append(messages, b.Messages[1:]...)
	} else {
		messages = append(messages, RoutingMessage{Role: "system", Content: content})
		messages = append(messages, b.Messages...)
	}
	b.Messages = messages
	return b
}

// RoutingUser is what the router reads from the authenticated user.
type RoutingUser struct {
	Tenant         uuid.UUID
	Username       string
	ContextRouting bool
	Tier           string
	TierOverride   string
}

// ContextRouter is ContextRouterMiddleware without the HTTP: 5C's middleware reads
// the body (see MaxRoutingBodyBytes), calls Rewrite and forwards what it returns.
type ContextRouter struct {
	// Manifests is the tenant's catalog.
	Manifests func(ctx context.Context, tenant uuid.UUID) ([]Manifest, error)
	// NewSelector builds the selector for a tenant and username.
	NewSelector func(tenant uuid.UUID, username string) Selector
	// Entitled is the cost gate, entitlement.PrivacyContextRouter in production.
	// nil allows everyone.
	Entitled func(RoutingUser) bool
}

// NewContextRouter wires the router to a catalog.
func NewContextRouter(catalog *Catalog, newSelector func(uuid.UUID, string) Selector, entitled func(RoutingUser) bool) *ContextRouter {
	return &ContextRouter{
		Manifests: func(_ context.Context, tenant uuid.UUID) ([]Manifest, error) {
			return catalog.Manifests(tenant), nil
		},
		NewSelector: newSelector,
		Entitled:    entitled,
	}
}

// Rewrite returns the body to forward. Every way of not routing returns body itself:
// flag off, not entitled, not a chat body, no user message, no catalog, no match.
func (r *ContextRouter) Rewrite(ctx context.Context, user RoutingUser, raw []byte) []byte {
	if !user.ContextRouting || (r.Entitled != nil && !r.Entitled(user)) {
		return raw
	}
	body, ok := DecodeChatRoutingBody(raw)
	if !ok {
		return raw
	}
	message, ok := body.LatestUserContent()
	if !ok {
		return raw
	}
	manifests, err := r.Manifests(ctx, user.Tenant)
	if err != nil || len(manifests) == 0 {
		return raw
	}
	chosen := r.NewSelector(user.Tenant, user.Username).SelectSkill(ctx, message, manifests, SelectorTimeout)
	if chosen == nil {
		return raw
	}
	payload, err := json.Marshal(body.PrependingSystem(chosen.Body))
	if err != nil {
		return raw
	}
	slog.InfoContext(ctx, "context-routing matched", "skill", chosen.Name, "tenant", ids.Upper(user.Tenant))
	return payload
}
