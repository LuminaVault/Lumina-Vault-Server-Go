package posthog

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// stub is a PostHog stand-in that records every request. No test talks to a
// real PostHog or uses a real project token.
type stub struct {
	*httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	reqs   []*http.Request
}

func newStub(t *testing.T, status int) *stub {
	t.Helper()
	s := &stub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, b)
		s.reqs = append(s.reqs, r)
		s.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(s.Close)
	return s
}

type wireEvent struct {
	Event      string         `json:"event"`
	DistinctID string         `json:"distinct_id"`
	Timestamp  string         `json:"timestamp"`
	Properties map[string]any `json:"properties"`
}

type wireBatch struct {
	APIKey string      `json:"api_key"`
	Batch  []wireEvent `json:"batch"`
}

func (s *stub) batches(t *testing.T) []wireBatch {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]wireBatch, 0, len(s.bodies))
	for _, b := range s.bodies {
		var w wireBatch
		require.NoError(t, json.Unmarshal(b, &w))
		out = append(out, w)
	}
	return out
}

func (s *stub) eventNames(t *testing.T) []string {
	t.Helper()
	names := []string{}
	for _, b := range s.batches(t) {
		for _, e := range b.Batch {
			names = append(names, e.Event)
		}
	}
	return names
}

func (s *stub) request(i int) (*http.Request, []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[i], s.bodies[i]
}

func (c *Client) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue)
}

// PostHogHTTPClientTests: "capture URL is derived from the host".
func TestCaptureURL(t *testing.T) {
	require.Equal(t, "https://eu.i.posthog.com/capture", captureURL("https://eu.i.posthog.com"))
	require.Equal(t, "https://eu.i.posthog.com/capture", captureURL("https://eu.i.posthog.com/"))
	require.Equal(t, "https://ph.example.com/capture", captureURL(" https://ph.example.com/capture "))
}

// PostHogHTTPClientTests: "flush posts one batch with api_key, distinct_id, timestamp and $lib".
func TestFlushPostsOneBatch(t *testing.T) {
	s := newStub(t, http.StatusOK)
	c := New("phc_test", s.URL, Options{})
	c.Capture("user_registered", "user-1", map[string]any{"auth_method": "password"})
	c.Capture("user_logged_in", "", map[string]any{"mfa_required": true, "grounded_hits": 3})
	require.Equal(t, 2, c.pending())

	c.Flush(context.Background())

	require.Zero(t, c.pending())
	batches := s.batches(t)
	require.Len(t, batches, 1)
	req, _ := s.request(0)
	require.Equal(t, "/capture", req.URL.Path)
	require.Equal(t, http.MethodPost, req.Method)
	require.Equal(t, "application/json", req.Header.Get("Content-Type"))
	require.Equal(t, "luminavault-server", req.Header.Get("User-Agent"))
	require.Equal(t, "phc_test", batches[0].APIKey)
	require.Len(t, batches[0].Batch, 2)

	first := batches[0].Batch[0]
	require.Equal(t, "user_registered", first.Event)
	require.Equal(t, "user-1", first.DistinctID)
	require.Equal(t, "password", first.Properties["auth_method"])
	require.Equal(t, "luminavault-server", first.Properties["$lib"])
	require.Contains(t, first.Timestamp, "T")
	require.Equal(t, byte('Z'), first.Timestamp[len(first.Timestamp)-1])

	second := batches[0].Batch[1]
	require.Equal(t, "luminavault-server", second.DistinctID, "no distinct ID means the server itself")
	require.Equal(t, true, second.Properties["mfa_required"])
	require.Equal(t, 3.0, second.Properties["grounded_hits"])
}

// The exact bytes: sorted keys, no HTML escaping, millisecond UTC timestamp.
func TestWireBytesMatchSwift(t *testing.T) {
	s := newStub(t, http.StatusOK)
	at := time.Date(2026, 11, 4, 10, 11, 12, 345678901, time.FixedZone("", 3600))
	c := New("phc_test", s.URL, Options{Now: func() time.Time { return at }})
	c.Capture("health_events_synced", "4EA10000-0000-4000-8000-0000000000A1", map[string]any{
		"skipped_count": 0, "dominant_type": "a<b>&c/d", "inserted_count": 2,
	})
	c.Flush(context.Background())
	_, body := s.request(0)
	require.Equal(t, `{"api_key":"phc_test","batch":[{"distinct_id":"4EA10000-0000-4000-8000-0000000000A1",`+
		`"event":"health_events_synced","properties":{"$lib":"luminavault-server","dominant_type":"a<b>&c/d",`+
		`"inserted_count":2,"skipped_count":0},"timestamp":"2026-11-04T09:11:12.345Z"}]}`, string(body))
}

// PostHogHTTPClientTests: "flush splits the queue into maxBatchSize posts".
func TestFlushSplitsIntoBatches(t *testing.T) {
	s := newStub(t, http.StatusOK)
	c := New("phc_test", s.URL, Options{MaxBatchSize: 3})
	want := []string{}
	for _, name := range []string{"e0", "e1", "e2", "e3", "e4", "e5", "e6"} {
		c.Capture(name, "", nil)
		want = append(want, name)
	}
	c.Flush(context.Background())
	sizes := []int{}
	for _, b := range s.batches(t) {
		sizes = append(sizes, len(b.Batch))
	}
	require.Equal(t, []int{3, 3, 1}, sizes)
	require.Equal(t, want, s.eventNames(t))
	require.Zero(t, c.pending())
}

// PostHogHTTPClientTests: "queue is bounded and overflow drops new events".
func TestQueueIsBounded(t *testing.T) {
	s := newStub(t, http.StatusOK)
	c := New("phc_test", s.URL, Options{MaxBatchSize: 2, MaxQueueDepth: 2})
	c.Capture("kept_1", "", nil)
	c.Capture("kept_2", "", nil)
	c.Capture("dropped", "", nil)
	require.Equal(t, 2, c.pending())
	c.Flush(context.Background())
	require.Equal(t, []string{"kept_1", "kept_2"}, s.eventNames(t))
}

// PostHogHTTPClientTests: "transport failures drop the batch without throwing".
func TestTransportFailureDropsBatch(t *testing.T) {
	s := newStub(t, http.StatusOK)
	url := s.URL
	s.Close() // connection refused
	c := New("phc_test", url, Options{})
	c.Capture("lost", "", nil)
	c.Flush(context.Background())
	require.Zero(t, c.pending())
}

// A non-2xx is logged and the batch is gone: no retry.
func TestRejectedBatchIsNotRetried(t *testing.T) {
	s := newStub(t, http.StatusUnauthorized)
	c := New("phc_test", s.URL, Options{})
	c.Capture("rejected", "", nil)
	c.Flush(context.Background())
	c.Flush(context.Background())
	require.Len(t, s.batches(t), 1)
	require.Zero(t, c.pending())
}

// PostHogHTTPClientTests: "run flushes on cancellation".
func TestRunFlushesOnCancellation(t *testing.T) {
	s := newStub(t, http.StatusOK)
	c := New("phc_test", s.URL, Options{FlushInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	c.Capture("before_shutdown", "", nil)
	cancel()
	<-done
	require.Equal(t, []string{"before_shutdown"}, s.eventNames(t))
}

func TestRunFlushesOnTimer(t *testing.T) {
	s := newStub(t, http.StatusOK)
	c := New("phc_test", s.URL, Options{FlushInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	c.Capture("ticked", "", nil)
	require.Eventually(t, func() bool { return len(s.batches(t)) == 1 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	<-done
	require.Equal(t, []string{"ticked"}, s.eventNames(t))
}

// PostHogHTTPClientTests: "factory returns nil when the token or host is absent".
func TestNewNeedsTokenAndHost(t *testing.T) {
	require.Nil(t, New("", "https://ph.example.com", Options{}))
	require.Nil(t, New("   ", "https://ph.example.com", Options{}))
	require.Nil(t, New("phc_x", "  ", Options{}))
	c := New(" phc_x ", "https://ph.example.com/", Options{})
	require.NotNil(t, c)
	require.Equal(t, "https://ph.example.com/capture", c.captureURL)
	require.Equal(t, "phc_x", c.apiKey, "the token is trimmed before it reaches the wire")
}

// PostHogHTTPClientTests: "PostHogAnalytics is a no-op without an installed sink
// and forwards with one". Go has no global sink: the "no sink" is a nil client,
// which is what an unset token produces, and a configured client forwards.
func TestNilClientDoesNothing(t *testing.T) {
	var c *Client
	require.NotPanics(t, func() {
		c.Capture("ignored", "u", map[string]any{"k": "v"})
		c.Flush(context.Background())
		c.Run(context.Background())
	})

	s := newStub(t, http.StatusOK)
	sink := New("phc_test", s.URL, Options{})
	sink.Capture("forwarded", "u", map[string]any{"has_space": false})
	require.Equal(t, 1, sink.pending())
	sink.Flush(context.Background())
	require.Equal(t, []string{"forwarded"}, s.eventNames(t))
}

// Capture must not wait on PostHog: with a stub that never answers, it still
// returns at once while a flush is stuck in flight.
func TestCaptureDoesNotBlockOnASlowServer(t *testing.T) {
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(s.Close)
	t.Cleanup(func() { close(release) })
	c := New("phc_test", s.URL, Options{})
	c.Capture("first", "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Flush(ctx) }()
	require.Eventually(t, func() bool { return c.pending() == 0 }, 5*time.Second, time.Millisecond)

	start := time.Now()
	c.Capture("second", "", nil)
	require.Less(t, time.Since(start), time.Second)
	require.Equal(t, 1, c.pending())
	cancel()
	<-done
}

// A cancelled flush leaves events queued rather than posting into a dead
// context and losing them.
func TestFlushStopsWhenContextEnds(t *testing.T) {
	s := newStub(t, http.StatusOK)
	c := New("phc_test", s.URL, Options{})
	c.Capture("kept", "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Flush(ctx)
	require.Equal(t, 1, c.pending())
	require.Empty(t, s.batches(t))
}

// Capture copies props, and $lib can't be overridden by a caller.
func TestCaptureCopiesProps(t *testing.T) {
	s := newStub(t, http.StatusOK)
	c := New("phc_test", s.URL, Options{})
	props := map[string]any{"k": "before", "$lib": "other"}
	c.Capture("copied", "", props)
	props["k"] = "after"
	c.Flush(context.Background())
	p := s.batches(t)[0].Batch[0].Properties
	require.Equal(t, "before", p["k"])
	require.Equal(t, "luminavault-server", p["$lib"])
}

// PostHogHTTPClientTests: "JSONValue bridges loosely typed property values".
func TestJSONValue(t *testing.T) {
	require.Equal(t, "s", jsonValue("s"))
	require.Equal(t, 3, jsonValue(3))
	require.Equal(t, 1.5, jsonValue(1.5))
	require.Equal(t, true, jsonValue(true))
	id := uuid.MustParse("11111111-2222-3333-4444-55555555abcd")
	require.Equal(t, "11111111-2222-3333-4444-55555555ABCD", jsonValue(id))
	require.Equal(t, []any{"a", 1}, jsonValue([]any{"a", 1}))
	require.Equal(t, map[string]any{"k": true}, jsonValue(map[string]any{"k": true}))
	require.Nil(t, jsonValue(nil))
	require.Equal(t, "2026-11-04T10:00:00.000Z", jsonValue(time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)))

	// Nested values are bridged too, as `$set` in Auth needs.
	nested := jsonValue(map[string]any{"$set": map[string]any{"id": id}})
	require.Equal(t, map[string]any{"$set": map[string]any{"id": "11111111-2222-3333-4444-55555555ABCD"}}, nested)

	// Anything else is stringified rather than failing the batch.
	require.Equal(t, "{1}", jsonValue(struct{ A int }{1}))
	require.Equal(t, "NaN", jsonValue(math.NaN()))
	require.Equal(t, "+Inf", jsonValue(math.Inf(1)))
	_, err := encode(jsonValue(map[string]any{"ch": make(chan int), "nan": math.NaN()}))
	require.NoError(t, err)
}
