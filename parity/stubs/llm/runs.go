package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
)

// maxRuns bounds what a long-lived stub remembers; the oldest run is forgotten.
const maxRuns = 1024

// The Hermes runs API, in the shapes HermesRunsClient and HermesRunEvent read:
//
//	GET  /v1/capabilities            features.approval_events and run_events_sse
//	POST /v1/runs                    {input, session_id?, model?} → 202 {run_id}
//	GET  /v1/runs/{id}               {status, last_event, output, error, session_id}
//	GET  /v1/runs/{id}/events        named SSE, replayed from the start per reader
//	POST /v1/runs/{id}/approval      {choice}; 409 approval_not_pending
//	POST /v1/runs/{id}/stop
//
// The directives apply to input. reply, tool and digest shape the run; error fails
// POST /v1/runs with that status. One more holds a run for its approval:
//
//	[[stub:approval:COMMAND]]   wait for POST .../approval, asking about COMMAND
//
// A tool event is only an event: nothing runs. Run ids are UUIDs so the harness
// masks them, and each POST gets its own, so two servers sharing a stub can't see
// each other's runs.
func (s *stub) mountRuns(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/capabilities", s.capabilities)
	mux.HandleFunc("POST /v1/runs", s.startRun)
	mux.HandleFunc("GET /v1/runs/{id}", s.runStatus)
	mux.HandleFunc("GET /v1/runs/{id}/events", s.runEvents)
	mux.HandleFunc("POST /v1/runs/{id}/approval", s.approveRun)
	mux.HandleFunc("POST /v1/runs/{id}/stop", s.stopRun)
}

var approvalRe = regexp.MustCompile(`(?s)\[\[stub:approval:(.*?)\]\]`)

type runEvent struct {
	name string
	data []byte
}

// run is guarded by stub.mu.
type run struct {
	id, sessionID string
	reply         answer
	status        string
	lastEvent     string
	output        string
	errorText     string
	pending       bool
	events        []runEvent
	// changed is closed and replaced whenever an event is added.
	changed chan struct{}
}

func (r *run) terminal() bool {
	switch r.status {
	case "completed", "failed", "stopped":
		return true
	}
	return false
}

func (r *run) add(name string, fields map[string]any) {
	body := map[string]any{"event": name, "run_id": r.id, "timestamp": fixedCreated}
	for k, v := range fields {
		body[k] = v
	}
	data, _ := json.Marshal(body)
	r.events = append(r.events, runEvent{name, data})
	r.lastEvent = name
	close(r.changed)
	r.changed = make(chan struct{})
}

// finish plays the rest of the run after any approval.
func (r *run) finish() {
	if r.reply.toolName != "" {
		r.add("tool.started", map[string]any{"tool": r.reply.toolName, "preview": r.reply.toolArgs})
		r.add("tool.completed", map[string]any{"tool": r.reply.toolName, "duration": 0.1, "error": false})
	}
	r.add("message.started", nil)
	for _, p := range pieces(r.reply.text) {
		r.add("message.delta", map[string]any{"delta": p})
	}
	r.status, r.output = "completed", r.reply.text
	r.add("run.completed", map[string]any{"output": r.reply.text})
}

func (s *stub) capabilities(w http.ResponseWriter, _ *http.Request) {
	s.count("/v1/capabilities")
	writeJSON(w, http.StatusOK, map[string]any{"version": stubVersion, "features": map[string]any{
		"approval_events": true, "run_events_sse": true,
		"session_resources": true, "session_chat": true,
		"jobs_admin": false, "skills_api": false, "multimodal_ingestion": false,
	}})
}

func hermesError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": code}})
}

func (s *stub) startRun(w http.ResponseWriter, r *http.Request) {
	s.count("/v1/runs")
	var req struct {
		Input     string `json:"input"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Input == "" {
		hermesError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reply := decide([]message{userMessage(req.Input)})
	if reply.errorCode != 0 {
		hermesError(w, reply.errorCode, "stub_error")
		return
	}
	session := req.SessionID
	if session == "" {
		session = r.Header.Get("X-Hermes-Session-Key")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.runCount++
	item := &run{id: runID(s.runCount), sessionID: session, reply: reply, status: "running", changed: make(chan struct{})}
	s.runs[item.id] = item
	s.runOrder = append(s.runOrder, item.id)
	if len(s.runOrder) > maxRuns {
		delete(s.runs, s.runOrder[0])
		s.runOrder = s.runOrder[1:]
	}
	item.add("run.started", nil)
	if m := approvalRe.FindStringSubmatch(req.Input); m != nil {
		item.status, item.pending = "waiting_for_approval", true
		item.add("approval.request", map[string]any{"command": m[1], "choices": []string{"once", "session", "always", "deny"}})
	} else {
		item.finish()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": item.id, "status": "started"})
}

// runID is a UUID-shaped digest of a counter.
func runID(n int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "run-%d", n))
	h := hex.EncodeToString(sum[:16])
	return h[:8] + "-" + h[8:12] + "-4" + h[13:16] + "-8" + h[17:20] + "-" + h[20:32]
}

// withRun answers 404 for an unknown run and otherwise calls f holding the lock.
func (s *stub) withRun(w http.ResponseWriter, r *http.Request, route string, f func(*run)) {
	s.count(route)
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.runs[r.PathValue("id")]
	if !ok {
		hermesError(w, http.StatusNotFound, "run_not_found")
		return
	}
	f(item)
}

func (s *stub) runStatus(w http.ResponseWriter, r *http.Request) {
	s.withRun(w, r, "/v1/runs/{id}", func(item *run) {
		out := map[string]any{"run_id": item.id, "status": item.status, "last_event": item.lastEvent}
		if item.output != "" {
			out["output"] = item.output
		}
		if item.errorText != "" {
			out["error"] = item.errorText
		}
		if item.sessionID != "" {
			out["session_id"] = item.sessionID
		}
		writeJSON(w, http.StatusOK, out)
	})
}

func (s *stub) approveRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Choice string `json:"choice"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		hermesError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.withRun(w, r, "/v1/runs/{id}/approval", func(item *run) {
		if !item.pending {
			hermesError(w, http.StatusConflict, "approval_not_pending")
			return
		}
		item.pending = false
		item.status = "running"
		item.add("approval.responded", map[string]any{"choice": req.Choice})
		if req.Choice == "deny" {
			item.status, item.errorText = "failed", "approval denied"
			item.add("run.failed", map[string]any{"error": item.errorText})
		} else {
			item.finish()
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": item.status})
	})
}

func (s *stub) stopRun(w http.ResponseWriter, r *http.Request) {
	s.withRun(w, r, "/v1/runs/{id}/stop", func(item *run) {
		if !item.terminal() {
			item.pending = false
			item.status = "stopped"
			item.add("run.cancelled", nil)
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": item.status})
	})
}

func (s *stub) runEvents(w http.ResponseWriter, r *http.Request) {
	var item *run
	s.withRun(w, r, "/v1/runs/{id}/events", func(found *run) { item = found })
	if item == nil {
		return
	}
	rc := sseHeaders(w)
	for sent := 0; ; {
		s.mu.Lock()
		events := item.events[sent:]
		done := item.terminal()
		changed := item.changed
		s.mu.Unlock()
		for _, e := range events {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, e.data)
		}
		sent += len(events)
		_ = rc.Flush()
		if done {
			return
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
	}
}
