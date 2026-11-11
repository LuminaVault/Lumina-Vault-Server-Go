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
