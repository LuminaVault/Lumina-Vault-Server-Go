// Package posthog is the Go port of LuminaVaultServer's server-side analytics
// client (Sources/App/Telemetry/PostHogHTTPClient.swift, PostHogAnalytics.swift):
// the HTTP batch capture API, a bounded in-memory queue, a timed flush.
//
// Capture is fire-and-forget. It takes a short mutex, appends to the queue and
// returns; it does no I/O, starts no goroutine and has no error to return, so a
// handler can call it anywhere. Delivery is best-effort: a failed post logs and
// drops that batch without a retry, and a full queue drops new events.
//
// A nil *Client is valid and does nothing. New returns nil when
// POSTHOG_PROJECT_TOKEN or POSTHOG_HOST is unset, so call sites never check
// whether analytics is configured.
package posthog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/swiftstr"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// lib is both the default distinct_id and the $lib property, as in Swift.
	lib = "luminavault-server"

	defaultFlushInterval = 10 * time.Second
	defaultMaxBatchSize  = 100
	defaultMaxQueueDepth = 2000
	// postTimeout is AsyncHTTPClientPostHogTransport's timeout.
	postTimeout = 10 * time.Second
	// shutdownBudget bounds Run's final flush, so a dead PostHog can't use up
	// the pod's termination grace period. Swift's drain has no such bound.
	shutdownBudget = 10 * time.Second
)

// Options overrides the Swift defaults. Tests use it; production passes the
// zero value.
type Options struct {
	FlushInterval time.Duration
	MaxBatchSize  int
	MaxQueueDepth int
	HTTPClient    *http.Client
	Now           func() time.Time
}

// Client batches events and posts them to {host}/capture.
type Client struct {
	apiKey        string
	captureURL    string
	http          *http.Client
	now           func() time.Time
	flushInterval time.Duration
	maxBatchSize  int
	maxQueueDepth int

	mu      sync.Mutex
	queue   []event
	dropped int
}

// event is one entry of the batch. Fields are in alphabetical order because
// Swift encodes with .sortedKeys.
type event struct {
	DistinctID string         `json:"distinct_id"`
	Event      string         `json:"event"`
	Properties map[string]any `json:"properties"`
	Timestamp  string         `json:"timestamp"`
}

type batchRequest struct {
	APIKey string  `json:"api_key"`
	Batch  []event `json:"batch"`
}

// New builds the client, or returns nil (analytics off, with a warning) when
// either value is blank after trimming, as makePostHogAnalytics does.
func New(projectToken, host string, opts Options) *Client {
	token := swiftstr.TrimWhitespacesAndNewlines(projectToken)
	trimmedHost := swiftstr.TrimWhitespacesAndNewlines(host)
	if token == "" || trimmedHost == "" {
		slog.Warn("PostHog is not configured; analytics events will not be sent", "config", "POSTHOG_PROJECT_TOKEN, POSTHOG_HOST")
		return nil
	}
	c := &Client{
		apiKey:        token,
		captureURL:    captureURL(trimmedHost),
		http:          opts.HTTPClient,
		now:           opts.Now,
		flushInterval: opts.FlushInterval,
		maxBatchSize:  opts.MaxBatchSize,
		maxQueueDepth: opts.MaxQueueDepth,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: postTimeout}
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.flushInterval <= 0 {
		c.flushInterval = defaultFlushInterval
	}
	if c.maxBatchSize <= 0 {
		c.maxBatchSize = defaultMaxBatchSize
	}
	if c.maxQueueDepth <= 0 {
		c.maxQueueDepth = defaultMaxQueueDepth
	}
	c.maxQueueDepth = max(c.maxBatchSize, c.maxQueueDepth)
	return c
}

// captureURL turns "https://eu.i.posthog.com" into ".../capture", tolerating
// surrounding whitespace, trailing slashes and an existing /capture suffix.
func captureURL(host string) string {
	base := strings.TrimRight(swiftstr.TrimWhitespacesAndNewlines(host), "/")
	if strings.HasSuffix(base, "/capture") {
		return base
	}
	return base + "/capture"
}

// Capture queues one event. distinctID is who it belongs to; pass
// ids.Upper(tenant), because Swift sends UUID.uuidString and PostHog treats a
// lowercase ID as a different person. Empty means the server itself ("nil" in
// Swift). props may be nil, and Capture copies it, so the caller may reuse it.
//
// Values may be strings, bools, numbers, nil, uuid.UUID (uppercased),
// time.Time, []any and map[string]any. Anything else is stringified, so an odd
// value never costs the event.
func (c *Client) Capture(name, distinctID string, props map[string]any) {
	if c == nil {
		return
	}
	properties := make(map[string]any, len(props)+1)
	for k, v := range props {
		properties[k] = jsonValue(v)
	}
	properties["$lib"] = lib
	if distinctID == "" {
		distinctID = lib
	}
	item := event{Event: name, DistinctID: distinctID, Timestamp: timestamp(c.now()), Properties: properties}

	c.mu.Lock()
	full := len(c.queue) >= c.maxQueueDepth
	if full {
		c.dropped++
	} else {
		c.queue = append(c.queue, item)
	}
	c.mu.Unlock()
	if full {
		slog.Debug("posthog queue full; dropped event", "event", name)
	}
}

// Run flushes on a timer until ctx is cancelled, then drains what is left under
// its own deadline. Cancel ctx only after the HTTP server has stopped, so the
// events of requests that finish during its graceful shutdown still go out.
func (c *Client) Run(ctx context.Context) {
	if c == nil {
		return
	}
	slog.Info("posthog analytics started", "endpoint", c.captureURL, "flushInterval", c.flushInterval.String())
	ticker := time.NewTicker(c.flushInterval)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			c.Flush(ctx)
		}
	}
	final, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownBudget)
	defer cancel()
	c.Flush(final)
	slog.Info("posthog analytics stopped")
}

// Flush posts every queued event in batches of MaxBatchSize. It is safe to call
// alongside Capture. When ctx ends it stops, leaving the rest queued.
func (c *Client) Flush(ctx context.Context) {
	if c == nil {
		return
	}
	for ctx.Err() == nil {
		batch, dropped := c.next()
		if dropped > 0 {
			slog.Warn("posthog queue overflowed; events dropped", "dropped", dropped)
		}
		if len(batch) == 0 {
			return
		}
		c.post(ctx, batch)
		if len(batch) < c.maxBatchSize {
			return
		}
	}
}

func (c *Client) next() (batch []event, dropped int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := min(c.maxBatchSize, len(c.queue))
	batch = c.queue[:n:n]
	c.queue = c.queue[n:]
	dropped, c.dropped = c.dropped, 0
	return batch, dropped
}

func (c *Client) post(ctx context.Context, batch []event) {
	body, err := encode(batchRequest{APIKey: c.apiKey, Batch: batch})
	if err != nil {
		slog.Error("posthog batch encoding failed", "error", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.captureURL, bytes.NewReader(body))
	if err != nil {
		slog.Warn("posthog capture failed", "error", err, "events", len(batch))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", lib)
	resp, err := c.http.Do(req) //nolint:gosec // G704: the URL is POSTHOG_HOST, operator config.
	if err != nil {
		slog.Warn("posthog capture failed", "error", err, "events", len(batch))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("posthog capture rejected", "status", resp.StatusCode, "events", len(batch))
		return
	}
	slog.Debug("posthog batch posted", "events", len(batch))
}

// encode matches Swift's JSONEncoder here: no HTML escaping, no trailing
// newline. Slashes are unescaped in both (.withoutEscapingSlashes).
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// timestamp is Date.ISO8601FormatStyle(includingFractionalSeconds: true): UTC
// with exactly three fractional digits.
func timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// jsonValue is JSONValue(anySendable:), the lossy bridge from a property bag.
// It only ever returns values encoding/json can marshal, so one odd property
// can't fail a whole batch. NaN and infinities are the one difference: Swift's
// encoder throws on them and loses the batch; here they become strings.
func jsonValue(v any) any {
	switch x := v.(type) {
	case nil, string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return x
	case float64:
		return finite(x)
	case float32:
		return finite(float64(x))
	case uuid.UUID:
		return ids.Upper(x)
	case time.Time:
		return timestamp(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = jsonValue(e)
		}
		return out
	default:
		return fmt.Sprint(x)
	}
}

func finite(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Sprint(f)
	}
	return f
}
