// Package tts serves POST /v1/tts, the Go port of LuminaVaultServer Sources/App/TTS,
// with one difference on purpose: Swift's only real adapter is the paid OpenAI tts-1,
// and Go has no such adapter. It speaks the same wire shape to a self-hosted service
// at TTS_BASE_URL, with no key, so it can't reach a paid API by misconfiguration.
//
// Whether the route exists at all is decided in one place, cmd/App/main.go. See there.
package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ratelimit"
	"lumina/internal/swiftstr"
	"lumina/internal/usagemeter"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

const (
	// maxInputBytes is TTSController.maxInputBytes, in UTF-8.
	maxInputBytes = 4096
	// defaultVoice is LuminaVault's voice when a request names none.
	defaultVoice = "lumina"
	// upstreamTimeout is URLSession.shared's.
	upstreamTimeout = 60 * time.Second
	meterTimeout    = 10 * time.Second
	// maxAudioBytes bounds a synthesized clip read into memory.
	maxAudioBytes = 64 << 20

	billedHeader = "X-LuminaVault-Characters-Billed"
)

var (
	errTextRequired = wire.NewError(http.StatusBadRequest, "text required")
	errTooLong      = wire.NewError(http.StatusRequestEntityTooLarge, "text exceeds "+strconv.Itoa(maxInputBytes)+" bytes")
	errUnavailable  = wire.NewError(http.StatusBadGateway, "tts upstream unavailable")
	// errRejected keeps Swift's words, provider name included: they're the contract.
	errRejected = wire.NewError(http.StatusBadGateway, "tts upstream rejected request (openai)")
)

// Config is the switch and the model.
type Config struct {
	// BaseURL is the service, with its version prefix.
	BaseURL      string
	DefaultModel string
}

// Module is the route group: jwtAuthenticator, the platform-funded chat entitlement,
// then the per-minute and daily limits (App+build.swift "ttsGroup").
type Module struct {
	usage    *usagemeter.Meter
	identify echo.MiddlewareFunc
	gate     echo.MiddlewareFunc
	limits   *ratelimit.Store
	upstream speechClient
	model    string
	log      *slog.Logger
}

// New builds the module. gate is entitlement.PlatformFundedMiddleware for chat.
func New(usage *usagemeter.Meter, identify, gate echo.MiddlewareFunc, limits *ratelimit.Store, cfg Config) *Module {
	return &Module{
		usage: usage, identify: identify, gate: gate, limits: limits,
		upstream: speechClient{
			endpoint: strings.TrimRight(swiftstr.TrimWhitespacesAndNewlines(cfg.BaseURL), "/") + "/audio/speech",
			client:   &http.Client{Timeout: upstreamTimeout},
		},
		model: cfg.DefaultModel,
		log:   slog.Default().With("module", "tts"),
	}
}

func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	r.POST("/v1/tts", m.synthesize,
		m.identify,
		m.gate,
		ratelimit.Middleware(m.limits, ratelimit.TTSByUserPerMinute, ratelimit.UserOrIPKey),
		ratelimit.Middleware(m.limits, ratelimit.TTSByUserDaily, ratelimit.UserOrIPKey))
}

// ttsRequest is what Swift's TTSRequest decodes. The spec also lists model and format,
// which it ignores.
type ttsRequest struct {
	Text  string  `json:"text"`
	Voice *string `json:"voice,omitempty"`
}

func (m *Module) synthesize(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	body, err := wire.Bind[ttsRequest](c)
	if err != nil {
		return err
	}
	if body.Text == "" {
		return errTextRequired
	}
	if len(body.Text) > maxInputBytes {
		return errTooLong
	}
	voice := defaultVoice
	if body.Voice != nil {
		if v := swiftstr.TrimWhitespacesAndNewlines(*body.Voice); v != "" {
			voice = v
		}
	}
	ctx := context.WithoutCancel(c.Request().Context())
	audio, err := m.upstream.synthesize(ctx, body.Text, mapVoice(voice), m.model)
	if err != nil {
		m.log.ErrorContext(ctx, "tts upstream failed", "error", err)
		var rejected *rejectedError
		if errors.As(err, &rejected) {
			return errRejected
		}
		return errUnavailable
	}
	billed := swiftstr.CharacterCount(body.Text)
	m.recordCharacters(tenant, int64(billed))
	c.Response().Header().Set(billedHeader, strconv.Itoa(billed))
	return c.Blob(http.StatusOK, "audio/mpeg", audio)
}

// recordCharacters runs on its own goroutine, as Swift's Task does: billing never
// blocks audio delivery, and losing a row beats a stuck response.
func (m *Module) recordCharacters(tenant uuid.UUID, chars int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), meterTimeout)
		defer cancel()
		m.usage.RecordCharacters(ctx, tenant, m.model, chars)
	}()
}

// mapVoice is OpenAITTSAdapter.mapVoice, the voice names of this wire shape. An unknown
// name falls through to alloy, so the app can ship a voice before the server knows it.
func mapVoice(voice string) string {
	switch v := strings.ToLower(voice); v {
	case "echo", "fable", "onyx", "nova", "shimmer":
		return v
	}
	return "alloy"
}

// rejectedError is ProviderError.permanent: anything but a 429 or a 5xx.
type rejectedError struct {
	status int
	detail string
}

func (e *rejectedError) Error() string {
	return fmt.Sprintf("tts upstream rejected: %d: %s", e.status, e.detail)
}

// speechClient posts to an OpenAI-shaped /audio/speech. It sends no Authorization.
type speechClient struct {
	endpoint string
	client   *http.Client
}

func (s speechClient) synthesize(ctx context.Context, text, voice, model string) ([]byte, error) {
	payload, err := json.Marshal(map[string]string{"model": model, "input": text, "voice": voice, "response_format": "mp3"})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxAudioBytes))
	if err != nil {
		return nil, err
	}
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return data, nil
	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
		return nil, fmt.Errorf("tts upstream transient: %d", res.StatusCode)
	}
	return nil, &rejectedError{status: res.StatusCode, detail: string(data[:min(len(data), 512)])}
}
