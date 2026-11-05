package voice

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/usagemeter"
	"lumina/internal/wire"
	"math"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const meterTimeout = 10 * time.Second

var (
	errNoProvider  = wire.NewError(http.StatusServiceUnavailable, "transcribe provider not configured")
	errRejected    = wire.NewError(http.StatusBadGateway, "transcribe upstream rejected request")
	errUnavailable = wire.NewError(http.StatusBadGateway, "transcribe upstream unavailable")
)

// responseSegment spells out the anonymous element type of TranscribeResponse.Segments.
type responseSegment = struct {
	End   wire.Float64 `json:"end"`
	Start wire.Float64 `json:"start"`
	Text  string       `json:"text"`
}

// service is TranscribeService, which both surfaces call, so metering can't drift.
type service struct {
	reg      registry
	usage    *usagemeter.Meter
	recorder usageRecorder
	log      *slog.Logger
}

// transcribe leaves exactly one usage event per attempt, success or not: metering only
// what worked makes a failing provider look like a drop in demand. A caller who hangs
// up doesn't cancel it, since Swift's handler runs on.
func (s *service) transcribe(ctx context.Context, audio []byte, mime string, tenant uuid.UUID, channel *string) (TranscribeResponse, error) {
	ctx = context.WithoutCancel(ctx)
	a := attempt{
		tenant: tenant, occurredAt: time.Now(), channel: sanitizedChannel(channel), audioBytes: int64(len(audio)),
	}
	if s.reg.active == nil {
		s.log.ErrorContext(ctx, "no active transcribe provider: check TRANSCRIBE_PROVIDER")
		s.meter(ctx, a, outcomeNoProvider, 0, nil, "")
		return TranscribeResponse{}, errNoProvider
	}
	result, err := s.reg.active.transcribe(ctx, audio, mime)
	if err != nil {
		s.log.ErrorContext(ctx, "transcribe provider error", "error", err)
		var pe *providerError
		if !errors.As(err, &pe) {
			pe = &providerError{outcome: outcomeNetwork, detail: err.Error()}
		}
		s.meter(ctx, a, pe.outcome, 0, nil, "")
		if pe.outcome == outcomeUpstreamPermanent {
			return TranscribeResponse{}, errRejected
		}
		return TranscribeResponse{}, errUnavailable
	}

	s.recordTokens(tenant, result.durationSeconds)

	id := ids.Upper(uuid.New())
	res := TranscribeResponse{
		Id: id, Text: result.text, Language: result.language,
		Confidence: wire.Float64(result.confidence), DurationSeconds: wire.Float64(result.durationSeconds),
	}
	if result.segments != nil {
		segments := make([]responseSegment, len(result.segments))
		for i, seg := range result.segments {
			segments[i] = responseSegment{End: wire.Float64(seg.end), Start: wire.Float64(seg.start), Text: seg.text}
		}
		res.Segments = &segments
	}
	s.meter(ctx, a, outcomeOK, result.durationSeconds, &result.language, id)
	return res, nil
}

// recordTokens credits the usage meter per second of audio, on its own goroutine as
// Swift's Task is. The default rate is zero, because the in-cluster service costs
// nothing per request, so normally nothing is written.
func (s *service) recordTokens(tenant uuid.UUID, durationSeconds float64) {
	tokens := math.Round(durationSeconds * s.reg.mtokPerSecond * 1_000_000)
	if s.usage == nil || !(tokens > 0) || tokens >= math.MaxInt64 {
		return
	}
	model := "transcribe:" + s.reg.kind
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), meterTimeout)
		defer cancel()
		s.usage.Record(ctx, tenant, model, int64(tokens), 0)
	}()
}

// attempt is what's fixed about an attempt before the upstream call.
type attempt struct {
	tenant     uuid.UUID
	occurredAt time.Time
	channel    string
	audioBytes int64
}

// meter records one attempt to the ledger and the live metrics, awaited rather than
// detached: a row is worth an insert's latency here, and a detached task can be dropped
// at shutdown. idempotencyKey is the response ID on success.
func (s *service) meter(ctx context.Context, a attempt, o outcome, durationSeconds float64, language *string, idempotencyKey string) {
	if idempotencyKey == "" {
		idempotencyKey = ids.Upper(uuid.New())
	}
	e := usageEvent{
		tenant: a.tenant, occurredAt: a.occurredAt, channel: a.channel, surface: voiceNoteSurface,
		provider: s.reg.kind, model: s.reg.model, outcome: o,
		durationMilliseconds: millisecondsFromSeconds(durationSeconds),
		latencyMilliseconds:  max(0, time.Since(a.occurredAt).Milliseconds()),
		audioBytes:           a.audioBytes, language: language,
		// Real spend. The in-cluster service bills nothing per request.
		usdMicros:        0,
		imputedUSDMicros: s.reg.card.imputedUSDMicros(durationSeconds),
		idempotencyKey:   idempotencyKey,
	}
	recordMetrics(ctx, e)
	if s.recorder != nil {
		s.recorder.record(ctx, e)
	}
}
