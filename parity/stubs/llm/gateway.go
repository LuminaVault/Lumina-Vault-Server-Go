package main

import (
	"encoding/json"
	"net/http"
)

// missingSession is a session id the gateway doesn't know, for the 404 path.
const missingSession = "stub-missing"

// fixedActive is an old last_active, so AgentGatewayClient derives is_active false
// on every server at any time.
const fixedActive = 1700000100.5

// The gateway's Agents page and per-tenant Hermes calls, in the shapes
// AgentGatewayClient and HermesGrokProxy read:
//
//	GET  /api/instance
//	GET  /health/detailed
//	GET  /api/profiles/sessions, /api/sessions           {data: [session]}
//	GET  /api/profiles/{profile}/sessions/{id}/messages, /api/sessions/{id}/messages
//	POST /api/sessions                                   201, never 409: no state
//	POST /api/sessions/{id}/chat                         {message, system_message}
//	POST /v1/responses                                   grok x-search
//	GET  /v1/skills                                      installed skills (plugins)
//
// Grok's chat and vision calls use POST /v1/chat/completions.
func (s *stub) mountGateway(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/instance", func(w http.ResponseWriter, _ *http.Request) {
		s.count("/api/instance")
		writeJSON(w, http.StatusOK, map[string]any{"hostname": "hermes-stub", "version": stubVersion, "profiles": []string{"default"}, "profile": "default"})
	})
	mux.HandleFunc("GET /health/detailed", func(w http.ResponseWriter, _ *http.Request) {
		s.count("/health/detailed")
		writeJSON(w, http.StatusOK, map[string]any{"platforms": map[string]any{"stub": map[string]any{"state": "connected"}}})
	})
	mux.HandleFunc("GET /api/profiles/sessions", s.listSessions("/api/profiles/sessions", "default"))
	mux.HandleFunc("GET /api/sessions", s.listSessions("/api/sessions", ""))
	mux.HandleFunc("GET /api/profiles/{profile}/sessions/{id}/messages", s.sessionMessages("/api/profiles/{profile}/sessions/{id}/messages"))
	mux.HandleFunc("GET /api/sessions/{id}/messages", s.sessionMessages("/api/sessions/{id}/messages"))
	mux.HandleFunc("POST /api/sessions", s.createSession)
	mux.HandleFunc("POST /api/sessions/{id}/chat", s.sessionChat)
	mux.HandleFunc("POST /v1/responses", s.responses)
	// GET /v1/skills is the installed-skills list /v1/plugins/hermes-skills reads (6F).
	mux.HandleFunc("GET /v1/skills", func(w http.ResponseWriter, _ *http.Request) {
		s.count("/v1/skills")
		writeJSON(w, http.StatusOK, map[string]any{"skills": []any{
			map[string]any{"name": "gif-search", "description": "Find GIFs\nfrom the hub"},
			"alpha",
			map[string]any{"id": "zeta-tool"},
			map[string]any{"name": "gif-search"},
		}})
	})
}

func (s *stub) listSessions(route, profile string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		s.count(route)
		session := map[string]any{
			"id": "stub-session-1", "title": "Stub session", "source": "api",
			"started_at": fixedCreated, "last_active": fixedActive, "ended_at": nil,
			"message_count": 2, "estimated_cost_usd": 0.01,
		}
		if profile != "" {
			session["profile"] = profile
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{session}})
	}
}

func (s *stub) sessionMessages(route string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.count(route)
		id := r.PathValue("id")
		if id == missingSession {
			hermesError(w, http.StatusNotFound, "session_not_found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "data": []any{
			map[string]any{"role": "user", "content": "hello", "timestamp": fixedCreated},
			map[string]any{"role": "assistant", "content": "stub reply", "tool_calls": nil, "timestamp": fixedActive},
		}})
	}
}

func (s *stub) createSession(w http.ResponseWriter, r *http.Request) {
	s.count("POST /api/sessions")
	var req struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		hermesError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": req.ID, "title": req.Title})
}

func (s *stub) sessionChat(w http.ResponseWriter, r *http.Request) {
	s.count("/api/sessions/{id}/chat")
	var req struct {
		Message       string `json:"message"`
		SystemMessage string `json:"system_message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Message == "" {
		hermesError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	system := userMessage(req.SystemMessage)
	system.Role = "system"
	a := decide([]message{system, userMessage(req.Message)})
	if a.errorCode != 0 {
		hermesError(w, a.errorCode, "stub_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message": map[string]any{"role": "assistant", "content": a.text},
		"usage":   map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
	})
}

func (s *stub) responses(w http.ResponseWriter, r *http.Request) {
	s.count("/v1/responses")
	var req struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		hermesError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	a := decide([]message{userMessage(req.Input)})
	if a.errorCode != 0 {
		hermesError(w, a.errorCode, "stub_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model": req.Model, "output": a.text,
		"citations": []any{map[string]any{"url": "https://stub.parity.test/post/1", "title": "Stub citation"}},
	})
}
