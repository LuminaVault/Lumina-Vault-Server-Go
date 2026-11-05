package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

func server(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(newStub().handler())
	t.Cleanup(s.Close)
	return s
}

func httptestServer(t *testing.T, h http.Handler) string {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.URL
}

func TestChatIsDeterministicAndShowsPromptDifferences(t *testing.T) {
	s := server(t)
	ask := func(text string) string {
		status, body := post(t, s.URL+"/v1/chat/completions", `{"model":"hermes-3","messages":[{"role":"system","content":"sys"},{"role":"user","content":"`+text+`"}]}`)
		require.Equal(t, http.StatusOK, status)
		return body
	}
	require.Equal(t, ask("hello"), ask("hello"))
	require.NotEqual(t, ask("hello"), ask("hello!"))

	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message      struct{ Content string } `json:"message"`
			FinishReason string                   `json:"finish_reason"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal([]byte(ask("hello")), &out))
	require.Equal(t, "hermes-3", out.Model)
	require.Contains(t, out.Choices[0].Message.Content, "stub reply ")
	require.Equal(t, "stop", out.Choices[0].FinishReason)
}

func TestChatDirectives(t *testing.T) {
	s := server(t)
	ask := func(content string) (int, string) {
		b, err := json.Marshal(map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": content}}})
		require.NoError(t, err)
		return post(t, s.URL+"/v1/chat/completions", string(b))
	}

	_, body := ask(`wrapped [[stub:reply:{"a":[1]}]] prompt`)
	var reply struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &reply))
	require.JSONEq(t, `{"a":[1]}`, reply.Choices[0].Message.Content)

	_, body = ask(`[[stub:tool:search:{"q":"x"}]]`)
	require.Contains(t, body, `"finish_reason":"tool_calls"`)
	require.Contains(t, body, `"name":"search"`)
	require.Contains(t, body, `"arguments":"{\"q\":\"x\"}"`)

	status, body := ask(`[[stub:error:503]]`)
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Contains(t, body, "stub error")
}

func TestChatStreamJoinsToTheSameReply(t *testing.T) {
	s := server(t)
	status, body := post(t, s.URL+"/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"[[stub:reply:one two  three]]"}]}`)
	require.Equal(t, http.StatusOK, status)
	var joined strings.Builder
	lines := strings.Split(strings.TrimSpace(body), "\n\n")
	require.Equal(t, "data: [DONE]", lines[len(lines)-1])
	for _, l := range lines[:len(lines)-1] {
		var chunk struct {
			Choices []struct {
				Delta struct{ Content string } `json:"delta"`
			} `json:"choices"`
		}
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &chunk))
		joined.WriteString(chunk.Choices[0].Delta.Content)
	}
	require.Equal(t, "one two  three", joined.String())
}

func TestMessagesRefusesWhatHaikuRefuses(t *testing.T) {
	s := server(t)
	for _, extra := range []string{`"temperature":0.2`, `"top_p":0.9`, `"top_k":5`} {
		status, body := post(t, s.URL+"/v1/messages", `{"model":"m","messages":[{"role":"user","content":"hi"}],`+extra+`}`)
		require.Equal(t, http.StatusBadRequest, status, extra)
		require.Contains(t, body, "invalid_request_error")
	}
	status, _ := post(t, s.URL+"/v1/messages", `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"{"}]}`)
	require.Equal(t, http.StatusBadRequest, status, "prefill")
}

func TestMessagesAnswers(t *testing.T) {
	s := server(t)
	status, body := post(t, s.URL+"/v1/messages", `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"text","text":"[[stub:reply:hi]]"}]}]}`)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `"text":"hi"`)
	require.Contains(t, body, `"stop_reason":"end_turn"`)

	_, body = post(t, s.URL+"/v1/messages", `{"model":"m","messages":[{"role":"user","content":"[[stub:tool:search:{\"q\":1}]]"}]}`)
	require.Contains(t, body, `"stop_reason":"tool_use"`)
	require.Contains(t, body, `"input":{"q":1}`)

	_, body = post(t, s.URL+"/v1/messages", `{"model":"m","stream":true,"messages":[{"role":"user","content":"[[stub:reply:a b]]"}]}`)
	require.True(t, strings.HasPrefix(body, "event: message_start\n"))
	require.Contains(t, body, `"text":"a "`)
	require.True(t, strings.HasSuffix(strings.TrimSpace(body), `"type":"message_stop"}`))
}

func TestStatsCountsAnsweredRequests(t *testing.T) {
	s := server(t)
	post(t, s.URL+"/v1/chat/completions", `{"model":"m","messages":[]}`)
	post(t, s.URL+"/v1/chat/completions", `{"model":"m","messages":[]}`)
	resp, err := http.Get(s.URL + "/stub/stats")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var stats map[string]int
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&stats))
	require.Equal(t, 2, stats["/v1/chat/completions"])
}

func TestEchoRepliesWithTheCanonicalRequest(t *testing.T) {
	s := server(t)
	status, body := post(t, s.URL+"/v1/chat/completions", `{"temperature":0.2,"model":"m","messages":[{"role":"user","content":"[[stub:echo]]"}]}`)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `{\"messages\":[{\"content\":\"[[stub:echo]]\",\"role\":\"user\"}],\"model\":\"m\",\"temperature\":0.2}`)
}

func TestToolResultsCanEndATurn(t *testing.T) {
	s := server(t)
	status, body := post(t, s.URL+"/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"[[stub:tool:session_search:{}]]"},{"role":"tool","content":"{\"content\":\"[[stub:reply:done]]\"}"}]}`)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `"content":"done"`)
}
