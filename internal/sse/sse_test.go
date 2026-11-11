package sse

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

type payload struct {
	Zebra string `json:"zebra"`
	Apple string `json:"apple"`
}

// script answers one entry per poll; a nil entry is an empty poll.
type script struct {
	polls    [][]Event
	terminal func(poll int) bool
	failAt   int
	afters   []int64
	count    atomic.Int32
}

func (s *script) Events(_ context.Context, after int64) ([]Event, error) {
	n := int(s.count.Add(1))
	s.afters = append(s.afters, after)
	if s.failAt == n {
		return nil, errors.New("db down")
	}
	if n <= len(s.polls) {
		return s.polls[n-1], nil
	}
	return nil, nil
}

func (s *script) Terminal(context.Context) (bool, error) {
	if s.terminal == nil {
		return false, nil
	}
	return s.terminal(int(s.count.Load())), nil
}

func serve(t *testing.T, src Source, after int64, opts Options) (int, http.Header, string) {
	t.Helper()
	e := echo.New()
	e.GET("/s", func(c *echo.Context) error { return Serve(c, src, after, opts) })
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/s")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, string(b)
}

func fast(maxPolls int) Options {
	return Options{EventName: "ingestion", Interval: time.Millisecond, MaxPolls: maxPolls}
}

func TestIngestionOptions(t *testing.T) {
	require.Equal(t, Options{EventName: "ingestion", Interval: 2 * time.Second, MaxPolls: 360}, Ingestion())
}

func TestHeadersAndFrames(t *testing.T) {
	src := &script{
		polls:    [][]Event{{{ID: 7, Data: payload{"a/b<c>", "z"}}, {ID: 8, Data: payload{"x", "y"}}}},
		terminal: func(int) bool { return true },
	}
	status, header, body := serve(t, src, 0, fast(5))
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "text/event-stream", header.Get("Content-Type"))
	require.Equal(t, "no-cache", header.Get("Cache-Control"))
	require.Equal(t, "no", header.Get("X-Accel-Buffering"))
	// Struct order, "/" escaped, "<" not.
	require.Equal(t, "id: 7\nevent: ingestion\ndata: {\"zebra\":\"a\\/b<c>\",\"apple\":\"z\"}\n\n"+
		"id: 8\nevent: ingestion\ndata: {\"zebra\":\"x\",\"apple\":\"y\"}\n\n", body)
}

// Terminal is checked before the heartbeat, so a finished batch gets none.
func TestTerminalEndsWithoutHeartbeat(t *testing.T) {
	src := &script{terminal: func(int) bool { return true }}
	_, _, body := serve(t, src, 0, fast(5))
	require.Empty(t, body)
	require.EqualValues(t, 1, src.count.Load())
}

func TestHeartbeatOnlyOnEmptyPolls(t *testing.T) {
	src := &script{
		polls:    [][]Event{nil, {{ID: 1, Data: payload{}}}, nil},
		terminal: func(poll int) bool { return poll == 3 },
	}
	_, _, body := serve(t, src, 0, fast(9))
	require.Equal(t, ": heartbeat\n\nid: 1\nevent: ingestion\ndata: {\"zebra\":\"\",\"apple\":\"\"}\n\n", body)
}

func TestCursorAdvances(t *testing.T) {
	src := &script{polls: [][]Event{{{ID: 12, Data: 1}, {ID: 15, Data: 2}}, nil}, terminal: func(poll int) bool { return poll == 2 }}
	serve(t, src, 10, fast(9))
	require.Equal(t, []int64{10, 15}, src.afters)
}

func TestStopsAfterMaxPolls(t *testing.T) {
	src := &script{}
	_, _, body := serve(t, src, 0, fast(3))
	require.EqualValues(t, 3, src.count.Load())
	require.Equal(t, ": heartbeat\n\n: heartbeat\n\n: heartbeat\n\n", body)
}

// A failed poll drops the connection, so the client can't take it for a stream
// that finished.
func TestPollFailureAbortsTheConnection(t *testing.T) {
	src := &script{polls: [][]Event{{{ID: 1, Data: 1}}}, failAt: 2}
	e := echo.New()
	e.GET("/s", func(c *echo.Context) error { return Serve(c, src, 0, fast(9)) })
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/s")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.Error(t, err, "the chunked body is cut, not finished")
	require.Contains(t, string(b), "id: 1\n")
}

func TestClientGoneEndsTheStream(t *testing.T) {
	src := &script{}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	e := echo.New()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	go func() { done <- Serve(c, src, 0, Options{EventName: "x", Interval: time.Hour, MaxPolls: 9}) }()
	require.Eventually(t, func() bool { return src.count.Load() == 1 }, 5*time.Second, time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not stop")
	}
}

func TestCursorPrecedence(t *testing.T) {
	cases := []struct {
		query, header string
		want          int64
	}{
		{"", "", 0},
		{"after=5", "", 5},
		{"", "9", 9},
		{"after=5", "9", 5},
		{"after=nope", "9", 9}, // an unparseable query falls through to the header
		{"after=nope", "nope", 0},
		{"after=", "9", 9},
		{"after=+5", "", 5}, // a literal "+" stays, and Int64 accepts it
		{"x=1&after=-2&after=3", "", -2},
		{"after", "9", 9}, // no "=": the value is "", which isn't an Int64
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil)
		if tc.header != "" {
			req.Header.Set("Last-Event-ID", tc.header)
		}
		c := echo.New().NewContext(req, httptest.NewRecorder())
		require.Equal(t, tc.want, Cursor(c), "%q %q", tc.query, tc.header)
	}
}

// workflowLike is the shape of WorkflowEventsSSEResponse: named per event, a
// different comment, no poll cap, and it swallows its own failures.
func workflowLike() Options {
	return Options{Interval: time.Millisecond, Heartbeat: ": keep-alive\n\n", UntilDrained: true, FinishOnError: true}
}

func TestEventNameComesFromTheEvent(t *testing.T) {
	src := &script{
		polls:    [][]Event{{{ID: 1, Name: "runQueued", Data: payload{}}, {ID: 2, Data: payload{}}}},
		terminal: func(int) bool { return true },
	}
	opts := workflowLike()
	opts.EventName = "fallback"
	_, _, body := serve(t, src, 0, opts)
	require.Contains(t, body, "id: 1\nevent: runQueued\n")
	require.Contains(t, body, "id: 2\nevent: fallback\n")
}

func TestHeartbeatOption(t *testing.T) {
	src := &script{terminal: func(poll int) bool { return poll >= 2 }}
	_, _, body := serve(t, src, 0, workflowLike())
	require.Equal(t, ": keep-alive\n\n", body)
}

func TestNoPollCapRunsUntilTerminal(t *testing.T) {
	src := &script{terminal: func(poll int) bool { return poll >= 500 }}
	_, _, _ = serve(t, src, 0, workflowLike())
	require.EqualValues(t, 500, src.count.Load())
}

func TestUntilDrainedSendsTheEventsATerminalSourceStillHas(t *testing.T) {
	src := &script{
		polls:    [][]Event{{{ID: 1, Name: "a", Data: payload{}}}, {{ID: 2, Name: "b", Data: payload{}}}},
		terminal: func(int) bool { return true },
	}
	_, _, body := serve(t, src, 0, workflowLike())
	require.Contains(t, body, "id: 1\n")
	require.Contains(t, body, "id: 2\n")
	require.NotContains(t, body, "keep-alive")
	require.EqualValues(t, 3, src.count.Load(), "ends on the first empty round")
}

func TestUntilDrainedDoesNotWaitAfterARoundWithEvents(t *testing.T) {
	src := &script{
		polls:    [][]Event{{{ID: 1, Name: "a", Data: payload{}}}, {{ID: 2, Name: "b", Data: payload{}}}},
		terminal: func(int) bool { return true },
	}
	opts := workflowLike()
	opts.Interval = time.Hour
	_, _, body := serve(t, src, 0, opts)
	require.Contains(t, body, "id: 2\n")
}

func TestFinishOnErrorEndsTheStreamNormally(t *testing.T) {
	src := &script{polls: [][]Event{{{ID: 1, Name: "a", Data: payload{}}}}, failAt: 2}
	code, _, body := serve(t, src, 0, workflowLike())
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, body, "id: 1\n")
}

func TestFinishOnErrorCoversAnEncodingFailure(t *testing.T) {
	src := &script{polls: [][]Event{{{ID: 1, Name: "a", Data: make(chan int)}}}}
	code, _, body := serve(t, src, 0, workflowLike())
	require.Equal(t, http.StatusOK, code)
	require.Empty(t, body)
}

// A body left unread stops net/http watching the connection, so the handler
// would poll for a client that has gone.
func TestClientWithARequestBodyGoneEndsTheStream(t *testing.T) {
	src := &script{}
	done := make(chan struct{})
	e := echo.New()
	e.POST("/s", func(c *echo.Context) error {
		defer close(done)
		return Serve(c, src, 0, workflowLike())
	})
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/s", strings.NewReader("unread body"))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler kept polling after the client left")
	}
}

func TestHeaderFirstCursorPrecedence(t *testing.T) {
	at := func(target, header string) int64 {
		e := echo.New()
		r := httptest.NewRequest(http.MethodGet, target, nil)
		if header != "" {
			r.Header.Set("Last-Event-ID", header)
		}
		return HeaderFirstCursor(e.NewContext(r, httptest.NewRecorder()))
	}
	require.EqualValues(t, 7, at("/s?after=3", "7"))
	require.EqualValues(t, 3, at("/s?after=3", "junk"))
	require.EqualValues(t, 3, at("/s?after=3", ""))
	require.EqualValues(t, 0, at("/s?after=junk", "junk"))
	require.EqualValues(t, 0, at("/s", ""))
}
