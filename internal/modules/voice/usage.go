package voice

import (
	"context"
	"encoding/json"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/swiftstr"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Channel values: what's recorded when the header says nothing usable, and the
// longest token kept. VoiceChannel in Swift.
const (
	channelUnknown   = "unknown"
	channelMaxLength = 32
)

// knownMetricLabels are the channels with their own metric dimension. Anything else
// is "other": a plugin platform names itself, and a fresh label per request would
// never stop costing.
var knownMetricLabels = map[string]bool{
	"app": true, "telegram": true, "discord": true, "whatsapp": true, "whatsapp_cloud": true,
	"slack": true, "signal": true, "bluebubbles": true, "cli": true, channelUnknown: true,
}

// sanitizedChannel is VoiceChannel.sanitized: a bare lowercase token, truncated at the
// first character outside [a-z0-9_-] rather than filtered, so "telegram\nX-Evil: 1"
// is telegram and not a plausible name no platform has. raw nil is an absent header.
func sanitizedChannel(raw *string) string {
	if raw == nil {
		return channelUnknown
	}
	var token strings.Builder
	n := 0
	for _, c := range swiftstr.Characters(strings.ToLower(swiftstr.TrimWhitespacesAndNewlines(*raw))) {
		if len(c) != 1 || !isChannelByte(c[0]) {
			break
		}
		token.WriteString(c)
		if n++; n == channelMaxLength {
			break
		}
	}
	if token.Len() == 0 {
		return channelUnknown
	}
	return token.String()
}

func isChannelByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}

// channelMetricLabel is VoiceChannel.metricLabel.
func channelMetricLabel(channel string) string {
	if knownMetricLabels[channel] {
		return channel
	}
	return "other"
}

// outcome is VoiceUsageOutcome. Saved dashboard queries read these strings from the
// JSONB column, so they don't change.
type outcome string

const (
	outcomeOK                outcome = "ok"
	outcomeUpstreamPermanent outcome = "upstream_permanent"
	outcomeUpstreamTransient outcome = "upstream_transient"
	outcomeNetwork           outcome = "network"
	outcomeDecode            outcome = "decode"
	outcomeNoProvider        outcome = "no_provider"
)

// voiceNoteSurface is VoiceUsageEvent.voiceNoteSurface.
const voiceNoteSurface = "voice_note"

// usageEvent is VoiceUsageEvent, one transcription attempt as metered. Integers,
// because the columns are.
type usageEvent struct {
	tenant               uuid.UUID
	occurredAt           time.Time
	channel              string
	surface              string
	provider             string
	model                string
	outcome              outcome
	durationMilliseconds int64
	latencyMilliseconds  int64
	audioBytes           int64
	language             *string
	usdMicros            int64
	imputedUSDMicros     int64
	idempotencyKey       string
}

// usageRecorder is VoiceUsageRecorder. It can't fail, so metering can't cost a user
// their voice note.
type usageRecorder interface {
	record(ctx context.Context, event usageEvent)
}

// rateCard is TranscribeRateCard: what this traffic would have cost hosted. Zero
// imputes nothing.
type rateCard struct{ imputedUSDPerAudioMinute float64 }

func (r rateCard) imputedUSDMicros(durationSeconds float64) int64 {
	if r.imputedUSDPerAudioMinute <= 0 || math.IsInf(durationSeconds, 0) || math.IsNaN(durationSeconds) || durationSeconds <= 0 {
		return 0
	}
	micros := math.Round(durationSeconds / 60 * r.imputedUSDPerAudioMinute * 1_000_000)
	if math.IsInf(micros, 0) || math.IsNaN(micros) || micros <= 0 {
		return 0
	}
	if micros >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(micros)
}

// millisecondsFromSeconds is TranscribeService.milliseconds(fromSeconds:). Swift traps
// on a duration too large for Int64. This clamps.
func millisecondsFromSeconds(seconds float64) int64 {
	if math.IsInf(seconds, 0) || math.IsNaN(seconds) || seconds <= 0 {
		return 0
	}
	ms := math.Round(seconds * 1000)
	if ms >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(ms)
}

// usageMetric matches the CHECK constraint on usage_events.metric.
const usageMetric = "voice_transcribe"

const insertUsageEvent = `INSERT INTO usage_events (tenant_id, occurred_at, metric, amount, source, idempotency_key, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
ON CONFLICT (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL
DO NOTHING`

// usageMetadata is the JSONB payload. Dashboard queries read these names.
type usageMetadata struct {
	Channel          string  `json:"channel"`
	Surface          string  `json:"surface"`
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	Outcome          string  `json:"outcome"`
	LatencyMs        int64   `json:"latencyMs"`
	AudioBytes       int64   `json:"audioBytes"`
	Language         *string `json:"language,omitempty"`
	USDMicros        int64   `json:"usdMicros"`
	ImputedUSDMicros int64   `json:"imputedUsdMicros"`
}

// pgUsageStore is VoiceUsageEventStore: one usage_events row per attempt. Errors are
// logged and swallowed.
type pgUsageStore struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

func (s pgUsageStore) record(ctx context.Context, e usageEvent) {
	metadata, err := json.Marshal(usageMetadata{
		Channel: e.channel, Surface: e.surface, Provider: e.provider, Model: e.model, Outcome: string(e.outcome),
		LatencyMs: e.latencyMilliseconds, AudioBytes: e.audioBytes, Language: e.language,
		USDMicros: e.usdMicros, ImputedUSDMicros: e.imputedUSDMicros,
	})
	if err != nil {
		s.log.ErrorContext(ctx, "voice usage metadata encode failed", "error", err)
		metadata = []byte("{}")
	}
	if _, err := s.pool.Exec(ctx, insertUsageEvent, e.tenant, e.occurredAt, usageMetric,
		e.durationMilliseconds, e.channel, e.idempotencyKey, string(metadata)); err != nil {
		s.log.ErrorContext(ctx, "usage_events voice record failed", "tenant_id", ids.Upper(e.tenant), "outcome", string(e.outcome), "error", err)
	}
}
