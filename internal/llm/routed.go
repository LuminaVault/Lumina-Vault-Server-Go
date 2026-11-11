package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// The non-streaming chat types are LuminaVaultShared's ChatRequest, ChatMessage,
// ChatResponse and HermesUpstreamResponse. Field order is Swift's declaration
// order, and an optional Swift leaves nil is omitted, as JSONEncoder does.

// ChatToolCallFunction is ChatToolCallFunction.
type ChatToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatToolCall is ChatToolCall.
type ChatToolCall struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function ChatToolCallFunction `json:"function"`
}

// CompletionMessage is LuminaVaultShared's ChatMessage, tool calls included.
// ChatMessage in this package is the streaming form, role and content only.
type CompletionMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []ChatToolCall `json:"tool_calls,omitzero"` // nil omitted, [] kept
}

// ChatToolDefinition is ChatToolDefinition; Parameters is an arbitrary JSON object.
type ChatToolDefinition struct {
	Name        string          `json:"name"`
	Description *string         `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ChatTool is ChatTool.
type ChatTool struct {
	Type     string             `json:"type"`
	Function ChatToolDefinition `json:"function"`
}

// CompletionRequest is LuminaVaultShared's ChatRequest as the non-streaming
// service takes it. Temperature is carried for the gateway and OpenAI-shaped
// providers; the Anthropic adapter drops it (Haiku trial rule).
type CompletionRequest struct {
	Messages    []CompletionMessage
	Model       string // empty means the service's default model
	Temperature *float64
	Tools       []ChatTool      // nil omits "tools"
	ToolChoice  json.RawMessage // nil omits "tool_choice"
}

// UpstreamChoice is HermesUpstreamChoice.
type UpstreamChoice struct {
	Index        *int              `json:"index,omitempty"`
	Message      CompletionMessage `json:"message"`
	FinishReason *string           `json:"finish_reason,omitempty"`
}

// UpstreamUsage is HermesUpstreamUsage.
type UpstreamUsage struct {
	PromptTokens     *int `json:"prompt_tokens,omitempty"`
	CompletionTokens *int `json:"completion_tokens,omitempty"`
	TotalTokens      *int `json:"total_tokens,omitempty"`
}

// UpstreamResponse is HermesUpstreamResponse: what Swift decoded, so fields it
// doesn't know are gone when it is encoded again as ChatResponse.raw.
type UpstreamResponse struct {
	ID      string           `json:"id"`
	Object  *string          `json:"object,omitempty"`
	Created *int             `json:"created,omitempty"`
	Model   string           `json:"model"`
	Choices []UpstreamChoice `json:"choices"`
	Usage   *UpstreamUsage   `json:"usage,omitempty"`
}

// ChatResponse is LuminaVaultShared's ChatResponse, the body of POST /v1/llm/chat.
type ChatResponse struct {
	ID      string            `json:"id"`
	Model   string            `json:"model"`
	Message CompletionMessage `json:"message"`
	Raw     UpstreamResponse  `json:"raw"`
}

// ChatError is the Hummingbird HTTPError the service throws for failures that
// aren't user-facing: a status and a message. Swift renders it as
// {"error":{"message":...}} with that status; the caller owns the rendering.
type ChatError struct {
	Status  int
	Message string
}

func (e *ChatError) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Message) }

// ChatService is HermesLLMService: one non-streaming completion. sessionKey is the
// tenant UUID string (X-Hermes-Session-Key), sessionID optional continuity.
type ChatService interface {
	Chat(ctx context.Context, sessionKey, sessionID string, req CompletionRequest) (*ChatResponse, error)
}

// RoutedService is RoutedHermesLLMService: /v1/llm/chat's service. It routes the
// completion through Transport (registry, router, failover), so Cerberus, the free
// lane, BYOK and the trial apply exactly as for every other transport call.
//
// For a managed tenant (anything not BYOK, see DisclosureFor) it prepends
// SystemPromptGuard as a system message and replaces the model id with "auto" in
// both the response and its raw copy. The assistant content is sanitized of tool
// stderr (SanitizeAssistantMessage) and tool failures are logged.
type RoutedService struct {
	Transport    ChatTransport
	DefaultModel string
	// Preferences resolves disclosure. Nil treats every tenant as managed.
	Preferences PreferenceSource
}

var _ ChatService = (*RoutedService)(nil)

// The meter returns no-op instruments when none is installed, so the errors are
// ignored, as upstreamErrorCounter's is.
var (
	chatSuccessCounter, _ = otel.Meter("lumina/llm").Int64Counter("luminavault.llm.chat.success")
	chatFailureCounter, _ = otel.Meter("lumina/llm").Int64Counter("luminavault.llm.chat.failure")
	chatDuration, _       = otel.Meter("lumina/llm").Float64Histogram("luminavault.llm.chat.duration", metric.WithUnit("s"))
)

// completionPayload is RoutedHermesLLMService's ChatCompletionPayload. Swift's
// synthesized encoder omits nil optionals, so a nil temperature, tools or
// tool_choice is absent, and "content": null is absent too: encodeIfPresent.
type completionPayload struct {
	Model       string            `json:"model"`
	Messages    []json.RawMessage `json:"messages"`
	Temperature *float64          `json:"temperature,omitempty"`
	Stream      bool              `json:"stream"`
	Tools       []ChatTool        `json:"tools,omitzero"`
	ToolChoice  json.RawMessage   `json:"tool_choice,omitempty"`
}

// encodeOutbound is ChatMessage.toOutbound() encoded: content "" becomes nil and
// is then omitted, as the synthesized Encodable does.
func encodeOutbound(m CompletionMessage) (json.RawMessage, error) {
	type withContent struct {
		Role      string         `json:"role"`
		Content   string         `json:"content"`
		ToolCalls []ChatToolCall `json:"tool_calls,omitzero"`
	}
	type withoutContent struct {
		Role      string         `json:"role"`
		ToolCalls []ChatToolCall `json:"tool_calls,omitzero"`
	}
	if m.Content == "" {
		return json.Marshal(withoutContent{Role: m.Role, ToolCalls: m.ToolCalls})
	}
	return json.Marshal(withContent(m))
}

// Chat runs one completion.
func (s *RoutedService) Chat(ctx context.Context, sessionKey, sessionID string, req CompletionRequest) (*ChatResponse, error) {
	started := time.Now()
	disclosure := DisclosureFor(ctx, s.Preferences, sessionKey)
	messages := req.Messages
	if disclosure == DisclosureHidden {
		messages = append([]CompletionMessage{{Role: "system", Content: SystemPromptGuard}}, messages...)
	}
	model := req.Model
	if model == "" {
		model = s.DefaultModel
	}
	payload := completionPayload{Model: model, Temperature: req.Temperature, Tools: req.Tools, ToolChoice: req.ToolChoice}
	for _, m := range messages {
		raw, err := encodeOutbound(m)
		if err != nil {
			chatFailureCounter.Add(ctx, 1)
			return nil, &ChatError{Status: http.StatusInternalServerError, Message: http.StatusText(http.StatusInternalServerError)}
		}
		payload.Messages = append(payload.Messages, raw)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode chat payload", "error", err)
		chatFailureCounter.Add(ctx, 1)
		return nil, &ChatError{Status: http.StatusInternalServerError, Message: http.StatusText(http.StatusInternalServerError)}
	}

	metadata, err := s.Transport.ChatCompletions(ctx, body, sessionKey, sessionID)
	if err != nil {
		chatFailureCounter.Add(ctx, 1)
		var userFacing UserFacingError
		var chatErr *ChatError
		if errors.As(err, &userFacing) || errors.As(err, &chatErr) {
			return nil, err
		}
		slog.ErrorContext(ctx, "llm chat failed", "error", err)
		return nil, &ChatError{Status: http.StatusBadGateway, Message: "llm upstream error: " + err.Error()}
	}
	decoded, err := DecodeUpstreamResponse(metadata.Data)
	if err != nil {
		chatFailureCounter.Add(ctx, 1)
		slog.ErrorContext(ctx, "llm chat failed", "error", err)
		return nil, &ChatError{Status: http.StatusBadGateway, Message: "llm upstream error: " + err.Error()}
	}
	if len(decoded.Choices) == 0 {
		chatFailureCounter.Add(ctx, 1)
		return nil, &ChatError{Status: http.StatusBadGateway, Message: "llm upstream returned no choices"}
	}
	assistant := decoded.Choices[0].Message
	ObserveToolErrors(ctx, ClassifyToolErrors(assistant.Content), decoded.Model, sessionKey)
	sanitized := SanitizeAssistantMessage(assistant)
	chatSuccessCounter.Add(ctx, 1)
	chatDuration.Record(ctx, time.Since(started).Seconds())
	slog.InfoContext(ctx, "llm reply ready", "model", decoded.Model, "sessionKey", sessionKey)
	if disclosure == DisclosureHidden {
		decoded.Model = GenericModelID
	}
	// raw keeps the upstream choices unsanitized, as Swift's does.
	return &ChatResponse{ID: decoded.ID, Model: decoded.Model, Message: sanitized, Raw: *decoded}, nil
}

// DecodeUpstreamResponse is JSONDecoder().decode(HermesUpstreamResponse.self): id,
// model and choices are required, and so are each choice's message role and
// content. A null content (a tool-call-only reply) fails, as it does in Swift,
// where ChatMessage.content is a non-optional String.
func DecodeUpstreamResponse(data []byte) (*UpstreamResponse, error) {
	var probe struct {
		ID      *string `json:"id"`
		Model   *string `json:"model"`
		Choices *[]struct {
			Message *struct {
				Role    *string `json:"role"`
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("decode HermesUpstreamResponse: %w", err)
	}
	switch {
	case probe.ID == nil:
		return nil, errors.New("decode HermesUpstreamResponse: missing id")
	case probe.Model == nil:
		return nil, errors.New("decode HermesUpstreamResponse: missing model")
	case probe.Choices == nil:
		return nil, errors.New("decode HermesUpstreamResponse: missing choices")
	}
	for i, c := range *probe.Choices {
		if c.Message == nil || c.Message.Role == nil || c.Message.Content == nil {
			return nil, fmt.Errorf("decode HermesUpstreamResponse: choices[%d].message needs role and content", i)
		}
	}
	var out UpstreamResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode HermesUpstreamResponse: %w", err)
	}
	return &out, nil
}
