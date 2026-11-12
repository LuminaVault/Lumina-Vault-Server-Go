package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NvidiaNIMLiveTests: the Swift test calls the live NIM endpoint when
// NVIDIA_API_KEY is set. Here a stub NIM server stands in for
// integrate.api.nvidia.com, and the test asserts the same request the shipped
// adapter sends and that the completion it returns decodes to non-empty content.
func TestNvidiaNIMAdapterReturnsACompletion(t *testing.T) {
	var gotPath, gotAuth, gotContentType string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotContentType = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"meta/llama-3.1-8b-instruct",
			"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	base, err := url.Parse(srv.URL)
	require.NoError(t, err)

	adapter := &OpenAICompatibleAdapter{ProviderKind: Nvidia, APIKey: "nvapi-stub", BaseURL: base, Client: srv.Client()}
	payload, err := json.Marshal(map[string]any{
		"model":       "meta/llama-3.1-8b-instruct",
		"messages":    []map[string]string{{"role": "user", "content": "reply with the single word: pong"}},
		"max_tokens":  8,
		"temperature": 0,
	})
	require.NoError(t, err)

	meta, err := adapter.ChatCompletions(context.Background(), payload, "nvidia-live-test", "")
	require.NoError(t, err)

	assert.Equal(t, "/v1/chat/completions", gotPath)
	assert.Equal(t, "Bearer nvapi-stub", gotAuth)
	assert.Equal(t, "application/json", gotContentType)
	assert.Equal(t, "meta/llama-3.1-8b-instruct", gotBody["model"])

	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(meta.Data, &decoded))
	require.NotEmpty(t, decoded.Choices)
	assert.NotEmpty(t, strings.TrimSpace(decoded.Choices[0].Message.Content))
}

// NvidiaNIMLiveTests: nvidia default base URL is the NIM integrate host.
func TestNvidiaDefaultBaseURLIsTheNIMIntegrateHost(t *testing.T) {
	assert.Equal(t, "https://integrate.api.nvidia.com", DefaultBaseURL(Nvidia).String())
	assert.Equal(t, "https://integrate.api.nvidia.com/v1/chat/completions", Endpoint(Nvidia, DefaultBaseURL(Nvidia)))
}
