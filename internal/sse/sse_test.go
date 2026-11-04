package sse

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
