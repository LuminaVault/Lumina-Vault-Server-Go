package hermesruns

import (
	"bufio"
	"context"
	"lumina/internal/dbtest"
	"lumina/internal/hermesruns"
	"lumina/internal/migrations"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// SkillRunnerLifecycleTests: Swift's SkillRunner.startEventSubscriptions opens
// one log-only EventBus subscription per event type and stopEventSubscriptions
// cancels them. Those loops only logged, so Go has no counterpart; the one
// in-process subscriber Go keeps is this module's SSE feed, which subscribes to
// the run bus when a stream starts and unsubscribes when it ends. These cases
// hold the feed to the same lifecycle the Swift suite holds the runner to:
// starting registers exactly one subscriber, stopping drains it, starting again
// does not double-subscribe, and events published while it runs reach it.

type liveFeed struct {
	lines  chan string
	cancel context.CancelFunc
	done   chan struct{}
}

// openFeed starts GET /v1/hermes/runs/:id/events over a real connection, so the
// stream is read as it is written.
func openFeed(t *testing.T, srv *httptest.Server, tenant, run uuid.UUID) *liveFeed {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/hermes/runs/"+run.String()+"/events", nil)
	require.NoError(t, err)
	token, err := scenario.Mint(testKeys, tenant)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req) //nolint:gosec,bodyclose // G704: the test server's own URL; the reader goroutine closes the body.
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	require.Equal(t, http.StatusOK, resp.StatusCode)
	f := &liveFeed{lines: make(chan string, 256), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(f.done)
		defer func() { _ = resp.Body.Close() }()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			select {
			case f.lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() { f.stop() })
	return f
}

func (f *liveFeed) stop() {
	f.cancel()
	<-f.done
}

func waitForSubscribers(t *testing.T, bus *hermesruns.Bus, want int, message string) {
	t.Helper()
	require.Eventually(t, func() bool { return bus.SubscriberCount() == want }, 3*time.Second, 5*time.Millisecond, message)
}

func TestFeedSubscriptionLifecycle(t *testing.T) {
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	v := newEnv(t, pool)
	srv := httptest.NewServer(v.e)
	t.Cleanup(srv.Close)
	bus := v.service.Bus()

	// SkillRunnerLifecycleTests: "startEventSubscriptions registers one subscriber per event type".
	t.Run("starting a feed registers exactly one subscriber", func(t *testing.T) {
		tenant := v.tenant()
		run := v.seedRun(tenant, hermesruns.Running, nil)
		f := openFeed(t, srv, tenant, run)
		waitForSubscribers(t, bus, 1, "the feed's subscriber never registered")
		f.stop()
		waitForSubscribers(t, bus, 0, "the feed's subscriber outlived it")
	})

	// SkillRunnerLifecycleTests: "stopEventSubscriptions drains all subscribers".
	t.Run("stopping drains every subscriber", func(t *testing.T) {
		tenant := v.tenant()
		feeds := []*liveFeed{}
		for range 3 {
			feeds = append(feeds, openFeed(t, srv, tenant, v.seedRun(tenant, hermesruns.Running, nil)))
		}
		waitForSubscribers(t, bus, 3, "subscribe never landed")
		for _, f := range feeds {
			f.stop()
		}
		waitForSubscribers(t, bus, 0, "subscribers not drained after stop")
	})

	// SkillRunnerLifecycleTests: "repeated startEventSubscriptions is idempotent".
	t.Run("restarting settles back to one subscriber", func(t *testing.T) {
		tenant := v.tenant()
		run := v.seedRun(tenant, hermesruns.Running, nil)
		first := openFeed(t, srv, tenant, run)
		waitForSubscribers(t, bus, 1, "first start never landed")
		// The reconnect path: the old stream ends, the new one takes its place.
		first.stop()
		second := openFeed(t, srv, tenant, run)
		waitForSubscribers(t, bus, 1, "a restart double-subscribed")
		second.stop()
		waitForSubscribers(t, bus, 0, "the restarted subscriber outlived its stream")
	})

	// SkillRunnerLifecycleTests: "events published before stop reach the runner".
	t.Run("events published while running reach the feed", func(t *testing.T) {
		tenant := v.tenant()
		run := v.seedRun(tenant, hermesruns.Running, nil)
		f := openFeed(t, srv, tenant, run)
		waitForSubscribers(t, bus, 1, "subscribe never landed")
		store := v.service.Store()
		for range 25 {
			updated, err := store.Append(context.Background(), run, "message.delta", map[string]any{"event": "message.delta"}, time.Now(), nil)
			require.NoError(t, err)
			store.Publish(updated, updated.LastSeq, "message.delta")
		}
		seen := 0
		deadline := time.After(5 * time.Second)
		for seen < 25 {
			select {
			case line := <-f.lines:
				if strings.HasPrefix(line, "data: ") {
					seen++
				}
			case <-deadline:
				t.Fatalf("only %d of 25 events reached the feed", seen)
			}
		}
		f.stop()
		waitForSubscribers(t, bus, 0, "the subscriber outlived its stream")
	})
}
