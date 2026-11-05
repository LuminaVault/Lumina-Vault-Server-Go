package sse

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/wire"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
)

// DefaultKeepAlive is EncodableSSEStreamResponse's keepAliveInterval default.
const DefaultKeepAlive = 20 * time.Second

const keepAliveFrame = ": keepalive\n\n"

// Stream is AsyncThrowingStream with the default unbounded buffer: yield never
// blocks, whether the client is slow or gone. Make one with Produce.
type Stream[E any] struct {
	mu       sync.Mutex
	queue    []E
	finished bool
	err      error
	wake     chan struct{}
	cancel   context.CancelFunc
}

// Produce runs fn on its own goroutine and returns the stream it fills. fn's error
// is continuation.finish(throwing:), a nil return is finish().
//
// ctx ends when the writer stops reading, which is onTermination's task.cancel()
// in every Swift caller, so fn should pass it to whatever it calls. yield reports
// false once nobody will read the event. Work that has to survive a client leaving
// takes context.WithoutCancel. A panic in fn ends the stream as an error rather
// than the process.
func Produce[E any](ctx context.Context, fn func(ctx context.Context, yield func(E) bool) error) *Stream[E] {
	ctx, cancel := context.WithCancel(ctx)
	s := &Stream[E]{wake: make(chan struct{}, 1), cancel: cancel}
	go func() {
		defer cancel()
		var err error
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(ctx, "sse producer panicked", "panic", r)
				err = fmt.Errorf("sse producer panicked: %v", r)
			}
			s.finish(err)
		}()
		err = fn(ctx, s.yield)
	}()
	return s
}

func (s *Stream[E]) yield(e E) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return false
	}
	s.queue = append(s.queue, e)
	s.signal()
	return true
}

func (s *Stream[E]) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finished, s.err = true, err
	s.signal()
}

// stop is the reader leaving: queued events are dropped and fn's context ends.
func (s *Stream[E]) stop() {
	s.mu.Lock()
	s.finished, s.queue = true, nil
	s.mu.Unlock()
	s.cancel()
}

func (s *Stream[E]) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// poll returns the next event if there is one. done means the stream has
// finished and is drained, and err is how it finished.
func (s *Stream[E]) poll() (e E, ok, done bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) > 0 {
		e, s.queue = s.queue[0], s.queue[1:]
		return e, true, false, nil
	}
	return e, false, s.finished, s.err
}

var errClientGone = errors.New("sse: client gone")

// pump hands events to emit until the stream ends and returns how it ended.
// tick, if not nil, runs each time keepAlive elapses. emit and tick return false
// when the client is gone. Disconnection ends an idle stream at once, which is
// what lets a handler behind ratelimit.InFlightLimit give its slot back: Swift
// would notice only at the next write.
func pump[E any](ctx context.Context, s *Stream[E], keepAlive time.Duration, tick func() bool, emit func(E) bool) error {
	var ticks <-chan time.Time
	if tick != nil {
		ticker := time.NewTicker(keepAlive)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		if ctx.Err() != nil {
			return errClientGone
		}
		e, ok, done, err := s.poll()
		switch {
		case ok:
			if !emit(e) {
				return errClientGone
			}
			continue
		case done:
			return err
		}
		select {
		case <-s.wake:
		case <-ticks:
			if !tick() {
				return errClientGone
			}
		case <-ctx.Done():
			return errClientGone
		}
	}
}

// DataFrame is SSEStreamResponse.encodeEventLine: "data: <json>\n\n". There is no
// event: line, the `type` key in the JSON is the discriminator.
func DataFrame(v any) ([]byte, error) {
	data, err := wire.MarshalFoundation(v)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(data)+8)
	out = append(out, "data: "...)
	out = append(out, data...)
	return append(out, "\n\n"...), nil
}

// NamedFrame is EncodableSSEStreamResponse.encodeEventLine:
// "event: <name>\ndata: <json>\n\n".
func NamedFrame(name string, v any) ([]byte, error) {
	data, err := DataFrame(v)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(data)+len(name)+8)
	out = append(out, "event: "...)
	out = append(out, name...)
	out = append(out, '\n')
	return append(out, data...), nil
}

// ServeStream is SSEStreamResponse: chat, query and parallel. Do everything that can
// answer with a status before calling it: it commits a 200 straight away. Register
// the route with Router.Streaming as well, or the gzip middleware holds frames back.
//
// A failed stream, or an event that won't encode, writes errorEvent(err) as a last
// frame and ends the response normally. Pass StreamErrorMessage.event(for:) as it
// is in internal/llm, never the raw error: its text can hold SQL, paths or provider
// payloads. There are no heartbeats, as in Swift.
func ServeStream[E any](c *echo.Context, s *Stream[E], errorEvent func(error) any) error {
	defer s.stop()
	w, rc, ok := start(c)
	if !ok {
		return nil
	}
	var encodeErr error
	err := pump(c.Request().Context(), s, 0, nil, func(e E) bool {
		frame, err := DataFrame(e)
		if err != nil {
			encodeErr = err
			return false
		}
		return write(w, rc, frame)
	})
	if encodeErr != nil {
		err = encodeErr
	}
	if err == nil || errors.Is(err, errClientGone) {
		return nil
	}
	// Best effort: if the client has gone, there is nothing useful left to do.
	if frame, encErr := DataFrame(errorEvent(err)); encErr == nil {
		write(w, rc, frame)
	}
	return nil
}

// ServeNamedStream is EncodableSSEStreamResponse: agent rooms, whose eventName is
// "agent.room". Every frame carries `event: <eventName>`, and an idle stream writes
// `: keepalive` every keepAlive (DefaultKeepAlive for rooms) so proxies keep the
// connection. Call it and register the route as ServeStream.
//
// A failed stream ends the response with no frame, because the payload type owns
// its own terminal event and the producer has logged. An event that won't encode
// is skipped.
func ServeNamedStream[E any](c *echo.Context, s *Stream[E], eventName string, keepAlive time.Duration) error {
	defer s.stop()
	w, rc, ok := start(c)
	if !ok {
		return nil
	}
	_ = pump(c.Request().Context(), s, keepAlive,
		func() bool { return write(w, rc, []byte(keepAliveFrame)) },
		func(e E) bool {
			frame, err := NamedFrame(eventName, e)
			if err != nil {
				return true
			}
			return write(w, rc, frame)
		})
	return nil
}
