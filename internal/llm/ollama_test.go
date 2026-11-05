package llm

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOllama(t *testing.T) {
	const reply = `{"message":{"role":"assistant","content":"hey"},"prompt_eval_count":3,"eval_count":4,"done_reason":"length"}`
	// Part of BYOKFailClosedTests: nothing reaches the deployment's own host.
	t.Run("ollama byok with no configured endpoint throws rather than using the deployment host", func(t *testing.T) {
		deployment := newWireStub(t, 200, reply)
		a := &OllamaAdapter{DefaultBaseURL: deployment.url(t)}
		_, err := a.ChatCompletions(modeCtx(ModeBYOK, nil), []byte(chatPayload), "k", "")
		assert.True(t, isByokRequired(err))
		assert.Zero(t, deployment.count())

		key := "ignored"
		a.Credentials = stubCredentials{creds: &ResolvedCredential{APIKey: &key}}
		_, err = collect(a.ChatStream(modeCtx(ModeBYOK, &User{ID: uuid.New()}), []byte(chatPayload), "k", ""))
		assert.True(t, isByokRequired(err), "a key is not an endpoint")
		assert.Zero(t, deployment.count())
	})
	t.Run("byok goes to the tenant's own host", func(t *testing.T) {
		deployment, own := newWireStub(t, 200, reply), newWireStub(t, 200, reply)
		a := &OllamaAdapter{DefaultBaseURL: deployment.url(t), Credentials: stubCredentials{creds: &ResolvedCredential{BaseURL: own.url(t)}}}
		_, err := a.ChatCompletions(modeCtx(ModeBYOK, &User{ID: uuid.New()}), []byte(chatPayload), "k", "")
		require.NoError(t, err)
		assert.Zero(t, deployment.count())
		assert.Equal(t, "/api/chat", own.last(t).path)
	})
	t.Run("managed keeps the default, the same messages and stream false", func(t *testing.T) {
		host := newWireStub(t, 200, reply)
		meta, err := (&OllamaAdapter{DefaultBaseURL: host.url(t)}).ChatCompletions(modeCtx(ModeManaged, nil), []byte(`{"messages":[{"role":"system","content":"s"}],"temperature":0.5}`), "k", "")
		require.NoError(t, err)
		sent := host.last(t)
		assert.Equal(t, obj{"model": "llama3.1", "messages": []any{obj{"role": "system", "content": "s"}}, "stream": false}, sent.body)

		var got obj
		require.NoError(t, json.Unmarshal(meta.Data, &got))
		assert.True(t, strings.HasPrefix(got["id"].(string), "ollama-"))
		assert.Equal(t, strings.ToUpper(got["id"].(string)[7:]), got["id"].(string)[7:])
		assert.Equal(t, "llama3.1", got["model"])
		choice := got["choices"].([]any)[0].(obj)
		assert.Equal(t, "length", choice["finish_reason"])
		assert.Equal(t, obj{"role": "assistant", "content": "hey"}, choice["message"])
		assert.Equal(t, obj{"prompt_tokens": 3.0, "completion_tokens": 4.0, "total_tokens": 7.0}, got["usage"])
	})
	t.Run("a bad payload is a permanent 400", func(t *testing.T) {
		_, err := (&OllamaAdapter{DefaultBaseURL: mustParseURL("http://x.test")}).ChatCompletions(modeCtx(ModeManaged, nil), []byte(`{"messages":[1]}`), "k", "")
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, KindPermanent, perr.Kind)
		assert.Equal(t, 400, perr.Status)
	})
	t.Run("QUIRK: an unparseable 2xx body comes back as a success with an error field", func(t *testing.T) {
		host := newWireStub(t, 200, `nope`)
		meta, err := (&OllamaAdapter{DefaultBaseURL: host.url(t)}).ChatCompletions(modeCtx(ModeManaged, nil), []byte(chatPayload), "k", "")
		require.NoError(t, err)
		assert.JSONEq(t, `{"error":"unparseable ollama response"}`, string(meta.Data))
	})
	t.Run("missing counts and a missing reason default", func(t *testing.T) {
		got := onWire(t, translateOllamaResponse([]byte(`{"prompt_eval_count":1.5}`), "m"))
		assert.Equal(t, obj{"prompt_tokens": 0.0, "completion_tokens": 0.0, "total_tokens": 0.0}, got["usage"])
		assert.Equal(t, "stop", got["choices"].([]any)[0].(obj)["finish_reason"])
	})
	t.Run("an upstream failure is classified", func(t *testing.T) {
		host := newWireStub(t, 500, `boom`)
		_, err := (&OllamaAdapter{DefaultBaseURL: host.url(t)}).ChatCompletions(modeCtx(ModeManaged, nil), []byte(chatPayload), "k", "")
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, 500, perr.Status)
	})
}

func TestOllamaStream(t *testing.T) {
	run := func(t *testing.T, reply string) ([]Chunk, capturedRequest, error) {
		t.Helper()
		host := newWireStub(t, 200, reply)
		base, err := url.Parse(host.server.URL)
		require.NoError(t, err)
		chunks, streamErr := collect((&OllamaAdapter{DefaultBaseURL: base}).ChatStream(modeCtx(ModeManaged, nil), []byte(`{"model":"m","messages":[]}`), "k", ""))
		return chunks, host.last(t), streamErr
	}
	t.Run("NDJSON deltas, then done carries the reason and stops the read", func(t *testing.T) {
		chunks, sent, err := run(t, `{"message":{"content":"a"},"done":false}`+"\n"+`{"message":{"content":""},"done":false}`+"\n\n"+
			`not json`+"\n"+`{"message":{"content":"b"},"done":true,"done_reason":"length"}`+"\n"+`{"message":{"content":"late"}}`+"\n")
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "a"}, {Delta: "b", FinishReason: "length"}}, chunks)
		assert.Equal(t, obj{"model": "m", "messages": []any{}, "stream": true}, sent.body)
		assert.Equal(t, "/api/chat", sent.path)
	})
	t.Run("done without a reason finishes with stop and a trailing line needs no newline", func(t *testing.T) {
		chunks, _, err := run(t, `{"done":true}`)
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{FinishReason: "stop"}}, chunks)
	})
	t.Run("an error line is transient", func(t *testing.T) {
		_, _, err := run(t, `{"error":"model not loaded"}`+"\n")
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, KindTransient, perr.Kind)
		assert.Contains(t, err.Error(), "model not loaded")
	})
}
