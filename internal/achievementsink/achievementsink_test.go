package achievementsink

import (
	"context"
	"errors"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"lumina/internal/modules/achievements"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type call struct {
	tenant uuid.UUID
	event  achievements.Event
}

type stubRecorder struct {
	mu      sync.Mutex
	calls   []call
	unlocks []achievements.Sub
	err     error
	gate    chan struct{}
}

func (r *stubRecorder) Record(_ context.Context, tenant uuid.UUID, e achievements.Event) ([]achievements.Sub, error) {
	if r.gate != nil {
		<-r.gate
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call{tenant, e})
	return r.unlocks, r.err
}

func (r *stubRecorder) events() []achievements.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []achievements.Event{}
	for _, c := range r.calls {
		out = append(out, c.event)
	}
	return out
}

type stubPusher struct {
	mu   sync.Mutex
	keys []string
	fail string
}

func (p *stubPusher) NotifyAchievement(_ context.Context, _ uuid.UUID, key, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = append(p.keys, key)
	if key == p.fail {
		return errors.New("push failed")
	}
	return nil
}

func run(t *testing.T, s *Sink) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Error("Run did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func TestDrainsInOrder(t *testing.T) {
	rec := &stubRecorder{}
	s := New(rec, nil, Options{})
	stop := run(t, s)
	tenant := uuid.New()
	s.Enqueue(tenant, achievements.EventVaultUploaded)
	s.Enqueue(tenant, achievements.EventMemoryUpserted)
	s.Enqueue(tenant, achievements.EventQueryRan)
	stop()
	require.Equal(t, []achievements.Event{achievements.EventVaultUploaded, achievements.EventMemoryUpserted, achievements.EventQueryRan}, rec.events())
	require.Equal(t, tenant, rec.calls[0].tenant)
}

// Shutdown beginning mid-job doesn't fail the job: Record sees a live context.
func TestJobInFlightSurvivesShutdown(t *testing.T) {
	var ctxErr error
	started, release := make(chan struct{}), make(chan struct{})
	rec := recorderFunc(func(ctx context.Context) {
		close(started)
		<-release
		ctxErr = ctx.Err()
	})
	s := New(rec, nil, Options{})
	stop := run(t, s)
	s.Enqueue(uuid.New(), achievements.EventQueryRan)
	<-started
	go stop()
	time.Sleep(20 * time.Millisecond)
	close(release)
	stop()
	require.NoError(t, ctxErr)
}

type recorderFunc func(context.Context)

func (f recorderFunc) Record(ctx context.Context, _ uuid.UUID, _ achievements.Event) ([]achievements.Sub, error) {
	f(ctx)
	return nil, nil
}

// The handler's side: with the worker stuck on the database, Enqueue still
// returns at once.
func TestEnqueueNeverWaits(t *testing.T) {
	rec := &stubRecorder{gate: make(chan struct{})}
	s := New(rec, nil, Options{})
	stop := run(t, s)
	start := time.Now()
	for range 10_000 {
		s.Enqueue(uuid.New(), achievements.EventQueryRan)
	}
	require.Less(t, time.Since(start), 2*time.Second)
	close(rec.gate)
	stop()
}

// AsyncStream's .bufferingNewest(n) keeps the newest n.
func TestFullInboxDropsTheOldest(t *testing.T) {
	rec := &stubRecorder{}
	s := New(rec, nil, Options{BufferSize: 2})
	tenant := uuid.New()
	s.Enqueue(tenant, achievements.EventVaultUploaded)
	s.Enqueue(tenant, achievements.EventMemoryUpserted)
	s.Enqueue(tenant, achievements.EventQueryRan)
	run(t, s)()
	require.Equal(t, []achievements.Event{achievements.EventMemoryUpserted, achievements.EventQueryRan}, rec.events())
}

func TestShutdownDrainsThenDropsLateEvents(t *testing.T) {
	rec := &stubRecorder{}
	s := New(rec, nil, Options{})
	tenant := uuid.New()
	s.Enqueue(tenant, achievements.EventVaultUploaded)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Run(ctx)
	require.Equal(t, []achievements.Event{achievements.EventVaultUploaded}, rec.events(), "queued work finishes")
	s.Enqueue(tenant, achievements.EventQueryRan)
	require.Empty(t, s.queue, "after shutdown an event is dropped")
}

func TestUnlocksPushAndNotify(t *testing.T) {
	first, second := achievements.Sub{Key: "a.one", Label: "One"}, achievements.Sub{Key: "a.two", Label: "Two"}
	rec := &stubRecorder{unlocks: []achievements.Sub{first, second}}
	push := &stubPusher{fail: "a.one"} // one failed push doesn't stop the next
	var unlocked []string
	s := New(rec, push, Options{OnUnlock: func(_ uuid.UUID, sub achievements.Sub) { unlocked = append(unlocked, sub.Key) }})
	s.Enqueue(uuid.New(), achievements.EventVaultUploaded)
	run(t, s)()
	require.Equal(t, []string{"a.one", "a.two"}, push.keys)
	require.Equal(t, []string{"a.one", "a.two"}, unlocked)
}

func TestRecordFailureIsSwallowed(t *testing.T) {
	rec := &stubRecorder{err: errors.New("db down"), unlocks: []achievements.Sub{{Key: "k"}}}
	push := &stubPusher{}
	s := New(rec, push, Options{})
	s.Enqueue(uuid.New(), achievements.EventVaultUploaded)
	s.Enqueue(uuid.New(), achievements.EventVaultUploaded)
	run(t, s)()
	require.Len(t, rec.events(), 2, "the worker carries on")
	require.Empty(t, push.keys)
}

func TestNilSink(t *testing.T) {
	var s *Sink
	require.NotPanics(t, func() {
		s.Enqueue(uuid.New(), achievements.EventQueryRan)
		s.Run(context.Background())
	})
}

// End to end against Postgres with the real Recorder: the counters both servers
// share, and a push when a threshold is crossed. Swift AchievementsServiceTests'
// "is idempotent on unlocked_at after threshold" and "partitions counters by
// tenant" cases at this layer.
func TestRecordsIntoPostgres(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	tenant, other := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{tenant, other} {
		_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
			VALUES ($1, $2, 'x', $3, now(), now())`, id, id.String()+"@t", id.String())
		require.NoError(t, err)
	}
	subs := achievements.Current.SubsMatching(achievements.EventVaultUploaded)
	require.NotEmpty(t, subs)
	first := subs[0]
	for _, sub := range subs {
		if sub.Target < first.Target {
			first = sub
		}
	}

	push := &stubPusher{}
	s := New(achievements.NewRecorder(pool), push, Options{})
	stop := run(t, s)
	for range first.Target + 1 {
		s.Enqueue(tenant, achievements.EventVaultUploaded)
	}
	s.Enqueue(other, achievements.EventVaultUploaded)
	stop()

	var (
		count    int64
		unlocked *time.Time
	)
	require.NoError(t, pool.QueryRow(ctx, `SELECT progress_count, unlocked_at FROM achievement_progress WHERE tenant_id = $1 AND achievement_key = $2`,
		tenant, first.Key).Scan(&count, &unlocked))
	require.Equal(t, first.Target+1, count)
	require.NotNil(t, unlocked)
	occurrences := 0
	for _, k := range push.keys {
		if k == first.Key {
			occurrences++
		}
	}
	require.Equal(t, 1, occurrences, "one push per unlock, however far past the threshold")
	require.NoError(t, pool.QueryRow(ctx, `SELECT progress_count FROM achievement_progress WHERE tenant_id = $1 AND achievement_key = $2`,
		other, first.Key).Scan(&count))
	require.EqualValues(t, 1, count, "counters are per tenant")
}
