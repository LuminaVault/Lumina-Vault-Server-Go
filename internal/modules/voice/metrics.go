package voice

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// VoiceMetrics: is voice healthy right now. Dimensions stay bounded, so no tenant ID
// and only known channels; what one tenant used is the usage_events rows' question.
var (
	meter               = otel.Meter("lumina/voice")
	requestCounter, _   = meter.Int64Counter("luminavault.voice.transcribe.requests")
	audioDurationHis, _ = meter.Float64Histogram("luminavault.voice.transcribe.audio_duration", metric.WithUnit("s"))
	latencyHis, _       = meter.Float64Histogram("luminavault.voice.transcribe.latency", metric.WithUnit("s"))
	audioBytesHis, _    = meter.Int64Histogram("luminavault.voice.transcribe.audio_bytes")
)

// metricAttributes is VoiceMetrics.dimensions.
func metricAttributes(e usageEvent) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("provider", e.provider),
		attribute.String("channel", channelMetricLabel(e.channel)),
		attribute.String("outcome", string(e.outcome)),
	}
}

// recordMetrics is VoiceMetrics.recordTranscribe. Every attempt counts, so a failure
// rate can be read off it. Only a success has a real duration.
func recordMetrics(ctx context.Context, e usageEvent) {
	attrs := metric.WithAttributes(metricAttributes(e)...)
	requestCounter.Add(ctx, 1, attrs)
	if e.outcome != outcomeOK {
		return
	}
	audioDurationHis.Record(ctx, float64(e.durationMilliseconds)/1000, attrs)
	latencyHis.Record(ctx, float64(e.latencyMilliseconds)/1000, attrs)
	audioBytesHis.Record(ctx, e.audioBytes, attrs)
}
