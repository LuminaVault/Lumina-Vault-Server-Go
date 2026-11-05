// Package voice serves speech to text and its refusal of spoken replies:
// POST /v1/transcribe, POST /v1/audio/transcriptions and POST /v1/audio/speech. It's
// the Go port of LuminaVaultServer Sources/App/LLM/Transcribe and LLM/OpenAIAudio.
//
// Transcription goes to the cluster's own whisper service over the OpenAI wire format.
// There is no paid speech provider, no vendor name in a type, and no ffmpeg. Hermes
// calls whisper directly, so voice rows in usage_events stay near zero: an accepted
// decision, not a bug to repoint here.
package voice

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/voice/oapi-codegen.yaml api/openapi.yaml"

import (
	"io"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/openaierror"
	"lumina/internal/ratelimit"
	"lumina/internal/swiftstr"
	"lumina/internal/usagemeter"
	"lumina/internal/wire"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	// maxTranscribeBytes is /v1/transcribe's cap on the raw audio body.
	maxTranscribeBytes = 10 * 1024 * 1024
	// maxAudioBytes is OpenAI's own ceiling, and the same cap Hermes applies.
	maxAudioBytes = 25 * 1024 * 1024
	// channelHeader names the messaging platform a voice note arrived on. Advisory:
	// an unusable value is recorded as unknown and never rejected.
	channelHeader = "X-Lumina-Channel"
	// defaultAppChannel is /v1/transcribe's channel with no header. It's the app's own
	// microphone route, so `app` is right rather than unknown.
	defaultAppChannel = "app"

	// speechMessage is the contract of /v1/audio/speech: Hermes relays it to the user.
	speechMessage = "Spoken replies are not available yet. Voice messages you send are still transcribed."
)

// acceptedMimes is TranscribeController.acceptedMimes. Each needs a real extension in
// audioFilename.
var acceptedMimes = map[string]bool{
	"audio/m4a": true, "audio/x-m4a": true, "audio/mp4": true, "audio/wav": true, "audio/x-wav": true,
	"audio/wave": true, "audio/mpeg": true, "audio/mpga": true, "audio/webm": true, "audio/ogg": true,
	"audio/opus": true, "audio/flac": true, "audio/x-flac": true,
}

// mimeForExtension is OpenAIAudioController.mimeForExtension. The extension wins over
// the part's Content-Type, because httpx labels file parts application/octet-stream
// while naming them correctly, and upstream dispatches on the extension anyway.
var mimeForExtension = map[string]string{
	"ogg": "audio/ogg", "oga": "audio/ogg", "opus": "audio/opus", "m4a": "audio/m4a", "mp4": "audio/mp4",
	"mp3": "audio/mpeg", "mpeg": "audio/mpeg", "mpga": "audio/mpga", "wav": "audio/wav", "webm": "audio/webm",
	"flac": "audio/flac",
}

var (
	errBadType = wire.NewError(http.StatusUnsupportedMediaType,
		"Content-Type must be one of: "+strings.Join(slices.Sorted(maps.Keys(acceptedMimes)), ", "))
	errTranscribeTooLarge = wire.NewError(http.StatusRequestEntityTooLarge,
		"audio body exceeds "+strconv.Itoa(maxTranscribeBytes)+" byte cap")

	errNoBoundary = wire.NewError(http.StatusBadRequest, "Content-Type must be multipart/form-data with a boundary.")
	errAudioLarge = wire.NewError(http.StatusRequestEntityTooLarge,
		"Audio file is too large. The maximum size is "+strconv.Itoa(maxAudioBytes/(1024*1024))+" MB.")
	errMalformed   = wire.NewError(http.StatusBadRequest, "Malformed multipart/form-data body.")
	errMissingFile = wire.NewError(http.StatusBadRequest, "Missing required form field: file.")
	errEmptyFile   = wire.NewError(http.StatusBadRequest, "Uploaded audio file is empty.")
	errBadFormat   = wire.NewError(http.StatusBadRequest,
		"Unsupported audio format. Supported formats: "+strings.Join(slices.Sorted(maps.Keys(mimeForExtension)), ", ")+".")
	errSpeech = wire.NewError(http.StatusNotImplemented, speechMessage)
)

// Module holds the two route groups (App+build.swift "transcribeGroup",
// "audioTranscriptionsGroup", "audioSpeechGroup"). The handlers ask for identity
// themselves, so with enforcement off an anonymous request still counts against the
// "ip:" buckets before its 401.
type Module struct {
	svc           *service
	identify      echo.MiddlewareFunc
	audioIdentify echo.MiddlewareFunc
	// transcribeGate is platform funded: /v1/transcribe spends what we run. audioGate
	// deliberately isn't: voice on /v1/audio is offered to every tier and bounded by
	// the tier-scaled daily cap.
	transcribeGate echo.MiddlewareFunc
	audioGate      echo.MiddlewareFunc
	limits         *ratelimit.Store
	tier           func(*echo.Context) (string, bool)
	log            *slog.Logger
}

// New builds the module. identify is auth.Identify and audioIdentify auth.AudioIdentify,
// the one authenticator that takes a token scoped to audio. transcribeGate is
// entitlement.PlatformFundedMiddleware for chat, audioGate entitlement.Middleware for
// chat with no credential check.
func New(pool *pgxpool.Pool, usage *usagemeter.Meter, identify, audioIdentify, transcribeGate, audioGate echo.MiddlewareFunc, limits *ratelimit.Store, cfg Config) *Module {
	log := slog.Default().With("module", "voice")
	return &Module{
		svc:           &service{reg: newRegistry(cfg, log), usage: usage, recorder: pgUsageStore{pool: pool, log: log}, log: log},
		identify:      identify,
		audioIdentify: audioIdentify, transcribeGate: transcribeGate, audioGate: audioGate,
		limits: limits, tier: ratelimit.PGTier(pool), log: log,
	}
}

// Register mounts the routes. The envelope middleware is outermost on /v1/audio, so it
// rewrites the rate limits and the paywall too. Both upload routes are exempt from the
// server's read timeout; a slow connection can't send 25 MB in 30 s.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	r.POST("/v1/transcribe", m.transcribe,
		httpserver.LongUpload(),
		m.identify,
		m.transcribeGate,
		ratelimit.Middleware(m.limits, ratelimit.TranscribeByUserPerMinute, ratelimit.UserOrIPKey),
		ratelimit.Middleware(m.limits, ratelimit.TranscribeByUserDaily, ratelimit.UserOrIPKey))
	r.POST("/v1/audio/transcriptions", m.audioTranscriptions,
		httpserver.LongUpload(),
		openaierror.Middleware(),
		m.audioIdentify,
		m.audioGate,
		ratelimit.InFlightLimit(ratelimit.AudioTranscriptionsInFlight, ratelimit.UserOrIPKey),
		ratelimit.Middleware(m.limits, ratelimit.AudioTranscriptionsByUserPerMinute, ratelimit.UserOrIPKey),
		ratelimit.TieredMiddleware(m.limits, ratelimit.AudioTranscriptionsByUserDaily, ratelimit.UserOrIPKey, m.tier))
	r.POST("/v1/audio/speech", m.speech,
		openaierror.Middleware(),
		m.audioIdentify,
		m.audioGate,
		ratelimit.Middleware(m.limits, ratelimit.AudioSpeechByUserPerMinute, ratelimit.UserOrIPKey),
		ratelimit.TieredMiddleware(m.limits, ratelimit.AudioSpeechByUserDaily, ratelimit.UserOrIPKey, m.tier))
}

// MARK: - POST /v1/transcribe

func (m *Module) transcribe(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	raw, _ := wire.HeaderValue(c.Request(), "Content-Type")
	mime := bareMediaType(raw)
	if !acceptedMimes[mime] {
		return errBadType
	}
	audio, err := m.readBody(c, maxTranscribeBytes, errTranscribeTooLarge)
	if err != nil {
		return err
	}
	channel := defaultAppChannel
	if v, ok := wire.HeaderValue(c.Request(), channelHeader); ok {
		channel = v
	}
	res, err := m.svc.transcribe(c.Request().Context(), audio, mime, tenant, &channel)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, res)
}

// bareMediaType is a Content-Type's first non-empty ";" piece, trimmed and lowercased.
func bareMediaType(header string) string {
	pieces := splitNonEmpty(header, ";")
	if len(pieces) == 0 {
		return ""
	}
	return strings.ToLower(swiftstr.TrimWhitespaces(pieces[0]))
}

// readBody refuses a declared length over limit before reading, then enforces it for a
// length nothing declared. A failed read answers tooLarge too, as `collect(upTo:)` does.
func (m *Module) readBody(c *echo.Context, limit int64, tooLarge error) ([]byte, error) {
	if c.Request().ContentLength > limit {
		return nil, tooLarge
	}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, limit+1))
	if err != nil {
		m.log.WarnContext(c.Request().Context(), "voice body collect failed", "error", err)
		return nil, tooLarge
	}
	if int64(len(body)) > limit {
		return nil, tooLarge
	}
	return body, nil
}

// MARK: - POST /v1/audio/speech

// speech is mounted rather than absent on purpose. Tenant containers are configured
// with tts.provider openai pointed here, because any unrecognised provider name in
// Hermes silently falls back to Edge TTS: free, keyless and outside every control on
// this path. A route that refuses cleanly is what makes "spoken replies are off" true.
func (m *Module) speech(c *echo.Context) error {
	if _, ok := auth.Identity(c); !ok {
		return wire.ErrUnauthenticated
	}
	return errSpeech
}

// MARK: - POST /v1/audio/transcriptions

// verboseTranscription is OpenAIVerboseTranscriptionJSON, which the spec doesn't model:
// duration in seconds, segments with absolute offsets.
type verboseTranscription struct {
	Task     string           `json:"task"`
	Language string           `json:"language"`
	Duration wire.Float64     `json:"duration"`
	Text     string           `json:"text"`
	Segments []verboseSegment `json:"segments"`
}

type verboseSegment struct {
	ID    int          `json:"id"`
	Start wire.Float64 `json:"start"`
	End   wire.Float64 `json:"end"`
	Text  string       `json:"text"`
}

func (m *Module) audioTranscriptions(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	contentType, _ := wire.HeaderValue(c.Request(), "Content-Type")
	boundary, ok := multipartBoundary(contentType)
	if !ok {
		return errNoBoundary
	}
	body, err := m.readBody(c, maxAudioBytes, errAudioLarge)
	if err != nil {
		return err
	}
	parts, err := parseMultipart(body, boundary, defaultMaxParts)
	if err != nil {
		m.log.WarnContext(c.Request().Context(), "audio/transcriptions multipart parse failed", "error", err)
		return errMalformed
	}
	file := firstPart(parts, "file")
	if file == nil {
		return errMissingFile
	}
	if len(file.body) == 0 {
		return errEmptyFile
	}
	mime, err := resolveMIME(file.filename, file.contentType)
	if err != nil {
		return err
	}
	format := ""
	if part := firstPart(parts, "response_format"); part != nil {
		format = strings.ToLower(strings.ToValidUTF8(string(part.body), "�"))
	}

	// model, language, temperature and prompt are accepted and ignored: the provider
	// and model are an operator's decision, not a tenant container's. Rejecting them
	// would break clients that always send a model name.
	var channel *string
	if v, ok := wire.HeaderValue(c.Request(), channelHeader); ok {
		channel = &v
	}
	res, err := m.svc.transcribe(c.Request().Context(), file.body, mime, tenant, channel)
	if err != nil {
		return err
	}
	return encodeTranscription(c, res, format)
}

func firstPart(parts []formPart, name string) *formPart {
	for i := range parts {
		if parts[i].name == name {
			return &parts[i]
		}
	}
	return nil
}

// resolveMIME is OpenAIAudioController.resolveMIME: the filename's extension, then the
// part's Content-Type if it's an accepted one.
func resolveMIME(filename, contentType *string) (string, error) {
	if filename != nil {
		if dot := strings.LastIndexByte(*filename, '.'); dot >= 0 {
			if mime, ok := mimeForExtension[strings.ToLower((*filename)[dot+1:])]; ok {
				return mime, nil
			}
		}
	}
	if contentType != nil {
		if mime := bareMediaType(*contentType); acceptedMimes[mime] {
			return mime, nil
		}
	}
	return "", errBadFormat
}

// encodeTranscription is OpenAIAudioController.encode. Hermes asks for text on whisper-1
// and json otherwise. srt and vtt are served as plain text, with no real timing track.
// Any other value is json. This route writes JSONEncoder's bytes directly, so slashes
// are escaped, unlike /v1/transcribe's.
func encodeTranscription(c *echo.Context, res TranscribeResponse, format string) error {
	switch format {
	case "text", "srt", "vtt":
		return c.Blob(http.StatusOK, "text/plain; charset=utf-8", []byte(res.Text))
	}
	var payload any = OpenAITranscriptionJSON{Text: res.Text}
	if format == "verbose_json" {
		segments := []verboseSegment{}
		if res.Segments != nil {
			for i, s := range *res.Segments {
				segments = append(segments, verboseSegment{ID: i, Start: s.Start, End: s.End, Text: s.Text})
			}
		}
		payload = verboseTranscription{Task: "transcribe", Language: res.Language, Duration: res.DurationSeconds, Text: res.Text, Segments: segments}
	}
	b, err := wire.MarshalFoundation(payload)
	if err != nil {
		return err
	}
	return c.Blob(http.StatusOK, wire.ContentTypeJSON, b)
}
