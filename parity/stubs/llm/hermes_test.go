package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // test server
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return resp.StatusCode, out
}

func startRunOn(t *testing.T, base, input string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"input": input, "session_id": "vault-1"})
	require.NoError(t, err)
	status, out := post(t, base+"/v1/runs", string(body))
	require.Equal(t, http.StatusAccepted, status, out)
	var started struct {
		RunID string `json:"run_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &started))
	return started.RunID
}

// eventNames reads a run's stream to its end.
func eventNames(t *testing.T, base, id string) []string {
	t.Helper()
	resp, err := http.Get(base + "/v1/runs/" + id + "/events")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	return readEventNames(t, resp.Body, id)
}

func readEventNames(t *testing.T, body io.Reader, id string) []string {
	t.Helper()
	raw, err := io.ReadAll(body)
	require.NoError(t, err)
	var names []string
	for _, block := range strings.Split(strings.TrimSpace(string(raw)), "\n\n") {
		name, data, ok := strings.Cut(block, "\n")
		require.True(t, ok, block)
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &payload))
		// HermesRunEvent.decode takes the name from the JSON and the time from `timestamp`.
		require.Equal(t, strings.TrimPrefix(name, "event: "), payload["event"])
		require.Equal(t, id, payload["run_id"])
		require.NotNil(t, payload["timestamp"])
		names = append(names, payload["event"].(string))
	}
	return names
}

func TestCapabilitiesGateRuns(t *testing.T) {
	s := server(t)
	status, out := getJSON(t, s.URL+"/v1/capabilities")
	require.Equal(t, http.StatusOK, status)
	features := out["features"].(map[string]any)
	require.Equal(t, true, features["approval_events"])
	require.Equal(t, true, features["run_events_sse"])
}

func TestRunPlaysToCompletion(t *testing.T) {
	s := server(t)
	id := startRunOn(t, s.URL, "[[stub:reply:two words]]")
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-8[0-9a-f]{3}-[0-9a-f]{12}$`), id)

	require.Equal(t, []string{"run.started", "message.started", "message.delta", "message.delta", "run.completed"}, eventNames(t, s.URL, id))
	// A second reader replays it.
	require.Len(t, eventNames(t, s.URL, id), 5)

	status, out := getJSON(t, s.URL+"/v1/runs/"+id)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "completed", out["status"])
	require.Equal(t, "run.completed", out["last_event"])
	require.Equal(t, "two words", out["output"])
	require.Equal(t, "vault-1", out["session_id"])

	require.NotEqual(t, id, startRunOn(t, s.URL, "[[stub:reply:two words]]"), "two servers sharing the stub never share a run")
}

func TestRunsReportTools(t *testing.T) {
	s := server(t)
	id := startRunOn(t, s.URL, `[[stub:tool:search:{"q":"x"}]]`)
	names := eventNames(t, s.URL, id)
	require.Equal(t, []string{"run.started", "tool.started", "tool.completed", "message.started", "run.completed"}, names)
}

func TestRunStartFails(t *testing.T) {
	s := server(t)
	status, body := post(t, s.URL+"/v1/runs", `{"input":"[[stub:error:503]]"}`)
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Contains(t, body, "stub_error")
	status, _ = post(t, s.URL+"/v1/runs", `{}`)
	require.Equal(t, http.StatusBadRequest, status)
}

func TestRunWaitsForItsApproval(t *testing.T) {
	s := server(t)
	id := startRunOn(t, s.URL, "do it [[stub:approval:rm -rf /]]")

	status, out := getJSON(t, s.URL+"/v1/runs/"+id)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "waiting_for_approval", out["status"])

	// A reader that's already attached sees the rest as it happens.
	resp, err := http.Get(s.URL + "/v1/runs/" + id + "/events")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	status, _ = post(t, s.URL+"/v1/runs/"+id+"/approval", `{"choice":"once"}`)
	require.Equal(t, http.StatusOK, status)
	names := readEventNames(t, resp.Body, id)
	require.Equal(t, []string{"run.started", "approval.request", "approval.responded", "message.started"}, names[:4])
	require.Equal(t, "run.completed", names[len(names)-1])

	status, body := post(t, s.URL+"/v1/runs/"+id+"/approval", `{"choice":"once"}`)
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, body, "approval_not_pending")
}

func TestRunDeniedOrStopped(t *testing.T) {
	s := server(t)
	denied := startRunOn(t, s.URL, "[[stub:approval:x]]")
	status, _ := post(t, s.URL+"/v1/runs/"+denied+"/approval", `{"choice":"deny"}`)
	require.Equal(t, http.StatusOK, status)
	names := eventNames(t, s.URL, denied)
	require.Equal(t, "run.failed", names[len(names)-1])
	_, out := getJSON(t, s.URL+"/v1/runs/"+denied)
	require.Equal(t, "failed", out["status"])
	require.Equal(t, "approval denied", out["error"])

	stopped := startRunOn(t, s.URL, "[[stub:approval:x]]")
	status, _ = post(t, s.URL+"/v1/runs/"+stopped+"/stop", "")
	require.Equal(t, http.StatusOK, status)
	names = eventNames(t, s.URL, stopped)
	require.Equal(t, "run.cancelled", names[len(names)-1])
	_, out = getJSON(t, s.URL+"/v1/runs/"+stopped)
	require.Equal(t, "stopped", out["status"])
}

func TestUnknownRunIsNotFound(t *testing.T) {
	s := server(t)
	status, out := getJSON(t, s.URL+"/v1/runs/nope")
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, "run_not_found", out["error"].(map[string]any)["code"])
	status, _ = post(t, s.URL+"/v1/runs/nope/stop", "")
	require.Equal(t, http.StatusNotFound, status)
	resp, err := http.Get(s.URL + "/v1/runs/nope/events")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// A long-lived stub on staging keeps a bounded amount of state.
func TestStubStateIsBounded(t *testing.T) {
	st := newStub()
	s := httptestServer(t, st.handler())
	first := startRunOn(t, s, "hello")
	for range maxRuns {
		_, _ = post(t, s+"/v1/runs", `{"input":"hello"}`)
	}
	status, _ := getJSON(t, s+"/v1/runs/"+first)
	require.Equal(t, http.StatusNotFound, status)
	st.mu.Lock()
	defer st.mu.Unlock()
	require.Len(t, st.runs, maxRuns)
	require.Len(t, st.runOrder, maxRuns)
	require.Equal(t, maxRuns+1, st.stats["/v1/runs"])
	require.Equal(t, 1, st.stats["/v1/runs/{id}"], "stats are per route, not per run")
}

func TestDigestAllCoversTheWholePrompt(t *testing.T) {
	s := server(t)
	ask := func(system, user string) string {
		b, err := json.Marshal(map[string]any{"model": "m", "messages": []any{
			map[string]any{"role": "system", "content": system},
			map[string]any{"role": "user", "content": user},
		}})
		require.NoError(t, err)
		status, body := post(t, s.URL+"/v1/chat/completions", string(b))
		require.Equal(t, http.StatusOK, status)
		return body
	}
	require.Equal(t, ask("one", "hi"), ask("two", "hi"), "the default digest is the last user message")
	require.NotEqual(t, ask("one", "hi [[stub:digest:all]]"), ask("two", "hi [[stub:digest:all]]"))
	require.Equal(t, ask("one", "hi [[stub:digest:all]]"), ask("one", "hi [[stub:digest:all]]"))
}

func TestDeepInfraPathAnswers(t *testing.T) {
	s := server(t)
	status, body := post(t, s.URL+"/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, "stub reply ")
}

func TestHealthCarriesAVersion(t *testing.T) {
	s := server(t)
	status, out := getJSON(t, s.URL+"/health")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, stubVersion, out["version"])
}

func TestGatewayInstanceAndSessions(t *testing.T) {
	s := server(t)
	_, instance := getJSON(t, s.URL+"/api/instance")
	require.Equal(t, "default", instance["profile"])
	require.Equal(t, []any{"default"}, instance["profiles"])

	_, detailed := getJSON(t, s.URL+"/health/detailed")
	require.Contains(t, detailed["platforms"], "stub")

	_, sessions := getJSON(t, s.URL+"/api/profiles/sessions?limit=5")
	row := sessions["data"].([]any)[0].(map[string]any)
	require.Equal(t, "stub-session-1", row["id"])
	require.Equal(t, "default", row["profile"])
	_, flat := getJSON(t, s.URL+"/api/sessions?limit=5")
	require.NotContains(t, flat["data"].([]any)[0], "profile")

	for _, path := range []string{"/api/sessions/abc/messages", "/api/profiles/default/sessions/abc/messages"} {
		status, messages := getJSON(t, s.URL+path)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "abc", messages["session_id"])
		require.Len(t, messages["data"], 2)
	}
	status, missing := getJSON(t, s.URL+"/api/sessions/"+missingSession+"/messages")
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, "session_not_found", missing["error"].(map[string]any)["code"], "a real 404 carries a code; AgentGatewayClient reads a bare one as an old Hermes")
}

func TestGatewayChat(t *testing.T) {
	s := server(t)
	status, _ := post(t, s.URL+"/api/sessions", `{"id":"room-1","title":"Room"}`)
	require.Equal(t, http.StatusCreated, status)

	status, body := post(t, s.URL+"/api/sessions/room-1/chat", `{"message":"hello","system_message":"be brief"}`)
	require.Equal(t, http.StatusOK, status)
	var reply struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &reply))
	require.Contains(t, reply.Message.Content, "stub reply ")
	require.Equal(t, 15, reply.Usage.TotalTokens)

	status, _ = post(t, s.URL+"/api/sessions/room-1/chat", `{"message":"[[stub:error:502]]"}`)
	require.Equal(t, http.StatusBadGateway, status)
}

func TestGrokResponses(t *testing.T) {
	s := server(t)
	status, body := post(t, s.URL+"/v1/responses", `{"model":"grok","input":"[[stub:reply:found it]]","tools":[{"type":"x_search"}]}`)
	require.Equal(t, http.StatusOK, status)
	var out struct {
		Model     string `json:"model"`
		Output    string `json:"output"`
		Citations []struct {
			URL string `json:"url"`
		} `json:"citations"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.Equal(t, "grok", out.Model)
	require.Equal(t, "found it", out.Output)
	require.Len(t, out.Citations, 1)
}

func upload(t *testing.T, base, filename string, audio []byte, fields map[string]string) (int, string) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if filename != "" {
		part, err := form.CreateFormFile("file", filename)
		require.NoError(t, err)
		_, err = part.Write(audio)
		require.NoError(t, err)
	}
	for k, v := range fields {
		require.NoError(t, form.WriteField(k, v))
	}
	require.NoError(t, form.Close())
	resp, err := http.Post(base+"/v1/audio/transcriptions", form.FormDataContentType(), &body)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(out)
}

func TestTranscriptionsAcceptTheFiveFormats(t *testing.T) {
	s := server(t)
	for _, name := range []string{"a.ogg", "a.opus", "a.m4a", "a.mp3", "a.wav", "a.webm", "A.WAV"} {
		status, body := upload(t, s.URL, name, []byte("audio"), map[string]string{"model": "whisper"})
		require.Equal(t, http.StatusOK, status, name)
		require.JSONEq(t, `{"text":"stub transcript"}`, body, name)
	}
	status, _ := upload(t, s.URL, "a.bin", []byte("audio"), map[string]string{"model": "whisper"})
	require.Equal(t, http.StatusBadRequest, status)
}

func TestTranscriptionsVerboseShape(t *testing.T) {
	s := server(t)
	status, body := upload(t, s.URL, "a.m4a", []byte("audio"), map[string]string{"model": "whisper", "response_format": "verbose_json"})
	require.Equal(t, http.StatusOK, status)
	var out struct {
		Text     string  `json:"text"`
		Language string  `json:"language"`
		Duration float64 `json:"duration"`
		Segments []struct {
			Start      float64  `json:"start"`
			End        float64  `json:"end"`
			Text       string   `json:"text"`
			AvgLogprob *float64 `json:"avg_logprob"`
		} `json:"segments"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.Equal(t, stubTranscript, out.Text)
	require.Equal(t, "en", out.Language)
	require.InDelta(t, 1.5, out.Duration, 0)
	require.Len(t, out.Segments, 1)
	require.NotNil(t, out.Segments[0].AvgLogprob, "the adapter's confidence comes from it")
}

func TestTranscriptionsRefuseAndFail(t *testing.T) {
	s := server(t)
	status, _ := upload(t, s.URL, "", nil, map[string]string{"model": "whisper"})
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = upload(t, s.URL, "a.wav", []byte("audio"), nil)
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = post(t, s.URL+"/v1/audio/transcriptions", `{}`)
	require.Equal(t, http.StatusBadRequest, status)

	status, _ = upload(t, s.URL, "a.wav", []byte("[[stub:error:503]]"), map[string]string{"model": "whisper"})
	require.Equal(t, http.StatusServiceUnavailable, status)
	status, body := upload(t, s.URL, "a.wav", []byte("[[stub:reply:said this]]"), map[string]string{"model": "whisper"})
	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, `{"text":"said this"}`, body)
	status, _ = upload(t, s.URL, "a.wav", append(bytes.Repeat([]byte{0}, audioDirectiveWindow), "[[stub:error:503]]"...), map[string]string{"model": "whisper"})
	require.Equal(t, http.StatusOK, status, "only the start of the audio is read for a directive")
}

func TestOllamaChat(t *testing.T) {
	s := server(t)
	status, body := post(t, s.URL+"/api/chat", `{"model":"llama3","stream":false,"messages":[{"role":"user","content":"[[stub:reply:two words]]"}]}`)
	require.Equal(t, http.StatusOK, status)
	var buffered map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &buffered))
	require.Equal(t, "two words", buffered["message"].(map[string]any)["content"])
	require.Equal(t, "stop", buffered["done_reason"])

	status, body = post(t, s.URL+"/api/chat", `{"model":"llama3","stream":true,"messages":[{"role":"user","content":"[[stub:reply:two words]]"}]}`)
	require.Equal(t, http.StatusOK, status)
	joined, done := "", false
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		var row struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Done bool `json:"done"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &row), line)
		joined += row.Message.Content
		done = row.Done
	}
	require.Equal(t, "two words", joined)
	require.True(t, done)

	status, _ = post(t, s.URL+"/api/chat", `{"model":"llama3","messages":[{"role":"user","content":"[[stub:error:500]]"}]}`)
	require.Equal(t, http.StatusInternalServerError, status)

	_, tags := getJSON(t, s.URL+"/api/tags")
	require.Equal(t, "llama3:8b", tags["models"].([]any)[0].(map[string]any)["name"])
}

func TestStatsNameEachRoute(t *testing.T) {
	s := server(t)
	for _, call := range []string{"/v1/capabilities", "/api/instance", "/health/detailed", "/api/tags"} {
		status, _ := getJSON(t, s.URL+call)
		require.Equal(t, http.StatusOK, status, call)
	}
	_, stats := getJSON(t, s.URL+"/stub/stats")
	for _, route := range []string{"/v1/capabilities", "/api/instance", "/health/detailed", "/api/tags"} {
		require.InDelta(t, 1, stats[route], 0, fmt.Sprint(route))
	}
}
