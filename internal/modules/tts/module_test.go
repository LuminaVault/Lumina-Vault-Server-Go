package tts_test

import (
	"context"
	"encoding/json"
	"io"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/entitlement"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/tts"
	"lumina/internal/ratelimit"
	"lumina/internal/usagemeter"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var (
	user = uuid.MustParse("70CE0000-0000-4000-8000-0000000000B3")
	free = uuid.MustParse("70CE0000-0000-4000-8000-0000000000D0")
	keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

// speechStub is the self-hosted service: it answers status and keeps what it was sent.
type speechStub struct {
	*httptest.Server
	mu     sync.Mutex
	last   *http.Request
	body   map[string]string
	status int
}

func newSpeechStub(t *testing.T, status int) *speechStub {
	t.Helper()
	s := &speechStub{status: status}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.last = r
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &s.body)
		w.WriteHeader(s.status)
		if s.status == http.StatusOK {
			_, _ = w.Write([]byte{0xFF, 0xFB, 0x90, 0x44}) // MP3 sync word and header
			return
		}
		_, _ = w.Write([]byte("nope"))
	}))
	t.Cleanup(s.Close)
	return s
}

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
}

func setup(t *testing.T, baseURL string, enforcement bool) env {
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
	m := tts.New(usagemeter.New(pool), auth.Identify(keys, auth.PGUsers(pool)),
		entitlement.PlatformFundedMiddleware(pool, entitlement.Chat, enforcement), ratelimit.NewStore(),
		tts.Config{BaseURL: baseURL, DefaultModel: "tts-1"})
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: []httpserver.Module{m}})
	return env{t: t, e: e, pool: pool}
}

func (v env) speak(who uuid.UUID, body string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/tts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if who != uuid.Nil {
		tok, err := scenario.Mint(keys, who)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec
}

// MARK: - Ports TTS/OpenAITTSAdapterTests.swift, on the self-hosted service. The Swift
// test's bearer key has no counterpart: Go sends none, which is the point.

func Test200ResponseReturnsAudioDataWithCorrectContentType(t *testing.T) {
	stub := newSpeechStub(t, http.StatusOK)
	v := setup(t, stub.URL+"/v1", false)
	rec := v.speak(user, `{"text":"Hello"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "audio/mpeg", rec.Header().Get("Content-Type"))
	require.Equal(t, "5", rec.Header().Get("X-LuminaVault-Characters-Billed"))
	require.Equal(t, []byte{0xFF, 0xFB, 0x90, 0x44}, rec.Body.Bytes())

	require.Equal(t, "/v1/audio/speech", stub.last.URL.Path)
	require.Equal(t, http.MethodPost, stub.last.Method)
	require.Equal(t, "application/json", stub.last.Header.Get("Content-Type"))
	require.Empty(t, stub.last.Header.Get("Authorization"), "no key: a paid API can't be reached by misconfiguration")
}

func TestRequestBodyCarriesLuminaToAlloyMappingAndMp3ResponseFormat(t *testing.T) {
	stub := newSpeechStub(t, http.StatusOK)
	v := setup(t, stub.URL, false)
	require.Equal(t, http.StatusOK, v.speak(user, `{"text":"Speak this"}`).Code)
	require.Equal(t, map[string]string{"model": "tts-1", "input": "Speak this", "voice": "alloy", "response_format": "mp3"}, stub.body)
}

func Test429MapsToTransient(t *testing.T) {
	stub := newSpeechStub(t, http.StatusTooManyRequests)
	v := setup(t, stub.URL, false)
	rec := v.speak(user, `{"text":"Hi"}`)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.JSONEq(t, `{"error":{"message":"tts upstream unavailable"}}`, rec.Body.String())
}

func Test400MapsToPermanent(t *testing.T) {
	stub := newSpeechStub(t, http.StatusBadRequest)
	v := setup(t, stub.URL, false)
	rec := v.speak(user, `{"text":"Hi"}`)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.JSONEq(t, `{"error":{"message":"tts upstream rejected request (openai)"}}`, rec.Body.String())
}

func TestUnknownVoiceFallsThroughToAlloy(t *testing.T) {
	stub := newSpeechStub(t, http.StatusOK)
	v := setup(t, stub.URL, false)
	for body, want := range map[string]string{
		`{"text":"x","voice":"wholly-unknown-voice"}`: "alloy",
		`{"text":"x","voice":"  NOVA "}`:              "nova",
		`{"text":"x","voice":"   "}`:                  "alloy",
		`{"text":"x","voice":null}`:                   "alloy",
		`{"text":"x","voice":"shimmer"}`:              "shimmer",
	} {
		require.Equal(t, http.StatusOK, v.speak(user, body).Code)
		require.Equal(t, want, stub.body["voice"], body)
	}
}

// MARK: - The route

func TestInputRules(t *testing.T) {
	stub := newSpeechStub(t, http.StatusOK)
	v := setup(t, stub.URL, false)
	rec := v.speak(user, `{"text":""}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.JSONEq(t, `{"error":{"message":"text required"}}`, rec.Body.String())

	require.Equal(t, http.StatusOK, v.speak(user, `{"text":"`+strings.Repeat("x", 4096)+`"}`).Code)
	rec = v.speak(user, `{"text":"`+strings.Repeat("x", 4097)+`"}`)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.JSONEq(t, `{"error":{"message":"text exceeds 4096 bytes"}}`, rec.Body.String())

	require.Equal(t, http.StatusUnauthorized, v.speak(uuid.Nil, `{"text":"x"}`).Code)
}

func TestGraphemesAreBilledAndMetered(t *testing.T) {
	stub := newSpeechStub(t, http.StatusOK)
	v := setup(t, stub.URL, false)
	rec := v.speak(user, `{"text":"👩‍👩‍👧é"}`)
	require.Equal(t, "2", rec.Header().Get("X-LuminaVault-Characters-Billed"))
	require.Eventually(t, func() bool {
		var chars int64
		err := v.pool.QueryRow(context.Background(), `SELECT chars_out FROM usage_meter WHERE tenant_id = $1 AND model = 'tts-1'`, user).Scan(&chars)
		return err == nil && chars == 2
	}, 5*time.Second, 20*time.Millisecond)
}

func TestAnUnreachableServiceIsUnavailable(t *testing.T) {
	stub := newSpeechStub(t, http.StatusOK)
	stub.Close()
	v := setup(t, stub.URL, false)
	rec := v.speak(user, `{"text":"Hi"}`)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.JSONEq(t, `{"error":{"message":"tts upstream unavailable"}}`, rec.Body.String())
}

func TestPlatformFundedFloor(t *testing.T) {
	stub := newSpeechStub(t, http.StatusOK)
	v := setup(t, stub.URL, true)
	require.Equal(t, http.StatusPaymentRequired, v.speak(free, `{"text":"x"}`).Code)
	require.Equal(t, http.StatusOK, v.speak(user, `{"text":"x"}`).Code)
}
