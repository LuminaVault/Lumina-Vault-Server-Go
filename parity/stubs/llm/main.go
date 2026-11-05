// Command llm is a deterministic stand-in for the LLM upstreams in the parity
// environment. Swift and Go point at it (HERMES_GATEWAY_URL; /v1/messages is there
// for the Anthropic adapter's base URL), so success paths compare on identical
// answers with no key and no cost.
//
//	POST /v1/chat/completions  OpenAI shape, as the Hermes gateway and the
//	                           OpenAI-compatible providers speak it; SSE on stream:true
//	POST /v1/messages          Anthropic Messages, also with stream:true
//	GET  /v1/models            one model, for gateway probes
//	GET  /health               200, with a version
//	GET  /stub/stats           requests answered per route, so a script can prove a
//	                           server reached the stub and didn't fail over or fail closed
//
// runs.go has the Hermes runs API, gateway.go the Agents page and per-tenant
// Hermes calls, providers.go Whisper and Ollama. Every shape is what the Swift
// client reads, not the whole upstream API.
//
// The stub also runs on staging as HERMES_GATEWAY_URL (wave-5-plan.md, "G2 for
// chat"), so it holds no secret, reaches nothing upstream, executes no tool, and
// keeps a bounded amount of state.
//
// By default the reply is "stub reply <digest>", with the digest taken from the
// last user message: a user prompt that differs between the servers shows up as
// a body difference. A directive anywhere in a message changes the answer. The
// newest message holding one wins:
//
//	[[stub:reply:TEXT]]         reply with exactly TEXT (for example, JSON a
//	                            structured-output route expects)
//	[[stub:tool:NAME:ARGS]]     call tool NAME with the JSON text ARGS
//	[[stub:error:STATUS]]       answer that HTTP status
//	[[stub:digest:all]]         take the digest over every message, role and text,
//	                            so a system prompt or history difference shows too.
//	                            Opt-in: only for prompts free of times and random ids
//
// /v1/messages refuses what Haiku 5.5 refuses (platform/infra
// docs/llm-haiku-trial.md): temperature, top_p, top_k and assistant prefill are
// a 400, so a port that adds them fails here rather than in production.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	fixedCreated = 1700000000
	fixedID      = "stub-0001"
	stubVersion  = "stub"
)

var directiveRe = regexp.MustCompile(`(?s)\[\[stub:(reply|tool|error|digest):(.*)\]\]`)

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// text is a message's content, a plain string or a list of typed parts in both
// the OpenAI and Anthropic shapes.
func (m message) text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

type answer struct {
	text      string
	toolName  string
	toolArgs  string
	errorCode int
}

// decide turns a conversation into the stub's answer.
func decide(messages []message) answer {
	for i := len(messages) - 1; i >= 0; i-- {
		m := directiveRe.FindStringSubmatch(messages[i].text())
		if m == nil {
			continue
		}
		switch m[1] {
		case "reply":
			return answer{text: m[2]}
		case "tool":
			name, args, _ := strings.Cut(m[2], ":")
			return answer{toolName: name, toolArgs: args}
		case "error":
			code, err := strconv.Atoi(m[2])
			if err != nil {
				code = http.StatusInternalServerError
			}
			return answer{errorCode: code}
		case "digest":
			return answer{text: replyFor(wholePrompt(messages))}
		}
	}
	last := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			last = messages[i].text()
			break
		}
	}
	return answer{text: replyFor(last)}
}

func replyFor(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return "stub reply " + hex.EncodeToString(sum[:8])
}

// wholePrompt is every message with its role, unambiguously delimited.
func wholePrompt(messages []message) string {
	var b strings.Builder
	for _, m := range messages {
		fmt.Fprintf(&b, "%q:%q\n", m.Role, m.text())
	}
	return b.String()
}

// userMessage wraps text in a message, for APIs that take a bare prompt.
func userMessage(text string) message {
	raw, _ := json.Marshal(text)
	return message{Role: "user", Content: raw}
}

// pieces splits a reply into stream deltas that join back to it exactly.
func pieces(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, ' ')
		if i < 0 {
			return append(out, s)
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

type stub struct {
	mu    sync.Mutex
	stats map[string]int
	// Under mu, oldest first in runOrder (runs.go).
	runs     map[string]*run
	runOrder []string
	runCount int
}

func newStub() *stub { return &stub{stats: map[string]int{}, runs: map[string]*run{}} }

func (s *stub) count(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats[path]++
}

func (s *stub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	// DeepInfra's adapter joins chat/completions straight onto its base URL.
	mux.HandleFunc("POST /chat/completions", s.chat)
	mux.HandleFunc("POST /v1/messages", s.messages)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		s.count(r.URL.Path)
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{
			map[string]any{"id": "hermes-3", "object": "model", "created": fixedCreated, "owned_by": "stub"},
		}})
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": stubVersion})
	})
	s.mountRuns(mux)
	s.mountGateway(mux)
	s.mountProviders(mux)
	mux.HandleFunc("GET /stub/stats", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, http.StatusOK, s.stats)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Stream   bool      `json:"stream"`
}

func (s *stub) chat(w http.ResponseWriter, r *http.Request) {
	s.count(r.URL.Path)
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "bad json", "type": "invalid_request_error"}})
		return
	}
	a := decide(req.Messages)
	if a.errorCode != 0 {
		writeJSON(w, a.errorCode, map[string]any{"error": map[string]any{"message": "stub error", "type": "stub_error"}})
		return
	}
	if req.Stream {
		chatStream(w, req.Model, a)
		return
	}
	msg := map[string]any{"role": "assistant", "content": a.text}
	finish := "stop"
	if a.toolName != "" {
		msg = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{openAIToolCall(a, 0, true)}}
		finish = "tool_calls"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": fixedID, "object": "chat.completion", "created": fixedCreated, "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
}

func openAIToolCall(a answer, index int, whole bool) map[string]any {
	call := map[string]any{
		"id": "call_stub_0001", "type": "function",
		"function": map[string]any{"name": a.toolName, "arguments": a.toolArgs},
	}
	if !whole {
		call["index"] = index
	}
	return call
}

func sseHeaders(w http.ResponseWriter) *http.ResponseController {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return http.NewResponseController(w)
}

func chatStream(w http.ResponseWriter, model string, a answer) {
	rc := sseHeaders(w)
	chunk := func(delta map[string]any, finish any) {
		b, _ := json.Marshal(map[string]any{
			"id": fixedID, "object": "chat.completion.chunk", "created": fixedCreated, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		_ = rc.Flush()
	}
	chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	finish := "stop"
	if a.toolName != "" {
		chunk(map[string]any{"tool_calls": []any{openAIToolCall(a, 0, false)}}, nil)
		finish = "tool_calls"
	}
	for _, p := range pieces(a.text) {
		chunk(map[string]any{"content": p}, nil)
	}
	chunk(map[string]any{}, finish)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	_ = rc.Flush()
}

type messagesRequest struct {
	Model       string          `json:"model"`
	Messages    []message       `json:"messages"`
	Stream      bool            `json:"stream"`
	Temperature json.RawMessage `json:"temperature"`
	TopP        json.RawMessage `json:"top_p"`
	TopK        json.RawMessage `json:"top_k"`
}

func anthropicError(w http.ResponseWriter, status int, kind, msg string) {
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": msg}})
}

func (s *stub) messages(w http.ResponseWriter, r *http.Request) {
	s.count(r.URL.Path)
	var req messagesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	if req.Temperature != nil || req.TopP != nil || req.TopK != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "temperature, top_p and top_k are not supported")
		return
	}
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "assistant" {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "assistant prefill is not supported")
		return
	}
	a := decide(req.Messages)
	if a.errorCode != 0 {
		anthropicError(w, a.errorCode, "api_error", "stub error")
		return
	}
	if req.Stream {
		messagesStream(w, req.Model, a)
		return
	}
	stop := "end_turn"
	content := []any{map[string]any{"type": "text", "text": a.text}}
	if a.toolName != "" {
		stop = "tool_use"
		content = []any{map[string]any{"type": "tool_use", "id": "toolu_stub_0001", "name": a.toolName, "input": json.RawMessage(a.toolArgs)}}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": fixedID, "type": "message", "role": "assistant", "model": req.Model,
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
	})
}

func messagesStream(w http.ResponseWriter, model string, a answer) {
	rc := sseHeaders(w)
	event := func(name string, v map[string]any) {
		v["type"] = name
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		_ = rc.Flush()
	}
	event("message_start", map[string]any{"message": map[string]any{
		"id": fixedID, "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 10, "output_tokens": 1},
	}})
	stop := "end_turn"
	if a.toolName != "" {
		stop = "tool_use"
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "tool_use", "id": "toolu_stub_0001", "name": a.toolName, "input": map[string]any{}}})
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": a.toolArgs}})
	} else {
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		for _, p := range pieces(a.text) {
			event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": p}})
		}
	}
	event("content_block_stop", map[string]any{"index": 0})
	event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 5}})
	event("message_stop", map[string]any{})
}

func main() {
	srv := &http.Server{Addr: ":8080", Handler: newStub().handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
