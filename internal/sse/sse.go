// Package sse serves polling event streams: the Go port of Swift's
// IngestionSSEResponse (Sources/App/HTTP/IngestionSSEResponse.swift). The events
// live in Postgres, so the stream works whichever server wrote them.
//
// The loop, in Swift's order: fetch events after the cursor and write them; stop
// if the source is terminal; write a heartbeat if there were no events; sleep.
// After MaxPolls rounds the stream ends normally, and the client reconnects with
// Last-Event-ID.
package sse

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
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
)

// Event is one frame. Data is marshalled as Foundation's JSONEncoder writes it:
// compact, keys in struct order, no HTML escaping, "/" escaped as "\/". Dates in
// it should be wire.Time, which matches .iso8601.
type Event struct {
	ID   int64
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
	EventName string
	Interval  time.Duration
	MaxPolls  int
}

// Ingestion is IngestionSSEResponse: 360 polls, 2 s apart, so 12 minutes.
func Ingestion() Options {
	return Options{EventName: ingestionEventName, Interval: ingestionInterval, MaxPolls: ingestionMaxPolls}
}

// Serve writes the stream until the source is terminal, MaxPolls is reached or
// the client goes away. Check access before calling: once it starts, the
// response is committed with a 200.
//
// A poll or encoding failure aborts the connection instead of ending the stream
// cleanly, because in Swift a throw from the body writer does the same. A client
// can then tell "the server failed" from "the stream is finished".
func Serve(c *echo.Context, src Source, after int64, opts Options) error {
	w := c.Response()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return nil //nolint:nilerr // headers are sent; nothing left to report
	}

	ctx := c.Request().Context()
	cursor := after
	for range opts.MaxPolls {
		events, err := src.Events(ctx, cursor)
		if err != nil {
			abort(ctx, err)
		}
		for _, ev := range events {
			frame, err := Frame(ev, opts.EventName)
			if err != nil {
				abort(ctx, err)
			}
			if !write(w, rc, frame) {
				return nil
			}
			cursor = ev.ID
		}
		terminal, err := src.Terminal(ctx)
		if err != nil {
			abort(ctx, err)
		}
		if terminal {
			return nil
		}
		if len(events) == 0 && !write(w, rc, []byte(heartbeat)) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(opts.Interval):
		}
	}
	return nil
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
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ev.Data); err != nil {
		return nil, err
	}
	data := bytes.ReplaceAll(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), []byte("/"), []byte(`\/`))
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
