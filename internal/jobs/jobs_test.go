package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"lumina/internal/config"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// fakeClock moves only when a test advances it. The runner's loop registers a timer
// only after a run has written its metrics and log line, so waitTimers doubles as
// "the previous run is fully finished".
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []fakeTimer
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, fakeTimer{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, timer := range c.timers {
		if timer.at.After(c.now) {
			kept = append(kept, timer)
		} else {
			timer.ch <- c.now
		}
	}
	c.timers = kept
}

func (c *fakeClock) waitTimers(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.timers) == n
	}, 5*time.Second, time.Millisecond, "waiting for %d timers", n)
}

// nextTimer is how far ahead the only pending timer sits.
func (c *fakeClock) nextTimer(t *testing.T) time.Duration {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	require.Len(t, c.timers, 1)
	return c.timers[0].at.Sub(c.now)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for line := range strings.Lines(b.buf.String()) {
		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &entry))
		if entry["msg"] == msg {
			out = append(out, entry)
		}
	}
	return out
}

type harness struct {
	clock  *fakeClock
	logs   *syncBuffer
	reader *sdkmetric.ManualReader
	opts   Options
}

var bootTime = time.Date(2026, 11, 5, 10, 30, 0, 0, time.UTC)

func newHarness() *harness {
	h := &harness{clock: &fakeClock{now: bootTime}, logs: &syncBuffer{}, reader: sdkmetric.NewManualReader()}
	h.opts = Options{
		Clock:         h.clock,
		Logger:        slog.New(slog.NewJSONHandler(h.logs, nil)),
		MeterProvider: sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader)),
		Jitter:        func(limit time.Duration) time.Duration { return limit / 2 },
	}
	return h
}

// start runs a Runner with every registered job enabled, and returns a stop that
// cancels it and waits for Run to return.
func (h *harness) start(t *testing.T, registered ...Job) (cancel, wait func()) {
	t.Helper()
	cfg := config.Config{OTelServiceName: "svc"}
	for _, job := range registered {
		cfg.LVGoJobs = append(cfg.LVGoJobs, job.Name)
	}
	runner, err := New(cfg, registered, h.opts)
	require.NoError(t, err)
	ctx, cancelCtx := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.Run(ctx)
	}()
	wait = func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return")
		}
	}
	t.Cleanup(func() {
		cancelCtx()
		// A test that ends a stuck-job scenario has already returned from Run.
		<-done
	})
	return cancelCtx, wait
}

func (h *harness) gauge(t *testing.T, job string) (float64, bool) {
	t.Helper()
	for _, m := range h.metrics(t) {
		if m.Name != lastSuccessMetric {
			continue
		}
		gauge, ok := m.Data.(metricdata.Gauge[float64])
		require.True(t, ok, "%s is %T", m.Name, m.Data)
		for _, point := range gauge.DataPoints {
			if v, _ := point.Attributes.Value("job"); v.AsString() == job {
				return point.Value, true
			}
		}
	}
	return 0, false
}

func (h *harness) runCount(t *testing.T, job, outcome string) int64 {
	t.Helper()
	for _, m := range h.metrics(t) {
		if m.Name != runsMetric {
			continue
		}
		sum, ok := m.Data.(metricdata.Sum[int64])
		require.True(t, ok, "%s is %T", m.Name, m.Data)
		require.True(t, sum.IsMonotonic)
		for _, point := range sum.DataPoints {
			gotJob, _ := point.Attributes.Value("job")
			gotOutcome, _ := point.Attributes.Value("outcome")
			if gotJob.AsString() == job && gotOutcome.AsString() == outcome {
				return point.Value
			}
		}
	}
	return 0
}

func (h *harness) metrics(t *testing.T) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, h.reader.Collect(context.Background(), &rm))
	var out []metricdata.Metrics
	for _, scope := range rm.ScopeMetrics {
		out = append(out, scope.Metrics...)
	}
	return out
}

func noop(context.Context) error { return nil }

func TestNewRefusesUnknownNameInLVGoJobs(t *testing.T) {
	known := Job{Name: "KnownWorker", Schedule: Every(time.Minute, 0), Run: noop}
	_, err := New(config.Config{LVGoJobs: []string{"KnownWorker", "KnownWorkr"}}, []Job{known}, Options{})
	require.ErrorContains(t, err, `unknown job "KnownWorkr"`)
	require.ErrorContains(t, err, "KnownWorker")
	// With nothing registered yet, any name is unknown.
	_, err = New(config.Config{LVGoJobs: []string{"KnownWorker"}}, nil, Options{})
	require.ErrorContains(t, err, `unknown job "KnownWorker"`)
	// LV_GO_JOBS="A, B" splits on the comma only.
	_, err = New(config.Config{LVGoJobs: []string{"KnownWorker", " KnownWorker"}}, []Job{known}, Options{})
	require.ErrorContains(t, err, `unknown job " KnownWorker"`)
}

func TestNewRefusesBadRegistrations(t *testing.T) {
	good := Schedule{interval: time.Minute}
	for name, registered := range map[string][]Job{
		"no name":     {{Schedule: good, Run: noop}},
		"no run":      {{Name: "A", Schedule: good}},
		"duplicate":   {{Name: "A", Schedule: good, Run: noop}, {Name: "A", Schedule: good, Run: noop}},
		"no schedule": {{Name: "A", Run: noop}},
		"bad hour":    {{Name: "A", Schedule: DailyUTC(24), Run: noop}},
		"bad jitter":  {{Name: "A", Schedule: Every(time.Minute, -time.Second), Run: noop}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(config.Config{}, registered, Options{})
			require.Error(t, err)
		})
	}
}

// With LV_GO_JOBS unset nothing starts, and Run returns at once. That is every
// deployment's state until a job moves.
func TestNoJobEnabledStartsNothing(t *testing.T) {
	h := newHarness()
	var runs atomic.Int32
	runner, err := New(config.Config{}, []Job{{Name: "A", Schedule: Every(time.Minute, 0), Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}}}, h.opts)
	require.NoError(t, err)
	runner.Run(context.Background())
	assert.Zero(t, runs.Load())
	lines := h.logs.lines(t, "background jobs")
	require.Len(t, lines, 1)
	assert.Equal(t, []any{}, lines[0]["enabled"])
	assert.Empty(t, h.metrics(t))
}

func TestOnlyEnabledJobsStartAndAreLogged(t *testing.T) {
	h := newHarness()
	var on, off atomic.Int32
	cfg := config.Config{LVGoJobs: []string{"On", "Also"}}
	counting := func(n *atomic.Int32) func(context.Context) error {
		return func(context.Context) error {
			n.Add(1)
			return nil
		}
	}
	runner, err := New(cfg, []Job{
		{Name: "On", Schedule: Every(time.Hour, 0), Run: counting(&on)},
		{Name: "Off", Schedule: Every(time.Hour, 0), Run: counting(&off)},
		{Name: "Also", Schedule: Every(time.Hour, 0), Run: noop},
	}, h.opts)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.Run(ctx)
	h.clock.waitTimers(t, 2)
	assert.EqualValues(t, 1, on.Load())
	assert.Zero(t, off.Load())
	assert.Equal(t, []any{"Also", "On"}, h.logs.lines(t, "background jobs")[0]["enabled"])
}

// A duplicate in LV_GO_JOBS doesn't start a job twice.
func TestDuplicateNameStartsOneLoop(t *testing.T) {
	h := newHarness()
	runner, err := New(config.Config{LVGoJobs: []string{"A", "A"}}, []Job{{Name: "A", Schedule: Every(time.Hour, 0), Run: noop}}, h.opts)
	require.NoError(t, err)
	assert.Len(t, runner.enabled, 1)
}

func TestEveryRunsAfterJitterThenIntervalAfterEachRunEnds(t *testing.T) {
	h := newHarness()
	runs := make(chan time.Time, 8)
	finish := make(chan struct{})
	h.start(t, Job{Name: "A", Schedule: Every(10*time.Minute, 30*time.Second), Run: func(context.Context) error {
		runs <- h.clock.Now()
		<-finish
		return nil
	}})

	h.clock.waitTimers(t, 1)
	assert.Equal(t, 15*time.Second, h.clock.nextTimer(t), "the harness's jitter is half the limit")
	assert.Empty(t, runs)
	h.clock.Advance(15 * time.Second)
	assert.Equal(t, bootTime.Add(15*time.Second), <-runs)

	// The run takes 3 minutes, and the interval starts when it ends.
	h.clock.Advance(3 * time.Minute)
	finish <- struct{}{}
	h.clock.waitTimers(t, 1)
	assert.Equal(t, 10*time.Minute, h.clock.nextTimer(t))
	h.clock.Advance(10 * time.Minute)
	assert.Equal(t, bootTime.Add(15*time.Second+13*time.Minute), <-runs)
	close(finish)
}

func TestEveryWithoutJitterRunsAtBoot(t *testing.T) {
	h := newHarness()
	runs := make(chan struct{}, 2)
	h.start(t, Job{Name: "A", Schedule: Every(time.Hour, 0), Run: func(context.Context) error {
		runs <- struct{}{}
		return nil
	}})
	<-runs
}

func TestScheduleWait(t *testing.T) {
	unused := func(time.Duration) time.Duration {
		t.Fatal("daily has no jitter")
		return 0
	}
	at := func(hour, minute, second int) time.Time {
		return time.Date(2026, 11, 5, hour, minute, second, 0, time.UTC)
	}
	for name, tc := range map[string]struct {
		now  time.Time
		want time.Duration
	}{
		"before the hour":    {at(2, 30, 0), 30 * time.Minute},
		"exactly the hour":   {at(3, 0, 0), 24 * time.Hour},
		"after the hour":     {at(3, 0, 1), 24*time.Hour - time.Second},
		"later that day":     {at(23, 0, 0), 4 * time.Hour},
		"non-UTC clock":      {time.Date(2026, 11, 5, 0, 0, 0, 0, time.FixedZone("x", -5*3600)), 22 * time.Hour},
		"month end":          {time.Date(2026, 11, 30, 4, 0, 0, 0, time.UTC), 23 * time.Hour},
		"sub-second to next": {at(2, 59, 59).Add(999 * time.Millisecond), time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, DailyUTC(3).wait(tc.now, true, unused))
			assert.Equal(t, tc.want, DailyUTC(3).wait(tc.now, false, unused))
		})
	}
}

func TestDailyDoesNotRunAtBootAndRepeats(t *testing.T) {
	h := newHarness() // boots at 10:30 UTC
	runs := make(chan time.Time, 2)
	h.start(t, Job{Name: "A", Schedule: DailyUTC(11), Run: func(context.Context) error {
		runs <- h.clock.Now()
		return nil
	}})
	h.clock.waitTimers(t, 1)
	assert.Empty(t, runs)
	h.clock.Advance(30 * time.Minute)
	assert.Equal(t, time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC), <-runs)
	h.clock.waitTimers(t, 1)
	assert.Equal(t, 24*time.Hour, h.clock.nextTimer(t))
}

func TestSuccessWritesMetricsAndOneLogLine(t *testing.T) {
	h := newHarness()
	h.start(t, Job{Name: "A", Schedule: Every(time.Hour, 0), Run: noop})
	h.clock.waitTimers(t, 1)

	value, ok := h.gauge(t, "A")
	require.True(t, ok)
	assert.InDelta(t, float64(bootTime.Unix()), value, 0)
	assert.EqualValues(t, 1, h.runCount(t, "A", outcomeOK))

	lines := h.logs.lines(t, "job run")
	require.Len(t, lines, 1)
	assert.Equal(t, "A", lines[0]["job"])
	assert.Equal(t, outcomeOK, lines[0]["outcome"])
	assert.Equal(t, "INFO", lines[0]["level"])
	assert.NotContains(t, lines[0], "error")

	// The label set the final gate reads, and the one that keeps environments apart.
	for _, m := range h.metrics(t) {
		if gauge, ok := m.Data.(metricdata.Gauge[float64]); ok {
			assert.True(t, gauge.DataPoints[0].Attributes.Equals(attrSet("job", "A", "service", "svc")), gauge.DataPoints[0].Attributes.Encoded(attribute.DefaultEncoder()))
		}
		assert.Empty(t, m.Unit, "%s: a unit would change the exported name", m.Name)
	}
}

func attrSet(kv ...string) *attribute.Set {
	var attrs []attribute.KeyValue
	for i := 0; i < len(kv); i += 2 {
		attrs = append(attrs, attribute.String(kv[i], kv[i+1]))
	}
	set := attribute.NewSet(attrs...)
	return &set
}

func TestFailureKeepsLastSuccessAndTheJobKeepsRunning(t *testing.T) {
	for name, tc := range map[string]struct {
		run     func(context.Context) error
		outcome string
		level   string
	}{
		"error":   {func(context.Context) error { return errors.New("boom") }, outcomeError, "ERROR"},
		"panic":   {func(context.Context) error { panic("boom") }, outcomePanic, "ERROR"},
		"skipped": {func(context.Context) error { return ErrSkipped }, outcomeSkipped, "INFO"},
		"wrapped": {func(context.Context) error { return errors.Join(ErrSkipped) }, outcomeSkipped, "INFO"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			var runs atomic.Int32
			h.start(t, Job{Name: "A", Schedule: Every(time.Hour, 0), Run: func(ctx context.Context) error {
				switch runs.Add(1) {
				case 1:
					return nil
				case 2:
					return tc.run(ctx)
				}
				return nil
			}})
			h.clock.waitTimers(t, 1)
			first := bootTime.Unix()

			h.clock.Advance(time.Hour)
			require.Eventually(t, func() bool { return runs.Load() == 2 }, 5*time.Second, time.Millisecond)
			h.clock.waitTimers(t, 1)
			value, _ := h.gauge(t, "A")
			assert.InDelta(t, float64(first), value, 0, "last-success must not move")
			assert.EqualValues(t, 1, h.runCount(t, "A", tc.outcome))

			lines := h.logs.lines(t, "job run")
			require.Len(t, lines, 2, "one line per run")
			assert.Equal(t, tc.outcome, lines[1]["outcome"])
			assert.Equal(t, tc.level, lines[1]["level"])
			if tc.outcome == outcomeSkipped {
				assert.NotContains(t, lines[1], "error")
			} else {
				assert.Contains(t, lines[1]["error"], "boom")
			}
			if tc.outcome == outcomePanic {
				assert.Contains(t, lines[1]["stack"], "goroutine")
			}

			h.clock.Advance(time.Hour)
			require.Eventually(t, func() bool { return runs.Load() == 3 }, 5*time.Second, time.Millisecond, "the job must survive")
			h.clock.waitTimers(t, 1)
			value, _ = h.gauge(t, "A")
			assert.InDelta(t, float64(bootTime.Add(2*time.Hour).Unix()), value, 0)
		})
	}
}

func TestOneJobDoesNotHoldUpAnother(t *testing.T) {
	h := newHarness()
	release := make(chan struct{})
	defer close(release)
	ran := make(chan struct{}, 1)
	h.start(t,
		Job{Name: "Stuck", Schedule: Every(time.Hour, 0), Run: func(context.Context) error {
			<-release
			return nil
		}},
		Job{Name: "Free", Schedule: Every(time.Hour, 0), Run: func(context.Context) error {
			ran <- struct{}{}
			return nil
		}},
	)
	<-ran
}

func TestShutdownWhileWaiting(t *testing.T) {
	h := newHarness()
	var runs atomic.Int32
	cancel, wait := h.start(t, Job{Name: "A", Schedule: DailyUTC(11), Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}})
	h.clock.waitTimers(t, 1)
	cancel()
	wait()
	assert.Zero(t, runs.Load())
	assert.Empty(t, h.logs.lines(t, "background jobs still running after shutdown timeout"))
}

// The run interrupted by shutdown gets a canceled context, Run waits for it, and it is
// still counted: the OTel SDK drops measurements made with a canceled context.
func TestShutdownCancelsTheRunWaitsForItAndCountsIt(t *testing.T) {
	h := newHarness()
	started := make(chan struct{})
	finish := make(chan struct{})
	cancel, wait := h.start(t, Job{Name: "A", Schedule: Every(time.Hour, 0), Run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		<-finish
		return ctx.Err()
	}})
	<-started
	cancel()
	h.clock.waitTimers(t, 1) // Run's shutdown timeout, so it has seen the cancel
	close(finish)
	wait()

	assert.EqualValues(t, 1, h.runCount(t, "A", outcomeCanceled))
	_, ok := h.gauge(t, "A")
	assert.False(t, ok)
	lines := h.logs.lines(t, "job run")
	require.Len(t, lines, 1)
	assert.Equal(t, "INFO", lines[0]["level"], "shutdown is not a failure")
	assert.NotContains(t, lines[0], "error")
}

// A run that finishes its work as shutdown arrives is a success.
func TestSuccessDuringShutdownIsRecorded(t *testing.T) {
	h := newHarness()
	started := make(chan struct{})
	cancel, wait := h.start(t, Job{Name: "A", Schedule: Every(time.Hour, 0), Run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return nil
	}})
	<-started
	cancel()
	wait()
	_, ok := h.gauge(t, "A")
	assert.True(t, ok)
}

// An error that isn't the cancellation is still a failure, even during shutdown.
func TestRealErrorDuringShutdownIsAFailure(t *testing.T) {
	h := newHarness()
	started := make(chan struct{})
	cancel, wait := h.start(t, Job{Name: "A", Schedule: Every(time.Hour, 0), Run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return errors.New("disk full")
	}})
	<-started
	cancel()
	wait()
	assert.EqualValues(t, 1, h.runCount(t, "A", outcomeError))
}

func TestShutdownGivesUpOnAJobThatIgnoresCancellation(t *testing.T) {
	h := newHarness()
	h.opts.ShutdownTimeout = time.Minute
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	cancel, wait := h.start(t, Job{Name: "A", Schedule: Every(time.Hour, 0), Run: func(context.Context) error {
		close(started)
		<-release
		return nil
	}})
	<-started
	cancel()
	h.clock.waitTimers(t, 1)
	h.clock.Advance(time.Minute)
	wait()
	lines := h.logs.lines(t, "background jobs still running after shutdown timeout")
	require.Len(t, lines, 1)
	assert.Equal(t, "1m0s", lines[0]["timeout"])
}
