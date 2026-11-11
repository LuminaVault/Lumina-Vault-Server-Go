package voice

import (
	"context"
	"lumina/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// VoiceMetrics: is voice healthy right now. Dimensions stay bounded, so no tenant ID
// and only known channels; what one tenant used is the usage_events rows' question.
// The histograms carry swift-otel's bucket bounds (telemetry.SwiftDurationBuckets),
// so Go's le labels line up with Swift's.
var (
	requestCounter   = telemetry.Global.Voice.Requests
	audioDurationHis = telemetry.Global.Voice.AudioDuration
	latencyHis       = telemetry.Global.Voice.Latency
	audioBytesHis    = telemetry.Global.Voice.AudioBytes
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
