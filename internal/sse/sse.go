// Package sse serves event streams. This file holds the polling stream, the Go
// port of Swift's IngestionSSEResponse (Sources/App/HTTP/IngestionSSEResponse.swift):
// the events live in Postgres, so the stream works whichever server wrote them.
// push.go holds the writer for events a request produces as it runs.
//
// The loop, in Swift's order: fetch events after the cursor and write them; stop
// if the source is terminal; write a heartbeat if there were no events; sleep.
// After MaxPolls rounds the stream ends normally, and the client reconnects with
// Last-Event-ID.
package sse

import (
	"context"
	"io"
	"log/slog"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
)

const (
	headerLastEventID = "Last-Event-ID"
	heartbeat         = ": heartbeat\n\n"

	ingestionEventName = "ingestion"
	ingestionInterval  = 2 * time.Second
	ingestionMaxPolls  = 360

	maxDrainedBody = 2 << 20
)

// Event is one frame. Data is marshalled as Foundation's JSONEncoder writes it:
// compact, keys in struct order, no HTML escaping, "/" escaped as "\/". Dates in
// it should be wire.Time, which matches .iso8601. Name, when set, replaces
// Options.EventName for this frame.
type Event struct {
	ID   int64
	Name string
	Data any
}

// Source is what a stream polls. Both calls are scoped by the implementation to
// one tenant and one resource.
type Source interface {
	// Events returns the events after the cursor, oldest first.
	Events(ctx context.Context, after int64) ([]Event, error)
	// Terminal reports that no more events will come.
	Terminal(ctx context.Context) (bool, error)
}

// Options describe a stream. Ingestion returns the ones Swift uses.
type Options struct {
	// EventName names frames whose Event has no Name.
	EventName string
	Interval  time.Duration
	// MaxPolls caps the rounds. Zero means no cap: the stream ends only when the
	// source is terminal or the client goes away.
	MaxPolls int
	// Heartbeat is the comment written after an empty round. Empty means
	// ": heartbeat\n\n".
	Heartbeat string
	// UntilDrained ends a terminal source only on an empty round, and polls again
	// straight after a round that had events. Otherwise the first terminal check
	// ends the stream, and every round waits Interval.
	UntilDrained bool
	// FinishOnError ends the stream normally on a poll or encoding failure, where
	// the default aborts the connection.
	FinishOnError bool
}

// Ingestion is IngestionSSEResponse: 360 polls, 2 s apart, so 12 minutes.
func Ingestion() Options {
	return Options{EventName: ingestionEventName, Interval: ingestionInterval, MaxPolls: ingestionMaxPolls}
}

// Serve writes the stream until the source is terminal, MaxPolls is reached or
// the client goes away. Check access before calling: once it starts, the
// response is committed with a 200. Register the route with Router.Streaming as
// well, or the gzip middleware holds frames back.
//
// A poll or encoding failure aborts the connection instead of ending the stream
// cleanly, because in Swift a throw from the body writer does the same. A client
// can then tell "the server failed" from "the stream is finished". FinishOnError
// is for the Swift stream that catches its own failures.
func Serve(c *echo.Context, src Source, after int64, opts Options) error {
	drainBody(c.Request())
	w, rc, ok := start(c)
	if !ok {
		return nil
	}

	ctx := c.Request().Context()
	fail := func(err error) {
		if !opts.FinishOnError {
			abort(ctx, err)
		}
		if ctx.Err() == nil {
			slog.ErrorContext(ctx, "sse stream failed", "error", err)
		}
	}
	heartbeatFrame := []byte(heartbeat)
	if opts.Heartbeat != "" {
		heartbeatFrame = []byte(opts.Heartbeat)
	}
	cursor := after
	for poll := 0; opts.MaxPolls == 0 || poll < opts.MaxPolls; poll++ {
		events, err := src.Events(ctx, cursor)
		if err != nil {
			fail(err)
			return nil
		}
		for _, ev := range events {
			name := ev.Name
			if name == "" {
				name = opts.EventName
			}
			frame, err := Frame(ev, name)
			if err != nil {
				fail(err)
				return nil
			}
			if !write(w, rc, frame) {
				return nil
			}
			cursor = ev.ID
		}
		terminal, err := src.Terminal(ctx)
		if err != nil {
			fail(err)
			return nil
		}
		if terminal && (!opts.UntilDrained || len(events) == 0) {
			return nil
		}
		if len(events) == 0 && !write(w, rc, heartbeatFrame) {
			return nil
		}
		if opts.UntilDrained && len(events) > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(opts.Interval):
		}
	}
	return nil
}

// drainBody reads a body nobody asked for, up to Hummingbird's limit. net/http
// only watches the connection once the body has been read, so without this a
// stream would never see its client leave.
func drainBody(r *http.Request) {
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxDrainedBody))
	}
}

// start commits the 200 and the three stream headers. False means the client has
// gone already; headers are sent, so there is nothing left to report.
func start(c *echo.Context) (http.ResponseWriter, *http.ResponseController, bool) {
	w := c.Response()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	return w, rc, rc.Flush() == nil
}

// abort ends the handler without completing the response. A client that left
// already is not a failure worth logging.
func abort(ctx context.Context, err error) {
	if ctx.Err() == nil {
		slog.ErrorContext(ctx, "sse stream failed", "error", err)
	}
	panic(http.ErrAbortHandler)
}

func write(w http.ResponseWriter, rc *http.ResponseController, b []byte) bool {
	if _, err := w.Write(b); err != nil {
		return false
	}
	return rc.Flush() == nil
}

// Frame is "id: <ID>\nevent: <name>\ndata: <json>\n\n".
func Frame(ev Event, eventName string) ([]byte, error) {
	data, err := wire.MarshalFoundation(ev.Data)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(data)+len(eventName)+48)
	out = append(out, "id: "...)
	out = strconv.AppendInt(out, ev.ID, 10)
	out = append(out, "\nevent: "...)
	out = append(out, eventName...)
	out = append(out, "\ndata: "...)
	out = append(out, data...)
	out = append(out, "\n\n"...)
	return out, nil
}

// Cursor is MultimodalIngestionController.events' starting point:
//
//	query["after"].flatMap { Int64($0) } ?? header["Last-Event-ID"].flatMap { Int64($0) } ?? 0
//
// so an unparseable ?after= falls through to the header, not to 0.
func Cursor(c *echo.Context) int64 {
	r := c.Request()
	if raw, ok := rawQueryValue(r.URL.RawQuery, "after"); ok {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return n
		}
	}
	if n, err := strconv.ParseInt(r.Header.Get(headerLastEventID), 10, 64); err == nil {
		return n
	}
	return 0
}

// HeaderFirstCursor is WorkflowController.runEventStream's starting point:
//
//	header["Last-Event-ID"].flatMap { Int64($0) } ?? query["after"].flatMap { Int64($0) } ?? 0
//
// the reverse of Cursor.
func HeaderFirstCursor(c *echo.Context) int64 {
	r := c.Request()
	if raw, ok := wire.HeaderValue(r, headerLastEventID); ok {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return n
		}
	}
	if raw, ok := rawQueryValue(r.URL.RawQuery, "after"); ok {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// rawQueryValue is Hummingbird's queryParameters[key] for the first match, for
// integer values only: pairs split on "&", the key is compared raw, and a literal
// "+" stays "+" (Int64 accepts "+5", where Go's url.Values would see " 5"). A
// percent-encoded digit is not decoded, a case no client sends.
func rawQueryValue(rawQuery, key string) (string, bool) {
	for _, pair := range strings.Split(rawQuery, "&") {
		k, v, _ := strings.Cut(pair, "=")
		if pair != "" && k == key {
			return v, true
		}
	}
	return "", false
}
