package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func ptr(s string) *string { return &s }

// MARK: - Ports LLM/OpenAIAudio/MultipartFormParserTests.swift

const realBoundary = "957a64b387861839220bf7620e771fe7"

// audioPayload is "OggS", two binary bytes, then a decoy boundary-looking run.
var audioPayload = append(append([]byte("OggS"), 0x00, 0x02), "BINARY\r\n--notaboundary"...)

// realHTTPXBody is what httpx, so the OpenAI Python SDK, puts on the wire.
func realHTTPXBody() []byte {
	head := "--" + realBoundary + "\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nwhisper-1\r\n" +
		"--" + realBoundary + "\r\nContent-Disposition: form-data; name=\"response_format\"\r\n\r\ntext\r\n" +
		"--" + realBoundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"audio.ogg\"\r\n" +
		"Content-Type: audio/ogg\r\n\r\n"
	return slices.Concat([]byte(head), audioPayload, []byte("\r\n--"+realBoundary+"--\r\n"))
}

func TestExtractsUnquotedBoundary(t *testing.T) {
	got, ok := multipartBoundary("multipart/form-data; boundary=" + realBoundary)
	require.True(t, ok)
	require.Equal(t, realBoundary, got)
}

func TestExtractsQuotedBoundary(t *testing.T) {
	got, ok := multipartBoundary(`multipart/form-data; boundary="abc"`)
	require.True(t, ok)
	require.Equal(t, "abc", got)
}

func TestBoundaryParameterNameIsCaseInsensitive(t *testing.T) {
	got, ok := multipartBoundary("multipart/form-data; BOUNDARY=xy")
	require.True(t, ok)
	require.Equal(t, "xy", got)
}

func TestReturnsNilWhenNoBoundaryPresent(t *testing.T) {
	for _, h := range []string{"application/json", "multipart/form-data", "multipart/form-data; boundary=", "", "boundary=x"} {
		_, ok := multipartBoundary(h)
		require.False(t, ok, h)
	}
}

func TestParsesRealHTTPXBody(t *testing.T) {
	parts, err := parseMultipart(realHTTPXBody(), realBoundary, defaultMaxParts)
	require.NoError(t, err)
	require.Len(t, parts, 3)
	require.Equal(t, "model", parts[0].name)
	require.Equal(t, "whisper-1", string(parts[0].body))
	require.Equal(t, "response_format", parts[1].name)
	require.Equal(t, "text", string(parts[1].body))
	require.Equal(t, "file", parts[2].name)
	require.Equal(t, ptr("audio.ogg"), parts[2].filename)
	require.Equal(t, ptr("audio/ogg"), parts[2].contentType)
	// Data fields carry no filename.
	require.Nil(t, parts[0].filename)
}

func TestBinaryPayloadContainingBoundaryLikeBytesSurvivesVerbatim(t *testing.T) {
	parts, err := parseMultipart(realHTTPXBody(), realBoundary, defaultMaxParts)
	require.NoError(t, err)
	require.Equal(t, audioPayload, firstPart(parts, "file").body)
}

// The reader against the writer we ship, so the two agree on the format.
func TestRoundTripsBodyProducedByTheAdapter(t *testing.T) {
	audio := make([]byte, 512)
	for i := range audio {
		audio[i] = byte(i % 256)
	}
	boundary := "roundtripboundary"
	written := buildMultipartBody(boundary, audio, "audio.ogg", "audio/ogg", "whisper-large-v3")
	parts, err := parseMultipart(written, boundary, defaultMaxParts)
	require.NoError(t, err)
	names := make([]string, len(parts))
	for i, p := range parts {
		names[i] = p.name
	}
	require.ElementsMatch(t, []string{"file", "model", "response_format", "temperature"}, names)
	file := firstPart(parts, "file")
	require.Equal(t, ptr("audio.ogg"), file.filename)
	require.Equal(t, ptr("audio/ogg"), file.contentType)
	require.Equal(t, audio, file.body)
}

func TestParsesPartWithEmptyBodyAndNoContentType(t *testing.T) {
	parts, err := parseMultipart([]byte("--zz\r\nContent-Disposition: form-data; name=\"empty\"\r\n\r\n\r\n--zz--\r\n"), "zz", defaultMaxParts)
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Empty(t, parts[0].body)
	require.Nil(t, parts[0].contentType)
}

func TestSkipsPreambleBeforeFirstDelimiter(t *testing.T) {
	parts, err := parseMultipart([]byte("ignored preamble\r\n--zz\r\nContent-Disposition: form-data; name=\"n\"\r\n\r\nv\r\n--zz--\r\n"), "zz", defaultMaxParts)
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, "n", parts[0].name)
}

func TestIgnoresEpilogueAfterClosingDelimiter(t *testing.T) {
	parts, err := parseMultipart([]byte("--zz\r\nContent-Disposition: form-data; name=\"n\"\r\n\r\nv\r\n--zz--\r\ntrailing junk"), "zz", defaultMaxParts)
	require.NoError(t, err)
	require.Len(t, parts, 1)
}

func TestThrowsWhenBodyIsNotMultipart(t *testing.T) {
	_, err := parseMultipart([]byte("hello"), "zzz", defaultMaxParts)
	require.ErrorIs(t, err, errNoOpeningDelimiter)
}

func TestThrowsWhenBodyIsTruncated(t *testing.T) {
	_, err := parseMultipart(realHTTPXBody()[:180], realBoundary, defaultMaxParts)
	require.Error(t, err)
}

func TestThrowsWhenClosingDelimiterIsMissing(t *testing.T) {
	_, err := parseMultipart([]byte("--zz\r\nContent-Disposition: form-data; name=\"n\"\r\n\r\nvalue"), "zz", defaultMaxParts)
	require.ErrorIs(t, err, errUnterminated)
}

func TestThrowsWhenPartHasNoName(t *testing.T) {
	_, err := parseMultipart([]byte("--zz\r\nContent-Disposition: form-data\r\n\r\nv\r\n--zz--\r\n"), "zz", defaultMaxParts)
	require.ErrorIs(t, err, errMalformedPart)
}

func TestThrowsWhenPartCountExceedsLimit(t *testing.T) {
	var raw strings.Builder
	for i := range 5 {
		raw.WriteString("--zz\r\nContent-Disposition: form-data; name=\"n" + string(rune('0'+i)) + "\"\r\n\r\nv\r\n")
	}
	raw.WriteString("--zz--\r\n")
	_, err := parseMultipart([]byte(raw.String()), "zz", 3)
	require.ErrorIs(t, err, errTooManyParts)
}

// MARK: - Ports LLM/Transcribe/OpenAICompatibleTranscribeAdapterTests.swift

// Accepting a type with no file name is a 400 upstream waiting to happen.
func TestEveryAcceptedMimeMapsToARealExtension(t *testing.T) {
	for m := range acceptedMimes {
		name := audioFilename(m)
		require.NotEqual(t, "audio.bin", name, m)
		require.Contains(t, name, ".", m)
	}
}

func TestTelegramVoiceNoteFormatsAreMapped(t *testing.T) {
	require.Equal(t, "audio.ogg", audioFilename("audio/ogg"))
	require.Equal(t, "audio.opus", audioFilename("audio/opus"))
	require.True(t, acceptedMimes["audio/ogg"])
	require.True(t, acceptedMimes["audio/opus"])
}

func TestKnownMimesMapToExpectedFilenames(t *testing.T) {
	for m, want := range map[string]string{
		"audio/m4a": "audio.m4a", "audio/x-m4a": "audio.m4a", "audio/mp4": "audio.mp4", "audio/wav": "audio.wav",
		"audio/x-wav": "audio.wav", "audio/mpeg": "audio.mp3", "audio/mpga": "audio.mpga", "audio/webm": "audio.webm",
		"audio/flac": "audio.flac",
	} {
		require.Equal(t, want, audioFilename(m), m)
	}
}

func TestUnknownMimeFallsBackToBinary(t *testing.T) {
	require.Equal(t, "audio.bin", audioFilename("application/octet-stream"))
	require.Equal(t, "audio.bin", audioFilename(""))
}

// The base URL carries the version prefix, so only the endpoint path is appended.
func TestAppendsOnlyTheEndpointPathToTheConfiguredBase(t *testing.T) {
	for base, want := range map[string]string{
		"http://whisper.horus.svc.cluster.local:8000/v1":  "http://whisper.horus.svc.cluster.local:8000/v1/audio/transcriptions",
		"http://whisper.horus.svc.cluster.local:8000/v1/": "http://whisper.horus.svc.cluster.local:8000/v1/audio/transcriptions",
		"https://api.openai.com/v1":                       "https://api.openai.com/v1/audio/transcriptions",
		"https://api.groq.com/openai/v1":                  "https://api.groq.com/openai/v1/audio/transcriptions",
	} {
		require.Equal(t, want, newOpenAICompatibleProvider(base, "", "m").endpoint)
	}
}

func TestMultipartBodyCarriesFilenameAndMimeForOgg(t *testing.T) {
	body := string(buildMultipartBody("testboundary", []byte("OggS"), audioFilename("audio/ogg"), "audio/ogg", "whisper-large-v3"))
	require.Contains(t, body, `filename="audio.ogg"`)
	require.Contains(t, body, "Content-Type: audio/ogg")
	require.Contains(t, body, `name="model"`)
	require.Contains(t, body, "whisper-large-v3")
	require.Contains(t, body, "verbose_json")
	require.True(t, strings.HasSuffix(body, "--testboundary--\r\n"))
}

func TestMultipartBodyPreservesAudioBytesVerbatim(t *testing.T) {
	audio := make([]byte, 256)
	for i := range audio {
		audio[i] = byte(i)
	}
	require.True(t, bytes.Contains(buildMultipartBody("b", audio, "audio.ogg", "audio/ogg", "m"), audio))
}

// whisperStub answers like the service, and keeps the last request.
type whisperStub struct {
	*httptest.Server
	mu    sync.Mutex
	last  *http.Request
	form  map[string]string
	audio []byte
	name  string
}

func newWhisperStub(t *testing.T, status int, reply string) *whisperStub {
	t.Helper()
	s := &whisperStub{form: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.last = r
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err == nil {
			form := multipart.NewReader(r.Body, params["boundary"])
			for {
				part, err := form.NextPart()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(part)
				if part.FormName() == "file" {
					s.audio, s.name = data, part.FileName()
					continue
				}
				s.form[part.FormName()] = string(data)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(s.Close)
	return s
}

const verboseReply = `{"task":"transcribe","text":"hello","language":"en","duration":1.5,
"segments":[{"id":0,"start":0,"end":1.5,"text":"hello","avg_logprob":-0.1}]}`

func TestProviderSendsTheWireFormatWithNoKey(t *testing.T) {
	stub := newWhisperStub(t, http.StatusOK, verboseReply)
	got, err := newOpenAICompatibleProvider(stub.URL+"/v1", "", "Systran/faster-whisper-small").transcribe(context.Background(), []byte("OggS"), "audio/ogg")
	require.NoError(t, err)
	require.Equal(t, "/v1/audio/transcriptions", stub.last.URL.Path)
	require.Empty(t, stub.last.Header.Get("Authorization"), "the in-cluster service authenticates by NetworkPolicy")
	require.Equal(t, "audio.ogg", stub.name)
	require.Equal(t, []byte("OggS"), stub.audio)
	require.Equal(t, map[string]string{"model": "Systran/faster-whisper-small", "response_format": "verbose_json", "temperature": "0"}, stub.form)
	require.Equal(t, "hello", got.text)
	require.Equal(t, "en", got.language)
	require.InDelta(t, 1.5, got.durationSeconds, 0)
	require.InDelta(t, math.Exp(-0.1), got.confidence, 0)
	require.Equal(t, []segment{{start: 0, end: 1.5, text: "hello"}}, got.segments)
}

func TestProviderSendsAKeyOnlyWhenConfigured(t *testing.T) {
	stub := newWhisperStub(t, http.StatusOK, verboseReply)
	_, err := newOpenAICompatibleProvider(stub.URL, "k", "m").transcribe(context.Background(), []byte("x"), "audio/ogg")
	require.NoError(t, err)
	require.Equal(t, "Bearer k", stub.last.Header.Get("Authorization"))
}

func TestProviderClassifiesFailures(t *testing.T) {
	for _, tc := range []struct {
		status int
		reply  string
		want   outcome
	}{
		{http.StatusBadRequest, "nope", outcomeUpstreamPermanent},
		{http.StatusNotFound, "nope", outcomeUpstreamPermanent},
		{http.StatusTooManyRequests, "slow", outcomeUpstreamTransient},
		{http.StatusServiceUnavailable, "down", outcomeUpstreamTransient},
		{http.StatusOK, "not json", outcomeDecode},
		{http.StatusOK, `{"language":"en"}`, outcomeDecode},
		{http.StatusOK, `{"text":null}`, outcomeDecode},
		{http.StatusOK, `{"text":"x","segments":[{"start":0,"end":1}]}`, outcomeDecode},
		{http.StatusOK, `{"text":"x","duration":"long"}`, outcomeDecode},
	} {
		stub := newWhisperStub(t, tc.status, tc.reply)
		_, err := newOpenAICompatibleProvider(stub.URL, "", "m").transcribe(context.Background(), []byte("x"), "audio/ogg")
		var pe *providerError
		require.ErrorAs(t, err, &pe, tc.reply)
		require.Equal(t, tc.want, pe.outcome, tc.reply)
	}
}

func TestProviderNetworkFailure(t *testing.T) {
	stub := newWhisperStub(t, http.StatusOK, verboseReply)
	stub.Close()
	_, err := newOpenAICompatibleProvider(stub.URL, "", "m").transcribe(context.Background(), []byte("x"), "audio/ogg")
	var pe *providerError
	require.ErrorAs(t, err, &pe)
	require.Equal(t, outcomeNetwork, pe.outcome)
}

// A hosted endpoint that sends only {"text"} reports unknown, not zero confidence.
func TestProviderReadsATextOnlyReply(t *testing.T) {
	stub := newWhisperStub(t, http.StatusOK, `{"text":"hi"}`)
	got, err := newOpenAICompatibleProvider(stub.URL, "", "m").transcribe(context.Background(), []byte("x"), "audio/ogg")
	require.NoError(t, err)
	require.Equal(t, "unknown", got.language)
	require.Zero(t, got.confidence)
	require.Zero(t, got.durationSeconds)
	require.Nil(t, got.segments)
}

func TestAggregateConfidenceAveragesAndClamps(t *testing.T) {
	lp := func(v float64) *float64 { return &v }
	require.Zero(t, aggregateConfidence(nil))
	require.Zero(t, aggregateConfidence([]segmentJSON{{}}))
	require.InDelta(t, (math.Exp(-0.5)+math.Exp(-1))/2, aggregateConfidence([]segmentJSON{{AvgLogprob: lp(-0.5)}, {}, {AvgLogprob: lp(-1)}}), 1e-15)
	require.InDelta(t, 1.0, aggregateConfidence([]segmentJSON{{AvgLogprob: lp(2)}}), 0)
}

// MARK: - Ports LLM/Transcribe/TranscribeProviderConfigTests.swift

func TestTheClusterWhisperIsConfiguredWithNoEnvironmentAtAll(t *testing.T) {
	r := newRegistry(Config{}, quiet())
	p, ok := r.active.(openAICompatibleProvider)
	require.True(t, ok, "the in-cluster provider must be configured by default")
	require.Equal(t, "http://whisper.horus.svc.cluster.local:8000/v1/audio/transcriptions", p.endpoint)
	require.Empty(t, p.apiKey, "the in-cluster service authenticates by NetworkPolicy")
}

func TestTheDefaultModelReachesTheUsageRow(t *testing.T) {
	require.Equal(t, "Systran/faster-whisper-small", newRegistry(Config{}, quiet()).model)
}

func TestTheImputedRateAppliesWithNoOtherTranscribeConfigSet(t *testing.T) {
	r := newRegistry(Config{ImputedUSDPerAudioMinute: 0.006}, quiet())
	require.Equal(t, int64(6000), r.card.imputedUSDMicros(60))
}

func TestAnExplicitBaseURLStillOverridesTheInClusterDefault(t *testing.T) {
	r := newRegistry(Config{BaseURL: "https://api.example.com/v1"}, quiet())
	p, ok := r.active.(openAICompatibleProvider)
	require.True(t, ok)
	require.Equal(t, "https://api.example.com/v1/audio/transcriptions", p.endpoint)
}

func TestTheCanonicalEnvSpellingsLoad(t *testing.T) {
	r := newRegistry(Config{BaseURL: "https://api.example.com/v1", APIKey: "sk-test", Model: "whisper-1", ImputedUSDPerAudioMinute: 0.006}, quiet())
	p, ok := r.active.(openAICompatibleProvider)
	require.True(t, ok)
	require.Equal(t, "sk-test", p.apiKey)
	require.Equal(t, "whisper-1", p.model)
	require.Equal(t, "whisper-1", r.model)
	require.InDelta(t, 0.006, r.card.imputedUSDPerAudioMinute, 0)
}

func TestAnUnsetProviderSelectsTheInClusterWhisper(t *testing.T) {
	r := newRegistry(Config{}, quiet())
	require.Equal(t, kindOpenAICompatible, r.kind)
	require.False(t, r.wasFallback)
}

func TestAnUnrecognisedProviderNameFallsBackAndIsReported(t *testing.T) {
	var logged bytes.Buffer
	r := newRegistry(Config{Provider: "groq"}, slog.New(slog.NewTextHandler(&logged, nil)))
	require.Equal(t, kindOpenAICompatible, r.kind)
	require.True(t, r.wasFallback)
	require.IsType(t, openAICompatibleProvider{}, r.active)
	require.Contains(t, logged.String(), "level=WARN")
	require.Contains(t, logged.String(), "groq")
}

func TestARecognisedProviderNameIsNotReportedAsAFallback(t *testing.T) {
	for _, name := range []string{"openai_compatible", " OPENAI_COMPATIBLE ", kindStub} {
		require.False(t, newRegistry(Config{Provider: name}, quiet()).wasFallback, name)
	}
}

func TestTheStubIsOnlyEverAskedFor(t *testing.T) {
	r := newRegistry(Config{Provider: "stub", StubText: "s", StubLanguage: "en", StubConfidence: 0.95, StubDurationSeconds: 30}, quiet())
	require.Equal(t, kindStub, r.kind)
	require.Empty(t, r.model, "the stub has no config row")
	got, err := r.active.transcribe(context.Background(), nil, "")
	require.NoError(t, err)
	require.Equal(t, []segment{{start: 0, end: 30, text: "s"}}, got.segments)
}

// App+build only makes an adapter with a usable URL. The config row is independent of it.
func TestABlankBaseURLHasNoAdapterButStillAConfig(t *testing.T) {
	r := newRegistry(Config{BaseURL: "  "}, quiet())
	require.Nil(t, r.active)
	require.Equal(t, DefaultModel, r.model)
}

func TestAnUnparseableBaseURLHasNeitherAdapterNorConfig(t *testing.T) {
	r := newRegistry(Config{BaseURL: "http://[bad", ImputedUSDPerAudioMinute: 1}, quiet())
	require.Nil(t, r.active)
	require.Empty(t, r.model)
	require.Zero(t, r.card.imputedUSDPerAudioMinute)
}

// MARK: - Ports LLM/Transcribe/TranscribeServiceUsageTests.swift

type recordingUsage struct {
	mu     sync.Mutex
	events []usageEvent
}

func (r *recordingUsage) record(_ context.Context, e usageEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

type fakeAdapter struct {
	result upstreamResult
	err    error
}

func (f fakeAdapter) transcribe(context.Context, []byte, string) (upstreamResult, error) {
	return f.result, f.err
}

func upstream(durationSeconds float64) upstreamResult {
	return upstreamResult{text: "hello", language: "en", confidence: 0.9, durationSeconds: durationSeconds}
}

func usageService(a adapter, rec usageRecorder) *service {
	return &service{
		reg:      registry{active: a, kind: kindStub, model: "stub", card: rateCard{imputedUSDPerAudioMinute: 0.006}},
		recorder: rec, log: quiet(),
	}
}

func runOne(t *testing.T, a adapter, channel *string) (usageEvent, TranscribeResponse, error) {
	t.Helper()
	rec := &recordingUsage{}
	res, err := usageService(a, rec).transcribe(context.Background(), []byte{1, 2, 3, 4}, "audio/ogg", uuid.New(), channel)
	require.Len(t, rec.events, 1, "exactly one event per attempt")
	return rec.events[0], res, err
}

func TestASuccessfulTranscriptionRecordsOneEvent(t *testing.T) {
	e, res, err := runOne(t, fakeAdapter{result: upstream(12)}, ptr("telegram"))
	require.NoError(t, err)
	require.Equal(t, outcomeOK, e.outcome)
	require.Equal(t, "telegram", e.channel)
	require.Equal(t, int64(12000), e.durationMilliseconds)
	require.Equal(t, kindStub, e.provider)
	require.Equal(t, "stub", e.model)
	require.Equal(t, voiceNoteSurface, e.surface)
	require.Equal(t, int64(4), e.audioBytes)
	require.Equal(t, ptr("en"), e.language)
	require.Equal(t, res.Id, e.idempotencyKey, "the response ID, so a retried write can't double-count")
}

func TestSubSecondAudioStillRecordsANonZeroDuration(t *testing.T) {
	e, _, err := runOne(t, fakeAdapter{result: upstream(0.9)}, ptr("telegram"))
	require.NoError(t, err)
	require.Equal(t, int64(900), e.durationMilliseconds)
}

func TestTheImputedCostRidesOnTheEvent(t *testing.T) {
	e, _, err := runOne(t, fakeAdapter{result: upstream(60)}, ptr("telegram"))
	require.NoError(t, err)
	require.Equal(t, int64(6000), e.imputedUSDMicros)
	require.Zero(t, e.usdMicros, "real spend stays zero")
}

func TestAnAbsentChannelHeaderIsRecordedAsUnknown(t *testing.T) {
	e, _, err := runOne(t, fakeAdapter{result: upstream(1)}, nil)
	require.NoError(t, err)
	require.Equal(t, channelUnknown, e.channel)
}

func TestAnUpstreamFailureRecordsAnEventAndStillThrows(t *testing.T) {
	e, _, err := runOne(t, fakeAdapter{err: &providerError{outcome: outcomeUpstreamPermanent, status: 400}}, ptr("telegram"))
	require.ErrorIs(t, err, errRejected)
	require.Equal(t, outcomeUpstreamPermanent, e.outcome)
	require.Equal(t, "telegram", e.channel)
	require.Zero(t, e.durationMilliseconds)
	require.Zero(t, e.imputedUSDMicros)
	require.Nil(t, e.language)
	require.NotEmpty(t, e.idempotencyKey)
}

func TestATransientFailureIsDistinguishableFromAPermanentOne(t *testing.T) {
	for o, want := range map[outcome]error{
		outcomeUpstreamTransient: errUnavailable, outcomeNetwork: errUnavailable, outcomeDecode: errUnavailable,
	} {
		e, _, err := runOne(t, fakeAdapter{err: &providerError{outcome: o}}, nil)
		require.ErrorIs(t, err, want)
		require.Equal(t, o, e.outcome)
	}
}

func TestNoConfiguredProviderRecordsANoProviderEvent(t *testing.T) {
	rec := &recordingUsage{}
	s := &service{reg: registry{kind: kindOpenAICompatible}, recorder: rec, log: quiet()}
	_, err := s.transcribe(context.Background(), []byte{1}, "audio/ogg", uuid.New(), ptr("telegram"))
	require.ErrorIs(t, err, errNoProvider)
	require.Len(t, rec.events, 1)
	require.Equal(t, outcomeNoProvider, rec.events[0].outcome)
}

type silentRecorder struct{}

func (silentRecorder) record(context.Context, usageEvent) {}

func TestARecorderThatDoesNothingStillYieldsAFullResponse(t *testing.T) {
	res, err := usageService(fakeAdapter{result: upstream(1)}, silentRecorder{}).transcribe(context.Background(), []byte{1}, "audio/ogg", uuid.New(), ptr("telegram"))
	require.NoError(t, err)
	require.Equal(t, "hello", res.Text)
}

// A provider error that isn't typed reads as a network failure rather than a 500.
func TestAnUntypedAdapterErrorIsUnavailable(t *testing.T) {
	e, _, err := runOne(t, fakeAdapter{err: errors.New("boom")}, nil)
	require.ErrorIs(t, err, errUnavailable)
	require.Equal(t, outcomeNetwork, e.outcome)
}

// A caller who hangs up doesn't cancel the attempt or lose its row.
type ctxAdapter struct{ seen *error }

func (a ctxAdapter) transcribe(ctx context.Context, _ []byte, _ string) (upstreamResult, error) {
	*a.seen = ctx.Err()
	return upstream(1), nil
}

func TestAHangUpDoesNotCancelTheAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var seen error
	rec := &recordingUsage{}
	_, err := usageService(ctxAdapter{&seen}, rec).transcribe(ctx, []byte{1}, "audio/ogg", uuid.New(), nil)
	require.NoError(t, err)
	require.NoError(t, seen)
	require.Len(t, rec.events, 1)
}

func TestMillisecondsFromSeconds(t *testing.T) {
	require.Zero(t, millisecondsFromSeconds(0))
	require.Zero(t, millisecondsFromSeconds(-1))
	require.Zero(t, millisecondsFromSeconds(math.NaN()))
	require.Zero(t, millisecondsFromSeconds(math.Inf(1)))
	require.Equal(t, int64(1), millisecondsFromSeconds(0.0006))
	require.Equal(t, int64(math.MaxInt64), millisecondsFromSeconds(1e300))
}

// MARK: - Ports LLM/Transcribe/VoiceUsageTests.swift

func TestAPlainPlatformNameSurvivesIntact(t *testing.T) {
	require.Equal(t, "telegram", sanitizedChannel(ptr("telegram")))
	require.Equal(t, "whatsapp_cloud", sanitizedChannel(ptr("whatsapp_cloud")))
}

func TestCaseAndSurroundingWhitespaceAreNormalisedAway(t *testing.T) {
	require.Equal(t, "telegram", sanitizedChannel(ptr("  Telegram  ")))
}

func TestInjectionAttemptsTruncateAtTheFirstInvalidCharacter(t *testing.T) {
	require.Equal(t, "telegram", sanitizedChannel(ptr("telegram\nX-Evil: 1")))
	require.Equal(t, "telegram", sanitizedChannel(ptr("telegram'; DROP TABLE usage_events--")))
}

func TestAMissingOrEmptyChannelIsUnknownRatherThanNil(t *testing.T) {
	require.Equal(t, channelUnknown, sanitizedChannel(nil))
	for _, raw := range []string{"", "   ", "!!!"} {
		require.Equal(t, channelUnknown, sanitizedChannel(&raw), raw)
	}
}

func TestAnAbsurdlyLongValueIsCapped(t *testing.T) {
	require.Len(t, sanitizedChannel(ptr(strings.Repeat("a", 500))), channelMaxLength)
}

func TestKnownChannelsKeepTheirOwnMetricLabel(t *testing.T) {
	for _, c := range []string{"telegram", "discord", "app", channelUnknown} {
		require.Equal(t, c, channelMetricLabel(c))
	}
}

func TestAnUnrecognisedChannelCollapsesToOtherInMetrics(t *testing.T) {
	require.Equal(t, "other", channelMetricLabel("somepluginplatform"))
	require.Equal(t, "somepluginplatform", sanitizedChannel(ptr("somepluginplatform")))
}

func TestImputedCostIsThePerMinuteRateAppliedToTheDuration(t *testing.T) {
	card := rateCard{imputedUSDPerAudioMinute: 0.006}
	require.Equal(t, int64(6000), card.imputedUSDMicros(60))
	require.Equal(t, int64(3000), card.imputedUSDMicros(30))
}

func TestAZeroRateImputesNothing(t *testing.T) {
	require.Zero(t, rateCard{}.imputedUSDMicros(600))
}

func TestNegativeOrNonFiniteDurationsClampToZero(t *testing.T) {
	card := rateCard{imputedUSDPerAudioMinute: 0.006}
	require.Zero(t, card.imputedUSDMicros(-10))
	require.Zero(t, card.imputedUSDMicros(math.NaN()))
	require.Zero(t, card.imputedUSDMicros(math.Inf(1)))
}

func TestEachUpstreamErrorMapsToItsOwnOutcome(t *testing.T) {
	for status, want := range map[int]outcome{400: outcomeUpstreamPermanent, 503: outcomeUpstreamTransient} {
		stub := newWhisperStub(t, status, "x")
		_, err := newOpenAICompatibleProvider(stub.URL, "", "m").transcribe(context.Background(), []byte("x"), "audio/ogg")
		var pe *providerError
		require.ErrorAs(t, err, &pe)
		require.Equal(t, want, pe.outcome)
	}
}

func TestOutcomeWireValuesAreStable(t *testing.T) {
	require.Equal(t, outcome("ok"), outcomeOK)
	require.Equal(t, outcome("upstream_permanent"), outcomeUpstreamPermanent)
	require.Equal(t, outcome("upstream_transient"), outcomeUpstreamTransient)
	require.Equal(t, outcome("network"), outcomeNetwork)
	require.Equal(t, outcome("decode"), outcomeDecode)
	require.Equal(t, outcome("no_provider"), outcomeNoProvider)
}

// MARK: - The surface: formats and MIME resolution, which Swift has no unit test for

func TestResolveMIMEPrefersTheExtension(t *testing.T) {
	got, err := resolveMIME(ptr("a.OGG"), ptr("application/octet-stream"))
	require.NoError(t, err)
	require.Equal(t, "audio/ogg", got)

	got, err = resolveMIME(ptr("a.aac"), ptr("AUDIO/WAV; x=1"))
	require.NoError(t, err)
	require.Equal(t, "audio/wav", got)

	_, err = resolveMIME(nil, nil)
	require.ErrorIs(t, err, errBadFormat)
	require.Contains(t, errBadFormat.Error(), "flac, m4a, mp3, mp4, mpeg, mpga, oga, ogg, opus, wav, webm.")
}

func TestUsageMetadataNames(t *testing.T) {
	raw, err := json.Marshal(usageMetadata{Channel: "c"})
	require.NoError(t, err)
	var keys map[string]any
	require.NoError(t, json.Unmarshal(raw, &keys))
	want := []string{"audioBytes", "channel", "imputedUsdMicros", "latencyMs", "model", "outcome", "provider", "surface", "usdMicros"}
	got := make([]string, 0, len(keys))
	for k := range keys {
		got = append(got, k)
	}
	slices.Sort(got)
	require.Equal(t, want, got, "language is left out when nil")
}

func TestUpstreamTimeoutsLeaveRoomForWhisper(t *testing.T) {
	// 7 s + 0.38x realtime on the 120 s cap is about 53 s.
	require.Greater(t, upstreamHeaderTimeout, 53*time.Second)
	require.Greater(t, upstreamTotalTimeout, upstreamHeaderTimeout)
	p := newOpenAICompatibleProvider("http://x", "", "m")
	require.Equal(t, upstreamTotalTimeout, p.client.Timeout)
	tr, ok := p.client.Transport.(*http.Transport)
	require.True(t, ok)
	require.Equal(t, upstreamHeaderTimeout, tr.ResponseHeaderTimeout)
}
