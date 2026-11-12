package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// DirectHermes is DefaultHermesLLMService: one non-streaming completion straight to
// the central Hermes gateway, with no routing, no BYO override and no retry. Only
// SelfImprovementService uses it in Swift.
type DirectHermes struct {
	BaseURL      *url.URL
	APIKey       string // empty sends no Authorization header
	DefaultModel string
	Client       *http.Client // nil means http.DefaultClient (URLSession.shared follows redirects)
	Log          *slog.Logger
}

// DirectChatError is the HTTPError(.badGateway, message:) DefaultHermesLLMService
// throws; Error() is its String(describing:).
type DirectChatError struct{ Message string }

func (e *DirectChatError) Error() string { return "HTTPError: Bad Gateway, " + e.Message }

type directMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type directRequest struct {
	Model       string          `json:"model"`
	Messages    []directMessage `json:"messages"`
	Temperature *float64        `json:"temperature,omitempty"`
	Stream      bool            `json:"stream"`
}

// Chat sends one user prompt. model empty means DefaultModel. It returns the
// upstream model and the sanitized assistant message.
func (h *DirectHermes) Chat(ctx context.Context, sessionKey, sessionID, model, prompt string, temperature *float64) (string, CompletionMessage, error) {
	if model == "" {
		model = h.DefaultModel
	}
	log := h.Log
	if log == nil {
		log = slog.Default()
	}
	body, err := json.Marshal(directRequest{
		Model: model, Messages: []directMessage{{Role: "user", Content: prompt}}, Temperature: temperature,
	})
	if err != nil {
		return "", CompletionMessage{}, err
	}
	endpoint := h.BaseURL.JoinPath("v1", "chat", "completions")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return "", CompletionMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hermes-Session-Key", sessionKey)
	if sessionID != "" {
		req.Header.Set("X-Hermes-Session-Id", sessionID)
	}
	if h.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.APIKey)
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", CompletionMessage{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", CompletionMessage{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.ErrorContext(ctx, fmt.Sprintf("hermes upstream %d: %s", resp.StatusCode, preview(data)))
		return "", CompletionMessage{}, &DirectChatError{Message: fmt.Sprintf("hermes upstream error (%d)", resp.StatusCode)}
	}
	raw, err := DecodeUpstreamResponse(data)
	if err != nil {
		log.ErrorContext(ctx, fmt.Sprintf("hermes decode failed: %v. body=%s", err, preview(data)))
		return "", CompletionMessage{}, &DirectChatError{Message: "hermes upstream returned malformed body"}
	}
	if len(raw.Choices) == 0 {
		return "", CompletionMessage{}, &DirectChatError{Message: "hermes upstream returned no choices"}
	}
	assistant := raw.Choices[0].Message
	ObserveToolErrors(ctx, ClassifyToolErrors(assistant.Content), raw.Model, sessionKey)
	log.InfoContext(ctx, "hermes reply ready model="+raw.Model+" sessionKey="+sessionKey)
	return raw.Model, SanitizeAssistantMessage(assistant), nil
}

// preview is String(data: data.prefix(512), encoding: .utf8) ?? "<binary>".
func preview(data []byte) string {
	head := data[:min(len(data), 512)]
	if !utf8.Valid(head) {
		return "<binary>"
	}
	return strings.Clone(string(head))
}
