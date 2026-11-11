package devicerpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/devicerpc"
	"lumina/internal/ws"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// recorder is a Broadcaster that keeps what it was asked to send and can
// answer from inside Broadcast.
type recorder struct {
	mu     sync.Mutex
	sent   []sent
	onSend func(message string)
}

type sent struct{ tenant, message string }

func (r *recorder) Broadcast(_ context.Context, tenantID, message string) {
	r.mu.Lock()
	r.sent = append(r.sent, sent{tenantID, message})
	f := r.onSend
	r.mu.Unlock()
	if f != nil {
		f(message)
	}
}

func (r *recorder) all() []sent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sent(nil), r.sent...)
}

// MARK: DeviceCommandBrokerTests (2)

func TestResolveCompletesAPendingRequest(t *testing.T) {
	broker := devicerpc.New(ws.NewManager(), 5*time.Second)
	command := devicerpc.NewCommand(devicerpc.KindPing, "", nil)
	type out struct {
		r   devicerpc.Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := broker.Request(context.Background(), uuid.New(), command)
		done <- out{r, err}
	}()
	time.Sleep(80 * time.Millisecond) // let request register its waiter
	broker.Resolve(devicerpc.Result{ID: command.ID, OK: true, Payload: map[string]string{"pong": "1"}})
	got := <-done
	require.NoError(t, got.err)
	require.True(t, got.r.OK)
	require.Equal(t, "1", got.r.Payload["pong"])
}

func TestRequestTimesOutWithoutAResult(t *testing.T) {
	broker := devicerpc.New(ws.NewManager(), 120*time.Millisecond)
	_, err := broker.Request(context.Background(), uuid.New(), devicerpc.NewCommand(devicerpc.KindPing, "", nil))
	require.ErrorIs(t, err, devicerpc.ErrTimeout)
}

// MARK: Go-only

func TestDefaultTimeoutIsTwelveSeconds(t *testing.T) {
	require.Equal(t, 12*time.Second, devicerpc.DefaultTimeout)
}

func TestRequestSendsTheSwiftEnvelopeToTheUpperCasedTenant(t *testing.T) {
	rec := &recorder{}
	broker := devicerpc.New(rec, 50*time.Millisecond)
	tenant := uuid.MustParse("a0000000-0000-4000-8000-000000000001")
	command := devicerpc.Command{
		ID: uuid.MustParse("c0000000-0000-4000-8000-00000000000a"), Kind: devicerpc.KindReminderCreate,
		Domain: "reminders", Payload: map[string]string{"title": "Call mom", "due": ""},
	}
	_, err := broker.Request(context.Background(), tenant, command)
	require.ErrorIs(t, err, devicerpc.ErrTimeout)
	got := rec.all()
	require.Len(t, got, 1)
	require.Equal(t, "A0000000-0000-4000-8000-000000000001", got[0].tenant)
	require.JSONEq(t, `{"type":"device_command","command":{"id":"C0000000-0000-4000-8000-00000000000A",
		"kind":"reminder_create","domain":"reminders","payload":{"title":"Call mom","due":""}}}`, got[0].message)
	// The envelope passes the socket guard, so a client relaying it is not refused.
	require.Equal(t, ws.Allow, ws.Evaluate(got[0].message).Reason)
}

func TestCommandWithoutDomainOrPayloadEncodesLikeSwift(t *testing.T) {
	b, err := json.Marshal(devicerpc.Command{ID: uuid.MustParse("c0000000-0000-4000-8000-00000000000b"), Kind: devicerpc.KindPing})
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"C0000000-0000-4000-8000-00000000000B","kind":"ping","payload":{}}`, string(b))
}

func TestAResultArrivingDuringTheSendIsNotLost(t *testing.T) {
	rec := &recorder{}
	broker := devicerpc.New(rec, 5*time.Second)
	command := devicerpc.NewCommand(devicerpc.KindPing, "", nil)
	rec.onSend = func(string) { broker.Resolve(devicerpc.Result{ID: command.ID, OK: true}) }
	r, err := broker.Request(context.Background(), uuid.New(), command)
	require.NoError(t, err)
	require.True(t, r.OK)
}

func TestRequestEndsWithTheCallersContext(t *testing.T) {
	broker := devicerpc.New(ws.NewManager(), 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	command := devicerpc.NewCommand(devicerpc.KindPing, "", nil)
	_, err := broker.Request(ctx, uuid.New(), command)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, errors.Is(err, devicerpc.ErrTimeout))
	// The waiter is gone: a late result is dropped without blocking.
	broker.Resolve(devicerpc.Result{ID: command.ID, OK: true})
}

func TestUnknownAndRepeatedResultsAreDropped(t *testing.T) {
	broker := devicerpc.New(ws.NewManager(), 5*time.Second)
	broker.Resolve(devicerpc.Result{ID: uuid.New(), OK: true})

	command := devicerpc.NewCommand(devicerpc.KindPing, "", nil)
	rec := &recorder{}
	broker = devicerpc.New(rec, 5*time.Second)
	rec.onSend = func(string) {
		broker.Resolve(devicerpc.Result{ID: command.ID, OK: false})
		broker.Resolve(devicerpc.Result{ID: command.ID, OK: true}) // second one has no waiter
	}
	r, err := broker.Request(context.Background(), uuid.New(), command)
	require.NoError(t, err)
	require.False(t, r.OK, "the first result wins")
}

func TestConcurrentRequestsGetTheirOwnResults(t *testing.T) {
	rec := &recorder{}
	broker := devicerpc.New(rec, 5*time.Second)
	rec.onSend = func(message string) {
		var env struct {
			Command struct {
				ID string `json:"id"`
			} `json:"command"`
		}
		require.NoError(t, json.Unmarshal([]byte(message), &env))
		id := uuid.MustParse(env.Command.ID)
		go broker.Resolve(devicerpc.Result{ID: id, OK: true, Payload: map[string]string{"id": id.String()}})
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			command := devicerpc.NewCommand(devicerpc.KindDeviceFetch, "calendar", nil)
			r, err := broker.Request(context.Background(), uuid.New(), command)
			require.NoError(t, err)
			require.Equal(t, command.ID.String(), r.Payload["id"])
		})
	}
	wg.Wait()
}
