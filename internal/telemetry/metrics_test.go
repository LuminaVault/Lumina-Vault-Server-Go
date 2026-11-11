package telemetry

import (
	"context"
	"errors"
	"lumina/internal/config"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace/noop"
)

func newTestMetrics(t *testing.T) (*Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	return NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), noop.NewTracerProvider()), reader
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

// Every name an alert rule or dashboard reads (plan P5, blocker B2), spelled as the
// Swift labels in Services/Observability.swift, Ingestion/IngestionMetrics.swift,
// Workflows/WorkflowMetrics.swift, LLM/Transcribe/VoiceMetrics.swift and the LLM
// services. Prometheus sees them with dots as underscores and _total on counters.
var p5Names = []string{
	"luminavault.ingestion.claimed", "luminavault.ingestion.completed", "luminavault.ingestion.failed",
	"luminavault.ingestion.retried", "luminavault.ingestion.deduplicated", "luminavault.ingestion.apns.failures",
	"luminavault.ingestion.queue_latency",
	"luminavault.llm.chat.success", "luminavault.llm.chat.failure", "luminavault.llm.chat.duration",
	"luminavault.llm.chat.stream.success", "luminavault.llm.chat.stream.failure",
	"luminavault.llm.chat.stream.byok", "luminavault.llm.chat.stream.byok.failure",
	"luminavault.llm.chat.upstream_error", "luminavault.llm.tool_failure",
	"luminavault.voice.transcribe.requests", "luminavault.voice.transcribe.audio_duration",
	"luminavault.voice.transcribe.latency", "luminavault.voice.transcribe.audio_bytes",
	"luminavault.workflow.queued", "luminavault.workflow.deduplicated", "luminavault.workflow.claimed",
	"luminavault.workflow.completed", "luminavault.workflow.failed", "luminavault.workflow.paused",
	"luminavault.workflow.cancelled", "luminavault.workflow.approvals_required",
	"luminavault.workflow.free_fallbacks", "luminavault.workflow.event_write_failures",
	"luminavault.workflow.managed_spend_usd_micros", "luminavault.workflow.queue_latency",
	"luminavault.auth.requests", "luminavault.auth.failures", "luminavault.auth.duration",
	"luminavault.llm.requests", "luminavault.llm.failures", "luminavault.llm.duration",
	"luminavault.soul.requests", "luminavault.soul.failures", "luminavault.soul.duration",
	"luminavault.tts.requests", "luminavault.tts.failures", "luminavault.tts.duration",
	"luminavault.memory.requests", "luminavault.memory.failures", "luminavault.memory.duration",
}

func TestEveryP5NameIsEmitted(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()
	created := time.Now().Add(-2 * time.Second)
	in, wf, llm, voice := m.Ingestion, m.Workflow, m.LLM, m.Voice
	for _, c := range []func(){
		func() { in.Claimed.Add(ctx, 1) }, func() { in.Completed.Add(ctx, 1) }, func() { in.Failed.Add(ctx, 1) },
		func() { in.Retried.Add(ctx, 1) }, func() { in.Deduplicated.Add(ctx, 1) }, func() { in.APNSFailures.Add(ctx, 1) },
		func() { in.RecordQueueLatency(ctx, &created) },
		func() { wf.Queued.Add(ctx, 1) }, func() { wf.Deduplicated.Add(ctx, 1) }, func() { wf.Claimed.Add(ctx, 1) },
		func() { wf.Completed.Add(ctx, 1) }, func() { wf.Failed.Add(ctx, 1) }, func() { wf.Paused.Add(ctx, 1) },
		func() { wf.Cancelled.Add(ctx, 1) }, func() { wf.ApprovalsRequired.Add(ctx, 1) },
		func() { wf.FreeFallbacks.Add(ctx, 1) }, func() { wf.EventWriteFailures.Add(ctx, 1) },
		func() { wf.ManagedSpendUSDMicros.Record(ctx, 1200) }, func() { wf.RecordQueueLatency(ctx, &created) },
		func() { llm.ChatSuccess.Add(ctx, 1) }, func() { llm.ChatFailure.Add(ctx, 1) },
		func() { llm.ChatDuration.Record(ctx, 0.2) }, func() { llm.StreamSuccess.Add(ctx, 1) },
		func() { llm.StreamFailure.Add(ctx, 1) }, func() { llm.StreamBYOK.Add(ctx, 1) },
		func() { llm.StreamBYOKFailure.Add(ctx, 1) }, func() { llm.UpstreamError.Add(ctx, 1) },
		func() { llm.ToolFailure.Add(ctx, 1) },
		func() { voice.Requests.Add(ctx, 1) }, func() { voice.AudioDuration.Record(ctx, 3) },
		func() { voice.Latency.Record(ctx, 8) }, func() { voice.AudioBytes.Record(ctx, 4096) },
	} {
		c()
	}
	for _, prefix := range RouteLabelPrefixes {
		_, _ = Observe(ctx, m.Route(prefix), prefix+".op", func(context.Context) (int, error) { return 0, errors.New("x") })
	}

	got := collect(t, reader)
	for _, name := range p5Names {
		assert.Contains(t, got, name)
	}
	assert.Len(t, got, len(p5Names), "no name beyond the P5 list")
}

// Only the timers Swift gives a ("unit", "s") dimension carry a unit; a unit is what
// adds _seconds to the Prometheus name, so luminavault_llm_chat_duration_bucket and
// luminavault_ingestion_queue_latency_seconds_bucket both keep their Swift spelling.
func TestUnitsMatchSwiftDimensions(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()
	created := time.Now()
	m.Ingestion.RecordQueueLatency(ctx, &created)
	m.Workflow.RecordQueueLatency(ctx, &created)
	m.LLM.ChatDuration.Record(ctx, 1)
	m.Voice.AudioDuration.Record(ctx, 1)
	m.Voice.Latency.Record(ctx, 1)
	m.Voice.AudioBytes.Record(ctx, 1)
	m.Workflow.ManagedSpendUSDMicros.Record(ctx, 1)
	_, _ = Observe(ctx, m.Route("tts"), "tts.synthesize", func(context.Context) (int, error) { return 0, nil })

	got := collect(t, reader)
	for name, unit := range map[string]string{
		IngestionQueueLatency: "s", WorkflowQueueLatency: "s", VoiceTranscribeDuration: "s", VoiceTranscribeLatency: "s",
		LLMChatDuration: "", "luminavault.tts.duration": "", VoiceTranscribeAudioBytes: "", WorkflowManagedSpendUSDMicros: "",
	} {
		assert.Equal(t, unit, got[name].Unit, name)
	}
}

// swift-otel's default bounds are 0 ... 10000 ms for timers, exported in seconds, and
// 0 ... 10000 for recorders. Matching them keeps one set of le labels per series.
func TestHistogramBoundsAreSwiftOTelDefaults(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()
	m.LLM.ChatDuration.Record(ctx, 0.3)
	m.Workflow.ManagedSpendUSDMicros.Record(ctx, 30)
	got := collect(t, reader)

	duration := got[LLMChatDuration].Data.(metricdata.Histogram[float64])
	assert.Equal(t, []float64{0, 0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10},
		duration.DataPoints[0].Bounds)
	values := got[WorkflowManagedSpendUSDMicros].Data.(metricdata.Histogram[int64])
	assert.Equal(t, []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000},
		values.DataPoints[0].Bounds)
}

// recordQueueLatency: no creation time records nothing; a future one records 0.
func TestQueueLatencyClampsAndSkipsNil(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	m.Ingestion.RecordQueueLatency(ctx, nil)
	m.Ingestion.RecordQueueLatency(ctx, &future)
	h := collect(t, reader)[IngestionQueueLatency].Data.(metricdata.Histogram[float64])
	require.Len(t, h.DataPoints, 1)
	assert.Equal(t, uint64(1), h.DataPoints[0].Count)
	assert.Zero(t, h.DataPoints[0].Sum)
}

// RouteTelemetry.observe: every call counts a request and a duration, a thrown error
// also a failure, and the error and result pass through unchanged.
func TestObserveCountsRequestsFailuresAndDuration(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()
	r := m.Route("auth")
	assert.Same(t, r, m.Route("auth"), "one RouteTelemetry per prefix")

	v, err := Observe(ctx, r, "auth.login", func(context.Context) (string, error) { return "ok", nil })
	require.NoError(t, err)
	assert.Equal(t, "ok", v)
	boom := echo.NewHTTPError(http.StatusUnauthorized, "nope")
	_, err = Observe(ctx, r, "auth.login", func(context.Context) (string, error) { return "", boom })
	assert.Same(t, boom, err)
	_, err = Observe(ctx, r, "auth.login", func(context.Context) (string, error) { return "", errors.New("db down") })
	require.Error(t, err)

	got := collect(t, reader)
	sum := func(name string) int64 {
		var n int64
		for _, dp := range got[name].Data.(metricdata.Sum[int64]).DataPoints {
			n += dp.Value
		}
		return n
	}
	assert.Equal(t, int64(3), sum("luminavault.auth.requests"))
	assert.Equal(t, int64(2), sum("luminavault.auth.failures"))
	h := got["luminavault.auth.duration"].Data.(metricdata.Histogram[float64])
	assert.Equal(t, uint64(3), h.DataPoints[0].Count)
}

// Both servers share OTEL_SERVICE_NAME, hence Prometheus's job label; the instance
// label (service.instance.id) is what keeps Go's Swift-named series apart from Swift's.
func TestResourceHasInstanceID(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)
	res, err := buildResource(config.Config{OTelServiceName: "luminavault-api-production"})
	require.NoError(t, err)
	got, ok := res.Set().Value(semconv.ServiceInstanceIDKey)
	require.True(t, ok)
	assert.Equal(t, host, got.AsString())
}
