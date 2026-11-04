package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func collectRecord(t *testing.T, record string) ([]Chunk, bool, error) {
	t.Helper()
	var chunks []Chunk
	done, err := ProcessOpenAIRecord(record, OpenAI, func(c Chunk) { chunks = append(chunks, c) })
	return chunks, done, err
}

// ProviderStreamKitTests. The Gemini case stays with the Gemini adapter.
func TestProcessOpenAIRecord(t *testing.T) {
	chunks, done, err := collectRecord(t, `data: {"choices":[{"delta":{"content":"Hel"},"finish_reason":null}]}`)
	require.NoError(t, err)
	assert.Equal(t, []Chunk{{Delta: "Hel"}}, chunks)
	assert.False(t, done)

	chunks, done, _ = collectRecord(t, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	assert.Equal(t, []Chunk{{FinishReason: "stop"}}, chunks)
	assert.True(t, done)

	chunks, done, _ = collectRecord(t, "data: [DONE]")
	assert.Empty(t, chunks)
	assert.True(t, done)

	_, _, err = collectRecord(t, `data: {"error":{"message":"bad"}}`)
	var perr *ProviderError
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, KindTransient, perr.Kind)
	assert.Equal(t, "bad", *perr.Body)

	chunks, done, _ = collectRecord(t, "event: hermes.tool.progress\ndata: {\"choices\":[{\"delta\":{\"content\":\"nope\"}}]}")
	assert.Empty(t, chunks)
	assert.False(t, done)

	chunks, _, _ = collectRecord(t, "event: message\ndata: {\"choices\":[{\"delta\":{\"content\":\"yes\"}}]}")
	assert.Equal(t, []Chunk{{Delta: "yes"}}, chunks)
}

// ProviderStreamKitStreamFlagTests.
func TestWithStreamFlag(t *testing.T) {
	flagged, ok := decodeObject(WithStreamFlag([]byte(`{"model":"gpt-stream","temperature":0.2,"messages":[{"role":"user","content":"Hello"}]}`)))
	require.True(t, ok)
	assert.Equal(t, true, flagged["stream"])
	assert.Equal(t, "gpt-stream", flagged["model"])
	assert.Equal(t, "0.2", fmt.Sprint(flagged["temperature"]))
	assert.Equal(t, "Hello", flagged["messages"].([]any)[0].(map[string]any)["content"])

	overridden, _ := decodeObject(WithStreamFlag([]byte(`{"model":"m","stream":false}`)))
	assert.Equal(t, true, overridden["stream"])

	garbage := []byte("not json")
	assert.Equal(t, garbage, WithStreamFlag(garbage))
}

func collect(s Stream) ([]Chunk, error) {
	var chunks []Chunk
	for c, err := range s {
		if err != nil {
			return chunks, err
		}
		chunks = append(chunks, c)
	}
	return chunks, nil
}

func stubStreamServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func runTestStream(ctx context.Context, url string, timeout time.Duration) Stream {
	return runStream(ctx, http.DefaultClient, OpenAI, timeout, framingSSE,
		func(context.Context) (streamRequest, error) { return streamRequest{url: url, body: []byte("{}")}, nil },
		func(record string, yield func(Chunk)) (bool, error) {
			return ProcessOpenAIRecord(record, OpenAI, yield)
		})
}

func TestRunStream(t *testing.T) {
	t.Run("frames across reads, and a multibyte character split between them survives", func(t *testing.T) {
		srv := stubStreamServer(t, func(w http.ResponseWriter, _ *http.Request) {
			record := []byte(`data: {"choices":[{"delta":{"content":"café"}}]}` + "\n\n")
			split := strings.Index(string(record), "é") + 1
			_, _ = w.Write(record[:split])
			w.(http.Flusher).Flush()
			_, _ = w.Write(record[split:])
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		})
		chunks, err := collect(runTestStream(context.Background(), srv.URL, time.Minute))
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "café"}}, chunks)
	})
	t.Run("a trailing record without a terminator is flushed", func(t *testing.T) {
		srv := stubStreamServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"end"}}]}`))
		})
		chunks, err := collect(runTestStream(context.Background(), srv.URL, time.Minute))
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "end"}}, chunks)
	})
	t.Run("non-2xx is classified from a bounded body", func(t *testing.T) {
		srv := stubStreamServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(403)
			_, _ = w.Write([]byte("insufficient_quota"))
		})
		_, err := collect(runTestStream(context.Background(), srv.URL, time.Minute))
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, KindCreditExhausted, perr.Kind)
	})
	t.Run("the deadline covers only the response head", func(t *testing.T) {
		release := make(chan struct{})
		srv := stubStreamServer(t, func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
		defer close(release)
		_, err := collect(runTestStream(context.Background(), srv.URL, 50*time.Millisecond))
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, KindNetwork, perr.Kind)
		assert.Equal(t, "network", perr.ReasonCode(), "AsyncHTTPClient errors are not URLErrors, so never upstream_timeout")

		slow := stubStreamServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			time.Sleep(150 * time.Millisecond)
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"late"}}]}` + "\n\ndata: [DONE]\n\n"))
		})
		chunks, err := collect(runTestStream(context.Background(), slow.URL, 50*time.Millisecond))
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "late"}}, chunks)
	})
	t.Run("breaking out stops the upstream", func(t *testing.T) {
		gone := make(chan struct{})
		srv := stubStreamServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"a"}}]}` + "\n\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(gone)
		})
		for range runTestStream(context.Background(), srv.URL, time.Minute) {
			break
		}
		select {
		case <-gone:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream request was not cancelled")
		}
	})
}

func TestSwiftLines(t *testing.T) {
	assert.Equal(t, []string{"a", "b"}, swiftLines("a\n\nb\n"))
	assert.Equal(t, []string{"a\r\nb"}, swiftLines("a\r\nb"), "a Swift Character \\r\\n is not \\n")
}

// OpenAICompletionReaderTests. The two probe cases stay with ProviderToolProbe.
func TestCompletionReader(t *testing.T) {
	completion := func(message string) []byte { return []byte(`{"choices":[{"message":` + message + `}]}`) }
	assert.Equal(t, []string{"search_memory", "read_vault_file"}, ToolCallNamesFrom(completion(
		`{"tool_calls":[{"type":"function","function":{"name":"search_memory","arguments":"{}"}},{"type":"function","function":{"name":"read_vault_file","arguments":"{}"}}]}`)))
	assert.Equal(t, []string{"lookup_job_status"}, ToolCallNamesFrom(completion(`{"function_call":{"name":"lookup_job_status","arguments":"{}"}}`)))

	prose := completion(`{"role":"assistant","content":"Here is the summary."}`)
	assert.Empty(t, ToolCallNamesFrom(prose))
	_, ok := AssistantMessage(prose)
	assert.True(t, ok)

	for _, body := range []string{"", "not json", `{"choices":[]}`} {
		_, ok := AssistantMessage([]byte(body))
		assert.False(t, ok, body)
	}
	assert.Equal(t, []string{"real_tool"}, ToolCallNamesFrom(completion(
		`{"tool_calls":[{"type":"function","function":{"arguments":"{}"}},{"type":"function","function":{"name":"","arguments":"{}"}},{"type":"function","function":{"name":"real_tool","arguments":"{}"}}]}`)))

	assert.Nil(t, ToolRecord(nil, nil), "no information")
	assert.Equal(t, &AgentToolCalls{Names: []string{}, Count: 0}, ToolRecord([]string{}, nil), "ran without tools")
	three := 3
	assert.Equal(t, 3, ToolRecord(nil, &three).Count)
	nine := 9
	assert.Equal(t, []string{"a"}, ToolRecord([]string{"a"}, &nine).Names)
	assert.Equal(t, 2, ToolRecord([]string{"a", "b"}, nil).Count)
}
