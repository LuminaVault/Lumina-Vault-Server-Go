// Package hermesrunstest is FakeHermesRunsGateway and RecordingRunPushNotifier
// (AppTests/Hermes/Runs/FakeHermesRunsGateway.swift) for Go: a scripted Hermes
// /v1/runs gateway on an httptest server, and a recording notifier.
package hermesrunstest

import (
	"context"
	"io"
	"lumina/internal/hermesruns"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// SupportedCapabilities reports full runs support.
const SupportedCapabilities = `{"features":{"approval_events":true,"run_events_sse":true}}`

// Request is one recorded call.
type Request struct {
	Method, Path  string
	Body          *string
	Authorization string
	SessionKey    string
}

type stub struct {
	status int
	body   string
}

// Gateway is a scripted Hermes. Unstubbed calls answer 404 run_not_found.
type Gateway struct {
	Server *httptest.Server

	mu            sync.Mutex
	requests      []Request
	stubs         map[string]stub
	events        chan string
	eventsCount   int
	eventsStatus  int
	eventsFailure string
}

// New starts a gateway; it closes with the test.
func New(t testing.TB) *Gateway {
	g := &Gateway{stubs: map[string]stub{}, eventsStatus: http.StatusOK}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(func() {
		g.FinishEvents()
		g.Server.Close()
	})
	return g
}

// Accepting is FakeHermesRunsGateway.accepting(runID:).
func Accepting(t testing.TB, runID string) *Gateway {
	g := New(t)
	g.Stub("GET v1/capabilities", http.StatusOK, SupportedCapabilities)
	g.Stub("POST v1/runs", http.StatusAccepted, `{"run_id":"`+runID+`"}`)
	g.Stub("POST v1/runs/"+runID+"/approval", http.StatusOK, `{"status":"accepted"}`)
	g.Stub("POST v1/runs/"+runID+"/stop", http.StatusOK, `{"status":"stopping"}`)
	return g
}

// Stub scripts "<METHOD> <path>" (path without the leading slash).
func (g *Gateway) Stub(key string, status int, body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stubs[key] = stub{status, body}
}

// FailEvents makes GET /events answer status and body instead of streaming.
func (g *Gateway) FailEvents(status int, body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.eventsStatus, g.eventsFailure = status, body
}

// Requests is every recorded call.
func (g *Gateway) Requests() []Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Request(nil), g.requests...)
}

// RequestsMatching is the calls to path.
func (g *Gateway) RequestsMatching(path string) []Request {
	var out []Request
	for _, r := range g.Requests() {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// EventsSubscriptionCount is how many times /events was requested.
func (g *Gateway) EventsSubscriptionCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.eventsCount
}

// Emit pushes one `data: <json>` record.
func (g *Gateway) Emit(json string) { g.EmitRaw("data: " + json + "\n\n") }

// EmitRaw pushes arbitrary text.
func (g *Gateway) EmitRaw(text string) {
	g.mu.Lock()
	ch := g.events
	g.mu.Unlock()
	if ch != nil {
		ch <- text
	}
}

// FinishEvents closes the live stream.
func (g *Gateway) FinishEvents() {
	g.mu.Lock()
	ch := g.events
	g.events = nil
	g.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// WaitForEventSubscription blocks until /events is being streamed.
func (g *Gateway) WaitForEventSubscription(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		open := g.events != nil
		g.mu.Unlock()
		if open {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("events never subscribed")
}

// URL is the gateway's base URL.
func (g *Gateway) URL() *url.URL {
	u, _ := url.Parse(g.Server.URL)
	return u
}

// Client is FakeHermesRunsGateway.client(sessionKey:).
func (g *Gateway) Client(sessionKey string) *hermesruns.Client {
	return &hermesruns.Client{
		BaseURL: g.URL(), AuthHeader: "Bearer fake-token", SessionKey: sessionKey,
		HTTP: hermesruns.NoRedirectClient(), RequestTimeout: 5 * time.Second, StreamIdleTimeout: 30 * time.Second,
	}
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimLeft(r.URL.EscapedPath(), "/")
	raw, _ := io.ReadAll(r.Body)
	rec := Request{Method: r.Method, Path: path, Authorization: r.Header.Get("Authorization"), SessionKey: r.Header.Get("X-Hermes-Session-Key")}
	if len(raw) > 0 {
		s := string(raw)
		rec.Body = &s
	}
	g.mu.Lock()
	g.requests = append(g.requests, rec)
	g.mu.Unlock()

	if r.Method == http.MethodGet && strings.HasSuffix(path, "/events") {
		g.serveEvents(w, r)
		return
	}
	g.mu.Lock()
	s, ok := g.stubs[r.Method+" "+path]
	g.mu.Unlock()
	if !ok {
		s = stub{http.StatusNotFound, `{"error":{"code":"run_not_found"}}`}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(s.status)
	_, _ = io.WriteString(w, s.body)
}

func (g *Gateway) serveEvents(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.eventsCount++
	status, failure := g.eventsStatus, g.eventsFailure
	var ch chan string
	if status == http.StatusOK {
		ch = make(chan string)
		g.events = ch
	}
	g.mu.Unlock()
	if ch == nil {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, failure)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	for {
		select {
		case <-r.Context().Done():
			g.mu.Lock()
			if g.events == ch {
				g.events = nil
			}
			g.mu.Unlock()
			return
		case text, ok := <-ch:
			if !ok {
				return
			}
			_, _ = io.WriteString(w, text)
			_ = rc.Flush()
		}
	}
}

// Notifier is RecordingRunPushNotifier.
type Notifier struct {
	mu          sync.Mutex
	approvals   []hermesruns.DTO
	completions []hermesruns.DTO
}

func (n *Notifier) ApprovalRequested(_ context.Context, _ uuid.UUID, run hermesruns.DTO) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.approvals = append(n.approvals, run)
}

func (n *Notifier) RunFinished(_ context.Context, _ uuid.UUID, run hermesruns.DTO) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.completions = append(n.completions, run)
}

// Approvals is every approval push so far.
func (n *Notifier) Approvals() []hermesruns.DTO {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]hermesruns.DTO(nil), n.approvals...)
}

// Completions is every finished push so far.
func (n *Notifier) Completions() []hermesruns.DTO {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]hermesruns.DTO(nil), n.completions...)
}

// WaitFor blocks until cond holds, or fails the test after 5 s.
func WaitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
