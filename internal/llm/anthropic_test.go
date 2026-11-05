package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testTrial   = &TrialConfig{APIKey: "trial-key", Model: TrialDefaultModel, Effort: "low", Thinking: ThinkingAdaptive, Scope: ScopeAll}
	testManaged = AnthropicRequestOptions{Managed: true, Trial: testTrial}
)

type obj = map[string]any

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}

// onWire reduces a translated body to the JSON the upstream actually receives.
func onWire(t *testing.T, v any) obj {
	t.Helper()
	data, err := encodeJSON(v)
	require.NoError(t, err)
	var out obj
	require.NoError(t, json.Unmarshal(data, &out))
	return out
}

func translate(t *testing.T, payload obj, options AnthropicRequestOptions) (AnthropicTranslatedRequest, obj) {
	t.Helper()
	request, err := TranslateRequest(mustJSON(t, payload), false, options)
	require.NoError(t, err)
	return request, onWire(t, request.Body)
}

func turnsOf(body obj) []obj {
	raw, _ := body["messages"].([]any)
	out := make([]obj, 0, len(raw))
	for _, turn := range raw {
		out = append(out, turn.(obj))
	}
	return out
}

func blocksOf(turn obj) []obj {
	raw, _ := turn["content"].([]any)
	out := make([]obj, 0, len(raw))
	for _, block := range raw {
		out = append(out, block.(obj))
	}
	return out
}

func rolesOf(turns []obj) []string {
	out := make([]string, 0, len(turns))
	for _, turn := range turns {
		out = append(out, turn["role"].(string))
	}
	return out
}

func userHi() []any { return []any{obj{"role": "user", "content": "hi"}} }

var lookupTool = []any{obj{"type": "function", "function": obj{"name": "lookup"}}}

func toolTranscript() []any {
	return []any{
		obj{"role": "user", "content": "go"},
		obj{"role": "assistant", "tool_calls": []any{obj{"id": "c", "type": "function", "function": obj{"name": "lookup", "arguments": "{}"}}}},
		obj{"role": "tool", "tool_call_id": "c", "content": "ok"},
	}
}

func requireTransient(t *testing.T, err error) {
	t.Helper()
	var perr *ProviderError
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, KindTransient, perr.Kind)
}

// AnthropicAdapterTranslationTests.
func TestTranslateSamplingAndSystem(t *testing.T) {
	t.Run("sampling parameters are never sent", func(t *testing.T) {
		_, body := translate(t, obj{"model": "claude-haiku-5-5", "temperature": 0.7, "top_p": 0.9, "top_k": 40, "messages": userHi()}, testManaged)
		assert.NotContains(t, body, "temperature")
		assert.NotContains(t, body, "top_p")
		assert.NotContains(t, body, "top_k")
	})
	t.Run("byok requests carry no temperature either", func(t *testing.T) {
		_, body := translate(t, obj{"model": "claude-sonnet-4-6", "messages": userHi()}, AnthropicRequestOptions{})
		assert.NotContains(t, body, "temperature")
	})
	t.Run("system messages become the top-level system field", func(t *testing.T) {
		_, body := translate(t, obj{"messages": []any{
			obj{"role": "system", "content": "Be brief."},
			obj{"role": "user", "content": "hi"},
			obj{"role": "system", "content": []any{obj{"type": "text", "text": "Be kind."}}},
		}}, testManaged)
		assert.Equal(t, "Be brief.\n\nBe kind.", body["system"])
		assert.NotContains(t, rolesOf(turnsOf(body)), "system")
	})
	t.Run("managed requests without a claude model use the trial model", func(t *testing.T) {
		request, body := translate(t, obj{"model": "deepseek/deepseek-v4-flash", "messages": userHi()}, testManaged)
		assert.Equal(t, "claude-haiku-5-5", request.Model)
		assert.Equal(t, "claude-haiku-5-5", body["model"])
	})
	t.Run("byok keeps the requested model and the old default", func(t *testing.T) {
		named, _ := translate(t, obj{"model": "claude-opus-4-7", "messages": userHi()}, AnthropicRequestOptions{})
		assert.Equal(t, "claude-opus-4-7", named.Model)
		unnamed, _ := translate(t, obj{"messages": userHi()}, AnthropicRequestOptions{})
		assert.Equal(t, AnthropicDefaultModel, unnamed.Model)
	})
}

func TestTranslateContent(t *testing.T) {
	t.Run("array content maps text parts and data URI images", func(t *testing.T) {
		_, body := translate(t, obj{"messages": []any{obj{"role": "user", "content": []any{
			obj{"type": "text", "text": "What is this?"},
			obj{"type": "image_url", "image_url": obj{"url": "data:image/png;base64,iVBORw0KGgo="}},
			obj{"type": "text", "text": "   "},
		}}}}, testManaged)
		blocks := blocksOf(turnsOf(body)[0])
		require.Len(t, blocks, 2, "blank text is dropped")
		assert.Equal(t, "What is this?", blocks[0]["text"])
		assert.Equal(t, "image", blocks[1]["type"])
		assert.Equal(t, obj{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}, blocks[1]["source"])
	})
	t.Run("an unsupported content part fails over", func(t *testing.T) {
		_, err := TranslateRequest(mustJSON(t, obj{"messages": []any{obj{"role": "user", "content": []any{
			obj{"type": "input_audio", "input_audio": obj{"data": "AAA"}},
		}}}}), false, testManaged)
		requireTransient(t, err)
	})
}

func TestTranslateTurns(t *testing.T) {
	t.Run("consecutive same-role turns merge and the transcript ends on a user turn", func(t *testing.T) {
		_, body := translate(t, obj{"messages": []any{
			obj{"role": "user", "content": "one"},
			obj{"role": "user", "content": "two"},
			obj{"role": "assistant", "content": "Sure"},
		}}, testManaged)
		turns := turnsOf(body)
		assert.Equal(t, []string{"user", "assistant", "user"}, rolesOf(turns))
		first := blocksOf(turns[0])
		assert.Equal(t, "one", first[0]["text"])
		assert.Equal(t, "two", first[1]["text"])
		assert.Equal(t, ContinuePrompt, blocksOf(turns[2])[0]["text"], "no assistant prefill reaches Haiku 5.5")
	})
	t.Run("tools translate and tool turns become tool_use and merged tool_result", func(t *testing.T) {
		_, body := translate(t, obj{
			"tools": []any{obj{"type": "function", "function": obj{
				"name": "search_vault", "description": "Search notes",
				"parameters": obj{"type": "object", "properties": obj{"q": obj{"type": "string"}}},
			}}},
			"tool_choice": "auto",
			"messages": []any{
				obj{"role": "user", "content": "find cats and dogs"},
				obj{"role": "assistant", "content": nil, "tool_calls": []any{
					obj{"id": "call_1", "type": "function", "function": obj{"name": "search_vault", "arguments": `{"q":"cats"}`}},
					obj{"id": "call_2", "type": "function", "function": obj{"name": "search_vault", "arguments": `{"q":"dogs"}`}},
				}},
				obj{"role": "tool", "tool_call_id": "call_1", "content": "3 cat notes"},
				obj{"role": "tool", "tool_call_id": "call_2", "content": "1 dog note"},
			},
		}, testManaged)
		tool := body["tools"].([]any)[0].(obj)
		assert.Equal(t, "search_vault", tool["name"])
		assert.Equal(t, "object", tool["input_schema"].(obj)["type"])
		assert.NotContains(t, body, "tool_choice", "auto is Anthropic's default")

		turns := turnsOf(body)
		assert.Equal(t, []string{"user", "assistant", "user"}, rolesOf(turns))
		uses := blocksOf(turns[1])
		require.Len(t, uses, 2)
		assert.Equal(t, "tool_use", uses[0]["type"])
		assert.Equal(t, "tool_use", uses[1]["type"])
		assert.Equal(t, "call_1", uses[0]["id"])
		assert.Equal(t, "cats", uses[0]["input"].(obj)["q"])
		results := blocksOf(turns[2])
		require.Len(t, results, 2)
		assert.Equal(t, "tool_result", results[0]["type"])
		assert.Equal(t, "call_1", results[0]["tool_use_id"])
		assert.Equal(t, "call_2", results[1]["tool_use_id"])
		assert.Equal(t, "1 dog note", results[1]["content"])
	})
	t.Run("an unanswered tool call gets an error result", func(t *testing.T) {
		_, body := translate(t, obj{"tools": lookupTool, "messages": []any{
			obj{"role": "user", "content": "go"},
			obj{"role": "assistant", "tool_calls": []any{obj{"id": "call_9", "type": "function", "function": obj{"name": "lookup", "arguments": ""}}}},
		}}, testManaged)
		turns := turnsOf(body)
		last := turns[len(turns)-1]
		assert.Equal(t, "user", last["role"])
		result := blocksOf(last)[0]
		assert.Equal(t, "tool_result", result["type"])
		assert.Equal(t, "call_9", result["tool_use_id"])
		assert.Equal(t, true, result["is_error"])
	})
	t.Run("tool turns flatten to text when the request declares no tools", func(t *testing.T) {
		_, body := translate(t, obj{"messages": append(toolTranscript(), obj{"role": "user", "content": "now answer"})}, testManaged)
		for _, turn := range turnsOf(body) {
			for _, block := range blocksOf(turn) {
				assert.NotEqual(t, "tool_use", block["type"])
				assert.NotEqual(t, "tool_result", block["type"])
			}
		}
		assert.NotContains(t, body, "tools")
	})
}

func outputConfig(body obj) obj {
	c, _ := body["output_config"].(obj)
	return c
}

func TestTranslateThinkingAndEffort(t *testing.T) {
	disabled := obj{"type": "disabled"}
	t.Run("adaptive thinking is left to the default and effort is sent", func(t *testing.T) {
		_, body := translate(t, obj{"max_tokens": 4096, "messages": userHi()}, testManaged)
		assert.NotContains(t, body, "thinking")
		assert.Equal(t, "low", outputConfig(body)["effort"])
	})
	t.Run("thinking is disabled when the transcript carries tool turns", func(t *testing.T) {
		_, body := translate(t, obj{"tools": lookupTool, "messages": toolTranscript()}, testManaged)
		assert.Equal(t, disabled, body["thinking"])
	})
	t.Run("thinking is disabled under 1024 max tokens", func(t *testing.T) {
		_, body := translate(t, obj{"max_tokens": 200, "messages": userHi()}, testManaged)
		assert.Equal(t, disabled, body["thinking"])
	})
	t.Run("ANTHROPIC_THINKING=disabled turns it off and clamps effort to high", func(t *testing.T) {
		trial := &TrialConfig{APIKey: "k", Model: TrialDefaultModel, Effort: "max", Thinking: ThinkingDisabled}
		_, body := translate(t, obj{"messages": userHi()}, AnthropicRequestOptions{Managed: true, Trial: trial})
		assert.Equal(t, disabled, body["thinking"])
		assert.Equal(t, "high", outputConfig(body)["effort"])
	})
	t.Run("Opus 5_5 never gets disabled thinking and falls back to low effort", func(t *testing.T) {
		trial := &TrialConfig{APIKey: "k", Model: "claude-opus-5-5", Thinking: ThinkingDisabled}
		_, body := translate(t, obj{"max_tokens": 200, "tools": lookupTool, "messages": toolTranscript()}, AnthropicRequestOptions{Managed: true, Trial: trial})
		assert.NotContains(t, body, "thinking")
		assert.Equal(t, "low", outputConfig(body)["effort"])
	})
	t.Run("Opus 5_5 keeps a configured effort when thinking would be disabled", func(t *testing.T) {
		trial := &TrialConfig{APIKey: "k", Model: "claude-opus-5-5", Effort: "max", Thinking: ThinkingDisabled}
		_, body := translate(t, obj{"messages": userHi()}, AnthropicRequestOptions{Managed: true, Trial: trial})
		assert.NotContains(t, body, "thinking")
		assert.Equal(t, "max", outputConfig(body)["effort"])
	})
	t.Run("forced tool_choice is dropped to auto on Opus 5_5 and kept on Haiku 5_5", func(t *testing.T) {
		named := obj{"type": "function", "function": obj{"name": "lookup"}}
		assert.Nil(t, TranslateToolChoice("required", "claude-opus-5-5"))
		assert.Nil(t, TranslateToolChoice(named, "claude-opus-5-5"))
		assert.Equal(t, "none", TranslateToolChoice("none", "claude-opus-5-5")["type"])
		assert.Equal(t, "any", TranslateToolChoice("required", "claude-haiku-5-5")["type"])
		assert.Equal(t, "lookup", TranslateToolChoice(named, "claude-haiku-5-5")["name"])
	})
	t.Run("byok requests never get trial thinking or effort", func(t *testing.T) {
		_, body := translate(t, obj{"model": "claude-opus-5-5", "max_tokens": 100, "messages": userHi()}, AnthropicRequestOptions{})
		assert.NotContains(t, body, "thinking")
		assert.NotContains(t, body, "output_config")
	})
}

func replyContent(t *testing.T, response AnthropicTranslatedResponse) obj {
	t.Helper()
	return onWire(t, response.OpenAI)["choices"].([]any)[0].(obj)
}

func TestTranslateJSONOutput(t *testing.T) {
	t.Run("json_object adds a system line and strips fences from the reply", func(t *testing.T) {
		request, body := translate(t, obj{"response_format": obj{"type": "json_object"}, "messages": []any{
			obj{"role": "system", "content": "Extract."}, obj{"role": "user", "content": "x"},
		}}, testManaged)
		assert.True(t, request.StripJSONFences)
		assert.Contains(t, body["system"], JSONOnlyInstruction)

		response, err := TranslateResponse(mustJSON(t, obj{
			"id": "msg_1", "stop_reason": "end_turn",
			"content": []any{obj{"type": "text", "text": "```json\n{\"a\":1}\n```"}},
			"usage":   obj{"input_tokens": 10, "output_tokens": 5},
		}), request.Model, true)
		require.NoError(t, err)
		assert.Equal(t, `{"a":1}`, replyContent(t, response)["message"].(obj)["content"])
	})
	t.Run("json_schema becomes output_config format on the managed trial", func(t *testing.T) {
		schema := obj{"type": "object", "properties": obj{"a": obj{"type": "integer"}}, "required": []any{"a"}, "additionalProperties": false}
		request, body := translate(t, obj{
			"response_format": obj{"type": "json_schema", "json_schema": obj{"name": "out", "schema": schema}},
			"messages":        []any{obj{"role": "user", "content": "x"}},
		}, testManaged)
		format := outputConfig(body)["format"].(obj)
		assert.Equal(t, "json_schema", format["type"])
		assert.Equal(t, "object", format["schema"].(obj)["type"])
		assert.False(t, request.StripJSONFences)
	})
}

func TestTranslateResponse(t *testing.T) {
	t.Run("response blocks are read by type and tool_use becomes tool_calls", func(t *testing.T) {
		response, err := TranslateResponse(mustJSON(t, obj{
			"id": "msg_2", "stop_reason": "tool_use",
			"content": []any{
				obj{"type": "thinking", "thinking": "", "signature": "sig"},
				obj{"type": "text", "text": "Let me look."},
				obj{"type": "tool_use", "id": "toolu_1", "name": "search_vault", "input": obj{"q": "cats"}},
			},
			"usage": obj{"input_tokens": 100, "output_tokens": 20, "cache_read_input_tokens": 30, "cache_creation_input_tokens": 5},
		}), "claude-haiku-5-5", false)
		require.NoError(t, err)
		choice := replyContent(t, response)
		assert.Equal(t, "tool_calls", choice["finish_reason"])
		message := choice["message"].(obj)
		assert.Equal(t, "Let me look.", message["content"])
		call := message["tool_calls"].([]any)[0].(obj)
		assert.Equal(t, "toolu_1", call["id"])
		assert.Equal(t, "search_vault", call["function"].(obj)["name"])
		assert.Equal(t, `{"q":"cats"}`, call["function"].(obj)["arguments"])
		usage := onWire(t, response.OpenAI)["usage"].(obj)
		assert.EqualValues(t, 135, usage["prompt_tokens"], "cache read and creation count as prompt")
		assert.EqualValues(t, 20, usage["completion_tokens"])
	})
	t.Run("max_tokens maps to length", func(t *testing.T) {
		response, err := TranslateResponse(mustJSON(t, obj{"stop_reason": "max_tokens", "content": []any{obj{"type": "text", "text": "partial"}}}), "m", false)
		require.NoError(t, err)
		assert.Equal(t, "length", replyContent(t, response)["finish_reason"])
	})
	t.Run("a refusal fails over", func(t *testing.T) {
		_, err := TranslateResponse(mustJSON(t, obj{"stop_reason": "refusal", "content": []any{obj{"type": "text", "text": "I can't help"}}}), "m", false)
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
	})
	t.Run("thinking that used the whole budget fails over", func(t *testing.T) {
		_, err := TranslateResponse(mustJSON(t, obj{"stop_reason": "max_tokens", "content": []any{obj{"type": "thinking", "thinking": "", "signature": "sig"}}}), "m", false)
		requireTransient(t, err)
	})
}

func anthropicRecord(event, data string) string { return "event: " + event + "\ndata: " + data }

func TestProcessAnthropicRecord(t *testing.T) {
	run := func(records ...string) ([]Chunk, AnthropicStreamState, error) {
		var state AnthropicStreamState
		var chunks []Chunk
		for _, record := range records {
			if _, err := ProcessAnthropicRecord(record, &state, func(c Chunk) { chunks = append(chunks, c) }); err != nil {
				return chunks, state, err
			}
		}
		return chunks, state, nil
	}
	t.Run("stream skips thinking deltas, maps the stop reason and accumulates usage", func(t *testing.T) {
		chunks, state, err := run(
			anthropicRecord("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1200,"output_tokens":1,"cache_read_input_tokens":300}}}`),
			anthropicRecord("content_block_delta", `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hmm"}}`),
			anthropicRecord("content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}`),
			anthropicRecord("message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":42}}`),
		)
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "Hello"}, {FinishReason: "length"}}, chunks)
		assert.Equal(t, 1200, state.Usage.InputTokens)
		assert.Equal(t, 300, state.Usage.CacheReadInputTokens)
		assert.Equal(t, 42, state.Usage.OutputTokens)
	})
	t.Run("a refusal before any text fails the stream over", func(t *testing.T) {
		_, _, err := run(
			anthropicRecord("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10}}}`),
			anthropicRecord("message_delta", `{"type":"message_delta","delta":{"stop_reason":"refusal"}}`),
		)
		requireTransient(t, err)
	})
}

func TestManagedFailure(t *testing.T) {
	for _, status := range []int{401, 402, 403} {
		assert.Equal(t, KindCreditExhausted, ManagedFailure(status, []byte("{}")).Kind, status)
	}
	for _, message := range []string{"You have reached your specified API usage limits.", "Your credit balance is too low."} {
		body := `{"type":"error","error":{"type":"invalid_request_error","message":"` + message + `"}}`
		assert.Equal(t, KindCreditExhausted, ManagedFailure(400, []byte(body)).Kind, message)
	}
	for _, status := range []int{400, 404, 413, 429, 500, 529} {
		err := ManagedFailure(status, []byte(`{"error":{"message":"nope"}}`))
		assert.True(t, err.IsRecoverable(), status)
		assert.Equal(t, KindTransient, err.Kind, status)
	}
	permanent := Classify(Anthropic, 404, []byte("not_found"))
	assert.False(t, permanent.IsRecoverable())
	assert.True(t, RemapManaged(permanent).IsRecoverable())
}

func TestAnthropicCostEstimate(t *testing.T) {
	assert.Equal(t, int64(5000+5000), AnthropicUsage{InputTokens: 50_000, OutputTokens: 10000}.EstimatedCostUsdMicros())
	assert.Equal(t, int64(15000*5), AnthropicUsage{InputTokens: 150_000}.EstimatedCostUsdMicros())
	assert.Equal(t, int64(600_000), CatalogCost(ProviderID(Anthropic), "claude-haiku-5-5", 1_000_000, 1_000_000, nil))
}

// wireStub records what reached the wire. An httptest server stands in for
// Swift's URLProtocol stub, and the keys are test strings.
type wireStub struct {
	mu       sync.Mutex
	requests []capturedRequest
	status   int
	reply    string
	server   *httptest.Server
}

type capturedRequest struct {
	path   string
	header http.Header
	raw    []byte
	body   obj
}

func newWireStub(t *testing.T, status int, reply string) *wireStub {
	t.Helper()
	stub := &wireStub{status: status, reply: reply}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body obj
		_ = json.Unmarshal(raw, &body)
		stub.mu.Lock()
		stub.requests = append(stub.requests, capturedRequest{r.URL.Path, r.Header.Clone(), raw, body})
		stub.mu.Unlock()
		w.WriteHeader(stub.status)
		_, _ = io.WriteString(w, stub.reply)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *wireStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *wireStub) last(t *testing.T) capturedRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotEmpty(t, s.requests)
	return s.requests[len(s.requests)-1]
}

func (s *wireStub) url(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse(s.server.URL)
	require.NoError(t, err)
	return u
}

const (
	anthropicOK      = `{"id":"msg_1","stop_reason":"end_turn","content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":5,"output_tokens":2}}`
	anthropicPayload = `{"model":"claude-haiku-5-5","temperature":0.4,"top_p":0.9,"top_k":40,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"prefill"}]}`
)

var trialForDispatch = TrialConfig{APIKey: "TRIAL-KEY", Model: TrialDefaultModel, Effort: "low", Thinking: ThinkingAdaptive, Scope: ScopeAll}

func managedCtx() context.Context {
	return WithValues(context.Background(), func(v *Values) { v.CredentialMode = ModeManaged })
}

// AnthropicAdapterManagedDispatchTests: the adapter on the wire.
func TestAnthropicDispatch(t *testing.T) {
	adapter := func(stub *wireStub, apiKey string, trial *TrialConfig) *AnthropicAdapter {
		return &AnthropicAdapter{APIKey: apiKey, BaseURL: stub.url(t), Trial: trial}
	}
	t.Run("managed requests spend the trial key with the trial shape", func(t *testing.T) {
		stub := newWireStub(t, 200, anthropicOK)
		meta, err := adapter(stub, "", &trialForDispatch).ChatCompletions(managedCtx(), []byte(anthropicPayload), "k", "")
		require.NoError(t, err)
		sent := stub.last(t)
		assert.Equal(t, "/v1/messages", sent.path)
		assert.Equal(t, "TRIAL-KEY", sent.header.Get("x-api-key"))
		assert.Equal(t, "2023-06-01", sent.header.Get("anthropic-version"))
		assert.Equal(t, "claude-haiku-5-5", sent.body["model"])
		assert.Equal(t, "low", outputConfig(sent.body)["effort"])
		var reply obj
		require.NoError(t, json.Unmarshal(meta.Data, &reply))
		assert.EqualValues(t, 5, reply["usage"].(obj)["prompt_tokens"])
	})
	t.Run("sampling fields and prefill are absent from the bytes on the wire", func(t *testing.T) {
		// Haiku 5.5 answers 400 to any of these, on both protocols.
		for _, stream := range []bool{false, true} {
			stub := newWireStub(t, 200, anthropicOK)
			a := adapter(stub, "", &trialForDispatch)
			if stream {
				stub.reply = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				_, err := collect(a.ChatStream(managedCtx(), []byte(anthropicPayload), "k", ""))
				require.NoError(t, err)
			} else {
				_, err := a.ChatCompletions(managedCtx(), []byte(anthropicPayload), "k", "")
				require.NoError(t, err)
			}
			sent := stub.last(t)
			for _, field := range []string{"temperature", "top_p", "top_k"} {
				assert.NotContains(t, sent.body, field, "stream=%v", stream)
				assert.NotContains(t, string(sent.raw), `"`+field+`"`, "stream=%v", stream)
			}
			turns := turnsOf(sent.body)
			assert.Equal(t, "user", turns[len(turns)-1]["role"], "the transcript never ends on an assistant turn")
			assert.Equal(t, stream, sent.body["stream"] == true)
		}
	})
	t.Run("without the trial the request is just as clean", func(t *testing.T) {
		stub := newWireStub(t, 200, anthropicOK)
		a := adapter(stub, "REGISTRY-KEY", nil)
		_, err := a.ChatCompletions(managedCtx(), []byte(anthropicPayload), "k", "")
		require.NoError(t, err)
		sent := stub.last(t)
		assert.Equal(t, "REGISTRY-KEY", sent.header.Get("x-api-key"))
		for _, field := range []string{"temperature", "top_p", "top_k", "thinking", "output_config"} {
			assert.NotContains(t, sent.body, field)
		}
	})
	t.Run("an overloaded 529 on the managed path is transient", func(t *testing.T) {
		stub := newWireStub(t, 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
		_, err := adapter(stub, "", &trialForDispatch).ChatCompletions(managedCtx(), []byte(anthropicPayload), "k", "")
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, KindTransient, perr.Kind)
		assert.Equal(t, 529, perr.Status)
	})
	t.Run("a 400 without the trial keeps the shared classification", func(t *testing.T) {
		stub := newWireStub(t, 400, `{"error":{"message":"bad"}}`)
		_, err := adapter(stub, "REGISTRY-KEY", nil).ChatCompletions(managedCtx(), []byte(anthropicPayload), "k", "")
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.False(t, perr.IsRecoverable())
	})
	t.Run("the trial key is never spent on another model", func(t *testing.T) {
		stub := newWireStub(t, 200, anthropicOK)
		opus := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
		_, err := adapter(stub, "", &trialForDispatch).ChatCompletions(managedCtx(), opus, "k", "")
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, KindPermanent, perr.Kind)
		assert.Equal(t, 401, perr.Status)
		assert.Zero(t, stub.count(), "nothing reached the wire")
	})
	t.Run("byok never spends the trial key", func(t *testing.T) {
		stub := newWireStub(t, 200, anthropicOK)
		ctx := WithValues(context.Background(), func(v *Values) { v.CredentialMode = ModeBYOK })
		_, err := adapter(stub, "", &trialForDispatch).ChatCompletions(ctx, []byte(anthropicPayload), "k", "")
		require.ErrorAs(t, err, &BYOKKeysRequiredError{})
		assert.Zero(t, stub.count())
	})
	t.Run("an anthropic failure hands the request to the original pick", func(t *testing.T) {
		stub := newWireStub(t, 404, `{"type":"error","error":{"type":"not_found_error","message":"model: claude-haiku-5-5"}}`)
		fallback := &scriptedAdapter{kind: OpenRouter, outcomes: []outcome{{data: "FALLBACK"}}}
		transport := &Transport{
			Registry: NewRegistry(nil, adapter(stub, "", &trialForDispatch), fallback),
			Router: fixedRouter{Decision{
				Primary:        trialForDispatch.Route(),
				Fallbacks:      []Route{{OpenRouter, "deepseek/deepseek-v4-flash"}},
				CredentialMode: ModeManaged,
			}},
		}
		result, err := transport.ChatCompletions(context.Background(), []byte(anthropicPayload), "k", "")
		require.NoError(t, err)
		assert.Equal(t, "FALLBACK", string(result.Data))
		assert.Equal(t, 1, stub.count(), "anthropic was tried first")
	})
}
