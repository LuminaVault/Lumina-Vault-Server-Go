// Package jobs runs the Go server's background jobs.
//
// A job is on only if LV_GO_JOBS names it. The runner takes no lock: exactly-one
// comes from LV_GO_JOBS on a single-replica Deployment and the infra move rule
// (docs/migration/jobs.md), so a job must not assume more than that.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/config"
	"math/rand/v2"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metric names. Neither instrument sets a unit: the OTLP-to-Prometheus conversion
// appends unit suffixes, and these names must reach alert rules as written.
const (
	lastSuccessMetric = "lv_job_last_success_timestamp_seconds"
	runsMetric        = "lv_job_runs_total"
)

// Outcomes of one run, used as the runs metric's outcome attribute and in the log line.
const (
	outcomeOK       = "ok"
	outcomeSkipped  = "skipped"
	outcomeCanceled = "canceled"
	outcomeError    = "error"
	outcomePanic    = "panic"
)

const defaultShutdownTimeout = 15 * time.Second

// ErrSkipped is what Run returns when a tick found nothing it was due to do, like a
// weekly job waking outside its hour. The run is counted but last-success doesn't
// move, so that metric keeps meaning the job did its work.
var ErrSkipped = errors.New("jobs: run skipped")

// Job is one background job.
type Job struct {
	// Name is the Swift type name, so LV_GO_JOBS reads like docs/migration/jobs.md.
	Name     string
	Schedule Schedule
	// Run does one pass. It must return soon after ctx is canceled.
	Run func(ctx context.Context) error
}

// Schedule says when a job runs. Build one with Every or DailyUTC.
type Schedule struct {
	interval    time.Duration
	startJitter time.Duration
	daily       bool
	hour        int
}

// Every runs the job once at boot, after a random delay below startJitter, then again
// interval after each run ends. This is the Swift workers' run-then-sleep loop, so a
// run never overlaps itself. startJitter 0 runs at boot straight away.
func Every(interval, startJitter time.Duration) Schedule {
	return Schedule{interval: interval, startJitter: startJitter}
}

// DailyUTC runs the job when the UTC clock next reaches hour:00, and not at boot.
func DailyUTC(hour int) Schedule {
	return Schedule{daily: true, hour: hour}
}

func (s Schedule) validate() error {
	switch {
	case s.daily && (s.hour < 0 || s.hour > 23):
		return fmt.Errorf("hour %d is not 0 to 23", s.hour)
	case !s.daily && s.interval <= 0:
		return fmt.Errorf("interval %s is not positive", s.interval)
	case s.startJitter < 0:
		return fmt.Errorf("start jitter %s is negative", s.startJitter)
	}
	return nil
}

// wait returns how long to sleep before the next run.
func (s Schedule) wait(now time.Time, first bool, jitter func(time.Duration) time.Duration) time.Duration {
	if s.daily {
		now = now.UTC()
		next := time.Date(now.Year(), now.Month(), now.Day(), s.hour, 0, 0, 0, time.UTC)
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		return next.Sub(now)
	}
	if !first {
		return s.interval
	}
	if s.startJitter == 0 {
		return 0
	}
	return jitter(s.startJitter)
}

// Clock is the runner's source of time. Tests pass a fake one.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Options are the runner's test seams. The zero value is production.
type Options struct {
	Clock  Clock
	Logger *slog.Logger // defaults to slog.Default()
	// MeterProvider defaults to the global one telemetry.Setup installs.
	MeterProvider metric.MeterProvider
	// Jitter returns a random duration in [0, limit). limit is always positive.
	Jitter func(limit time.Duration) time.Duration
	// ShutdownTimeout bounds how long Run waits for a job that ignores cancellation.
	ShutdownTimeout time.Duration
}

// Runner runs the enabled jobs until its context ends.
type Runner struct {
	enabled         []Job
	clock           Clock
	logger          *slog.Logger
	jitter          func(time.Duration) time.Duration
	shutdownTimeout time.Duration
	service         attribute.KeyValue
	lastSuccess     metric.Float64Gauge
	runs            metric.Int64Counter
}

// New checks the registered jobs against cfg. It fails on a name in LV_GO_JOBS that no
// registered job has, because a typo would otherwise silently disable a job, so the
// caller should treat its error as a boot failure.
func New(cfg config.Config, registered []Job, opts Options) (*Runner, error) {
	known := make([]string, 0, len(registered))
	var enabled []Job
	for _, job := range registered {
		if job.Name == "" {
			return nil, errors.New("jobs: a registered job has no name")
		}
		if slices.Contains(known, job.Name) {
			return nil, fmt.Errorf("jobs: %s registered twice", job.Name)
		}
		if job.Run == nil {
			return nil, fmt.Errorf("jobs: %s has no Run", job.Name)
		}
		if err := job.Schedule.validate(); err != nil {
			return nil, fmt.Errorf("jobs: %s: %w", job.Name, err)
		}
		known = append(known, job.Name)
		if cfg.JobEnabled(job.Name) {
			enabled = append(enabled, job)
		}
	}
	for _, name := range cfg.LVGoJobs {
		if name != config.AllJobs && !slices.Contains(known, name) {
			slices.Sort(known)
			return nil, fmt.Errorf("jobs: LV_GO_JOBS names unknown job %q (registered: [%s])", name, strings.Join(known, ", "))
		}
	}

	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MeterProvider == nil {
		opts.MeterProvider = otel.GetMeterProvider()
	}
	if opts.Jitter == nil {
		opts.Jitter = func(limit time.Duration) time.Duration {
			return rand.N(limit) //nolint:gosec // schedule spread, not a secret
		}
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = defaultShutdownTimeout
	}
	meter := opts.MeterProvider.Meter("lumina/jobs")
	lastSuccess, err := meter.Float64Gauge(lastSuccessMetric)
	if err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	runs, err := meter.Int64Counter(runsMetric)
	if err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	return &Runner{
		enabled:         enabled,
		clock:           opts.Clock,
		logger:          opts.Logger,
		jitter:          opts.Jitter,
		shutdownTimeout: opts.ShutdownTimeout,
		// Alloy lets a data point's job attribute replace the job label it derives from
		// service.name, so without this production's and staging's series for a job are
		// the same series.
		service:     attribute.String("service", cfg.OTelServiceName),
		lastSuccess: lastSuccess,
		runs:        runs,
	}, nil
}

// Run logs the enabled jobs, runs them until ctx ends, then waits for runs in progress,
// up to the shutdown timeout. With no job enabled it returns straight away.
func (r *Runner) Run(ctx context.Context) {
	names := make([]string, 0, len(r.enabled))
	for _, job := range r.enabled {
		names = append(names, job.Name)
	}
	slices.Sort(names)
	r.logger.Info("background jobs", "enabled", names)
	if len(r.enabled) == 0 {
		return
	}

	var loops sync.WaitGroup
	for _, job := range r.enabled {
		loops.Go(func() { r.loop(ctx, job) })
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		loops.Wait()
	}()
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	select {
	case <-done:
	case <-r.clock.After(r.shutdownTimeout):
		r.logger.Warn("background jobs still running after shutdown timeout", "timeout", r.shutdownTimeout.String())
	}
}

func (r *Runner) loop(ctx context.Context, job Job) {
	for first := true; ; first = false {
		if wait := job.Schedule.wait(r.clock.Now(), first, r.jitter); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-r.clock.After(wait):
			}
		}
		if ctx.Err() != nil {
			return
		}
		r.runOnce(ctx, job)
	}
}

// runOnce does one run and writes its metrics and its one log line.
func (r *Runner) runOnce(ctx context.Context, job Job) {
	start := r.clock.Now()
	stack, err := call(ctx, job)
	end := r.clock.Now()

	outcome, level := outcomeOK, slog.LevelInfo
	switch {
	case stack != nil:
		outcome, level = outcomePanic, slog.LevelError
	case err == nil:
	case errors.Is(err, ErrSkipped):
		outcome = outcomeSkipped
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		outcome = outcomeCanceled
	default:
		outcome, level = outcomeError, slog.LevelError
	}

	// Not ctx: the OTel SDK drops a measurement whose context is canceled, and the run
	// that was interrupted by shutdown is the one most likely to have one.
	metricCtx := context.WithoutCancel(ctx)
	name := attribute.String("job", job.Name)
	if outcome == outcomeOK {
		r.lastSuccess.Record(metricCtx, float64(end.Unix()), metric.WithAttributes(name, r.service))
	}
	r.runs.Add(metricCtx, 1, metric.WithAttributes(name, r.service, attribute.String("outcome", outcome)))

	attrs := []any{"job", job.Name, "outcome", outcome, "duration_ms", end.Sub(start).Milliseconds()}
	if err != nil && outcome != outcomeSkipped && outcome != outcomeCanceled {
		attrs = append(attrs, "error", err.Error())
	}
	if stack != nil {
		attrs = append(attrs, "stack", string(stack))
	}
	r.logger.Log(metricCtx, level, "job run", attrs...)
}

// call runs the job and turns a panic into an error plus its stack.
func call(ctx context.Context, job Job) (stack []byte, err error) {
	defer func() {
		if p := recover(); p != nil {
			stack, err = debug.Stack(), fmt.Errorf("panic: %v", p)
		}
	}()
	return nil, job.Run(ctx)
}
