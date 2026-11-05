package ratelimit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/ratelimit"
	"lumina/internal/wire"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// gate holds a handler until the test lets it go.
type gate struct {
	entered chan struct{}
	release chan struct{}
}

func newGate() *gate { return &gate{entered: make(chan struct{}, 16), release: make(chan struct{})} }

// server is a real one: a recorder has no client to disconnect.
func server(t *testing.T, h echo.HandlerFunc, mw ...echo.MiddlewareFunc) *httptest.Server {
	t.Helper()
	e := echo.New()
	e.HTTPErrorHandler = wire.HTTPErrorHandler
	e.GET("/stream", h, mw...)
	e.POST("/stream", h, mw...)
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv
}

func byHeader(c *echo.Context) string { return c.Request().Header.Get("X-Key") }

func call(ctx context.Context, url, method, key string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url+"/stream", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Key", key)
	return http.DefaultClient.Do(req)
}

func status(t *testing.T, url, key string) int {
	t.Helper()
	resp, err := call(context.Background(), url, http.MethodGet, key, nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}

// waitFree retries a probe: a release follows the disconnect, it doesn't precede it.
func waitFree(t *testing.T, url, key string) {
	t.Helper()
	require.Eventually(t, func() bool { return status(t, url, key) == http.StatusOK }, 5*time.Second, 10*time.Millisecond,
		"the slot never came back")
}

// holding blocks a request marked X-Hold until the gate opens or the client leaves.
// An unmarked one answers straight away, so a probe can ask whether a slot is free.
func holding(g *gate) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if c.Request().Header.Get("X-Hold") == "" {
			return c.NoContent(http.StatusOK)
		}
		g.entered <- struct{}{}
		select {
		case <-g.release:
		case <-c.Request().Context().Done():
		}
		return c.NoContent(http.StatusOK)
	}
}

func hold(ctx context.Context, t *testing.T, url, key string) chan error {
	t.Helper()
	done := make(chan error, 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/stream", nil)
	require.NoError(t, err)
	req.Header.Set("X-Key", key)
	req.Header.Set("X-Hold", "1")
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			err = resp.Body.Close()
		}
		done <- err
	}()
	return done
}

// RateLimitMiddlewareTests.swift's "rejects a second concurrent stream", for query and
// conversation: the same assertion, so one test.
func TestSecondConcurrentRequestIsRejected(t *testing.T) {
	g := newGate()
	srv := server(t, holding(g), ratelimit.InFlightLimit(1, byHeader))
	first := hold(context.Background(), t, srv.URL, "u")
	<-g.entered

	resp, err := call(context.Background(), srv.URL, http.MethodGet, "u", nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "1", resp.Header.Get("Retry-After"))
	var body map[string]map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Equal(t, "too many concurrent streams", body["error"]["message"])

	require.Equal(t, http.StatusOK, status(t, srv.URL, "other"), "another caller has its own count")

	close(g.release)
	require.NoError(t, <-first)
	waitFree(t, srv.URL, "u")
}

func TestUpToMaxConcurrentRun(t *testing.T) {
	g := newGate()
	srv := server(t, holding(g), ratelimit.InFlightLimit(3, byHeader))
	var held []chan error
	for range 3 {
		held = append(held, hold(context.Background(), t, srv.URL, "u"))
		<-g.entered
	}
	require.Equal(t, http.StatusTooManyRequests, status(t, srv.URL, "u"))
	close(g.release)
	for _, h := range held {
		require.NoError(t, <-h)
	}
}

func TestMaxBelowOneCountsAsOne(t *testing.T) {
	g := newGate()
	srv := server(t, holding(g), ratelimit.InFlightLimit(0, byHeader))
	first := hold(context.Background(), t, srv.URL, "u")
	<-g.entered
	require.Equal(t, http.StatusTooManyRequests, status(t, srv.URL, "u"))
	close(g.release)
	require.NoError(t, <-first)
}

// "Each group gets its own store, so a chat stream and a query stream are counted apart."
func TestEachMountCountsApart(t *testing.T) {
	g := newGate()
	chat := server(t, holding(g), ratelimit.InFlightLimit(1, byHeader))
	query := server(t, holding(g), ratelimit.InFlightLimit(1, byHeader))
	first := hold(context.Background(), t, chat.URL, "u")
	<-g.entered
	require.Equal(t, http.StatusOK, status(t, query.URL, "u"))
	require.Equal(t, http.StatusTooManyRequests, status(t, chat.URL, "u"))
	close(g.release)
	require.NoError(t, <-first)
}

func TestSlotComesBackOnDisconnect(t *testing.T) {
	t.Run("GET", func(t *testing.T) {
		g := newGate()
		srv := server(t, holding(g), ratelimit.InFlightLimit(1, byHeader))
		ctx, cancel := context.WithCancel(context.Background())
		first := hold(ctx, t, srv.URL, "u")
		<-g.entered
		require.Equal(t, http.StatusTooManyRequests, status(t, srv.URL, "u"))
		cancel()
		require.ErrorIs(t, <-first, context.Canceled)
		waitFree(t, srv.URL, "u")
	})

	t.Run("POST whose handler read the body, as wire.Bind does", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		h := func(c *echo.Context) error {
			if c.Request().Method == http.MethodGet {
				return c.NoContent(http.StatusOK)
			}
			if _, err := wire.ReadBody(c); err != nil {
				return err
			}
			entered <- struct{}{}
			<-c.Request().Context().Done()
			return nil
		}
		srv := server(t, h, ratelimit.InFlightLimit(1, byHeader))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			if resp, err := call(ctx, srv.URL, http.MethodPost, "u", []byte(`{"content":"hi"}`)); err == nil {
				_ = resp.Body.Close()
			}
		}()
		<-entered
		require.Equal(t, http.StatusTooManyRequests, status(t, srv.URL, "u"))
		cancel()
		<-done
		waitFree(t, srv.URL, "u")
	})

	t.Run("a handler that ignores the disconnect can't keep it", func(t *testing.T) {
		entered, stuck := make(chan struct{}, 1), make(chan struct{})
		h := func(c *echo.Context) error {
			if c.Request().Header.Get("X-Hold") == "" {
				return c.NoContent(http.StatusOK)
			}
			entered <- struct{}{}
			<-stuck
			return nil
		}
		srv := server(t, h, ratelimit.InFlightLimit(1, byHeader))
		t.Cleanup(func() { close(stuck) }) // runs before srv.Close, which waits for the handler
		ctx, cancel := context.WithCancel(context.Background())
		first := hold(ctx, t, srv.URL, "u")
		<-entered
		require.Equal(t, http.StatusTooManyRequests, status(t, srv.URL, "u"))
		cancel()
		require.ErrorIs(t, <-first, context.Canceled)
		waitFree(t, srv.URL, "u")
	})
}

func TestSlotComesBackHoweverTheHandlerEnds(t *testing.T) {
	for name, end := range map[string]func() error{
		"a clean return": func() error { return nil },
		"an error":       func() error { return errors.New("boom") },
		"a client error": func() error { return wire.NewError(http.StatusBadRequest, "no") },
		"an abort":       func() error { panic(http.ErrAbortHandler) },
	} {
		t.Run(name, func(t *testing.T) {
			h := func(c *echo.Context) error {
				if c.Request().Header.Get("X-Hold") == "" {
					return c.NoContent(http.StatusOK)
				}
				return end()
			}
			srv := server(t, h, ratelimit.InFlightLimit(1, byHeader))
			if err := <-hold(context.Background(), t, srv.URL, "u"); err != nil {
				require.Contains(t, err.Error(), "EOF", "only the abort drops the connection")
			}
			waitFree(t, srv.URL, "u")
		})
	}
}

// A rejected request must not release the slot of the one that holds it.
func TestRejectionDoesNotReleaseTheHolder(t *testing.T) {
	g := newGate()
	srv := server(t, holding(g), ratelimit.InFlightLimit(1, byHeader))
	first := hold(context.Background(), t, srv.URL, "u")
	<-g.entered
	for range 5 {
		require.Equal(t, http.StatusTooManyRequests, status(t, srv.URL, "u"))
	}
	close(g.release)
	require.NoError(t, <-first)
}

func TestConcurrencyNeverExceedsTheCeiling(t *testing.T) {
	var running, peak atomic.Int32
	h := func(c *echo.Context) error {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		running.Add(-1)
		return c.NoContent(http.StatusOK)
	}
	srv := server(t, h, ratelimit.InFlightLimit(2, byHeader))
	var wg sync.WaitGroup
	var served atomic.Int32
	for range 40 {
		wg.Go(func() {
			resp, err := call(context.Background(), srv.URL, http.MethodGet, "u", nil)
			if err != nil {
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				served.Add(1)
			}
		})
	}
	wg.Wait()
	require.LessOrEqual(t, peak.Load(), int32(2))
	require.Positive(t, served.Load())
	require.Equal(t, http.StatusOK, status(t, srv.URL, "u"), "nothing leaked")
}

type everyoneExists struct{}

func (everyoneExists) Exists(context.Context, uuid.UUID) (bool, error) { return true, nil }

// RateLimitMiddlewareTests.swift's two keying tests, through the real authenticator.
func TestUserOrIPKeying(t *testing.T) {
	keys := auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
	policy := ratelimit.Policy{Name: "test-shared", Max: 2, Window: time.Minute}
	e := echo.New()
	e.HTTPErrorHandler = wire.HTTPErrorHandler
	e.POST("/limited", func(c *echo.Context) error { return c.NoContent(http.StatusOK) },
		auth.Identify(keys, everyoneExists{}), ratelimit.Middleware(ratelimit.NewStore(), policy, ratelimit.UserOrIPKey))
	post := func(user uuid.UUID, ip string) int {
		req := httptest.NewRequest(http.MethodPost, "/limited", nil)
		if user != uuid.Nil {
			tok, err := scenario.Mint(keys, user)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		req.Header.Set("X-Real-Ip", ip)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}

	t.Run("users behind one IP have separate buckets", func(t *testing.T) {
		a, b := uuid.New(), uuid.New()
		require.Equal(t, http.StatusOK, post(a, "10.0.0.1"))
		require.Equal(t, http.StatusOK, post(a, "10.0.0.1"))
		require.Equal(t, http.StatusTooManyRequests, post(a, "10.0.0.1"))
		require.Equal(t, http.StatusOK, post(b, "10.0.0.1"))
	})
	t.Run("no identity falls back to the IP", func(t *testing.T) {
		require.Equal(t, http.StatusOK, post(uuid.Nil, "1.2.3.4"))
		require.Equal(t, http.StatusOK, post(uuid.Nil, "1.2.3.4"))
		require.Equal(t, http.StatusTooManyRequests, post(uuid.Nil, "1.2.3.4"))
		require.Equal(t, http.StatusOK, post(uuid.Nil, "5.6.7.8"))
	})
}

func TestIPOnlyKeyHasNoPrefix(t *testing.T) {
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	c.Request().Header.Set("X-Real-Ip", "1.2.3.4")
	require.Equal(t, "1.2.3.4", ratelimit.IPOnlyKey(c))
}
