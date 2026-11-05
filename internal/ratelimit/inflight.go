package ratelimit

import (
	"context"
	"lumina/internal/wire"
	"net/http"
	"sync"

	"github.com/labstack/echo/v5"
)

// Concurrency ceilings Swift mounts InFlightLimitMiddleware with.
const (
	// DefaultStreamsPerUser is http.maxConcurrentStreamsPerUser's default, on
	// /v1/llm and /v1/query. config.MaxConcurrentStreamsPerUser reads the key.
	DefaultStreamsPerUser = 3
	// AudioTranscriptionsInFlight is the literal on /v1/audio/transcriptions: a
	// 25 MB upload is 25 MB resident while it is buffered.
	AudioTranscriptionsInFlight = 2
)

// ErrTooManyStreams is InFlightLimitMiddleware's 429 body.
var ErrTooManyStreams = wire.NewError(http.StatusTooManyRequests, "too many concurrent streams")

// InFlightLimit is InFlightLimitMiddleware: at most maxConcurrent requests per key
// run at once, and the rest get a 429 with Retry-After: 1. A maxConcurrent below 1
// counts as 1. The count lives in this process, like InFlightLimitStore.
//
// Each call has its own counters, as each Swift mount has its own store, so a chat
// stream and a query stream are counted apart. Don't share one middleware value
// between groups.
//
// A slot comes back when the handler returns, however it returns, or when the client
// disconnects, whichever is first, so a handler stuck past a disconnect can't lock a
// user out. One limit of net/http: it doesn't watch the connection while a request
// body is unread. Handlers that block before writing must read the body first, as
// wire.Bind and wire.ReadBody do. sse's first flush reads it for them.
func InFlightLimit(maxConcurrent int, key func(*echo.Context) string) echo.MiddlewareFunc {
	maxConcurrent = max(1, maxConcurrent)
	var mu sync.Mutex
	counts := map[string]int{}

	acquire := func(k string) bool {
		mu.Lock()
		defer mu.Unlock()
		if counts[k] >= maxConcurrent {
			return false
		}
		counts[k]++
		return true
	}
	release := func(k string) {
		mu.Lock()
		defer mu.Unlock()
		if counts[k] <= 1 {
			delete(counts, k)
			return
		}
		counts[k]--
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			k := key(c)
			if !acquire(k) {
				c.Response().Header().Set("Retry-After", "1")
				return ErrTooManyStreams
			}
			once := sync.OnceFunc(func() { release(k) })
			defer once()
			stop := context.AfterFunc(c.Request().Context(), once)
			defer stop()
			return next(c)
		}
	}
}
