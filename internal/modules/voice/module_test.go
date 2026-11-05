package voice_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/entitlement"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/voice"
	"lumina/internal/ratelimit"
	"lumina/internal/usagemeter"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func fixture(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("70CE0000-0000-4000-8000-%012X", n))
}

var (
	userA   = fixture(0xA0)
	userB   = fixture(0xA1)
	userC   = fixture(0xA2)
	free    = fixture(0xC2)
	trial   = fixture(0xD3)
	archive = fixture(0xD2)
	vbFree  = fixture(0xD0)
	revoked = fixture(0xC0)
	keys    = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

const multipartType = "multipart/form-data; boundary=zz"

func form(fields map[string]string) string {
	var b strings.Builder
	b.WriteString("--zz\r\nContent-Disposition: form-data; name=\"file\"; filename=\"audio.ogg\"\r\nContent-Type: audio/ogg\r\n\r\nOggS\r\n")
	for k, v := range fields {
		b.WriteString("--zz\r\nContent-Disposition: form-data; name=\"" + k + "\"\r\n\r\n" + v + "\r\n")
	}
	b.WriteString("--zz--\r\n")
	return b.String()
}

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
}

func setup(t *testing.T, cfg voice.Config, enforcement bool) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "voice.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	m := voice.New(pool, usagemeter.New(pool), auth.Identify(keys, auth.PGUsers(pool)), auth.AudioIdentify(keys, auth.PGAudioUsers(pool)),
		entitlement.PlatformFundedMiddleware(pool, entitlement.Chat, enforcement),
		entitlement.Middleware(pool, entitlement.Chat, enforcement, nil), ratelimit.NewStore(), cfg)
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: []httpserver.Module{m}})
	return env{t: t, e: e, pool: pool}
}

func stubCfg() voice.Config {
	return voice.Config{Provider: "stub", StubText: "stub transcript", StubLanguage: "en", StubConfidence: 0.95, StubDurationSeconds: 30}
}

func (v env) post(path, contentType, body, authorization string, headers ...string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec
}

func session(t *testing.T, user uuid.UUID) string {
	t.Helper()
	tok, err := scenario.Mint(keys, user)
	require.NoError(t, err)
	return "Bearer " + tok
}

func scoped(t *testing.T, user uuid.UUID, scope string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub": strings.ToUpper(user.String()), "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"jti": strings.ToUpper(uuid.NewString()), "scp": scope,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = "t"
	signed, err := tok.SignedString([]byte("test-secret-test-secret-test-0000"))
	require.NoError(t, err)
	return "Bearer " + signed
}

type envelope struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Code    *string `json:"code"`
	} `json:"error"`
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) envelope {
	t.Helper()
	var out envelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	return out
}

// row is one usage_events row, as dashboards read it.
type row struct {
	amount  int64
	source  string
	key     string
	outcome string
	model   string
	channel string
}

func rows(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID) []row {
	t.Helper()
	r, err := pool.Query(context.Background(), `SELECT amount, source, idempotency_key, metadata->>'outcome', metadata->>'model', metadata->>'channel'
FROM usage_events WHERE tenant_id = $1 AND metric = 'voice_transcribe' ORDER BY occurred_at`, tenant)
	require.NoError(t, err)
	defer r.Close()
	var out []row
	for r.Next() {
		var x row
		require.NoError(t, r.Scan(&x.amount, &x.source, &x.key, &x.outcome, &x.model, &x.channel))
		out = append(out, x)
	}
	require.NoError(t, r.Err())
	return out
}

func TestTranscribeWritesARow(t *testing.T) {
	v := setup(t, stubCfg(), false)
	rec := v.post("/v1/transcribe", "audio/ogg", "OggS", session(t, userA))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		ID              string  `json:"id"`
		Text            string  `json:"text"`
		Confidence      float64 `json:"confidence"`
		DurationSeconds float64 `json:"duration_seconds"`
		Segments        []struct {
			End float64 `json:"end"`
		} `json:"segments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "stub transcript", out.Text)
	require.Equal(t, strings.ToUpper(out.ID), out.ID)
	require.InDelta(t, 30.0, out.DurationSeconds, 0)
	require.Len(t, out.Segments, 1)

	got := rows(t, v.pool, userA)
	require.Len(t, got, 1)
	require.Equal(t, row{amount: 30000, source: "app", key: out.ID, outcome: "ok", model: "", channel: "app"}, got[0])
}

func TestChannelDefaultsDifferBySurface(t *testing.T) {
	v := setup(t, stubCfg(), false)
	require.Equal(t, http.StatusOK, v.post("/v1/transcribe", "audio/ogg", "x", session(t, userA), "X-Lumina-Channel", "Telegram").Code)
	require.Equal(t, http.StatusOK, v.post("/v1/audio/transcriptions", multipartType, form(nil), session(t, userB)).Code)
	require.Equal(t, "telegram", rows(t, v.pool, userA)[0].channel)
	require.Equal(t, "unknown", rows(t, v.pool, userB)[0].channel)
}

func TestAudioResponseFormats(t *testing.T) {
	v := setup(t, stubCfg(), false)
	tok := session(t, userA)
	for format, want := range map[string]string{
		"":             `{"text":"stub transcript"}`,
		"json":         `{"text":"stub transcript"}`,
		"yaml":         `{"text":"stub transcript"}`,
		"verbose_json": `{"task":"transcribe","language":"en","duration":30,"text":"stub transcript","segments":[{"id":0,"start":0,"end":30,"text":"stub transcript"}]}`,
	} {
		fields := map[string]string{}
		if format != "" {
			fields["response_format"] = format
		}
		rec := v.post("/v1/audio/transcriptions", multipartType, form(fields), tok)
		require.Equal(t, http.StatusOK, rec.Code, format)
		require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
		require.JSONEq(t, want, rec.Body.String(), format)
	}
	for _, format := range []string{"text", "srt", "vtt", "TEXT"} {
		rec := v.post("/v1/audio/transcriptions", multipartType, form(map[string]string{"response_format": format}), tok)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
		require.Equal(t, "stub transcript", rec.Body.String())
	}
}

// The OpenAI route writes JSONEncoder's bytes, slashes escaped.
func TestAudioJSONEscapesSlashes(t *testing.T) {
	cfg := stubCfg()
	cfg.StubText = "a/b"
	v := setup(t, cfg, false)
	rec := v.post("/v1/audio/transcriptions", multipartType, form(nil), session(t, userA))
	require.Equal(t, `{"text":"a\/b"}`, rec.Body.String())
}

func TestSpeechIsTheExactRefusal(t *testing.T) {
	v := setup(t, stubCfg(), false)
	rec := v.post("/v1/audio/speech", "application/json", `{"input":"hi"}`, session(t, userA))
	require.Equal(t, http.StatusNotImplemented, rec.Code)
	got := decodeEnvelope(t, rec)
	require.Equal(t, "Spoken replies are not available yet. Voice messages you send are still transcribed.", got.Error.Message)
	require.Equal(t, "server_error", got.Error.Type)
	require.Equal(t, "not_implemented", *got.Error.Code)
}

// The security model is an asymmetry, so it's tested from both sides: a compromised
// Hermes container holds a credential for /v1/audio and for nothing else.
func TestAudioTokenIsTakenOnlyOnAudioRoutes(t *testing.T) {
	v := setup(t, stubCfg(), false)
	audio := scoped(t, userC, auth.ScopeAudio)
	require.Equal(t, http.StatusNotImplemented, v.post("/v1/audio/speech", "", "", audio).Code)
	require.Equal(t, http.StatusOK, v.post("/v1/audio/transcriptions", multipartType, form(nil), audio).Code)
	require.Equal(t, http.StatusUnauthorized, v.post("/v1/transcribe", "audio/ogg", "x", audio).Code)

	other := scoped(t, userC, "mcp")
	rec := v.post("/v1/audio/speech", "", "", other)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, "authentication_error", decodeEnvelope(t, rec).Error.Type)
	require.Equal(t, http.StatusUnauthorized, v.post("/v1/transcribe", "audio/ogg", "x", other).Code)
}

func TestScopedTokenIsCheckedAgainstTheEpoch(t *testing.T) {
	v := setup(t, stubCfg(), false)
	// vo_rev's epoch is 2026-06-01: a token issued now is after it, and this one before.
	old := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": strings.ToUpper(revoked.String()), "exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), "jti": "A", "scp": auth.ScopeAudio,
	})
	old.Header["kid"] = "t"
	signed, err := old.SignedString([]byte("test-secret-test-secret-test-0000"))
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, v.post("/v1/audio/speech", "", "", "Bearer "+signed).Code)
	require.Equal(t, http.StatusNotImplemented, v.post("/v1/audio/speech", "", "", session(t, revoked)).Code)
}

func TestTranscribeIsPlatformFundedAndAudioIsNot(t *testing.T) {
	v := setup(t, stubCfg(), true)
	rec := v.post("/v1/transcribe", "audio/ogg", "x", session(t, vbFree))
	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	require.JSONEq(t, `{"paywall":true,"paywallId":"default","requiredTier":"pro"}`, rec.Body.String())
	require.Equal(t, http.StatusOK, v.post("/v1/transcribe", "audio/ogg", "x", session(t, trial)).Code)
	require.Equal(t, http.StatusOK, v.post("/v1/audio/transcriptions", multipartType, form(nil), session(t, vbFree)).Code)

	rec = v.post("/v1/audio/transcriptions", multipartType, form(nil), session(t, archive))
	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	got := decodeEnvelope(t, rec)
	require.Equal(t, "Voice is not available on this plan.", got.Error.Message)
	require.Equal(t, "insufficient_quota", got.Error.Type)
}

func TestAudioRateLimitIsAnEnvelopeWithRetryAfter(t *testing.T) {
	v := setup(t, stubCfg(), false)
	tok := session(t, userA)
	for range ratelimit.AudioSpeechByUserPerMinute.Max {
		require.Equal(t, http.StatusNotImplemented, v.post("/v1/audio/speech", "", "", tok).Code)
	}
	rec := v.post("/v1/audio/speech", "", "", tok)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Retry-After"))
	got := decodeEnvelope(t, rec)
	require.Equal(t, "rate_limit_error", got.Error.Type)
	require.Equal(t, "rate_limit_exceeded", *got.Error.Code)
}

func TestFreeTierDailyAudioCapScales(t *testing.T) {
	v := setup(t, stubCfg(), false)
	tok := session(t, free)
	for range 10 {
		require.Equal(t, http.StatusBadRequest, v.post("/v1/audio/transcriptions", "application/json", "{}", tok).Code)
	}
	require.Equal(t, http.StatusTooManyRequests, v.post("/v1/audio/transcriptions", "application/json", "{}", tok).Code)
}

// blockingWhisper holds every request until released.
func blockingWhisper(t *testing.T) (url string, entered chan struct{}, release func()) {
	t.Helper()
	entered = make(chan struct{}, 8)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-done
		_, _ = w.Write([]byte(`{"text":"late"}`))
	}))
	var released bool
	release = func() {
		if !released {
			released = true
			close(done)
		}
	}
	t.Cleanup(func() {
		release()
		srv.Close()
	})
	return srv.URL + "/v1", entered, release
}

// Harness steps run in sequence, so the in-flight limit is only provable here.
func TestAudioInFlightLimitIsTwoAndReleases(t *testing.T) {
	url, entered, release := blockingWhisper(t)
	v := setup(t, voice.Config{BaseURL: url}, false)
	srv := httptest.NewServer(v.e)
	defer srv.Close()
	tok := session(t, userA)

	send := func() (int, envelope) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/audio/transcriptions", strings.NewReader(form(nil)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", multipartType)
		req.Header.Set("Authorization", tok)
		res, err := srv.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = res.Body.Close() }()
		var out envelope
		if res.StatusCode != http.StatusOK {
			require.NoError(t, json.NewDecoder(res.Body).Decode(&out))
			require.Equal(t, "1", res.Header.Get("Retry-After"))
		}
		return res.StatusCode, out
	}
	results := make(chan int, 2)
	for range 2 {
		go func() {
			code, _ := send()
			results <- code
		}()
		<-entered
	}
	code, out := send()
	require.Equal(t, http.StatusTooManyRequests, code)
	require.Equal(t, "too many concurrent streams", out.Error.Message)
	require.Equal(t, "rate_limit_error", out.Error.Type)

	release()
	require.Equal(t, http.StatusOK, <-results)
	require.Equal(t, http.StatusOK, <-results)
	code, _ = send()
	require.Equal(t, http.StatusOK, code, "slots come back")
}

// A real caller, a real upstream that takes its time: what the routes do past the
// server's read timeout is in internal/httpserver's tests.
func TestUpstreamFailureKeepsItsMessageInBothShapes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	v := setup(t, voice.Config{BaseURL: srv.URL + "/v1"}, false)

	rec := v.post("/v1/transcribe", "audio/ogg", "x", session(t, userA))
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.JSONEq(t, `{"error":{"message":"transcribe upstream unavailable"}}`, rec.Body.String())

	rec = v.post("/v1/audio/transcriptions", multipartType, form(nil), session(t, userB))
	require.Equal(t, http.StatusBadGateway, rec.Code)
	got := decodeEnvelope(t, rec)
	require.Equal(t, "transcribe upstream unavailable", got.Error.Message)
	require.Equal(t, "upstream_error", *got.Error.Code)

	r := rows(t, v.pool, userA)
	require.Len(t, r, 1)
	require.Equal(t, "upstream_transient", r[0].outcome)
	require.Equal(t, voice.DefaultModel, r[0].model)
	require.Equal(t, int64(0), r[0].amount)
}

func TestMtokRateWritesTheMeter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"text":"x","duration":10}`))
	}))
	defer srv.Close()
	v := setup(t, voice.Config{BaseURL: srv.URL, MtokPerSecond: 0.001}, false)
	require.Equal(t, http.StatusOK, v.post("/v1/transcribe", "audio/ogg", "x", session(t, userA)).Code)
	require.Eventually(t, func() bool {
		var in int64
		err := v.pool.QueryRow(context.Background(), `SELECT mtok_in FROM usage_meter WHERE tenant_id = $1 AND model = 'transcribe:openai_compatible'`, userA).Scan(&in)
		return err == nil && in == 10000
	}, 5*time.Second, 20*time.Millisecond)
}

func TestNoMeterRowAtTheDefaultRate(t *testing.T) {
	v := setup(t, stubCfg(), false)
	require.Equal(t, http.StatusOK, v.post("/v1/transcribe", "audio/ogg", "x", session(t, userA)).Code)
	var n int
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM usage_meter WHERE tenant_id = $1`, userA).Scan(&n))
	require.Zero(t, n)
}

func TestBodyCaps(t *testing.T) {
	v := setup(t, stubCfg(), false)
	tok := session(t, userA)
	rec := v.post("/v1/transcribe", "audio/ogg", strings.Repeat("x", 10*1024*1024+1), tok)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.JSONEq(t, `{"error":{"message":"audio body exceeds `+strconv.Itoa(10*1024*1024)+` byte cap"}}`, rec.Body.String())

	rec = v.post("/v1/audio/transcriptions", multipartType, strings.Repeat("x", 25*1024*1024+1), tok)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	got := decodeEnvelope(t, rec)
	require.Equal(t, "Audio file is too large. The maximum size is 25 MB.", got.Error.Message)
	require.Equal(t, "file_too_large", *got.Error.Code)
}

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)
	v := setup(t, stubCfg(), false)
	for _, tc := range []struct {
		path, contentType, body string
		status                  int
	}{
		{"/v1/transcribe", "audio/ogg", "x", http.StatusOK},
		{"/v1/audio/transcriptions", multipartType, form(nil), http.StatusOK},
		{"/v1/audio/transcriptions", "application/json", "{}", http.StatusBadRequest},
		{"/v1/audio/speech", "application/json", `{"input":"hi"}`, http.StatusNotImplemented},
	} {
		r := v.post(tc.path, tc.contentType, tc.body, session(t, userC))
		require.Equal(t, tc.status, r.Code, r.Body.String())
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", tc.contentType)
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, tc.path)
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{
				Request: req, PathParams: params, Route: route,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
			},
			Status: r.Code,
			Header: http.Header{"Content-Type": {r.Header().Get("Content-Type")}},
		}
		out.SetBodyBytes(r.Body.Bytes())
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s: %s", tc.path, r.Body.String())
	}
}
