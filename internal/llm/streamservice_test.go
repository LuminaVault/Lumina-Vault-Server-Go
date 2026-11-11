package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatewayStub serves one SSE completion and records the request it got.
func gatewayStub(t *testing.T, body string) (*url.URL, *http.Request) {
	t.Helper()
	got := new(http.Request)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = *r.Clone(context.Background())
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return u, got
}

const gatewayStubBody = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"

// Ports HermesLLMStreamServiceTests: Swift asserts makeStreamRequest's headers
// directly; here a stub gateway records the request the service really sends.
func TestHermesLLMStreamService(t *testing.T) {
	t.Run("bearer + session headers are sent when apiKey and sessionID are non-empty", func(t *testing.T) {
		base, got := gatewayStub(t, gatewayStubBody)
		s := &GatewayStreamService{BaseURL: base, DefaultModel: "hermes-3", APIKey: "k-test"}
		chunks, err := collect(s.ChatStream(t.Context(), "tenant-uuid", "conv-123", ChatRequest{}))
		require.NoError(t, err)
		require.NotEmpty(t, chunks)
		assert.Equal(t, http.MethodPost, got.Method)
		assert.Equal(t, "/v1/chat/completions", got.URL.Path)
		assert.Equal(t, "Bearer k-test", got.Header.Get("Authorization"))
		assert.Equal(t, "tenant-uuid", got.Header.Get("X-Hermes-Session-Key"))
		assert.Equal(t, "conv-123", got.Header.Get("X-Hermes-Session-Id"))
		assert.Equal(t, "application/json", got.Header.Get("Content-Type"))
		assert.Equal(t, "text/event-stream", got.Header.Get("Accept"))
		assert.Empty(t, got.Header.Values("X-Hermes-Profile"))
	})
	t.Run("bearer header is omitted when apiKey is empty", func(t *testing.T) {
		base, got := gatewayStub(t, gatewayStubBody)
		s := &GatewayStreamService{BaseURL: base, DefaultModel: "hermes-3"}
		_, err := collect(s.ChatStream(t.Context(), "tenant-uuid", "", ChatRequest{}))
		require.NoError(t, err)
		assert.Empty(t, got.Header.Values("Authorization"))
		assert.Equal(t, "tenant-uuid", got.Header.Get("X-Hermes-Session-Key"))
		assert.Empty(t, got.Header.Values("X-Hermes-Session-Id"))
		assert.Equal(t, "application/json", got.Header.Get("Content-Type"))
		assert.Equal(t, "text/event-stream", got.Header.Get("Accept"))
	})
	t.Run("session id header is omitted when sessionID is empty string", func(t *testing.T) {
		base, got := gatewayStub(t, gatewayStubBody)
		s := &GatewayStreamService{BaseURL: base, APIKey: "k-test"}
		_, err := collect(s.ChatStream(t.Context(), "tenant-uuid", "", ChatRequest{}))
		require.NoError(t, err)
		assert.Empty(t, got.Header.Values("X-Hermes-Session-Id"))
		assert.Equal(t, "tenant-uuid", got.Header.Get("X-Hermes-Session-Key"))
		assert.Equal(t, "Bearer k-test", got.Header.Get("Authorization"))
	})
	t.Run("stream parser throws when upstream sends error payload", func(t *testing.T) {
		var yielded []Chunk
		_, err := processGatewayRecord(`data: {"error":{"message":"Provider returned error","code":400}}`, func(c Chunk) { yielded = append(yielded, c) })
		var upstream *GatewayStreamError
		require.ErrorAs(t, err, &upstream)
		assert.Equal(t, "Provider returned error", upstream.Message)
		assert.Equal(t, "400", upstream.Code)
		assert.Empty(t, yielded)
	})
	t.Run("stream parser exposes each running Hermes tool call as metadata", func(t *testing.T) {
		var yielded []Chunk
		done, err := processGatewayRecord("event: hermes.tool.progress\n"+
			`data: {"toolCallId":"call-search-1","tool":"web_search","status":"running"}`, func(c Chunk) { yielded = append(yielded, c) })
		require.NoError(t, err)
		assert.False(t, done)
		assert.Equal(t, []Chunk{{ToolCallID: "call-search-1"}}, yielded)
	})
	t.Run("stream parser ignores completed tool progress to avoid double counting", func(t *testing.T) {
		var yielded []Chunk
		done, err := processGatewayRecord("event: hermes.tool.progress\n"+
			`data: {"toolCallId":"call-search-1","tool":"web_search","status":"completed"}`, func(c Chunk) { yielded = append(yielded, c) })
		require.NoError(t, err)
		assert.False(t, done)
		assert.Empty(t, yielded)
	})
	// ChatStreamCompletionPolicy.emptyCompletionEvent.
	t.Run("empty assistant completion maps to stream error event", func(t *testing.T) {
		assert.True(t, IsEmptyCompletion("", 0))
		assert.True(t, IsEmptyCompletion(" \n", 3), "whitespace-only content is empty too")
		assert.True(t, IsEmptyCompletion("text", 0), "no tokens is empty even with a buffer")
		assert.Equal(t, "Lumina did not receive a model response. Check Hermes provider/model configuration.", EmptyCompletionMessage)
	})
	t.Run("non-empty assistant completion does not map to stream error event", func(t *testing.T) {
		assert.False(t, IsEmptyCompletion("Hello", 1))
	})
}

// HERMES_STREAM_IDLE_TIMEOUT: silence between records longer than IdleTimeout
// ends the stream with the idle error; tokens before it still arrive.
func TestGatewayStreamIdleTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	base, _ := url.Parse(srv.URL)
	s := &GatewayStreamService{BaseURL: base, IdleTimeout: 150 * time.Millisecond}
	started := time.Now()
	chunks, err := collect(s.ChatStream(t.Context(), "tenant", "", ChatRequest{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, errStreamIdle), "got %v", err)
	assert.Equal(t, []Chunk{{Delta: "a"}}, chunks)
	assert.Less(t, time.Since(started), 5*time.Second)
}

// A caller that already picked (Quick Assistant's classic fallback) binds its
// decision as PreDecidedRoute, so the stream service streams it rather than
// pick again and claim a second free-lane grant.
func TestPreDecidedRouteIsStreamedWithoutPicking(t *testing.T) {
	req := ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}}
	decided := Decision{Primary: Route{OpenRouter, "some/free:free"}, Cerberus: &CerberusMetadata{IsFreeLane: true, Mode: ModeManaged}}
	router := &countingRouter{decision: Decision{Primary: Route{HermesGateway, "hermes-3"}}}
	tr := &decidedTransport{recordingTransport: recordingTransport{chunks: []Chunk{{Delta: "free"}}}}
	fb := &recordingFallback{}
	s := &RoutedStreamService{
		Fallback: fb, Transport: tr, Router: router,
		Preferences: stubPreferences{pref: &Preference{Mode: ModeBYOK, PrimaryProvider: OpenAI, PrimaryModel: "gpt-4o"}},
	}
	ctx := WithValues(t.Context(), func(v *Values) { v.PreDecidedRoute = &decided })

	chunks, err := collect(s.ChatStream(ctx, uuid.NewString(), "", req))
	require.NoError(t, err)
	assert.Equal(t, []Chunk{{Delta: "free"}}, chunks)
	assert.Zero(t, router.picks.Load(), "never picks again")
	require.Len(t, tr.decided, 1)
	assert.Equal(t, decided, tr.decided[0])
	assert.JSONEq(t, `{"model":"some/free:free","messages":[{"role":"user","content":"hi"}]}`, string(tr.payloads[0]))
	assert.Nil(t, RoutingValues(tr.ctxs[0]).ForcedRoute, "goes before the BYOK pin")
	assert.Empty(t, fb.requests)
}

func TestPreDecidedRouteRunsItsPreflight(t *testing.T) {
	req := ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}}
	decided := Decision{Primary: Route{OpenRouter, "some/free:free"}, Cerberus: &CerberusMetadata{IsFreeLane: true, FreeLaneExhausted: true, FreeLaneRetryAfterSeconds: 60}}
	router := &countingRouter{}
	tr := &decidedTransport{}
	s := &RoutedStreamService{Fallback: &recordingFallback{}, Transport: tr, Router: router}
	ctx := WithValues(t.Context(), func(v *Values) { v.PreDecidedRoute = &decided })

	_, err := collect(s.ChatStream(ctx, "alice", "", req))
	var exhausted *FreeLaneExhaustedError
	require.ErrorAs(t, err, &exhausted)
	assert.Empty(t, tr.decided, "no transport call")
	assert.Empty(t, tr.payloads)
	assert.Zero(t, router.picks.Load())
}

func TestPreDecidedRouteWithoutDecidedTransportFailsClosed(t *testing.T) {
	req := ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}}
	decided := Decision{Primary: Route{OpenRouter, "some/free:free"}, Cerberus: &CerberusMetadata{IsFreeLane: true}}
	tr := &recordingTransport{}
	fb := &recordingFallback{}
	s := &RoutedStreamService{Fallback: fb, Transport: tr, Router: &countingRouter{}}
	ctx := WithValues(t.Context(), func(v *Values) { v.PreDecidedRoute = &decided })

	_, err := collect(s.ChatStream(ctx, "alice", "", req))
	var up *UpstreamErrorResponse
	require.ErrorAs(t, err, &up)
	assert.Equal(t, "free_lane_unroutable", up.Reason)
	assert.Empty(t, tr.payloads)
	assert.Empty(t, fb.requests, "never the paid gateway")
}

// Without a bound decision the service picks as before; TestRoutedStreamServiceOrder
// covers the free-lane, BYOK and managed branches unchanged.
func TestNoPreDecidedRouteIsUnchanged(t *testing.T) {
	req := ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}}
	lane := Decision{Primary: Route{OpenRouter, "some/free:free"}, Cerberus: &CerberusMetadata{IsFreeLane: true}}
	router := &countingRouter{decision: lane}
	tr := &decidedTransport{}
	s := &RoutedStreamService{Fallback: &recordingFallback{}, Transport: tr, Router: router}

	_, err := collect(s.ChatStream(t.Context(), "alice", "", req))
	require.NoError(t, err)
	assert.Equal(t, int32(1), router.picks.Load())
	require.Len(t, tr.decided, 1)
	assert.Equal(t, lane, tr.decided[0])
}
