package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const geminiFixtureKey = "gemini-stub-key"

// geminiStub records the raw URL, which wireStub does not keep: Gemini's key is in
// the query string.
type geminiStub struct {
	server *httptest.Server
	paths  []string
	bodies []obj
	status int
	reply  string
}

func newGeminiStub(t *testing.T, status int, reply string) *geminiStub {
	t.Helper()
	stub := &geminiStub{status: status, reply: reply}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body obj
		_ = json.Unmarshal(raw, &body)
		stub.paths = append(stub.paths, r.URL.RequestURI())
		stub.bodies = append(stub.bodies, body)
		w.WriteHeader(stub.status)
		_, _ = io.WriteString(w, stub.reply)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *geminiStub) adapter(t *testing.T, source CredentialSource) *GeminiContentsAdapter {
	t.Helper()
	base, err := url.Parse(s.server.URL)
	require.NoError(t, err)
	return &GeminiContentsAdapter{APIKey: geminiFixtureKey, BaseURL: base, Credentials: source}
}

func geminiTranslate(t *testing.T, payload string) obj {
	t.Helper()
	openAI, messages, ok := parseOpenAIMessages([]byte(payload))
	require.True(t, ok)
	return onWire(t, translateToGemini(openAI, messages))
}

// Part of BYOKFailClosedTests, and ProviderStreamKitTests' Gemini case.
func TestGeminiFailClosed(t *testing.T) {
	t.Run("gemini byok with no resolvable credential throws and spends nothing", func(t *testing.T) {
		stub := newGeminiStub(t, 200, `{}`)
		_, err := stub.adapter(t, nil).ChatCompletions(modeCtx(ModeBYOK, nil), []byte(chatPayload), "k", "")
		assert.True(t, isByokRequired(err))
		assert.Empty(t, stub.paths)
	})
	t.Run("a stream fails closed the same way", func(t *testing.T) {
		stub := newGeminiStub(t, 200, ``)
		_, err := collect(stub.adapter(t, nil).ChatStream(modeCtx(ModeBYOK, nil), []byte(chatPayload), "k", ""))
		assert.True(t, isByokRequired(err))
		assert.Empty(t, stub.paths)
	})
	t.Run("an unreadable credential fails closed", func(t *testing.T) {
		stub := newGeminiStub(t, 200, `{}`)
		_, err := stub.adapter(t, stubCredentials{err: errors.New("decrypt")}).ChatCompletions(modeCtx(ModeBYOK, &User{ID: uuid.New()}), []byte(chatPayload), "k", "")
		assert.True(t, isByokRequired(err))
		assert.Empty(t, stub.paths)
	})
	t.Run("gemini stream object extracts delta and finish", func(t *testing.T) {
		delta, finish, ok := parseGeminiStreamObject([]byte(`{"candidates":[{"content":{"parts":[{"text":"yo"}]},"finishReason":"STOP"}]}`))
		require.True(t, ok)
		assert.Equal(t, "yo", delta)
		require.NotNil(t, finish)
		assert.Equal(t, "STOP", *finish)
	})
}

func TestGeminiDispatch(t *testing.T) {
	reply := `{"candidates":[{"content":{"parts":[{"text":"hi"},{"text":" there"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"totalTokenCount":7}}`
	t.Run("managed spends the platform key on the generateContent path", func(t *testing.T) {
		stub := newGeminiStub(t, 200, reply)
		meta, err := stub.adapter(t, nil).ChatCompletions(modeCtx(ModeManaged, nil), []byte(`{"model":"gemini-flash","messages":[{"role":"user","content":"hi"}]}`), "k", "")
		require.NoError(t, err)
		assert.Equal(t, "/v1beta/models/gemini-2.5-flash:generateContent?key="+geminiFixtureKey, stub.paths[0])

		var got obj
		require.NoError(t, json.Unmarshal(meta.Data, &got))
		assert.Equal(t, "gemini-flash", got["model"], "the requested model, not the resolved one")
		assert.Equal(t, "chat.completion", got["object"])
		assert.Equal(t, strings.ToUpper(got["id"].(string)), got["id"], "uuidString is uppercase")
		choice := got["choices"].([]any)[0].(obj)
		assert.Equal(t, "length", choice["finish_reason"])
		assert.Equal(t, obj{"role": "assistant", "content": "hi there"}, choice["message"])
		assert.Equal(t, obj{"prompt_tokens": 3.0, "completion_tokens": 4.0, "total_tokens": 7.0}, got["usage"])
	})
	t.Run("byok spends the tenant's key, never the platform's", func(t *testing.T) {
		stub := newGeminiStub(t, 200, reply)
		key := "tenant-key"
		_, err := stub.adapter(t, stubCredentials{creds: &ResolvedCredential{APIKey: &key}}).ChatCompletions(modeCtx(ModeBYOK, &User{ID: uuid.New()}), []byte(chatPayload), "k", "")
		require.NoError(t, err)
		assert.Contains(t, stub.paths[0], "key=tenant-key")
		assert.NotContains(t, stub.paths[0], geminiFixtureKey)
	})
	t.Run("a bad payload is a 400 before any key is touched", func(t *testing.T) {
		stub := newGeminiStub(t, 200, reply)
		_, err := stub.adapter(t, nil).ChatCompletions(modeCtx(ModeBYOK, nil), []byte(`{"messages":"nope"}`), "k", "")
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, KindPermanent, perr.Kind)
		assert.Equal(t, 400, perr.Status)
	})
	t.Run("an upstream failure is classified", func(t *testing.T) {
		stub := newGeminiStub(t, 429, `{"error":{"message":"slow down"}}`)
		_, err := stub.adapter(t, nil).ChatCompletions(modeCtx(ModeManaged, nil), []byte(chatPayload), "k", "")
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, 429, perr.Status)
	})
	t.Run("an unparseable 2xx body becomes an assistant error, not a failure", func(t *testing.T) {
		stub := newGeminiStub(t, 200, `not json`)
		meta, err := stub.adapter(t, nil).ChatCompletions(modeCtx(ModeManaged, nil), []byte(chatPayload), "k", "")
		require.NoError(t, err)
		assert.Equal(t, "Error: invalid gemini response body", ExtractContent(meta.Data))
	})
	t.Run("the key never reaches a transport error", func(t *testing.T) {
		stub := newGeminiStub(t, 200, reply)
		a := stub.adapter(t, nil)
		a.Client = &http.Client{Transport: refusingTransport{}}

		_, err := a.ChatCompletions(modeCtx(ModeManaged, nil), []byte(chatPayload), "k", "")
		require.Error(t, err)
		assert.NotContains(t, err.Error(), geminiFixtureKey)
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, KindNetwork, perr.Kind)

		_, err = collect(a.ChatStream(modeCtx(ModeManaged, nil), []byte(chatPayload), "k", ""))
		require.Error(t, err)
		assert.NotContains(t, err.Error(), geminiFixtureKey)
	})
	t.Run("redaction keeps timeout detection and leaves other errors alone", func(t *testing.T) {
		timeout := &url.Error{Op: "Post", URL: "https://x.test/a?key=SECRET", Err: context.DeadlineExceeded}
		redacted := redactURLError(timeout)
		assert.NotContains(t, redacted.Error(), "SECRET")
		assert.Contains(t, redacted.Error(), "https://x.test/a")
		assert.Equal(t, netTimeout, networkKindOf(redacted))
		plain := errors.New("plain")
		assert.Equal(t, plain, redactURLError(plain))
		noQuery := &url.Error{Op: "Post", URL: "https://x.test/a", Err: plain}
		assert.Equal(t, error(noQuery), redactURLError(noQuery))
	})
}

type refusingTransport struct{}

func (refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

func TestGeminiStream(t *testing.T) {
	t.Run("alt=sse frames become chunks and finishReason ends the stream", func(t *testing.T) {
		stub := newGeminiStub(t, 200, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"he\"},{\"text\":\"l\"}]}}]}\n\n"+
			"data: {\"usageMetadata\":{}}\n\n"+
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"lo\"}]},\"finishReason\":\"SAFETY\"}]}\n\n"+
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"late\"}]}}]}\n\n")
		chunks, err := collect(stub.adapter(t, nil).ChatStream(modeCtx(ModeManaged, nil), []byte(`{"model":"other","messages":[{"role":"user","content":"hi"}]}`), "k", ""))
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "hel"}, {Delta: "lo", FinishReason: "content_filter"}}, chunks)
		assert.Equal(t, "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse&key="+geminiFixtureKey, stub.paths[0])
	})
	t.Run("a stream resolves the key before validating the payload", func(t *testing.T) {
		stub := newGeminiStub(t, 200, ``)
		_, err := collect(stub.adapter(t, nil).ChatStream(modeCtx(ModeBYOK, nil), []byte(`not json`), "k", ""))
		assert.True(t, isByokRequired(err))
		_, err = collect(stub.adapter(t, nil).ChatStream(modeCtx(ModeManaged, nil), []byte(`not json`), "k", ""))
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, 400, perr.Status)
	})
}

func TestGeminiModelResolution(t *testing.T) {
	for raw, want := range map[string]string{
		"gemini-pro":          "gemini-2.5-pro",
		"GEMINI-PRO-LATEST":   "gemini-2.5-pro",
		"gemini-2.5-pro":      "gemini-2.5-pro",
		"gemini-flash":        "gemini-2.5-flash",
		"gemini-flash-latest": "gemini-2.5-flash",
		"Gemini-3-Exp":        "Gemini-3-Exp",
		"claude-opus":         "gemini-2.5-pro",
		"":                    "gemini-2.5-pro",
	} {
		assert.Equal(t, want, resolveGeminiModel(raw), raw)
	}
}

func TestGeminiRequestTranslation(t *testing.T) {
	t.Run("the last system message wins, unknown roles send as user", func(t *testing.T) {
		body := geminiTranslate(t, `{"messages":[
			{"role":"system","content":"first"},{"role":"system","content":"second"},
			{"role":"user","content":"u"},{"role":"assistant","content":"a"},{"role":"developer","content":"d"},{"content":"no role"}]}`)
		assert.Equal(t, obj{"parts": []any{obj{"text": "second"}}}, body["system_instruction"])
		assert.Equal(t, []any{
			obj{"role": "user", "parts": []any{obj{"text": "u"}}},
			obj{"role": "model", "parts": []any{obj{"text": "a"}}},
			obj{"role": "user", "parts": []any{obj{"text": "d"}}},
		}, body["contents"])
	})
	t.Run("an empty content sends empty parts, and no contents key without messages", func(t *testing.T) {
		body := geminiTranslate(t, `{"messages":[{"role":"user","content":""}]}`)
		assert.Equal(t, []any{obj{"role": "user", "parts": []any{}}}, body["contents"])
		assert.NotContains(t, geminiTranslate(t, `{"messages":[]}`), "contents")
	})
	t.Run("QUIRK: a tool call part is bare, not wrapped in functionCall", func(t *testing.T) {
		body := geminiTranslate(t, `{"messages":[{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{\"a\":1}"}},{"id":"c2","function":{"name":"g","arguments":"not json"}},{"function":{"name":"skipped"}}]}]}`)
		assert.Equal(t, []any{obj{"role": "model", "parts": []any{
			obj{"text": ""},
			obj{"name": "f", "args": obj{"a": 1.0}, "_callId": "c1"},
			obj{"name": "g", "args": obj{}, "_callId": "c2"},
		}}}, body["contents"])
	})
	t.Run("a null content is present, so no empty text part is added", func(t *testing.T) {
		body := geminiTranslate(t, `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c1","function":{"name":"f"}}]}]}`)
		assert.Equal(t, []any{obj{"role": "model", "parts": []any{obj{"name": "f", "args": obj{}, "_callId": "c1"}}}}, body["contents"])
	})
	t.Run("tool results become functionResponse parts", func(t *testing.T) {
		body := geminiTranslate(t, `{"messages":[
			{"role":"tool","name":"plain","content":"text"},
			{"role":"tool","name":"json","content":"{\"x\":1}"},
			{"role":"tool","name":"array","content":"[1]"},
			{"role":"tool","name":"nothing"},
			{"role":"tool","content":"nameless"}]}`)
		response := func(name string, r obj) obj {
			return obj{"role": "user", "parts": []any{obj{"functionResponse": obj{"name": name, "response": r}}}}
		}
		assert.Equal(t, []any{
			response("plain", obj{"content": "text"}),
			response("json", obj{"x": 1.0}),
			response("array", obj{"content": "[1]"}),
			response("nothing", obj{}),
		}, body["contents"])
	})
	t.Run("all declarations share one tool entry", func(t *testing.T) {
		body := geminiTranslate(t, `{"messages":[],"tools":[
			{"type":"function","function":{"name":"a","description":"d","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"b"}},{"type":"other"}],"tool_choice":"required"}`)
		assert.Equal(t, []any{obj{"function_declarations": []any{
			obj{"name": "a", "description": "d", "parameters": obj{"type": "object"}},
			obj{"name": "b", "description": "", "parameters": obj{}},
		}}}, body["tools"])
		assert.Equal(t, obj{"function_calling_config": obj{"mode": "ANY"}}, body["tool_config"])
	})
	t.Run("tool_choice mapping", func(t *testing.T) {
		tools := `"tools":[{"function":{"name":"a"}}]`
		mode := func(choice string) any {
			return geminiTranslate(t, `{"messages":[],`+tools+`,"tool_choice":`+choice+`}`)["tool_config"]
		}
		assert.Equal(t, obj{"function_calling_config": obj{"mode": "NONE"}}, mode(`"NONE"`))
		assert.Equal(t, obj{"function_calling_config": obj{"mode": "ANY"}}, mode(`"any"`))
		assert.Equal(t, obj{"function_calling_config": obj{"mode": "AUTO"}}, mode(`"auto"`))
		assert.Equal(t, obj{"function_calling_config": obj{"mode": "AUTO"}}, mode(`null`))
		assert.Equal(t, obj{"function_calling_config": obj{"mode": "ANY", "allowed_function_names": []any{"a"}}}, mode(`{"function":{"name":"a"}}`))
		assert.NotContains(t, geminiTranslate(t, `{"messages":[],"tool_choice":"required"}`), "tool_config", "no tools, no config")
	})
	t.Run("generation config, and response_format is deliberately dropped", func(t *testing.T) {
		body := geminiTranslate(t, `{"messages":[],"temperature":0.7,"max_tokens":100,"top_p":1,"stop":["x"],"response_format":{"type":"json_object"}}`)
		assert.Equal(t, obj{"temperature": 0.7, "maxOutputTokens": 100.0, "topP": 1.0, "stopSequences": []any{"x"}}, body["generationConfig"])
		assert.NotContains(t, geminiTranslate(t, `{"messages":[],"stop":"x","max_tokens":1.5}`), "generationConfig")
	})
}

func TestGeminiResponseTranslation(t *testing.T) {
	choiceOf := func(t *testing.T, body string) obj {
		t.Helper()
		got := onWire(t, translateFromGemini([]byte(body), "m"))
		return got["choices"].([]any)[0].(obj)
	}
	t.Run("functionCall becomes a tool call and content is left out", func(t *testing.T) {
		choice := choiceOf(t, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{"b/c":1},"_callId":"c9"}}]}}]}`)
		assert.Equal(t, "stop", choice["finish_reason"], "a missing finishReason reads as STOP")
		message := choice["message"].(obj)
		assert.NotContains(t, message, "content")
		call := message["tool_calls"].([]any)[0].(obj)
		assert.Equal(t, "c9", call["id"])
		assert.Equal(t, `{"b\/c":1}`, call["function"].(obj)["arguments"], "JSONSerialization escapes slashes")
	})
	t.Run("a call without an id gets an uppercase dashless uuid", func(t *testing.T) {
		choice := choiceOf(t, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f"}}]}}]}`)
		call := choice["message"].(obj)["tool_calls"].([]any)[0].(obj)
		id := call["id"].(string)
		assert.Len(t, id, 32)
		assert.Equal(t, strings.ToUpper(id), id)
		assert.Equal(t, "{}", call["function"].(obj)["arguments"])
	})
	t.Run("an empty reply keeps an empty content", func(t *testing.T) {
		assert.Equal(t, obj{"role": "assistant", "content": ""}, choiceOf(t, `{"candidates":[{}]}`)["message"])
	})
	t.Run("a safety block is annotated with its blocked categories", func(t *testing.T) {
		choice := choiceOf(t, `{"candidates":[{"finishReason":"SAFETY","safetyRatings":[{"category":"A","blocked":true},{"category":"B","blocked":false},{"category":"C","blocked":true}]}]}`)
		assert.Equal(t, "content_filter", choice["finish_reason"])
		assert.Equal(t, "\n[Blocked: A, C]", choice["message"].(obj)["content"])
	})
	t.Run("finish reasons map to the OpenAI vocabulary", func(t *testing.T) {
		for reason, want := range map[string]string{
			"STOP": "stop", "MAX_TOKENS": "length", "SAFETY": "content_filter", "BLOCKLIST": "content_filter",
			"PROHIBITED_CONTENT": "content_filter", "RECITATION": "content_filter", "OTHER": "stop", "NEW": "stop",
		} {
			assert.Equal(t, want, mapGeminiFinishReason(reason), reason)
		}
	})
	t.Run("no candidates is an empty choices array and empty usage", func(t *testing.T) {
		got := onWire(t, translateFromGemini([]byte(`{}`), "m"))
		assert.Equal(t, []any{}, got["choices"])
		assert.Equal(t, obj{}, got["usage"])
	})
}
