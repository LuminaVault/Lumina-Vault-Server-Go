package devicerpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/ids"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultTimeout is DeviceCommandBroker's 12 s wait for the app's result.
const DefaultTimeout = 12 * time.Second

// ErrTimeout is DeviceCommandError.timeout: no result arrived in time. The app
// is closed, offline, or connected to another process.
var ErrTimeout = errors.New("device command timed out")

// ErrEncodingFailed is DeviceCommandError.encodingFailed.
var ErrEncodingFailed = errors.New("device command encoding failed")

// Broadcaster sends one text message to every socket of a tenant. *ws.Manager
// is the one cmd/App uses.
type Broadcaster interface {
	Broadcast(ctx context.Context, tenantID, message string)
}

// Broker is DeviceCommandBroker. One per process, shared by every caller and by
// the result route.
type Broker struct {
	out     Broadcaster
	timeout time.Duration

	mu      sync.Mutex
	pending map[uuid.UUID]chan Result
}

// New returns a Broker that sends through out. A timeout of 0 is DefaultTimeout.
func New(out Broadcaster, timeout time.Duration) *Broker {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Broker{out: out, timeout: timeout, pending: map[uuid.UUID]chan Result{}}
}

// Request sends cmd to the tenant's sockets inside a device_command envelope
// and waits for the Result with cmd.ID. It returns ErrTimeout after the
// broker's timeout, or ctx.Err() if ctx ends first. Swift sends even when the
// tenant has no socket, and only the timeout tells the caller; so does this.
//
// The waiter is registered before the send, so a result that comes back before
// Request starts waiting is not lost (in Swift it would be, and the call would
// time out).
func (b *Broker) Request(ctx context.Context, tenant uuid.UUID, cmd Command) (Result, error) {
	data, err := json.Marshal(envelope{Type: EnvelopeType, Command: cmd})
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrEncodingFailed, err)
	}
	ch := make(chan Result, 1)
	b.mu.Lock()
	b.pending[cmd.ID] = ch
	b.mu.Unlock()
	defer b.forget(cmd.ID, ch)

	b.out.Broadcast(ctx, ids.Upper(tenant), string(data))
	slog.InfoContext(ctx, fmt.Sprintf("device.command sent tenant=%s id=%s kind=%s", ids.Upper(tenant), ids.Upper(cmd.ID), cmd.Kind))

	timer := time.NewTimer(b.timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r, nil
	case <-timer.C:
		slog.WarnContext(ctx, "device.command timed out id="+ids.Upper(cmd.ID))
		return Result{}, ErrTimeout
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Resolve hands a result to the Request waiting for r.ID. A result nobody is
// waiting for (late, unknown, or for a command another process sent) is
// dropped. Like Swift, it doesn't check the tenant: command ids are random v4
// UUIDs sent only to the owner's sockets.
func (b *Broker) Resolve(r Result) {
	b.mu.Lock()
	ch, ok := b.pending[r.ID]
	delete(b.pending, r.ID)
	b.mu.Unlock()
	if ok {
		ch <- r
	}
}

func (b *Broker) forget(id uuid.UUID, ch chan Result) {
	b.mu.Lock()
	if b.pending[id] == ch {
		delete(b.pending, id)
	}
	b.mu.Unlock()
}
