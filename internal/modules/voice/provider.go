package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/swiftstr"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TranscribeProviderKind raw values, which TRANSCRIBE_PROVIDER names. There is one
// real kind: which endpoint it reaches is a base URL, not a code path.
const (
	kindOpenAICompatible = "openai_compatible"
	kindStub             = "stub"
)

const (
	// DefaultBaseURL is the cluster's own whisper: free per request, no key (access
	// is by NetworkPolicy), and no audio leaves the cluster. The base URL carries the
	// version prefix. ~/Work/production/CLAUDE.md forbids adding a paid speech API.
	DefaultBaseURL = "http://whisper.horus.svc.cluster.local:8000/v1"
	// DefaultModel is the multilingual model the in-cluster service preloads. The
	// `.en` one would return confident nonsense for the other fifteen locales.
	DefaultModel = "Systran/faster-whisper-small"

	// upstreamHeaderTimeout is URLSession.shared's 60 s, which is an idle timeout:
	// whisper sends nothing until it's done, about 7 s plus 0.38x realtime.
	upstreamHeaderTimeout = 60 * time.Second
	// upstreamTotalTimeout bounds a stalled reply, which URLSession's seven-day
	// resource timeout effectively doesn't.
	upstreamTotalTimeout = 5 * time.Minute
	// maxUpstreamBody caps the JSON read back. An oversized reply fails to decode.
	maxUpstreamBody = 32 << 20
)

// Config is the transcribe.* keys. An empty BaseURL means DefaultBaseURL and an empty
// Model DefaultModel. A base URL of only whitespace configures no adapter, and the
// routes answer 503.
type Config struct {
	Provider      string
	BaseURL       string
	APIKey        string
	Model         string
	MtokPerSecond float64
	// ImputedUSDPerAudioMinute is a shadow rate, never real spend.
	ImputedUSDPerAudioMinute float64
	StubText                 string
	StubLanguage             string
	StubConfidence           float64
	StubDurationSeconds      float64
}

// segment is TranscribeSegment.
type segment struct {
	start, end float64
	text       string
}

// upstreamResult is TranscribeUpstreamResult. Segments nil is "the provider sent none".
type upstreamResult struct {
	text            string
	language        string
	confidence      float64
	durationSeconds float64
	segments        []segment
}

// adapter is TranscribeProviderAdapter. Failures are *providerError.
type adapter interface {
	transcribe(ctx context.Context, audio []byte, mime string) (upstreamResult, error)
}

// providerError is TranscribeProviderError.
type providerError struct {
	outcome outcome
	status  int
	detail  string
}

func (e *providerError) Error() string {
	return fmt.Sprintf("transcribe provider: %s %d: %s", e.outcome, e.status, e.detail)
}

// registry is TranscribeProviderRegistry: one active adapter per boot, and what the
// usage rows read off its config.
type registry struct {
	active        adapter
	kind          string
	wasFallback   bool
	model         string
	mtokPerSecond float64
	card          rateCard
}

// newRegistry is what App+build and TranscribeProviderRegistry.from do. An unknown
// provider falls back rather than failing boot, with a warning: "groq" was the default
// for months and is no longer a kind.
func newRegistry(cfg Config, log *slog.Logger) registry {
	name := strings.ToLower(swiftstr.TrimWhitespacesAndNewlines(cfg.Provider))
	if name == "" {
		name = kindOpenAICompatible
	}
	r := registry{kind: kindOpenAICompatible}
	switch name {
	case kindStub:
		r.kind = kindStub
		// The stub has no config row, so its usage rows carry no model and no rate.
		r.active = stubAdapter{
			result: upstreamResult{
				text: cfg.StubText, language: cfg.StubLanguage, confidence: cfg.StubConfidence, durationSeconds: cfg.StubDurationSeconds,
				segments: []segment{{start: 0, end: cfg.StubDurationSeconds, text: cfg.StubText}},
			},
		}
		return r
	case kindOpenAICompatible:
	default:
		r.wasFallback = true
		log.Warn("transcribe.provider is not a known provider, falling back", "provider", name, "active", kindOpenAICompatible,
			"known", kindOpenAICompatible+", "+kindStub)
	}

	model := cfg.Model
	if model == "" {
		model = DefaultModel
	}
	raw := cfg.BaseURL
	if raw == "" {
		raw = DefaultBaseURL
	}
	raw = swiftstr.TrimWhitespacesAndNewlines(raw)
	_, parseErr := url.Parse(raw)
	if raw != "" && parseErr == nil {
		r.active = newOpenAICompatibleProvider(raw, cfg.APIKey, model)
	}
	// TranscribeProviderConfig.isEnabled: an unusable URL with no key has no config
	// row. A blank one still does, on the default base URL.
	if parseErr == nil || swiftstr.TrimWhitespacesAndNewlines(cfg.APIKey) != "" {
		r.model = model
		r.mtokPerSecond = cfg.MtokPerSecond
		r.card = rateCard{imputedUSDPerAudioMinute: cfg.ImputedUSDPerAudioMinute}
	}
	return r
}

// stubAdapter is StubTranscribeAdapter, selected only by TRANSCRIBE_PROVIDER=stub.
type stubAdapter struct{ result upstreamResult }

func (s stubAdapter) transcribe(context.Context, []byte, string) (upstreamResult, error) {
	return s.result, nil
}

// openAICompatibleProvider is speech to text over the OpenAI /audio/transcriptions
// wire format, named for the format and not a vendor (OpenAICompatibleTranscribeAdapter
// in Swift). The key is optional: an empty one sends no Authorization header at all.
type openAICompatibleProvider struct {
	endpoint string
	apiKey   string
	model    string
	client   *http.Client
}

func newOpenAICompatibleProvider(baseURL, apiKey, model string) openAICompatibleProvider {
	var client http.Client
	client.Timeout = upstreamTotalTimeout
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t = t.Clone()
		t.ResponseHeaderTimeout = upstreamHeaderTimeout
		client.Transport = t
	}
	return openAICompatibleProvider{
		endpoint: strings.TrimRight(baseURL, "/") + "/audio/transcriptions",
		apiKey:   apiKey,
		model:    model,
		client:   &client,
	}
}

func (p openAICompatibleProvider) transcribe(ctx context.Context, audio []byte, mime string) (upstreamResult, error) {
	boundary := "Boundary-" + strings.ToUpper(uuid.NewString())
	body := buildMultipartBody(boundary, audio, audioFilename(mime), mime, p.model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return upstreamResult{}, &providerError{outcome: outcomeNetwork, detail: err.Error()}
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)

	res, err := p.client.Do(req)
	if err != nil {
		return upstreamResult{}, &providerError{outcome: outcomeNetwork, detail: err.Error()}
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxUpstreamBody+1))
	if err != nil {
		return upstreamResult{}, &providerError{outcome: outcomeNetwork, detail: err.Error()}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		preview := string(data[:min(len(data), 512)])
		if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
			return upstreamResult{}, &providerError{outcome: outcomeUpstreamTransient, status: res.StatusCode, detail: preview}
		}
		return upstreamResult{}, &providerError{outcome: outcomeUpstreamPermanent, status: res.StatusCode, detail: preview}
	}
	result, err := decodeVerboseJSON(data)
	if err != nil {
		return upstreamResult{}, &providerError{outcome: outcomeDecode, status: res.StatusCode, detail: err.Error()}
	}
	return result, nil
}

// buildMultipartBody is OpenAICompatibleTranscribeAdapter.buildMultipartBody: the file,
// the model, verbose_json and temperature 0. The audio passes through untouched. There
// is no ffmpeg in this cluster, and needing one would mean something is wrong.
func buildMultipartBody(boundary string, audio []byte, filename, mime, model string) []byte {
	var b bytes.Buffer
	prefix := "--" + boundary + "\r\n"
	b.WriteString(prefix)
	b.WriteString(`Content-Disposition: form-data; name="file"; filename="` + filename + "\"\r\n")
	b.WriteString("Content-Type: " + mime + "\r\n\r\n")
	b.Write(audio)
	b.WriteString("\r\n")
	field := func(name, value string) {
		b.WriteString(prefix)
		b.WriteString(`Content-Disposition: form-data; name="` + name + "\"\r\n\r\n")
		b.WriteString(value + "\r\n")
	}
	field("model", model)
	field("response_format", "verbose_json")
	field("temperature", "0")
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes()
}

// audioFilename is OpenAICompatibleTranscribeAdapter.filename(for:). Whisper-family
// endpoints pick the decoder from the extension, not the part's Content-Type, so every
// accepted type needs a real one. audio.bin is unreachable for accepted input.
func audioFilename(mime string) string {
	switch mime {
	case "audio/m4a", "audio/x-m4a":
		return "audio.m4a"
	case "audio/mp4":
		return "audio.mp4"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "audio.wav"
	case "audio/mpeg":
		return "audio.mp3"
	case "audio/mpga":
		return "audio.mpga"
	case "audio/webm":
		return "audio.webm"
	case "audio/ogg", "audio/vorbis":
		return "audio.ogg"
	case "audio/opus":
		return "audio.opus"
	case "audio/flac", "audio/x-flac":
		return "audio.flac"
	}
	return "audio.bin"
}

// The wire shape is TranscriptionVerboseJSON. Pointers, because Swift's Decodable
// refuses a missing or null required field where encoding/json would read a zero.
type verboseJSON struct {
	Text     *string        `json:"text"`
	Language *string        `json:"language"`
	Duration *float64       `json:"duration"`
	Segments *[]segmentJSON `json:"segments"`
}

type segmentJSON struct {
	Start      *float64 `json:"start"`
	End        *float64 `json:"end"`
	Text       *string  `json:"text"`
	AvgLogprob *float64 `json:"avg_logprob"`
}

var errMissingField = errors.New("missing required field")

func decodeVerboseJSON(data []byte) (upstreamResult, error) {
	var v verboseJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return upstreamResult{}, err
	}
	if v.Text == nil {
		return upstreamResult{}, errMissingField
	}
	out := upstreamResult{text: *v.Text, language: "unknown"}
	if v.Language != nil {
		out.language = *v.Language
	}
	if v.Duration != nil {
		out.durationSeconds = *v.Duration
	}
	if v.Segments == nil {
		return out, nil
	}
	out.segments = make([]segment, 0, len(*v.Segments))
	for i, s := range *v.Segments {
		if s.Start == nil || s.End == nil || s.Text == nil {
			return upstreamResult{}, fmt.Errorf("segment %s: %w", strconv.Itoa(i), errMissingField)
		}
		out.segments = append(out.segments, segment{start: *s.Start, end: *s.End, text: *s.Text})
	}
	out.confidence = aggregateConfidence(*v.Segments)
	return out, nil
}

// aggregateConfidence maps per-segment avg_logprob onto [0,1]: exp of it recovers the
// geometric-mean token probability, averaged across segments. 0 with none to go on.
func aggregateConfidence(segments []segmentJSON) float64 {
	var sum float64
	var n int
	for _, s := range segments {
		if s.AvgLogprob != nil {
			sum += math.Exp(*s.AvgLogprob)
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return max(0, min(1, sum/float64(n)))
}
