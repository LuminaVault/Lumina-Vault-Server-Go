package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingTransport is a ChatTransport that records its payloads and answers
// with a canned body, error or stream. It is not a DecidedStreamTransport.
type recordingTransport struct {
	answer   []byte
	err      error
	payloads [][]byte
	ctxs     []context.Context
	chunks   []Chunk
}

func (r *recordingTransport) ChatCompletions(ctx context.Context, payload []byte, _, _ string) (Metadata, error) {
	r.payloads = append(r.payloads, payload)
	r.ctxs = append(r.ctxs, ctx)
	return Metadata{Data: r.answer}, r.err
}

func (r *recordingTransport) ChatStream(ctx context.Context, payload []byte, _, _ string) Stream {
	r.payloads = append(r.payloads, payload)
	r.ctxs = append(r.ctxs, ctx)
	return func(yield func(Chunk, error) bool) {
		for _, c := range r.chunks {
			if !yield(c, nil) {
				return
			}
		}
	}
}

// decidedTransport adds ChatStreamDecided.
type decidedTransport struct {
	recordingTransport
	decided []Decision
}

func (d *decidedTransport) ChatStreamDecided(ctx context.Context, payload []byte, sessionKey, sessionID string, decision Decision) Stream {
	d.decided = append(d.decided, decision)
	return d.ChatStream(ctx, payload, sessionKey, sessionID)
}

// recordingFallback is the managed gateway stream.
type recordingFallback struct {
	requests []ChatRequest
}

func (f *recordingFallback) ChatStream(_ context.Context, _, _ string, req ChatRequest) Stream {
	f.requests = append(f.requests, req)
	return func(yield func(Chunk, error) bool) { yield(Chunk{Delta: "gateway"}, nil) }
}

const upstreamAnswer = `{"id":"cmpl-1","object":"chat.completion","created":7,"model":"deepseek/deepseek-v4-flash",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"Done.\n/usr/bin/bash: line 1: pip: command not found"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5},"system_fingerprint":"dropped"}`

// Ports RoutedHermesLLMService (no Swift suite of its own; LLMControllerPushSideEffectTests
// drive it through the route, which group 5C owns). Disclosure cases follow
// ModelDisclosurePolicyTests.
func TestRoutedService(t *testing.T) {
	tenant := uuid.New()
	byok := stubPreferences{pref: &Preference{Mode: ModeBYOK}}

	t.Run("managed tenant gets the guard and a scrubbed model", func(t *testing.T) {
		tr := &recordingTransport{answer: []byte(upstreamAnswer)}
		s := &RoutedService{Transport: tr, DefaultModel: "hermes-3"}
		resp, err := s.Chat(t.Context(), tenant.String(), "", CompletionRequest{Messages: []CompletionMessage{{Role: "user", Content: "hi"}}})
		require.NoError(t, err)
		require.Len(t, tr.payloads, 1)
		assert.JSONEq(t, `{"model":"hermes-3","messages":[{"role":"system","content":`+jsonString(SystemPromptGuard)+`},{"role":"user","content":"hi"}],"stream":false}`,
			string(tr.payloads[0]))
		assert.Equal(t, "cmpl-1", resp.ID)
		assert.Equal(t, GenericModelID, resp.Model)
		assert.Equal(t, GenericModelID, resp.Raw.Model)
		assert.Equal(t, "Done.", resp.Message.Content, "stderr is sanitized")
		assert.Contains(t, resp.Raw.Choices[0].Message.Content, "/usr/bin/bash", "raw keeps the upstream content")
		raw, err := json.Marshal(resp)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "system_fingerprint", "raw is the decoded Swift struct")
		assert.NotContains(t, string(raw), "deepseek")
	})
	t.Run("BYOK tenant sees the model and gets no guard", func(t *testing.T) {
		tr := &recordingTransport{answer: []byte(upstreamAnswer)}
		temp := 0.4
		s := &RoutedService{Transport: tr, DefaultModel: "hermes-3", Preferences: byok}
		resp, err := s.Chat(t.Context(), tenant.String(), "conv", CompletionRequest{
			Model: "gpt-4o", Temperature: &temp,
			Messages:   []CompletionMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: ""}},
			Tools:      []ChatTool{{Type: "function", Function: ChatToolDefinition{Name: "search"}}},
			ToolChoice: json.RawMessage(`"auto"`),
		})
		require.NoError(t, err)
		assert.JSONEq(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"},{"role":"assistant"}],"temperature":0.4,"stream":false,`+
			`"tools":[{"type":"function","function":{"name":"search"}}],"tool_choice":"auto"}`, string(tr.payloads[0]))
		assert.Equal(t, "deepseek/deepseek-v4-flash", resp.Model)
	})
	t.Run("unparsable session key is hidden", func(t *testing.T) {
		tr := &recordingTransport{answer: []byte(upstreamAnswer)}
		s := &RoutedService{Transport: tr, Preferences: byok}
		resp, err := s.Chat(t.Context(), "not-a-uuid", "", CompletionRequest{})
		require.NoError(t, err)
		assert.Equal(t, GenericModelID, resp.Model)
	})
	t.Run("no choices is a 502", func(t *testing.T) {
		tr := &recordingTransport{answer: []byte(`{"id":"x","model":"m","choices":[]}`)}
		_, err := (&RoutedService{Transport: tr}).Chat(t.Context(), tenant.String(), "", CompletionRequest{})
		var chatErr *ChatError
		require.ErrorAs(t, err, &chatErr)
		assert.Equal(t, http.StatusBadGateway, chatErr.Status)
		assert.Equal(t, "llm upstream returned no choices", chatErr.Message)
	})
	t.Run("null content fails to decode, as Swift's non-optional content does", func(t *testing.T) {
		tr := &recordingTransport{answer: []byte(`{"id":"x","model":"m","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[]}}]}`)}
		_, err := (&RoutedService{Transport: tr}).Chat(t.Context(), tenant.String(), "", CompletionRequest{})
		var chatErr *ChatError
		require.ErrorAs(t, err, &chatErr)
		assert.Equal(t, http.StatusBadGateway, chatErr.Status)
		assert.Contains(t, chatErr.Message, "llm upstream error: ")
	})
	t.Run("user-facing errors pass through", func(t *testing.T) {
		tr := &recordingTransport{err: &UsageCapExceededError{}}
		_, err := (&RoutedService{Transport: tr}).Chat(t.Context(), tenant.String(), "", CompletionRequest{})
		var capErr *UsageCapExceededError
		assert.ErrorAs(t, err, &capErr)
	})
	t.Run("other errors become a 502", func(t *testing.T) {
		tr := &recordingTransport{err: errors.New("boom")}
		_, err := (&RoutedService{Transport: tr}).Chat(t.Context(), tenant.String(), "", CompletionRequest{})
		var chatErr *ChatError
		require.ErrorAs(t, err, &chatErr)
		assert.Equal(t, "llm upstream error: boom", chatErr.Message)
	})
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// RoutedHermesLLMStreamService's decision order: parallel strategy, BYOK pinned,
// managed Auto, free lane (fail closed), gateway fallback. auditC_stream_test
// covers the BYOK and free-lane execution paths through the real transport.
func TestRoutedStreamServiceOrder(t *testing.T) {
	tenant := uuid.New()
	req := ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "twelve chars"}}}

	t.Run("parallel strategy goes to the transport as router-auto, before BYOK", func(t *testing.T) {
		tr := &recordingTransport{chunks: []Chunk{{Delta: "p"}}}
		s := &RoutedStreamService{
			Fallback: &recordingFallback{}, Transport: tr,
			Preferences: stubPreferences{pref: &Preference{Mode: ModeBYOK, PrimaryProvider: OpenAI, PrimaryModel: "gpt-4o"}},
		}
		ctx := WithValues(t.Context(), func(v *Values) { v.ParallelStrategy = "fusion" })
		chunks, err := collect(s.ChatStream(ctx, tenant.String(), "", req))
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "p"}}, chunks)
		assert.JSONEq(t, `{"model":"router-auto","messages":[{"role":"user","content":"twelve chars"}]}`, string(tr.payloads[0]))
	})
	t.Run("BYOK pins the preference route", func(t *testing.T) {
		tr := &recordingTransport{}
		s := &RoutedStreamService{
			Fallback: &recordingFallback{}, Transport: tr,
			Preferences: stubPreferences{pref: &Preference{Mode: ModeBYOK, PrimaryProvider: OpenAI, PrimaryModel: "gpt-4o"}},
		}
		_, err := collect(s.ChatStream(t.Context(), tenant.String(), "", req))
		require.NoError(t, err)
		forced := RoutingValues(tr.ctxs[0]).ForcedRoute
		require.NotNil(t, forced)
		assert.Equal(t, WireRoute{Provider: "openai", Model: "gpt-4o"}, *forced)
	})
	t.Run("BYOK without a model falls through to the gateway", func(t *testing.T) {
		fb := &recordingFallback{}
		s := &RoutedStreamService{
			Fallback: fb, Transport: &recordingTransport{},
			Preferences: stubPreferences{pref: &Preference{Mode: ModeBYOK, PrimaryProvider: OpenAI}},
		}
		chunks, err := collect(s.ChatStream(t.Context(), tenant.String(), "", req))
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "gateway"}}, chunks)
		require.Len(t, fb.requests, 1)
		assert.Empty(t, fb.requests[0].Model, "no router: deployment default")
	})
	lane := Decision{Primary: Route{Provider: OpenRouter, ModelID: "some/free:free"}, Cerberus: &CerberusMetadata{IsFreeLane: true}}
	t.Run("free lane fails closed when the transport can't execute decisions", func(t *testing.T) {
		fb := &recordingFallback{}
		s := &RoutedStreamService{Fallback: fb, Transport: &recordingTransport{}, Router: fixedRouter{decision: lane}}
		_, err := collect(s.ChatStream(t.Context(), tenant.String(), "", req))
		var up *UpstreamErrorResponse
		require.ErrorAs(t, err, &up)
		assert.Equal(t, "free_lane_unroutable", up.Reason)
		assert.Equal(t, "Free messages are unavailable right now. Add your own API key in Settings, or upgrade.", up.Message)
		assert.Equal(t, http.StatusBadGateway, up.HTTPStatus())
		assert.Empty(t, fb.requests, "never the paid gateway")
	})
	t.Run("free lane runs as decided", func(t *testing.T) {
		tr := &decidedTransport{}
		s := &RoutedStreamService{Fallback: &recordingFallback{}, Transport: tr, Router: fixedRouter{decision: lane}}
		_, err := collect(s.ChatStream(t.Context(), tenant.String(), "", req))
		require.NoError(t, err)
		require.Len(t, tr.decided, 1)
		assert.JSONEq(t, `{"model":"some/free:free","messages":[{"role":"user","content":"twelve chars"}]}`, string(tr.payloads[0]))
	})
	t.Run("free lane preflight error comes first", func(t *testing.T) {
		exhausted := lane
		exhausted.Cerberus = &CerberusMetadata{IsFreeLane: true, FreeLaneExhausted: true, FreeLaneRetryAfterSeconds: 60}
		s := &RoutedStreamService{Fallback: &recordingFallback{}, Transport: &recordingTransport{}, Router: fixedRouter{decision: exhausted}}
		_, err := collect(s.ChatStream(t.Context(), tenant.String(), "", req))
		var ex *FreeLaneExhaustedError
		assert.ErrorAs(t, err, &ex)
	})
	auto := Decision{Primary: Route{Provider: OpenRouter, ModelID: "openai/gpt-4o"}, Cerberus: &CerberusMetadata{
		Mode: ModeManaged, RoutingPolicy: string(PolicyAutoSmart), TaskType: "coding",
		Routes: []WireRoute{{Provider: "openRouter", Model: "openai/gpt-4o"}},
	}}
	t.Run("managed Auto rides the gateway with the picked model and publishes the gauge", func(t *testing.T) {
		fb := &recordingFallback{}
		var events []CerberusEvent
		ctx := WithValues(t.Context(), func(v *Values) { v.CerberusSink = func(e CerberusEvent) { events = append(events, e) } })
		s := &RoutedStreamService{Fallback: fb, Transport: &recordingTransport{}, Router: fixedRouter{decision: auto}}
		_, err := collect(s.ChatStream(ctx, tenant.String(), "", req))
		require.NoError(t, err)
		require.Len(t, fb.requests, 1)
		assert.Equal(t, "openai/gpt-4o", fb.requests[0].Model)
		require.NotEmpty(t, events)
		routing := events[0].Routing
		require.NotNil(t, routing)
		require.NotNil(t, routing.PromptTokens)
		assert.Equal(t, 12/4+4, *routing.PromptTokens)
		require.NotNil(t, routing.ContextWindowTokens, "openai/gpt-4o resolves through the prefix strip")
		assert.Equal(t, 128000, *routing.ContextWindowTokens)
	})
	t.Run("managed Auto over budget is a usage cap", func(t *testing.T) {
		denied := auto
		meta := *auto.Cerberus
		meta.BudgetDenied = true
		denied.Cerberus = &meta
		fb := &recordingFallback{}
		s := &RoutedStreamService{Fallback: fb, Transport: &recordingTransport{}, Router: fixedRouter{decision: denied}}
		_, err := collect(s.ChatStream(t.Context(), tenant.String(), "", req))
		var capErr *UsageCapExceededError
		assert.ErrorAs(t, err, &capErr)
		assert.Empty(t, fb.requests)
	})
	t.Run("a non-Auto decision keeps the deployment default", func(t *testing.T) {
		fb := &recordingFallback{}
		plain := Decision{Primary: Route{Provider: HermesGateway, ModelID: "x"}}
		s := &RoutedStreamService{Fallback: fb, Transport: &recordingTransport{}, Router: fixedRouter{decision: plain}}
		_, err := collect(s.ChatStream(t.Context(), tenant.String(), "", req))
		require.NoError(t, err)
		assert.Empty(t, fb.requests[0].Model)
	})
}

// PromptSizeEstimator, which RoutingContextMeterTests also drive.
func TestPromptSizeEstimator(t *testing.T) {
	assert.Equal(t, 0, EstimatePromptTokens(nil))
	assert.Equal(t, 1+8, EstimatePromptTokens([]ChatMessage{{Content: "abcd"}, {Content: "é"}}))
	w, ok := ContextWindowForModel("gpt-4o")
	assert.True(t, ok)
	assert.Equal(t, 128000, w)
	_, ok = ContextWindowForModel("openrouter/auto")
	assert.False(t, ok, "a catalogue entry with no window stops the search")
	_, ok = ContextWindowForModel("")
	assert.False(t, ok)
	_, ok = ContextWindowForModel("nobody/knows-this")
	assert.False(t, ok)
}
