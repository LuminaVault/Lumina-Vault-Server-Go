package hermesruns

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// EventBusTests: Swift's EventBus is one registry keyed by event type. The Go
// port keeps the one slice that has a real subscriber, hermes_run_event, as Bus
// (memory_upserted is a direct callback, the vault and health events have only
// log subscribers in Swift and were dropped), so these run against Bus.

func sampleEvent(event string) Published {
	return Published{RunID: uuid.New(), Tenant: uuid.New(), Seq: 1, Event: event, Status: Running}
}

func receive(t *testing.T, ch <-chan Published) Published {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("no event arrived")
		return Published{}
	}
}

// EventBusTests: "subscriber receives published event of matching type".
func TestBusSubscriberReceivesThePublishedEvent(t *testing.T) {
	bus := NewBus()
	ch, cancel := bus.Subscribe()
	defer cancel()
	require.Equal(t, 1, bus.SubscriberCount())
	event := sampleEvent("run.started")
	bus.Publish(event)
	require.Equal(t, event, receive(t, ch))
}

// EventBusTests: "subscriber ignores events of different type". In Go each event
// type that survived has its own carrier, so the type filter is the carrier: a
// subscriber to one bus never sees what is published on another.
func TestBusSubscriberIgnoresEventsOfAnotherCarrier(t *testing.T) {
	runs, other := NewBus(), NewBus()
	ch, cancel := runs.Subscribe()
	defer cancel()
	other.Publish(sampleEvent("memory_upserted"))
	event := sampleEvent("run.completed")
	runs.Publish(event)
	require.Equal(t, event, receive(t, ch), "a run subscriber must not see another carrier's events")
	select {
	case p := <-ch:
		t.Fatalf("unexpected extra event %v", p)
	default:
	}
}

// EventBusTests: "multiple subscribers all receive event".
func TestBusMultipleSubscribersAllReceive(t *testing.T) {
	bus := NewBus()
	a, cancelA := bus.Subscribe()
	defer cancelA()
	b, cancelB := bus.Subscribe()
	defer cancelB()
	require.Equal(t, 2, bus.SubscriberCount())
	event := sampleEvent("run.started")
	bus.Publish(event)
	require.Equal(t, event, receive(t, a))
	require.Equal(t, event, receive(t, b))
}

// EventBusTests: "bounded buffer drops oldest when consumer lags". Swift buffers
// .bufferingNewest(64).
func TestBusBoundedBufferDropsOldestWhenTheConsumerLags(t *testing.T) {
	bus := NewBus()
	ch, cancel := bus.Subscribe()
	defer cancel()
	for i := range 100 {
		bus.Publish(Published{Event: fmt.Sprintf("notes/%d.md", i)})
	}
	var received []string
	for range 64 {
		received = append(received, receive(t, ch).Event)
	}
	require.Len(t, received, 64)
	require.Equal(t, "notes/99.md", received[63])
	require.NotContains(t, received, "notes/0.md")
	require.Equal(t, "notes/36.md", received[0], "the 64 newest survive, in order")
	select {
	case p := <-ch:
		t.Fatalf("buffer held more than 64: %v", p)
	default:
	}
}

// EventBusTests: "subscriber removed on stream termination".
func TestBusSubscriberRemovedOnTermination(t *testing.T) {
	bus := NewBus()
	_, cancel := bus.Subscribe()
	require.Equal(t, 1, bus.SubscriberCount())
	cancel()
	require.Equal(t, 0, bus.SubscriberCount())
}

// EventBusTests: "publish with no subscribers is noop".
func TestBusPublishWithNoSubscribersIsANoop(t *testing.T) {
	bus := NewBus()
	require.NotPanics(t, func() { bus.Publish(sampleEvent("run.started")) })
	require.Equal(t, 0, bus.SubscriberCount())
}

// EventBusTests: "HER-200 H1 subscribe registers synchronously without Task hop".
func TestBusSubscribeRegistersSynchronously(t *testing.T) {
	bus := NewBus()
	_, cancel := bus.Subscribe()
	defer cancel()
	require.Equal(t, 1, bus.SubscriberCount(), "registered before Subscribe returns")
}

// EventBusTests: "HER-200 H1 dropped stream before any publish unregisters cleanly".
func TestBusDroppedBeforeAnyPublishUnregistersCleanly(t *testing.T) {
	bus := NewBus()
	for range 50 {
		_, cancel := bus.Subscribe()
		cancel()
	}
	require.Equal(t, 0, bus.SubscriberCount())
}

// EventBusTests: "HER-200 H1 concurrent subscribe and publish does not deadlock".
func TestBusConcurrentSubscribeAndPublishDoesNotDeadlock(t *testing.T) {
	bus := NewBus()
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			ch, cancel := bus.Subscribe()
			defer cancel()
			// Publish after subscribing, so the event can't predate registration.
			bus.Publish(sampleEvent("run.started"))
			select {
			case <-ch:
			case <-time.After(2 * time.Second):
				t.Error("a subscriber received nothing")
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: subscribers never finished")
	}
	require.Equal(t, 0, bus.SubscriberCount())
}
