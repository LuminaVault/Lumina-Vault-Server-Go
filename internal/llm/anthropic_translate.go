package llm

import (
	"fmt"
	"lumina/internal/swiftstr"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AnthropicRequestOptions says how a request is being paid for.
type AnthropicRequestOptions struct {
	Managed bool // the platform key is being spent and the trial is configured
	Trial   *TrialConfig
}

// AnthropicTranslatedRequest is a Messages request body plus what the response
// side needs. StripJSONFences: json_object was emulated with a system line, so
// the reply may still arrive wrapped in a Markdown fence.
type AnthropicTranslatedRequest struct {
	Body            map[string]any
	Model           string
	StripJSONFences bool
}

// AnthropicTranslatedResponse is the OpenAI-shaped reply and its usage.
type AnthropicTranslatedResponse struct {
	OpenAI map[string]any
	Usage  *AnthropicUsage
}

// AnthropicUsage is token usage as Anthropic reports it.
type AnthropicUsage struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
}

// PromptTokens is everything the prompt cost, cached or not: what OpenAI's
// prompt_tokens means and what the meters downstream expect.
func (u AnthropicUsage) PromptTokens() int {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// EstimatedCostUsdMicros is the Haiku trial rate card, $0.10 and $0.50 per MTok
// up to 100K prompt tokens and 5x above. An estimate for the log line.
func (u AnthropicUsage) EstimatedCostUsdMicros() int64 {
	multiplier := int64(1)
	if u.PromptTokens() > 100_000 {
		multiplier = 5
	}
	return (int64(u.PromptTokens())*100_000 + int64(u.OutputTokens)*500_000) * multiplier / 1_000_000
}

// merge overwrites the counters present in json. message_delta repeats some of
// message_start's fields with cumulative values, so the last write wins.
func (u *AnthropicUsage) merge(json map[string]any) {
	set := func(key string, dst *int) {
		if v, ok := intOf(json[key]); ok {
			*dst = v
		}
	}
	set("input_tokens", &u.InputTokens)
	set("output_tokens", &u.OutputTokens)
	set("cache_read_input_tokens", &u.CacheReadInputTokens)
	set("cache_creation_input_tokens", &u.CacheCreationInputTokens)
}

// AnthropicStreamState is carried across SSE records.
type AnthropicStreamState struct {
	Usage       AnthropicUsage
	Model       string
	EmittedText bool
}

const (
	// AnthropicDefaultModel is used when a non-managed request names none.
	AnthropicDefaultModel = "claude-sonnet-4-6"
	// anthropicDefaultMaxTokens: Anthropic requires max_tokens and thinking counts
	// toward it, so leave room for both.
	anthropicDefaultMaxTokens = 16000
	thinkingMinimumMaxTokens  = 1024
	// JSONOnlyInstruction is the system line that emulates json_object.
	JSONOnlyInstruction = "Respond with a single JSON object, no code fences."
	// ContinuePrompt is a user turn added when the transcript would otherwise end
	// on the assistant: current models reject assistant prefill.
	ContinuePrompt = "Continue."
)

var anthropicImageMediaTypes = []string{"image/jpeg", "image/png", "image/gif", "image/webp"}

// thinkingAlwaysOn: models that reject thinking {type: disabled} at every effort.
func thinkingAlwaysOn(model string) bool {
	return hasAnyPrefix(model, "claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5")
}

// rejectsForcedToolChoice: models that reject tool_choice any and tool.
func rejectsForcedToolChoice(model string) bool {
	return hasAnyPrefix(model, "claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1")
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(s, p) })
}

type anthropicTurn struct {
	role   string
	blocks []map[string]any
}

// TranslateRequest turns an OpenAI chat-completions payload into a Messages body.
// Shared by the buffered and streaming paths.
//
//   - System and developer messages become the top-level system field.
//   - String or array content; image_url data URIs become base64 image blocks and
//     http(s) URLs become URL blocks. Any other part type is transient, so the
//     router moves on to a provider that takes it.
//   - tools become Anthropic tools, assistant tool_calls become tool_use, and tool
//     messages become tool_result blocks merged into one user turn.
//   - Consecutive same-role turns merge, blank text is dropped, and the transcript
//     always ends on a user turn (no prefill).
//   - temperature, top_p and top_k are never sent: Haiku 5.5 returns a 400 on any
//     non-default value. Do not add them back.
func TranslateRequest(payload []byte, stream bool, options AnthropicRequestOptions) (AnthropicTranslatedRequest, error) {
	openAI, ok := decodeObject(payload)
	var messages []map[string]any
	if ok {
		messages, ok = objects(openAI["messages"])
	}
	if !ok {
		return AnthropicTranslatedRequest{}, Permanent(Anthropic, 400, strPtr("invalid OpenAI payload: cannot parse messages"))
	}

	requested, _ := openAI["model"].(string)
	model := resolveAnthropicModel(requested, options)
	maxTokens, ok := intOf(openAI["max_tokens"])
	if !ok {
		maxTokens, ok = intOf(openAI["max_completion_tokens"])
	}
	if !ok {
		maxTokens = anthropicDefaultMaxTokens
	}
	// Trial knobs only for the trial model: older models a registry key can reach
	// reject some of them.
	var trial *TrialConfig
	if options.Managed && options.Trial != nil && options.Trial.Model == model {
		trial = options.Trial
	}

	system, input, hasToolTurns, err := translateMessages(messages)
	if err != nil {
		return AnthropicTranslatedRequest{}, err
	}
	tools := translateTools(openAI["tools"])
	turns := normalizeTurns(input, len(tools) > 0)

	outputConfig := map[string]any{}
	stripJSONFences := false
	responseFormat, _ := openAI["response_format"].(map[string]any)
	formatType, _ := responseFormat["type"].(string)
	switch formatType {
	case "json_object":
		system = append(system, JSONOnlyInstruction)
		stripJSONFences = true
	case "json_schema":
		var schema map[string]any
		if wrapper, ok := responseFormat["json_schema"].(map[string]any); ok {
			schema, _ = wrapper["schema"].(map[string]any)
		}
		if trial != nil && schema != nil {
			outputConfig["format"] = map[string]any{"type": "json_schema", "schema": schema}
		} else {
			system = append(system, JSONOnlyInstruction)
			stripJSONFences = true
		}
	}

	wireTurns := make([]any, 0, len(turns))
	for _, t := range turns {
		wireTurns = append(wireTurns, map[string]any{"role": t.role, "content": t.blocks})
	}
	body := map[string]any{"model": model, "messages": wireTurns, "max_tokens": maxTokens}
	if len(system) > 0 {
		body["system"] = strings.Join(system, "\n\n")
	}
	if len(tools) > 0 {
		body["tools"] = tools
		if choice := TranslateToolChoice(openAI["tool_choice"], model); choice != nil {
			body["tool_choice"] = choice
		}
	}
	if stops := translateStops(openAI["stop"]); len(stops) > 0 {
		body["stop_sequences"] = stops
	}
	if trial != nil {
		// Thinking is adaptive by default. On models that allow it, it is off when
		// the transcript carries earlier tool turns (their thinking blocks cannot
		// survive the OpenAI-shaped transcript to be replayed) and when max_tokens
		// leaves no room. Models that reject disabled get low effort instead.
		alwaysOn := thinkingAlwaysOn(model)
		wantsOff := hasToolTurns || trial.Thinking == ThinkingDisabled || maxTokens < thinkingMinimumMaxTokens
		effort := trial.Effort
		if wantsOff && alwaysOn && effort == "" {
			effort = "low"
		}
		if wantsOff && !alwaysOn {
			body["thinking"] = map[string]any{"type": "disabled"}
			// disabled is accepted only at effort high or below.
			if effort == "xhigh" || effort == "max" {
				effort = "high"
			}
		}
		if effort != "" {
			outputConfig["effort"] = effort
		}
	}
	if len(outputConfig) > 0 {
		body["output_config"] = outputConfig
	}
	if stream {
		body["stream"] = true
	}
	return AnthropicTranslatedRequest{Body: body, Model: model, StripJSONFences: stripJSONFences}, nil
}

func translateStops(v any) []string {
	var stops []string
	if list, ok := stringsOf(v); ok {
		stops = list
	} else if s, ok := v.(string); ok {
		stops = []string{s}
	}
	return slices.DeleteFunc(stops, isBlank)
}

// resolveAnthropicModel: managed requests that name no Claude model, the router's
// slug for another provider say, get the trial model, since a non-Claude id would
// only 404. Everything else keeps the requested model.
func resolveAnthropicModel(requested string, options AnthropicRequestOptions) string {
	model := swiftstr.TrimWhitespacesAndNewlines(requested)
	if options.Managed && options.Trial != nil {
		if strings.HasPrefix(strings.ToLower(model), "claude-") {
			return model
		}
		return options.Trial.Model
	}
	if model == "" {
		return AnthropicDefaultModel
	}
	return model
}

func generatedToolID() string {
	return "toolu_" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))
}

func translateMessages(messages []map[string]any) (system []string, turns []anthropicTurn, hasToolTurns bool, err error) {
	// tool_use ids of the latest assistant turn not yet answered. Pairs tool
	// messages that arrive without a tool_call_id.
	var unanswered []string
	add := func(role string, blocks []map[string]any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(turns); n > 0 && turns[n-1].role == role {
			turns[n-1].blocks = append(turns[n-1].blocks, blocks...)
			return
		}
		turns = append(turns, anthropicTurn{role, blocks})
	}
	for _, message := range messages {
		role := "user"
		if r, ok := message["role"].(string); ok {
			role = strings.ToLower(r)
		}
		switch role {
		case "system", "developer":
			if text := plainText(message["content"]); !isBlank(text) {
				system = append(system, text)
			}
		case "assistant":
			blocks, err := contentBlocks(message["content"])
			if err != nil {
				return nil, nil, false, err
			}
			if calls, ok := objects(message["tool_calls"]); ok && len(calls) > 0 {
				hasToolTurns = true
				unanswered = nil
				for _, call := range calls {
					function, _ := call["function"].(map[string]any)
					name, _ := function["name"].(string)
					if name == "" {
						continue
					}
					id, _ := call["id"].(string)
					if id == "" {
						id = generatedToolID()
					}
					blocks = append(blocks, map[string]any{
						"type": "tool_use", "id": id, "name": name, "input": toolInput(function["arguments"]),
					})
					unanswered = append(unanswered, id)
				}
			}
			add("assistant", blocks)
		case "tool", "function":
			hasToolTurns = true
			id, _ := message["tool_call_id"].(string)
			if id == "" && len(unanswered) > 0 {
				id = unanswered[0]
			}
			unanswered = slices.DeleteFunc(unanswered, func(u string) bool { return u == id })
			block := map[string]any{"type": "tool_result", "tool_use_id": id}
			if text := plainText(message["content"]); !isBlank(text) {
				block["content"] = text
			}
			add("user", []map[string]any{block})
		default:
			blocks, err := contentBlocks(message["content"])
			if err != nil {
				return nil, nil, false, err
			}
			add("user", blocks)
		}
	}
	return system, turns, hasToolTurns, nil
}

// normalizeTurns makes the transcript one the Messages API accepts. With tools
// declared, each assistant tool_use must be answered by a tool_result at the
// front of the very next user turn: unanswered calls get an error result, and
// results that answer nothing become text. With no tools declared (a final
// "answer now" call), tool blocks are rejected outright, so they are flattened.
func normalizeTurns(input []anthropicTurn, toolsDeclared bool) []anthropicTurn {
	var turns []anthropicTurn
	if toolsDeclared {
		for _, turn := range input {
			if turn.role != "user" {
				turns = append(turns, turn)
				continue
			}
			var expected []string
			if n := len(turns); n > 0 && turns[n-1].role == "assistant" {
				expected = toolUseIDs(turns[n-1])
			}
			var results, others []map[string]any
			answered := map[string]bool{}
			for _, block := range turn.blocks {
				if block["type"] != "tool_result" {
					others = append(others, block)
					continue
				}
				id, _ := block["tool_use_id"].(string)
				if slices.Contains(expected, id) && !answered[id] {
					results = append(results, block)
					answered[id] = true
				} else {
					others = append(others, flattenToolBlock(block))
				}
			}
			for _, id := range expected {
				if !answered[id] {
					results = append(results, missingToolResult(id))
				}
			}
			turns = append(turns, anthropicTurn{"user", append(results, others...)})
		}
		if n := len(turns); n > 0 && turns[n-1].role == "assistant" {
			if pending := toolUseIDs(turns[n-1]); len(pending) > 0 {
				blocks := make([]map[string]any, 0, len(pending))
				for _, id := range pending {
					blocks = append(blocks, missingToolResult(id))
				}
				turns = append(turns, anthropicTurn{"user", blocks})
			}
		}
	} else {
		for _, turn := range input {
			blocks := make([]map[string]any, 0, len(turn.blocks))
			for _, block := range turn.blocks {
				blocks = append(blocks, flattenToolBlock(block))
			}
			turns = append(turns, anthropicTurn{turn.role, blocks})
		}
	}

	// Drop empty turns, merge what that makes adjacent, and bracket the transcript
	// with user turns.
	var merged []anthropicTurn
	for _, turn := range turns {
		if len(turn.blocks) == 0 {
			continue
		}
		if n := len(merged); n > 0 && merged[n-1].role == turn.role {
			merged[n-1].blocks = append(merged[n-1].blocks, turn.blocks...)
		} else {
			merged = append(merged, turn)
		}
	}
	continueTurn := anthropicTurn{"user", []map[string]any{textBlock(ContinuePrompt)}}
	if len(merged) == 0 || merged[0].role != "user" {
		merged = append([]anthropicTurn{continueTurn}, merged...)
	}
	if merged[len(merged)-1].role != "user" {
		merged = append(merged, continueTurn)
	}
	return merged
}

func translateTools(v any) []map[string]any {
	tools, ok := objects(v)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, tool := range tools {
		if typ, ok := tool["type"].(string); ok && typ != "function" {
			continue
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := function["name"].(string)
		if name == "" {
			continue
		}
		schema, ok := function["parameters"].(map[string]any)
		if !ok {
			schema = map[string]any{}
		}
		if _, ok := schema["type"]; !ok {
			schema["type"] = "object"
		}
		translated := map[string]any{"name": name, "input_schema": schema}
		if description, ok := function["description"].(string); ok && !isBlank(description) {
			translated["description"] = description
		}
		out = append(out, translated)
	}
	return out
}

// TranslateToolChoice: auto, or absent, is Anthropic's default and is omitted.
// Models that reject forced tool use fall back to auto and rely on the prompt.
func TranslateToolChoice(v any, model string) map[string]any {
	forcedAllowed := !rejectsForcedToolChoice(model)
	switch choice := v.(type) {
	case string:
		switch {
		case choice == "none":
			return map[string]any{"type": "none"}
		case choice == "required" && forcedAllowed:
			return map[string]any{"type": "any"}
		}
	case map[string]any:
		if function, ok := choice["function"].(map[string]any); ok {
			if name, ok := function["name"].(string); ok && forcedAllowed {
				return map[string]any{"type": "tool", "name": name}
			}
		}
	}
	return nil
}

func contentBlocks(content any) ([]map[string]any, error) {
	if text, ok := content.(string); ok {
		if isBlank(text) {
			return nil, nil
		}
		return []map[string]any{textBlock(text)}, nil
	}
	parts, ok := objects(content)
	if !ok {
		return nil, nil
	}
	var blocks []map[string]any
	for _, part := range parts {
		typ, ok := part["type"].(string)
		if !ok {
			typ = "text"
		}
		switch typ {
		case "text", "input_text":
			if text, ok := part["text"].(string); ok && !isBlank(text) {
				blocks = append(blocks, textBlock(text))
			}
		case "image_url":
			block, err := imageBlock(part["image_url"])
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		default:
			return nil, Transient(Anthropic, 0, strPtr("unsupported content part type for anthropic: "+typ))
		}
	}
	return blocks, nil
}

func imageBlock(v any) (map[string]any, error) {
	target := ""
	if m, ok := v.(map[string]any); ok {
		target, _ = m["url"].(string)
	} else if s, ok := v.(string); ok {
		target = s
	}
	if strings.HasPrefix(target, "data:") {
		if comma := strings.IndexByte(target, ','); comma >= 0 {
			header := target[5:comma]
			mediaType := ""
			if parts := strings.Split(header, ";"); parts[0] != "" {
				mediaType = strings.ToLower(parts[0])
			} else if len(parts) > 1 {
				// split(separator:) omits empty pieces, so ";base64" names "base64".
				mediaType = strings.ToLower(parts[1])
			}
			data := target[comma+1:]
			if strings.Contains(strings.ToLower(header), ";base64") && slices.Contains(anthropicImageMediaTypes, mediaType) && data != "" {
				return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": data}}, nil
			}
			return nil, Transient(Anthropic, 0, strPtr("unsupported image data URI for anthropic: "+mediaType))
		}
	}
	if strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "http://") {
		return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": target}}, nil
	}
	return nil, Transient(Anthropic, 0, strPtr("unsupported image_url for anthropic"))
}

// toolInput: OpenAI carries arguments as a JSON string, Anthropic wants an object.
func toolInput(arguments any) map[string]any {
	if object, ok := arguments.(map[string]any); ok {
		return object
	}
	raw, ok := arguments.(string)
	if !ok || isBlank(raw) {
		return map[string]any{}
	}
	if object, ok := decodeObject([]byte(raw)); ok {
		return object
	}
	return map[string]any{"arguments": raw}
}

func toolUseIDs(turn anthropicTurn) []string {
	var ids []string
	for _, block := range turn.blocks {
		if block["type"] == "tool_use" {
			if id, ok := block["id"].(string); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func missingToolResult(id string) map[string]any {
	return map[string]any{"type": "tool_result", "tool_use_id": id, "content": "No result was returned for this tool call.", "is_error": true}
}

func flattenToolBlock(block map[string]any) map[string]any {
	switch block["type"] {
	case "tool_use":
		name, ok := block["name"].(string)
		if !ok {
			name = "tool"
		}
		input := "{}"
		if object, ok := block["input"].(map[string]any); ok {
			if b, err := swiftJSON(object); err == nil {
				input = string(b)
			}
		}
		return textBlock(fmt.Sprintf("[Called tool %s with input %s]", name, input))
	case "tool_result":
		content, _ := block["content"].(string)
		if isBlank(content) {
			return textBlock("[Tool result: (empty)]")
		}
		return textBlock("[Tool result: " + content + "]")
	}
	return block
}

func plainText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	parts, ok := objects(content)
	if !ok {
		return ""
	}
	var texts []string
	for _, part := range parts {
		if text, ok := part["text"].(string); ok {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

func textBlock(text string) map[string]any { return map[string]any{"type": "text", "text": text} }

// TranslateResponse turns a Messages reply into the OpenAI shape. Blocks are read
// by type: thinking is ignored and tool_use becomes tool_calls. A refusal, or a
// reply with neither text nor tool calls (thinking used the whole max_tokens, say),
// is transient so the router fails over instead of returning an empty answer.
func TranslateResponse(body []byte, model string, stripJSONFences bool) (AnthropicTranslatedResponse, error) {
	anthropic, ok := decodeObject(body)
	if !ok {
		return AnthropicTranslatedResponse{}, Transient(Anthropic, 200, strPtr("unparseable anthropic response"))
	}
	stopReason, hasStop := anthropic["stop_reason"].(string)
	if stopReason == "refusal" {
		return AnthropicTranslatedResponse{}, Transient(Anthropic, 200, strPtr("anthropic refusal"))
	}

	text := ""
	var toolCalls []any
	content, _ := objects(anthropic["content"])
	for _, block := range content {
		switch block["type"] {
		case "text":
			t, _ := block["text"].(string)
			text += t
		case "tool_use":
			input, ok := block["input"].(map[string]any)
			if !ok {
				input = map[string]any{}
			}
			id, ok := block["id"].(string)
			if !ok {
				id = generatedToolID()
			}
			name, _ := block["name"].(string)
			arguments := "{}"
			if b, err := swiftJSON(input); err == nil {
				arguments = string(b)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": arguments},
			})
		}
	}
	if stripJSONFences {
		text = StripCodeFences(text)
	}
	if isBlank(text) && len(toolCalls) == 0 {
		reason := "none"
		if hasStop {
			reason = stopReason
		}
		return AnthropicTranslatedResponse{}, Transient(Anthropic, 200, strPtr("anthropic returned no text (stop_reason="+reason+")"))
	}

	var usage AnthropicUsage
	if u, ok := anthropic["usage"].(map[string]any); ok {
		usage.merge(u)
	}
	message := map[string]any{"role": "assistant", "content": text}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	id, ok := anthropic["id"].(string)
	if !ok {
		id = strings.ToUpper(uuid.NewString())
	}
	return AnthropicTranslatedResponse{
		OpenAI: map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": int(time.Now().Unix()),
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": openAIFinishReason(stopReason),
			}},
			"usage": map[string]any{
				"prompt_tokens":     usage.PromptTokens(),
				"completion_tokens": usage.OutputTokens,
				"total_tokens":      usage.PromptTokens() + usage.OutputTokens,
			},
		},
		Usage: &usage,
	}, nil
}

func openAIFinishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return "stop"
}

// StripCodeFences removes a Markdown code fence around a JSON reply. A Swift
// Character "\r\n" is not "\n", so a CRLF does not end the opening fence line.
func StripCodeFences(text string) string {
	trimmed := swiftstr.TrimWhitespacesAndNewlines(text)
	if !strings.HasPrefix(trimmed, "```") {
		return trimmed
	}
	newline := -1
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] == '\n' && (i == 0 || trimmed[i-1] != '\r') {
			newline = i
			break
		}
	}
	if newline >= 0 {
		trimmed = trimmed[newline+1:]
	} else {
		trimmed = trimmed[3:]
	}
	trimmed = strings.TrimSuffix(trimmed, "```")
	return swiftstr.TrimWhitespacesAndNewlines(trimmed)
}

// ProcessAnthropicRecord parses one Anthropic SSE record, accumulating usage into
// state, and returns true on message_stop.
//
// Only text deltas are forwarded: thinking, signature and tool-input deltas are
// dropped. A stream reaching its stop reason without any text (a refusal, thinking
// that used the whole budget, a tool call this text-only stream cannot carry) is
// transient before anything was yielded, so the transport can still fail over.
func ProcessAnthropicRecord(record string, state *AnthropicStreamState, yield func(Chunk)) (bool, error) {
	eventName, hasEvent := "", false
	for _, line := range swiftLines(record) {
		if strings.HasPrefix(line, "event:") {
			eventName, hasEvent = trimWhitespaces(line[6:]), true
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := trimWhitespaces(line[5:])
		if payload == "" {
			continue
		}
		obj, ok := decodeObject([]byte(payload))
		if !ok {
			continue
		}
		// The type rides both the event line and the JSON; prefer the line.
		event := eventName
		if !hasEvent {
			event, _ = obj["type"].(string)
		}
		switch event {
		case "message_start":
			if message, ok := obj["message"].(map[string]any); ok {
				if usage, ok := message["usage"].(map[string]any); ok {
					state.Usage.merge(usage)
				}
			}
		case "content_block_delta":
			delta, ok := obj["delta"].(map[string]any)
			if !ok {
				break
			}
			deltaType, hasType := delta["type"].(string)
			if !hasType || deltaType == "text_delta" {
				if text, ok := delta["text"].(string); ok && text != "" {
					state.EmittedText = true
					yield(Chunk{Delta: text})
				}
			}
		case "message_delta":
			if usage, ok := obj["usage"].(map[string]any); ok {
				state.Usage.merge(usage)
			}
			if delta, ok := obj["delta"].(map[string]any); ok {
				if stop, ok := delta["stop_reason"].(string); ok {
					if !state.EmittedText {
						body := "anthropic streamed no text (stop_reason=" + stop + ")"
						if stop == "refusal" {
							body = "anthropic refusal"
						}
						return false, Transient(Anthropic, 200, &body)
					}
					yield(Chunk{FinishReason: openAIFinishReason(stop)})
				}
			}
		case "message_stop":
			return true, nil
		case "error":
			message := "anthropic stream error"
			if e, ok := obj["error"].(map[string]any); ok {
				if m, ok := e["message"].(string); ok {
					message = m
				}
			}
			return false, Transient(Anthropic, 0, &message)
		}
	}
	return false, nil
}
