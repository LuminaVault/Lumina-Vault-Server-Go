package llm

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// OllamaAdapter is OllamaAdapter: a user's self-hosted Ollama. No API key is
// involved. The base URL is the user's own, from user_provider_credentials.
type OllamaAdapter struct {
	DefaultBaseURL *url.URL
	Client         *http.Client
	Credentials    CredentialSource
}

var _ Adapter = (*OllamaAdapter)(nil)

func (a *OllamaAdapter) Kind() ProviderKind           { return Ollama }
func (a *OllamaAdapter) AcceptsUserCredentials() bool { return a.Credentials != nil }

func (a *OllamaAdapter) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

const ollamaInvalidPayload = "invalid OpenAI payload"

// ollamaBody is the same messages array under Ollama's envelope.
func ollamaBody(payload []byte, stream bool) ([]byte, string, error) {
	openAI, messages, ok := parseOpenAIMessages(payload)
	if !ok {
		return nil, "", Permanent(Ollama, 400, strPtr(ollamaInvalidPayload))
	}
	model, ok := openAI["model"].(string)
	if !ok {
		model = "llama3.1"
	}
	body, err := encodeJSON(map[string]any{"model": model, "messages": messages, "stream": stream})
	if err != nil {
		return nil, "", Permanent(Ollama, 400, strPtr("failed to serialize ollama payload"))
	}
	return body, model, nil
}

// ChatCompletions never reports an unparseable 2xx body as a failure: Swift returns
// it as {"error": ...}, as a success.
func (a *OllamaAdapter) ChatCompletions(ctx context.Context, payload []byte, _, _ string) (Metadata, error) {
	body, model, err := ollamaBody(payload, false)
	if err != nil {
		return Metadata{}, err
	}
	base, err := a.resolveBaseURL(ctx)
	if err != nil {
		return Metadata{}, err
	}
	status, headers, data, err := postBuffered(ctx, a.client(), joinURL(base, "api", "chat"), [][2]string{{"Content-Type", "application/json"}}, body, urlSessionTimeout)
	if err != nil {
		return Metadata{}, Network(Ollama, err)
	}
	if status >= 200 && status < 300 {
		out, err := swiftJSON(translateOllamaResponse(data, model))
		if err != nil {
			return Metadata{}, Transient(Ollama, status, strPtr("failed to encode OpenAI response"))
		}
		return Metadata{Data: out, Headers: headers}, nil
	}
	perr := Classify(Ollama, status, data)
	slog.ErrorContext(ctx, "ollama upstream failed", "reason", perr.ReasonCode(), "status", status)
	return Metadata{}, perr
}

// ChatStream speaks Ollama's NDJSON: deltas, then a terminal object with done set.
func (a *OllamaAdapter) ChatStream(ctx context.Context, payload []byte, _, _ string) Stream {
	return runStream(ctx, a.client(), Ollama, streamHeadTimeout, framingNDJSON,
		func(ctx context.Context) (streamRequest, error) {
			body, _, err := ollamaBody(payload, true)
			if err != nil {
				return streamRequest{}, err
			}
			base, err := a.resolveBaseURL(ctx)
			if err != nil {
				return streamRequest{}, err
			}
			return streamRequest{url: joinURL(base, "api", "chat"), body: body}, nil
		},
		func(line string, yield func(Chunk)) (bool, error) {
			trimmed := trimWhitespaces(line)
			if trimmed == "" {
				return false, nil
			}
			obj, ok := decodeObject([]byte(trimmed))
			if !ok {
				return false, nil
			}
			if message, ok := obj["error"].(string); ok {
				return false, Transient(Ollama, 0, &message)
			}
			content := ""
			if message, ok := obj["message"].(map[string]any); ok {
				content, _ = message["content"].(string)
			}
			if done, _ := obj["done"].(bool); done {
				finish, ok := obj["done_reason"].(string)
				if !ok {
					finish = "stop"
				}
				yield(Chunk{Delta: content, FinishReason: finish})
				return true, nil
			}
			if content != "" {
				yield(Chunk{Delta: content})
			}
			return false, nil
		})
}

// resolveBaseURL: proxying a BYOK call to the deployment's own Ollama would be a
// cross-tenant privacy bug, so it fails closed like the keyed adapters.
func (a *OllamaAdapter) resolveBaseURL(ctx context.Context) (*url.URL, error) {
	values := RoutingValues(ctx)
	mode := values.CredentialMode
	if mode == ModeManaged {
		return a.DefaultBaseURL, nil
	}
	if a.Credentials == nil || values.CurrentUser == nil {
		if mode == ModeBYOK {
			slog.ErrorContext(ctx, "byok request for ollama has no resolvable tenant; failing closed")
			return nil, BYOKKeysRequiredError{}
		}
		return a.DefaultBaseURL, nil
	}
	creds, err := a.Credentials.Credential(ctx, Ollama, values.CurrentUser.ID)
	if err != nil {
		slog.ErrorContext(ctx, "user credential lookup failed for ollama", "error", err)
		if mode == ModeBYOK {
			return nil, BYOKKeysRequiredError{}
		}
		return a.DefaultBaseURL, nil
	}
	if creds != nil && creds.BaseURL != nil {
		return creds.BaseURL, nil
	}
	if mode == ModeBYOK {
		slog.ErrorContext(ctx, "byok request for ollama has no endpoint configured; failing closed")
		return nil, BYOKKeysRequiredError{}
	}
	return a.DefaultBaseURL, nil
}

func translateOllamaResponse(body []byte, model string) map[string]any {
	ollama, ok := decodeObject(body)
	if !ok {
		return map[string]any{"error": "unparseable ollama response"}
	}
	content := ""
	if message, ok := ollama["message"].(map[string]any); ok {
		content, _ = message["content"].(string)
	}
	promptTokens, _ := intOf(ollama["prompt_eval_count"])
	completionTokens, _ := intOf(ollama["eval_count"])
	finish, ok := ollama["done_reason"].(string)
	if !ok {
		finish = "stop"
	}
	return map[string]any{
		"id":      "ollama-" + swiftUUID(uuid.New()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
}
