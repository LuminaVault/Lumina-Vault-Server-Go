package sse

import (
	"bufio"
	"context"
	"errors"
	"io"
	"lumina/internal/config"
	"lumina/internal/httpserver"
	"lumina/internal/wire"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// tokenEvent stands in for QueryStreamEvent: the `type` is in the JSON.
type tokenEvent struct {
	Type    string `json:"type"`
	Payload string `json:"payload,omitempty"`
}

type errorEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func errorFrame(err error) any {
	return errorEvent{Type: "error", Message: "upstream failure: " + err.Error()}
}

func recorded(t *testing.T, h echo.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	e.GET("/s", h)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/s", nil))
	return rec
}

func events(evs ...tokenEvent) func(context.Context, func(tokenEvent) bool) error {
	return func(_ context.Context, yield func(tokenEvent) bool) error {
		for _, ev := range evs {
			yield(ev)
		}
		return nil
	}
}

func TestServeStreamFrames(t *testing.T) {
	rec := recorded(t, func(c *echo.Context) error {
		s := Produce(c.Request().Context(), events(tokenEvent{"token", "hello"}, tokenEvent{Type: "done"}))
		return ServeStream(c, s, errorFrame)
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	require.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	require.Equal(t, "no", rec.Header().Get("X-Accel-Buffering"))
	require.Equal(t, `data: {"type":"token","payload":"hello"}`+"\n\n"+`data: {"type":"done"}`+"\n\n", rec.Body.String(),
		"data: only, and `done` has no payload key")
}

// Foundation's encoder, with .iso8601 dates, or the client throws on the first
// Date-bearing frame.
func TestServeStreamEncodesAsFoundation(t *testing.T) {
	type source struct {
		URL string    `json:"url"`
		At  wire.Time `json:"createdAt"`
		HTM string    `json:"html"`
	}
	at := time.Date(2026, 11, 5, 12, 30, 0, 999, time.FixedZone("x", 3600))
	frame, err := DataFrame(source{URL: "a/b", At: wire.Time{Time: at}, HTM: "<b>&"})
	require.NoError(t, err)
	require.Equal(t, `data: {"url":"a\/b","createdAt":"2026-11-05T11:30:00Z","html":"<b>&"}`+"\n\n", string(frame))
}

func TestServeStreamFailureIsAFinalErrorFrame(t *testing.T) {
	rec := recorded(t, func(c *echo.Context) error {
		s := Produce(c.Request().Context(), func(_ context.Context, yield func(tokenEvent) bool) error {
			yield(tokenEvent{"token", "partial"})
			return errors.New("boom")
		})
		return ServeStream(c, s, errorFrame)
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, `data: {"type":"token","payload":"partial"}`+"\n\n"+
		`data: {"type":"error","message":"upstream failure: boom"}`+"\n\n", rec.Body.String(),
		"events before the failure are delivered first")
}

func TestServeStreamEventThatWontEncodeEndsWithErrorFrame(t *testing.T) {
	rec := recorded(t, func(c *echo.Context) error {
		s := Produce(c.Request().Context(), func(_ context.Context, yield func(any) bool) error {
			yield(tokenEvent{"token", "a"})
			yield(math.Inf(1))
			yield(tokenEvent{"token", "never"})
			return nil
		})
		return ServeStream(c, s, func(error) any { return errorEvent{Type: "error", Message: "upstream failure"} })
	})
	require.Equal(t, `data: {"type":"token","payload":"a"}`+"\n\n"+`data: {"type":"error","message":"upstream failure"}`+"\n\n", rec.Body.String())
}

func TestServeStreamHasNoHeartbeat(t *testing.T) {
	rec := recorded(t, func(c *echo.Context) error {
		s := Produce(c.Request().Context(), func(_ context.Context, yield func(tokenEvent) bool) error {
			time.Sleep(50 * time.Millisecond)
			yield(tokenEvent{Type: "done"})
			return nil
		})
		return ServeStream(c, s, errorFrame)
	})
	require.Equal(t, `data: {"type":"done"}`+"\n\n", rec.Body.String(), "no comment frames, however long it idles")
}

func TestProducerPanicEndsTheStreamAsAnError(t *testing.T) {
	rec := recorded(t, func(c *echo.Context) error {
		s := Produce(c.Request().Context(), func(context.Context, func(tokenEvent) bool) error { panic("nil map") })
		return ServeStream(c, s, func(error) any { return errorEvent{Type: "error", Message: "upstream failure"} })
	})
	require.Equal(t, `data: {"type":"error","message":"upstream failure"}`+"\n\n", rec.Body.String())
}

func TestNamedFrames(t *testing.T) {
	rec := recorded(t, func(c *echo.Context) error {
		s := Produce(c.Request().Context(), events(tokenEvent{"token", "a/b"}))
		return ServeNamedStream(c, s, "agent.room", DefaultKeepAlive)
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	require.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	require.Equal(t, "no", rec.Header().Get("X-Accel-Buffering"))
	require.Equal(t, "event: agent.room\ndata: {\"type\":\"token\",\"payload\":\"a\\/b\"}\n\n", rec.Body.String())
	require.Equal(t, 20*time.Second, DefaultKeepAlive)
}

func TestNamedStreamFailureEndsWithNoFrame(t *testing.T) {
	rec := recorded(t, func(c *echo.Context) error {
		s := Produce(c.Request().Context(), func(_ context.Context, yield func(tokenEvent) bool) error {
			yield(tokenEvent{Type: "one"})
			return errors.New("logged by the producer")
		})
		return ServeNamedStream(c, s, "e", time.Hour)
	})
	require.Equal(t, "event: e\ndata: {\"type\":\"one\"}\n\n", rec.Body.String(), "the payload type owns its terminal event")
}

func TestNamedStreamSkipsAnEventThatWontEncode(t *testing.T) {
	rec := recorded(t, func(c *echo.Context) error {
		s := Produce(c.Request().Context(), func(_ context.Context, yield func(any) bool) error {
			yield(math.Inf(1))
			yield(tokenEvent{Type: "after"})
			return nil
		})
		return ServeNamedStream(c, s, "e", time.Hour)
	})
	require.Equal(t, "event: e\ndata: {\"type\":\"after\"}\n\n", rec.Body.String())
}

func TestKeepAliveOnAnIdleNamedStream(t *testing.T) {
	rec := recorded(t, func(c *echo.Context) error {
		s := Produce(c.Request().Context(), func(_ context.Context, yield func(tokenEvent) bool) error {
			time.Sleep(90 * time.Millisecond)
			yield(tokenEvent{Type: "late"})
			return nil
		})
		return ServeNamedStream(c, s, "e", 20*time.Millisecond)
	})
	body := rec.Body.String()
	require.GreaterOrEqual(t, strings.Count(body, ": keepalive\n\n"), 2, body)
	require.True(t, strings.HasSuffix(body, "event: e\ndata: {\"type\":\"late\"}\n\n"), body)
}

// served runs h on a real server. A recorder can neither stream nor disconnect.
func served(t *testing.T, h echo.HandlerFunc) *httptest.Server {
	t.Helper()
	e := echo.New()
	e.GET("/s", h)
	e.POST("/s", h)
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv
}

func TestFramesReachTheClientAsTheyAreYielded(t *testing.T) {
	next := make(chan struct{})
	srv := served(t, func(c *echo.Context) error {
		s := Produce(c.Request().Context(), func(_ context.Context, yield func(tokenEvent) bool) error {
			yield(tokenEvent{Type: "first"})
			<-next
			yield(tokenEvent{Type: "second"})
			return nil
		})
		return ServeStream(c, s, errorFrame)
	})
	resp, err := http.Get(srv.URL + "/s") //nolint:noctx // a test
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	r := bufio.NewReader(resp.Body)

	line, err := r.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, `data: {"type":"first"}`+"\n", line, "readable before the producer has finished")
	close(next)
	rest, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, "\n"+`data: {"type":"second"}`+"\n\n", string(rest), "and it ends cleanly")
}

func TestDisconnectEndsAnIdleStreamAndItsProducer(t *testing.T) {
	writers := map[string]func(*echo.Context, *Stream[tokenEvent]) error{
		"ServeStream":      func(c *echo.Context, s *Stream[tokenEvent]) error { return ServeStream(c, s, errorFrame) },
		"ServeNamedStream": func(c *echo.Context, s *Stream[tokenEvent]) error { return ServeNamedStream(c, s, "e", time.Hour) },
	}
	for name, writer := range writers {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(name+" "+method, func(t *testing.T) {
				handlerDone, producerCancelled := make(chan struct{}), make(chan struct{})
				// The body is never read: the first flush has to read it, or net/http
				// wouldn't watch the connection and the handler would never end.
				srv := served(t, func(c *echo.Context) error {
					defer close(handlerDone)
					s := Produce(c.Request().Context(), func(ctx context.Context, yield func(tokenEvent) bool) error {
						yield(tokenEvent{Type: "first"})
						<-ctx.Done()
						close(producerCancelled)
						return ctx.Err()
					})
					return writer(c, s)
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, method, srv.URL+"/s", strings.NewReader(`{"content":"hi"}`))
				require.NoError(t, err)
				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				defer func() { _ = resp.Body.Close() }()
				_, err = bufio.NewReader(resp.Body).ReadString('\n')
				require.NoError(t, err)

				cancel()
				for name, ch := range map[string]chan struct{}{"handler": handlerDone, "producer": producerCancelled} {
					select {
					case <-ch:
					case <-time.After(5 * time.Second):
						t.Fatal(name + " never saw the disconnect")
					}
				}
			})
		}
	}
}

// Yield doesn't block on a slow client and reports false after the reader has left,
// so a producer can't wedge on either.
func TestYieldNeverBlocks(t *testing.T) {
	started := make(chan struct{})
	var afterStop atomic.Bool
	finished := make(chan struct{})
	s := Produce(context.Background(), func(ctx context.Context, yield func(int) bool) error {
		defer close(finished)
		for i := range 10_000 {
			if !yield(i) {
				return errors.New("yield refused while the reader was there")
			}
		}
		close(started)
		<-ctx.Done()
		afterStop.Store(!yield(1))
		return nil
	})
	<-started
	s.stop()
	<-finished
	require.True(t, afterStop.Load(), "yield is false once the reader has gone")
}

func TestStreamDeliversInOrderBeforeFinishing(t *testing.T) {
	s := Produce(context.Background(), func(_ context.Context, yield func(int) bool) error {
		for i := range 1000 {
			yield(i)
		}
		return errors.New("after the queue")
	})
	defer s.stop()
	var got []int
	err := pump(context.Background(), s, 0, nil, func(i int) bool {
		got = append(got, i)
		return true
	})
	require.EqualError(t, err, "after the queue")
	require.Len(t, got, 1000)
	for i, v := range got {
		require.Equal(t, i, v)
	}
}

type streamingModule struct{ next chan struct{} }

func (m streamingModule) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	r.POST("/v1/test/push", func(c *echo.Context) error {
		s := Produce(c.Request().Context(), func(_ context.Context, yield func(tokenEvent) bool) error {
			yield(tokenEvent{Type: "first"})
			<-m.next
			return nil
		})
		return ServeStream(c, s, errorFrame)
	})
	r.Streaming(http.MethodPost, "/v1/test/push")
}

// Through the real middleware chain, with a client that takes gzip and a frame below
// gzip's MinLength: Router.Streaming is what gets it out before the stream ends.
func TestFrameGetsThroughTheServerChainUncompressed(t *testing.T) {
	m := streamingModule{next: make(chan struct{})}
	srv := httptest.NewServer(httpserver.New(config.Config{}, httpserver.Deps{Modules: []httpserver.Module{m}}))
	defer srv.Close()
	defer close(m.next)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/test/push", nil) //nolint:noctx // a test
	require.NoError(t, err)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Empty(t, resp.Header.Get("Content-Encoding"))
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, `data: {"type":"first"}`+"\n", line)
}
