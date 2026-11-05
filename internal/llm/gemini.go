package llm

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// GeminiContentsAdapter is GeminiContentsAdapter: it translates the OpenAI payload
// into Google's generateContent v1beta shape and the reply back.
//
// The API key travels in the query string, as Swift sends it. Go's *url.Error
// quotes the full URL, so every transport error here goes through redactURLError.
type GeminiContentsAdapter struct {
	APIKey string
	Client *http.Client
	// Credentials is optional. Without it only the deployment key is spent.
	Credentials CredentialSource
	// BaseURL is a test seam. Swift hard-codes the host and reads no config for it.
	BaseURL *url.URL
}

var _ Adapter = (*GeminiContentsAdapter)(nil)

func (a *GeminiContentsAdapter) Kind() ProviderKind           { return Gemini }
func (a *GeminiContentsAdapter) AcceptsUserCredentials() bool { return a.Credentials != nil }

func (a *GeminiContentsAdapter) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

// resolveKey: managed spends the platform key, BYOK the tenant's key or nothing.
func (a *GeminiContentsAdapter) resolveKey(ctx context.Context) (string, error) {
	values := RoutingValues(ctx)
	mode := values.CredentialMode
	if mode == ModeManaged {
		return a.APIKey, nil
	}
	if a.Credentials == nil || values.CurrentUser == nil {
		if mode == ModeBYOK {
			slog.ErrorContext(ctx, "byok request for gemini has no resolvable tenant; failing closed")
			return "", BYOKKeysRequiredError{}
		}
		return a.APIKey, nil
	}
	creds, err := a.Credentials.Credential(ctx, Gemini, values.CurrentUser.ID)
	if err != nil {
		slog.ErrorContext(ctx, "user credential lookup failed for gemini", "error", err)
		if mode == ModeBYOK {
			return "", BYOKKeysRequiredError{}
		}
		return a.APIKey, nil
	}
	if creds != nil && creds.APIKey != nil && *creds.APIKey != "" {
		return *creds.APIKey, nil
	}
	if mode == ModeBYOK {
		slog.ErrorContext(ctx, "byok request for gemini has no usable credential; failing closed")
		return "", BYOKKeysRequiredError{}
	}
	return a.APIKey, nil
}

// geminiURL builds the endpoint by hand: JoinPath would clean a model id.
func (a *GeminiContentsAdapter) geminiURL(model, method string, query []string) string {
	u := url.URL{Scheme: "https", Host: "generativelanguage.googleapis.com"}
	if a.BaseURL != nil {
		u.Scheme, u.Host = a.BaseURL.Scheme, a.BaseURL.Host
	}
	u.Path = "/v1beta/models/" + model + ":" + method
	u.RawQuery = strings.Join(query, "&")
	return u.String()
}

// parseOpenAIMessages is the guard every translating adapter opens with.
func parseOpenAIMessages(payload []byte) (map[string]any, []map[string]any, bool) {
	openAI, ok := decodeObject(payload)
	if !ok {
		return nil, nil, false
	}
	messages, ok := objects(openAI["messages"])
	if !ok {
		return nil, nil, false
	}
	return openAI, messages, true
}

const geminiInvalidPayload = "invalid OpenAI payload: cannot parse messages"

// ChatCompletions validates the payload before resolving a key, as Swift does.
func (a *GeminiContentsAdapter) ChatCompletions(ctx context.Context, payload []byte, _, _ string) (Metadata, error) {
	openAI, messages, ok := parseOpenAIMessages(payload)
	if !ok {
		return Metadata{}, Permanent(Gemini, 400, strPtr(geminiInvalidPayload))
	}
	rawModel, _ := openAI["model"].(string)
	key, err := a.resolveKey(ctx)
	if err != nil {
		return Metadata{}, err
	}
	body, err := encodeJSON(translateToGemini(openAI, messages))
	if err != nil {
		return Metadata{}, Permanent(Gemini, 400, strPtr("failed to encode Gemini request: "+err.Error()))
	}
	target := a.geminiURL(resolveGeminiModel(rawModel), "generateContent", []string{"key=" + url.QueryEscape(key)})
	status, headers, data, err := postBuffered(ctx, a.client(), target, [][2]string{{"Content-Type", "application/json"}}, body, urlSessionTimeout)
	if err != nil {
		return Metadata{}, Network(Gemini, redactURLError(err))
	}
	if status >= 200 && status < 300 {
		out, err := swiftJSON(translateFromGemini(data, rawModel))
		if err != nil {
			return Metadata{}, Transient(Gemini, status, strPtr("failed to encode OpenAI response"))
		}
		return Metadata{Data: out, Headers: headers}, nil
	}
	perr := Classify(Gemini, status, data)
	slog.ErrorContext(ctx, "gemini upstream failed", "reason", perr.ReasonCode(), "status", status)
	return Metadata{}, perr
}

// ChatStream uses alt=sse, so each frame is one JSON object. Gemini has no [DONE]:
// the frame carrying finishReason is the end. In this path the key resolves before
// the payload is validated, as Swift evaluates makeStreamRequest's arguments first.
func (a *GeminiContentsAdapter) ChatStream(ctx context.Context, payload []byte, _, _ string) Stream {
	return runStream(ctx, a.client(), Gemini, streamHeadTimeout, framingSSE,
		func(ctx context.Context) (streamRequest, error) {
			key, err := a.resolveKey(ctx)
			if err != nil {
				return streamRequest{}, err
			}
			openAI, messages, ok := parseOpenAIMessages(payload)
			if !ok {
				return streamRequest{}, Permanent(Gemini, 400, strPtr(geminiInvalidPayload))
			}
			rawModel, _ := openAI["model"].(string)
			body, err := encodeJSON(translateToGemini(openAI, messages))
			if err != nil {
				return streamRequest{}, err
			}
			return streamRequest{
				url:     a.geminiURL(resolveGeminiModel(rawModel), "streamGenerateContent", []string{"alt=sse", "key=" + url.QueryEscape(key)}),
				headers: [][2]string{{"Accept", "text/event-stream"}},
				body:    body,
			}, nil
		},
		func(record string, yield func(Chunk)) (bool, error) {
			for _, line := range swiftLines(record) {
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				data := trimWhitespaces(line[5:])
				if data == "" {
					continue
				}
				delta, finish, ok := parseGeminiStreamObject([]byte(data))
				if !ok {
					continue
				}
				mapped := ""
				if finish != nil {
					mapped = mapGeminiFinishReason(*finish)
				}
				if delta != "" || finish != nil {
					yield(Chunk{Delta: delta, FinishReason: mapped})
				}
				if finish != nil {
					return true, nil
				}
			}
			return false, nil
		})
}

// parseGeminiStreamObject is parseStreamObject. ok is false for a frame with no
// candidate.
func parseGeminiStreamObject(data []byte) (string, *string, bool) {
	obj, ok := decodeObject(data)
	if !ok {
		return "", nil, false
	}
	candidates, ok := objects(obj["candidates"])
	if !ok || len(candidates) == 0 {
		return "", nil, false
	}
	first := candidates[0]
	var finish *string
	if s, ok := first["finishReason"].(string); ok {
		finish = &s
	}
	var text strings.Builder
	if content, ok := first["content"].(map[string]any); ok {
		if parts, ok := objects(content["parts"]); ok {
			for _, part := range parts {
				if t, ok := part["text"].(string); ok {
					text.WriteString(t)
				}
			}
		}
	}
	return text.String(), finish, true
}

// resolveGeminiModel expands short aliases. A gemini- name passes through with its
// case kept, anything else becomes the latest pro.
func resolveGeminiModel(raw string) string {
	lower := strings.ToLower(raw)
	switch lower {
	case "gemini-2.5-pro", "gemini-pro", "gemini-pro-latest":
		return "gemini-2.5-pro"
	case "gemini-2.5-flash", "gemini-flash", "gemini-flash-latest":
		return "gemini-2.5-flash"
	}
	if strings.HasPrefix(lower, "gemini-") {
		return raw
	}
	return "gemini-2.5-pro"
}

// translateToGemini is translateToGemini. response_format is deliberately not
// mapped: gemini-2.5-flash hangs when responseMimeType is set.
func translateToGemini(openAI map[string]any, messages []map[string]any) map[string]any {
	result := map[string]any{}
	var contents []map[string]any
	var system []map[string]any
	for _, msg := range messages {
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		parts := geminiParts(msg)
		switch role {
		case "system":
			system = parts // the last one wins
		case "assistant":
			contents = append(contents, map[string]any{"role": "model", "parts": parts})
		case "tool":
			if response := geminiFunctionResponseParts(msg); len(response) > 0 {
				contents = append(contents, map[string]any{"role": "user", "parts": response})
			}
		default:
			contents = append(contents, map[string]any{"role": "user", "parts": parts})
		}
	}
	if len(system) > 0 {
		result["system_instruction"] = map[string]any{"parts": system}
	}
	if len(contents) > 0 {
		result["contents"] = contents
	}

	// All declarations in one tool entry: one tool per declaration made Gemini
	// ignore them.
	if tools, ok := objects(openAI["tools"]); ok {
		var declarations []map[string]any
		for _, tool := range tools {
			function, ok := tool["function"].(map[string]any)
			if !ok {
				continue
			}
			declarations = append(declarations, map[string]any{
				"name":        valueOr(function, "name", ""),
				"description": valueOr(function, "description", ""),
				"parameters":  valueOr(function, "parameters", map[string]any{}),
			})
		}
		if len(declarations) > 0 {
			result["tools"] = []map[string]any{{"function_declarations": declarations}}
			result["tool_config"] = geminiToolConfig(openAI["tool_choice"])
		}
	}

	generation := map[string]any{}
	if v, ok := doubleOf(openAI["temperature"]); ok {
		generation["temperature"] = v
	}
	if v, ok := intOf(openAI["max_tokens"]); ok {
		generation["maxOutputTokens"] = v
	}
	if v, ok := doubleOf(openAI["top_p"]); ok {
		generation["topP"] = v
	}
	if v, ok := stringsOf(openAI["stop"]); ok {
		generation["stopSequences"] = v
	}
	if len(generation) > 0 {
		result["generationConfig"] = generation
	}
	return result
}

// valueOr is dict[key] ?? fallback: a JSON null is present, so it stays.
func valueOr(dict map[string]any, key string, fallback any) any {
	if v, ok := dict[key]; ok {
		return v
	}
	return fallback
}

func geminiToolConfig(choice any) map[string]any {
	config := func(mode string, names ...string) map[string]any {
		fcc := map[string]any{"mode": mode}
		if len(names) > 0 {
			fcc["allowed_function_names"] = names
		}
		return map[string]any{"function_calling_config": fcc}
	}
	switch c := choice.(type) {
	case string:
		switch strings.ToLower(c) {
		case "none":
			return config("NONE")
		case "required", "any":
			return config("ANY")
		}
		return config("AUTO")
	case map[string]any:
		if fn, ok := c["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return config("ANY", name)
			}
		}
	}
	return config("AUTO")
}

// geminiParts is partsFromMessage. QUIRK kept from Swift: a tool call becomes a
// bare {"name","args","_callId"} part, not wrapped in "functionCall", so Gemini
// never sees the replayed call as one. Parts is never nil: an empty string content
// sends "parts": [].
func geminiParts(msg map[string]any) []map[string]any {
	parts := []map[string]any{}
	_, hasContent := msg["content"]
	_, hasToolCalls := msg["tool_calls"]
	if text, ok := msg["content"].(string); ok && text != "" {
		parts = append(parts, map[string]any{"text": text})
	} else if !hasContent && hasToolCalls {
		parts = append(parts, map[string]any{"text": ""})
	}
	calls, ok := objects(msg["tool_calls"])
	if !ok {
		return parts
	}
	for _, call := range calls {
		id, ok := call["id"].(string)
		if !ok {
			continue
		}
		function, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		name, ok := function["name"].(string)
		if !ok {
			continue
		}
		args := map[string]any{}
		if raw, ok := function["arguments"].(string); ok {
			if parsed, ok := decodeObject([]byte(raw)); ok {
				args = parsed
			}
		}
		parts = append(parts, map[string]any{"name": name, "args": args, "_callId": id})
	}
	return parts
}

// geminiFunctionResponseParts is buildFunctionResponseParts: a JSON-object content
// becomes the whole response, anything else stays under "content".
func geminiFunctionResponseParts(msg map[string]any) []map[string]any {
	name, ok := msg["name"].(string)
	if !ok {
		return nil
	}
	response := map[string]any{}
	if content, ok := msg["content"]; ok {
		response["content"] = content
	}
	if text, ok := msg["content"].(string); ok {
		if parsed, ok := decodeObject([]byte(text)); ok {
			response = parsed
		}
	}
	return []map[string]any{{"functionResponse": map[string]any{"name": name, "response": response}}}
}

func geminiFallbackResponse(message, model string) map[string]any {
	return map[string]any{
		"id":      swiftUUID(uuid.New()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "Error: " + message},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
}

func translateFromGemini(body []byte, requestedModel string) map[string]any {
	parsed, ok := decodeObject(body)
	if !ok {
		return geminiFallbackResponse("invalid gemini response body", requestedModel)
	}
	choices := []any{}
	if candidates, ok := objects(parsed["candidates"]); ok {
		for index, candidate := range candidates {
			choices = append(choices, geminiChoice(index, candidate))
		}
	}
	usage := map[string]any{}
	if meta, ok := parsed["usageMetadata"].(map[string]any); ok {
		for from, to := range map[string]string{
			"promptTokenCount":     "prompt_tokens",
			"candidatesTokenCount": "completion_tokens",
			"totalTokenCount":      "total_tokens",
		} {
			if v, ok := meta[from]; ok {
				usage[to] = v
			}
		}
	}
	return map[string]any{
		"id":      swiftUUID(uuid.New()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   requestedModel,
		"choices": choices,
		"usage":   usage,
	}
}

func geminiChoice(index int, candidate map[string]any) map[string]any {
	finishRaw, hasFinish := candidate["finishReason"].(string)
	finish := "STOP"
	if hasFinish {
		finish = finishRaw
	}
	var text strings.Builder
	toolCalls := []any{}
	if content, ok := candidate["content"].(map[string]any); ok {
		if parts, ok := objects(content["parts"]); ok {
			for _, part := range parts {
				if t, ok := part["text"].(string); ok {
					text.WriteString(t)
				} else if call, ok := part["functionCall"].(map[string]any); ok {
					toolCalls = append(toolCalls, geminiToolCall(call))
				}
			}
		}
	}
	if hasFinish && (finishRaw == "SAFETY" || finishRaw == "BLOCKLIST" || finishRaw == "PROHIBITED_CONTENT") {
		if ratings, ok := objects(candidate["safetyRatings"]); ok {
			var flags []string
			for _, rating := range ratings {
				category, isString := rating["category"].(string)
				blocked, isBool := rating["blocked"].(bool)
				if isString && isBool && blocked {
					flags = append(flags, category)
				}
			}
			if len(flags) > 0 {
				text.WriteString("\n[Blocked: " + strings.Join(flags, ", ") + "]")
			}
		}
	}
	message := map[string]any{"role": "assistant"}
	if text.Len() > 0 || len(toolCalls) == 0 {
		message["content"] = text.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	return map[string]any{"finish_reason": mapGeminiFinishReason(finish), "index": index, "message": message}
}

func geminiToolCall(call map[string]any) map[string]any {
	name, _ := call["name"].(string)
	args, ok := call["args"].(map[string]any)
	if !ok {
		args = map[string]any{}
	}
	id, ok := call["_callId"].(string)
	if !ok {
		id = strings.ReplaceAll(swiftUUID(uuid.New()), "-", "")
	}
	arguments := "{}"
	if encoded, err := swiftJSON(args); err == nil {
		arguments = string(encoded)
	}
	return map[string]any{
		"id":   id,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}
}

func mapGeminiFinishReason(reason string) string {
	switch reason {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "BLOCKLIST", "PROHIBITED_CONTENT", "RECITATION":
		return "content_filter"
	}
	return "stop"
}

// redactURLError strips the query from a *url.Error, which quotes the full request
// URL. Gemini's query holds the API key, and this text reaches logs and
// provider_failover_events. Timeout behaviour survives: the copy keeps the cause.
func redactURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) || !strings.Contains(ue.URL, "?") {
		return err
	}
	clean := *ue
	clean.URL, _, _ = strings.Cut(ue.URL, "?")
	return &clean
}
