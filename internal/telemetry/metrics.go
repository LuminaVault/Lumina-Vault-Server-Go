package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Swift's metric names, emitted from Go so the alert rules and dashboards that read
// them keep working as routes move (plan P5, blocker B2). Every name is the Swift
// label verbatim; Alloy's otelcol.exporter.prometheus turns the dots into
// underscores and adds _total to counters.
//
// How swift-otel's OTLPMetricsFactory exports each swift-metrics type, and so what
// Go must send to land on the same Prometheus series:
//   - Counter: a monotonic int sum. No unit unless a "unit" dimension is given.
//   - Timer: a histogram of durations, exported in seconds, with explicit bounds of
//     0, 5, 10, 25, ... 10000 ms (SwiftDurationBuckets). Its unit is the "unit"
//     dimension, if any: only the timers that pass ("unit", "s") get a _seconds
//     suffix in Prometheus. luminavault.llm.chat.duration and the RouteTelemetry
//     timers pass none, so they have no unit here either.
//   - Recorder (aggregate): a histogram of plain values with bounds 0, 5, ... 10000
//     (SwiftValueBuckets).
//
// hb_requests_total is Hummingbird's own MetricsMiddleware and is deliberately not
// emitted: the rules that read it move to the otelhttp name at D3.
const (
	// Ingestion/IngestionMetrics.swift.
	IngestionClaimed      = "luminavault.ingestion.claimed"
	IngestionCompleted    = "luminavault.ingestion.completed"
	IngestionFailed       = "luminavault.ingestion.failed"
	IngestionRetried      = "luminavault.ingestion.retried"
	IngestionDeduplicated = "luminavault.ingestion.deduplicated"
	IngestionAPNSFailures = "luminavault.ingestion.apns.failures"
	IngestionQueueLatency = "luminavault.ingestion.queue_latency" // unit s

	// Workflows/WorkflowMetrics.swift.
	WorkflowQueued                = "luminavault.workflow.queued"
	WorkflowDeduplicated          = "luminavault.workflow.deduplicated"
	WorkflowClaimed               = "luminavault.workflow.claimed"
	WorkflowCompleted             = "luminavault.workflow.completed"
	WorkflowFailed                = "luminavault.workflow.failed"
	WorkflowPaused                = "luminavault.workflow.paused"
	WorkflowCancelled             = "luminavault.workflow.cancelled"
	WorkflowApprovalsRequired     = "luminavault.workflow.approvals_required"
	WorkflowFreeFallbacks         = "luminavault.workflow.free_fallbacks"
	WorkflowEventWriteFailures    = "luminavault.workflow.event_write_failures"
	WorkflowManagedSpendUSDMicros = "luminavault.workflow.managed_spend_usd_micros" // Recorder
	WorkflowQueueLatency          = "luminavault.workflow.queue_latency"            // unit s

	// LLM/HermesLLMService.swift, RoutedHermesLLMService.swift.
	LLMChatSuccess  = "luminavault.llm.chat.success"
	LLMChatFailure  = "luminavault.llm.chat.failure"
	LLMChatDuration = "luminavault.llm.chat.duration" // no unit
	// LLM/HermesLLMStreamService.swift, RoutedHermesLLMStreamService.swift.
	LLMChatStreamSuccess      = "luminavault.llm.chat.stream.success"
	LLMChatStreamFailure      = "luminavault.llm.chat.stream.failure"
	LLMChatStreamBYOK         = "luminavault.llm.chat.stream.byok"
	LLMChatStreamBYOKFailure  = "luminavault.llm.chat.stream.byok.failure"
	LLMChatUpstreamError      = "luminavault.llm.chat.upstream_error" // LLM/UpstreamErrorTelemetry.swift
	LLMToolFailure            = "luminavault.llm.tool_failure"        // LLM/HermesToolErrorClassifier.swift
	VoiceTranscribeRequests   = "luminavault.voice.transcribe.requests"
	VoiceTranscribeDuration   = "luminavault.voice.transcribe.audio_duration" // unit s
	VoiceTranscribeLatency    = "luminavault.voice.transcribe.latency"        // unit s
	VoiceTranscribeAudioBytes = "luminavault.voice.transcribe.audio_bytes"    // Recorder
)

// RouteLabelPrefixes are the RouteTelemetry(labelPrefix:) instances App+build.swift
// builds. Each gives luminavault.<prefix>.requests, .failures and .duration.
var RouteLabelPrefixes = []string{"auth", "llm", "soul", "tts", "memory"}

// SwiftDurationBuckets is swift-otel's default duration histogram (0 ms ... 10 s),
// in seconds, the unit Go records durations in.
var SwiftDurationBuckets = []float64{0, 0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}

// SwiftValueBuckets is swift-otel's default value histogram, for Recorders.
var SwiftValueBuckets = []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000}

// Metrics holds every Swift-named instrument.
type Metrics struct {
	meter  metric.Meter
	tracer trace.Tracer

	Ingestion IngestionMetrics
	Workflow  WorkflowMetrics
	LLM       LLMMetrics
	Voice     VoiceMetrics

	mu     sync.Mutex
	routes map[string]*RouteTelemetry
}

// IngestionMetrics is IngestionMetrics.
type IngestionMetrics struct {
	Claimed, Completed, Failed, Retried, Deduplicated, APNSFailures metric.Int64Counter
	QueueLatency                                                    metric.Float64Histogram
}

// RecordQueueLatency is IngestionMetrics.recordQueueLatency: nothing without a
// creation time, and a clock skew reads as zero.
func (m IngestionMetrics) RecordQueueLatency(ctx context.Context, createdAt *time.Time) {
	recordQueueLatency(ctx, m.QueueLatency, createdAt)
}

// WorkflowMetrics is WorkflowMetrics.
type WorkflowMetrics struct {
	Queued, Deduplicated, Claimed, Completed, Failed, Paused, Cancelled metric.Int64Counter
	ApprovalsRequired, FreeFallbacks, EventWriteFailures                metric.Int64Counter
	ManagedSpendUSDMicros                                               metric.Int64Histogram
	QueueLatency                                                        metric.Float64Histogram
}

// RecordQueueLatency is WorkflowMetrics.recordQueueLatency.
func (m WorkflowMetrics) RecordQueueLatency(ctx context.Context, createdAt *time.Time) {
	recordQueueLatency(ctx, m.QueueLatency, createdAt)
}

// LLMMetrics are the chat, stream, upstream error and tool failure instruments.
type LLMMetrics struct {
	ChatSuccess, ChatFailure                                    metric.Int64Counter
	ChatDuration                                                metric.Float64Histogram
	StreamSuccess, StreamFailure, StreamBYOK, StreamBYOKFailure metric.Int64Counter
	UpstreamError, ToolFailure                                  metric.Int64Counter
}

// VoiceMetrics is VoiceMetrics. Attributes are the caller's (provider, channel,
// outcome).
type VoiceMetrics struct {
	Requests      metric.Int64Counter
	AudioDuration metric.Float64Histogram
	Latency       metric.Float64Histogram
	AudioBytes    metric.Int64Histogram
}

func recordQueueLatency(ctx context.Context, h metric.Float64Histogram, createdAt *time.Time) {
	if createdAt == nil {
		return
	}
	h.Record(ctx, max(0, time.Since(*createdAt).Seconds()))
}

// Global is the process's instruments on the global MeterProvider. Instruments made
// before Setup installs the SDK provider are delegated to it once it does, and are
// no-ops when OTel is off, as DiscardingMetricsFactory is in Swift.
var Global = NewMetrics(otel.GetMeterProvider(), otel.GetTracerProvider())

// NewMetrics builds every instrument on provider. Tests pass a ManualReader's.
func NewMetrics(provider metric.MeterProvider, tracers trace.TracerProvider) *Metrics {
	m := &Metrics{
		meter:  provider.Meter("lumina"),
		tracer: tracers.Tracer("lumina"),
		routes: map[string]*RouteTelemetry{},
	}
	c := m.counter
	m.Ingestion = IngestionMetrics{
		Claimed: c(IngestionClaimed), Completed: c(IngestionCompleted), Failed: c(IngestionFailed),
		Retried: c(IngestionRetried), Deduplicated: c(IngestionDeduplicated), APNSFailures: c(IngestionAPNSFailures),
		QueueLatency: m.duration(IngestionQueueLatency, "s"),
	}
	m.Workflow = WorkflowMetrics{
		Queued: c(WorkflowQueued), Deduplicated: c(WorkflowDeduplicated), Claimed: c(WorkflowClaimed),
		Completed: c(WorkflowCompleted), Failed: c(WorkflowFailed), Paused: c(WorkflowPaused),
		Cancelled: c(WorkflowCancelled), ApprovalsRequired: c(WorkflowApprovalsRequired),
		FreeFallbacks: c(WorkflowFreeFallbacks), EventWriteFailures: c(WorkflowEventWriteFailures),
		ManagedSpendUSDMicros: m.values(WorkflowManagedSpendUSDMicros),
		QueueLatency:          m.duration(WorkflowQueueLatency, "s"),
	}
	m.LLM = LLMMetrics{
		ChatSuccess: c(LLMChatSuccess), ChatFailure: c(LLMChatFailure), ChatDuration: m.duration(LLMChatDuration, ""),
		StreamSuccess: c(LLMChatStreamSuccess), StreamFailure: c(LLMChatStreamFailure),
		StreamBYOK: c(LLMChatStreamBYOK), StreamBYOKFailure: c(LLMChatStreamBYOKFailure),
		UpstreamError: c(LLMChatUpstreamError), ToolFailure: c(LLMToolFailure),
	}
	m.Voice = VoiceMetrics{
		Requests:      c(VoiceTranscribeRequests),
		AudioDuration: m.duration(VoiceTranscribeDuration, "s"),
		Latency:       m.duration(VoiceTranscribeLatency, "s"),
		AudioBytes:    m.values(VoiceTranscribeAudioBytes),
	}
	for _, prefix := range RouteLabelPrefixes {
		m.Route(prefix)
	}
	return m
}

// The meter only errs on an invalid name, and every name here is a constant the
// tests collect, so the no-op instrument it returns alongside is never reached.
func (m *Metrics) counter(name string) metric.Int64Counter {
	c, _ := m.meter.Int64Counter(name)
	return c
}

func (m *Metrics) duration(name, unit string) metric.Float64Histogram {
	opts := []metric.Float64HistogramOption{metric.WithExplicitBucketBoundaries(SwiftDurationBuckets...)}
	if unit != "" {
		opts = append(opts, metric.WithUnit(unit))
	}
	h, _ := m.meter.Float64Histogram(name, opts...)
	return h
}

func (m *Metrics) values(name string) metric.Int64Histogram {
	h, _ := m.meter.Int64Histogram(name, metric.WithExplicitBucketBoundaries(SwiftValueBuckets...))
	return h
}

// RouteTelemetry is RouteTelemetry (Services/Observability.swift): a request counter,
// a failure counter and a duration timer under luminavault.<labelPrefix>.
type RouteTelemetry struct {
	label    string
	tracer   trace.Tracer
	requests metric.Int64Counter
	failures metric.Int64Counter
	duration metric.Float64Histogram
}

// Route returns the RouteTelemetry for labelPrefix, one per prefix per Metrics.
func (m *Metrics) Route(labelPrefix string) *RouteTelemetry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.routes[labelPrefix]; ok {
		return r
	}
	name := "luminavault." + labelPrefix
	r := &RouteTelemetry{
		label:    "lv." + labelPrefix,
		tracer:   m.tracer,
		requests: m.counter(name + ".requests"),
		failures: m.counter(name + ".failures"),
		duration: m.duration(name+".duration", ""),
	}
	m.routes[labelPrefix] = r
	return r
}

// Observe is RouteTelemetry.observe: count the request, run body in a server span
// named operation, record its duration either way, and count and log a failure. A
// failure whose status is below 500 logs at info, Swift's notice; any other at error.
func Observe[T any](ctx context.Context, r *RouteTelemetry, operation string, body func(context.Context) (T, error)) (T, error) {
	r.requests.Add(ctx, 1)
	started := time.Now()
	ctx, span := r.tracer.Start(ctx, operation, trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()
	result, err := body(ctx)
	r.duration.Record(ctx, time.Since(started).Seconds())
	if err != nil {
		r.failures.Add(ctx, 1)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		level := slog.LevelError
		var status interface{ StatusCode() int }
		if errors.As(err, &status) && status.StatusCode() < 500 {
			level = slog.LevelInfo
		}
		slog.Log(ctx, level, operation+" failed: "+err.Error(), "logger", r.label)
		return result, err
	}
	slog.InfoContext(ctx, operation+" succeeded", "logger", r.label)
	return result, nil
}
