package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// DefaultHermesLLMService: POST <base>/v1/chat/completions with the session
// headers and bearer key, the request model or the default, temperature as given,
// stream false; the reply's model and message come back.
func TestDirectHermesChat(t *testing.T) {
	var gotPath string
	var gotHeaders http.Header
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHeaders = r.URL.Path, r.Header.Clone()
		data, _ := io.ReadAll(r.Body)
		gotBody = nil
		_ = json.Unmarshal(data, &gotBody)
		_, _ = w.Write([]byte(`{"id":"c1","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	base, err := url.Parse(srv.URL + "/gw")
	require.NoError(t, err)
	h := &DirectHermes{BaseURL: base, APIKey: "k", DefaultModel: "default-model"}

	temperature := 0.1
	model, message, err := h.Chat(context.Background(), "TENANT", "", "", "prompt", &temperature)
	require.NoError(t, err)
	require.Equal(t, "upstream-model", model)
	require.Equal(t, "hi", message.Content)
	require.Equal(t, "/gw/v1/chat/completions", gotPath)
	require.Equal(t, "TENANT", gotHeaders.Get("X-Hermes-Session-Key"))
	require.Empty(t, gotHeaders.Get("X-Hermes-Session-Id"))
	require.Equal(t, "Bearer k", gotHeaders.Get("Authorization"))
	require.Equal(t, map[string]any{
		"model": "default-model", "temperature": 0.1, "stream": false,
		"messages": []any{map[string]any{"role": "user", "content": "prompt"}},
	}, gotBody)

	h.APIKey = ""
	_, _, err = h.Chat(context.Background(), "TENANT", "S1", "chosen", "p", nil)
	require.NoError(t, err)
	require.Empty(t, gotHeaders.Get("Authorization"))
	require.Equal(t, "S1", gotHeaders.Get("X-Hermes-Session-Id"))
	require.Equal(t, "chosen", gotBody["model"])
	require.NotContains(t, gotBody, "temperature")
}

// Failures are HTTPError(.badGateway) with Swift's messages.
func TestDirectHermesErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"status":     {500, `oops`, "HTTPError: Bad Gateway, hermes upstream error (500)"},
		"malformed":  {200, `{"choices":[]`, "HTTPError: Bad Gateway, hermes upstream returned malformed body"},
		"no choices": {200, `{"id":"c","model":"m","choices":[]}`, "HTTPError: Bad Gateway, hermes upstream returned no choices"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			base, err := url.Parse(srv.URL)
			require.NoError(t, err)
			_, _, err = (&DirectHermes{BaseURL: base}).Chat(context.Background(), "T", "", "m", "p", nil)
			require.EqualError(t, err, tc.want)
		})
	}
}
