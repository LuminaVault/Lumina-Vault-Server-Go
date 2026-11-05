package grok_test

import (
	"context"
	"encoding/json"
	"io"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/entitlement"
	"lumina/internal/hermescontainers"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/grok"
	"lumina/internal/secretbox"
	"lumina/parity/scenario"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// Fixture ids from parity/seed/grok.sql. Its keys are sealed with parityMasterKey (the
// golden-fixture key); none is real, and every upstream here is a stub.
var (
	ann      = uuid.MustParse("670C0000-0000-4000-8000-0000000000A1") // pro, linked
	ben      = uuid.MustParse("670C0000-0000-4000-8000-0000000000A2") // ultimate, linked
	nobox    = uuid.MustParse("670C0000-0000-4000-8000-0000000000A4") // pro, no container
	unlinked = uuid.MustParse("670C0000-0000-4000-8000-0000000000A5") // pro, Nous only
	badkey   = uuid.MustParse("670C0000-0000-4000-8000-0000000000A6") // pro, key sealed for ann
	trial    = uuid.MustParse("670C0000-0000-4000-8000-0000000000B1")
	keys     = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

const parityMasterKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

// call is one request the stub container received.
type call struct {
	host, path, bearer string
	body               map[string]any
}

type env struct {
	t  *testing.T
	e  *echo.Echo
	mu *sync.Mutex
	// reply answers the stub container's next call.
	reply func(w http.ResponseWriter, r *http.Request)
	calls *[]call
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "grok.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err)
	}
	box, err := secretbox.New(parityMasterKey)
	require.NoError(t, err)

	v := &env{t: t, mu: &sync.Mutex{}, calls: &[]call{}}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c := call{host: r.Host, path: r.URL.Path, bearer: r.Header.Get("Authorization")}
		_ = json.Unmarshal(raw, &c.body)
		v.mu.Lock()
		*v.calls = append(*v.calls, c)
		reply := v.reply
		v.mu.Unlock()
		reply(w, r)
	}))
	t.Cleanup(stub.Close)
	// handle.baseURL is http://<container_name>:8642: dial the stub whatever the host.
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, stub.Listener.Addr().String())
		},
	}}
	t.Cleanup(client.CloseIdleConnections)

	v.e = httpserver.New(config.Config{}, httpserver.Deps{
		Auth: auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{
			grok.New(hermescontainers.New(pool, box), entitlement.PremiumGuardMiddleware(pool), client),
		},
	})
	return v
}

func (v *env) answer(status int, body string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.reply = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func (v *env) lastCall() call {
	v.t.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	require.NotEmpty(v.t, *v.calls)
	return (*v.calls)[len(*v.calls)-1]
}

func (v *env) callCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(*v.calls)
}

type resp struct {
	code int
	ct   string
	body string
}

func (v *env) do(user uuid.UUID, path, body string) resp {
	v.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.String()}
}

const (
	chatBody   = `{"messages":[{"role":"user","content":"hi"}]}`
	searchBody = `{"query":"q"}`
	visionBody = `{"prompt":"describe","imageURLs":["https://x.test/a.png","https://x.test/b.png"]}`
	goodChat   = `{"model":"grok-up","choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
)

func message(t *testing.T, r resp) string {
	t.Helper()
	var b struct {
		Error struct{ Message string } `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.body), &b), r.body)
	return b.Error.Message
}

// Grok/GrokControllerTests.swift and PremiumGuardMiddlewareTests.swift, all six, at the
// route level. The guard's tier table is in internal/entitlement/premium_test.go.
func TestSwiftControllerAndGuard(t *testing.T) {
	v := setup(t)
	v.answer(http.StatusOK, goodChat)

	t.Run("premium user with no xai container hits 409 conflict", func(t *testing.T) {
		r := v.do(nobox, "/v1/grok/chat", chatBody)
		require.Equal(t, http.StatusConflict, r.code, r.body)
		require.Equal(t, "xai_not_connected", message(t, r))
	})
	t.Run("x-search rejects empty query with 400", func(t *testing.T) {
		r := v.do(nobox, "/v1/grok/x-search", `{"query":""}`)
		require.Equal(t, http.StatusBadRequest, r.code)
		require.Equal(t, "query required", message(t, r))
	})
	t.Run("vision rejects empty imageURLs with 400", func(t *testing.T) {
		r := v.do(nobox, "/v1/grok/vision", `{"prompt":"describe","imageURLs":[]}`)
		require.Equal(t, http.StatusBadRequest, r.code)
		require.Equal(t, "imageURLs required", message(t, r))
	})
	t.Run("tts returns 501 while upstream provider is disabled", func(t *testing.T) {
		r := v.do(nobox, "/v1/grok/tts", `{"text":"hello"}`)
		require.Equal(t, http.StatusNotImplemented, r.code)
		require.Equal(t, "tts_coming_soon", message(t, r))
	})
	t.Run("trial user hits 402 on grok routes", func(t *testing.T) {
		for _, path := range []string{"/v1/grok/chat", "/v1/grok/x-search", "/v1/grok/vision", "/v1/grok/tts"} {
			r := v.do(trial, path, "not json")
			require.Equal(t, http.StatusPaymentRequired, r.code, path)
			require.JSONEq(t, `{"error":"premium_required","tier":"trial","requiresXaiConnect":true}`, r.body)
		}
	})
	t.Run("unauthenticated request returns 401 before reaching premium guard", func(t *testing.T) {
		r := v.do(uuid.Nil, "/v1/grok/chat", `{"messages":[]}`)
		require.Equal(t, http.StatusUnauthorized, r.code)
	})
	require.Zero(t, v.callCount(), "none of these may reach a container")
}

func TestErrorOrder(t *testing.T) {
	v := setup(t)
	v.answer(http.StatusOK, goodChat)

	r := v.do(nobox, "/v1/grok/chat", `{"messages":`)
	require.Equal(t, http.StatusBadRequest, r.code, "the body is decoded before the container is looked up")
	r = v.do(unlinked, "/v1/grok/vision", `{"prompt":"p","imageURLs":[]}`)
	require.Equal(t, http.StatusBadRequest, r.code, "validation beats 409")
	for _, path := range []string{"/v1/grok/chat", "/v1/grok/x-search", "/v1/grok/vision"} {
		body := map[string]string{"/v1/grok/chat": chatBody, "/v1/grok/x-search": searchBody, "/v1/grok/vision": visionBody}[path]
		r = v.do(unlinked, path, body)
		require.Equal(t, http.StatusConflict, r.code, "no xAI marker, %s", path)
		require.Equal(t, "xai_not_connected", message(t, r))
		r = v.do(badkey, path, body)
		require.Equal(t, http.StatusInternalServerError, r.code, "a key that won't open isn't a 409, %s", path)
		require.Empty(t, r.body)
	}
	require.Equal(t, http.StatusNotImplemented, v.do(badkey, "/v1/grok/tts", "").code, "tts never looks the container up")
	require.Zero(t, v.callCount())
}

func TestChatCallsTheTenantsContainer(t *testing.T) {
	v := setup(t)
	v.answer(http.StatusOK, goodChat)

	r := v.do(ann, "/v1/grok/chat", chatBody)
	require.Equal(t, http.StatusOK, r.code, r.body)
	require.Equal(t, "application/json; charset=utf-8", r.ct)
	require.JSONEq(t, `{"answer":"hello","model":"grok-up","usage":{"promptTokens":10,"completionTokens":5}}`, r.body)
	c := v.lastCall()
	require.Equal(t, "grok-hermes-ann:8642", c.host)
	require.Equal(t, "/v1/chat/completions", c.path)
	require.Equal(t, "Bearer parity-grok-key-ann", c.bearer, "the container's key, not a platform xAI key")
	require.Equal(t, map[string]any{
		"model":    "grok-4.3",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, c.body)

	r = v.do(ben, "/v1/grok/chat", `{"messages":[],"model":"custom","stream":true,"maxTokens":0}`)
	require.Equal(t, http.StatusOK, r.code, r.body)
	c = v.lastCall()
	require.Equal(t, "grok-hermes-ben:8642", c.host)
	require.Equal(t, "Bearer parity-grok-key-ben", c.bearer)
	require.Equal(t, map[string]any{"model": "custom", "messages": []any{}, "max_tokens": float64(0)}, c.body, "stream isn't sent on")

	v.answer(http.StatusOK, `{"choices":[{"message":{"content":"x"}}]}`)
	r = v.do(ann, "/v1/grok/chat", `{"messages":[],"model":"asked"}`)
	require.JSONEq(t, `{"answer":"x","model":"asked"}`, r.body, "no model and no usage in the reply")
}

func TestXSearch(t *testing.T) {
	v := setup(t)
	v.answer(http.StatusOK, `{"model":"up","output":"text","citations":[
		{"url":"u1","title":"T","published_at":"2026-01-02T03:04:05+02:00"},{"url":"u2","title":null,"published_at":null}]}`)

	r := v.do(ann, "/v1/grok/x-search", `{"query":"q","allowedXHandles":["a"],"excludedXHandles":[],"fromDate":"2026-01-01","toDate":"later","enableImageUnderstanding":true,"enableVideoUnderstanding":false}`)
	require.Equal(t, http.StatusOK, r.code, r.body)
	require.JSONEq(t, `{"answer":"text","model":"up","citations":[{"url":"u1","title":"T","publishedAt":"2026-01-02T01:04:05Z"},{"url":"u2"}]}`, r.body)
	c := v.lastCall()
	require.Equal(t, "/v1/responses", c.path)
	require.Equal(t, map[string]any{
		"model": "grok-4.20-reasoning", "input": "q",
		"tools": []any{map[string]any{"type": "x_search", "x_search": map[string]any{
			"allowed_x_handles": []any{"a"}, "excluded_x_handles": []any{}, "from_date": "2026-01-01", "to_date": "later",
			"enable_image_understanding": true, "enable_video_understanding": false,
		}}},
	}, c.body)

	v.answer(http.StatusOK, `{}`)
	r = v.do(ann, "/v1/grok/x-search", searchBody)
	require.JSONEq(t, `{"answer":"","model":"grok-4.20-reasoning","citations":[]}`, r.body, "x-search has no missing-answer failure")
	require.Equal(t, map[string]any{
		"model": "grok-4.20-reasoning", "input": "q",
		"tools": []any{map[string]any{"type": "x_search", "x_search": map[string]any{}}},
	}, v.lastCall().body)
}

func TestVision(t *testing.T) {
	v := setup(t)
	v.answer(http.StatusOK, goodChat)

	r := v.do(ann, "/v1/grok/vision", visionBody)
	require.Equal(t, http.StatusOK, r.code, r.body)
	require.JSONEq(t, `{"answer":"hello","model":"grok-up"}`, r.body, "vision drops usage")
	c := v.lastCall()
	require.Equal(t, "/v1/chat/completions", c.path)
	require.Equal(t, map[string]any{
		"model": "grok-4.3",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "describe"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x.test/a.png"}},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x.test/b.png"}},
		}}},
	}, c.body)
}

func TestUpstreamFailures(t *testing.T) {
	v := setup(t)
	for _, tc := range []struct {
		name   string
		status int
		reply  string
		want   string
	}{
		{"429", http.StatusTooManyRequests, `{"error":"x"}`, "grok_upstream_429"},
		{"500", http.StatusInternalServerError, ``, "grok_upstream_500"},
		{"a good body on a bad status", http.StatusBadGateway, goodChat, "grok_upstream_502"},
		{"no choices key", http.StatusOK, `{"model":"m"}`, "grok_upstream_decode_failed"},
		{"null choices", http.StatusOK, `{"choices":null}`, "grok_upstream_decode_failed"},
		{"empty choices", http.StatusOK, `{"choices":[]}`, "grok_upstream_missing_answer"},
		{"null content is a decode failure, not a missing answer", http.StatusOK, `{"choices":[{"message":{"content":null}}]}`, "grok_upstream_decode_failed"},
		{"a bad later choice fails the lot", http.StatusOK, `{"choices":[{"message":{"content":"a"}},{"message":{}}]}`, "grok_upstream_decode_failed"},
		{"partial usage", http.StatusOK, `{"choices":[{"message":{"content":"a"}}],"usage":{"prompt_tokens":1}}`, "grok_upstream_decode_failed"},
		{"fractional usage", http.StatusOK, `{"choices":[{"message":{"content":"a"}}],"usage":{"prompt_tokens":1.5,"completion_tokens":1}}`, "grok_upstream_decode_failed"},
		{"keys are case sensitive", http.StatusOK, `{"CHOICES":[{"message":{"content":"a"}}]}`, "grok_upstream_decode_failed"},
		{"not json", http.StatusOK, `nope`, "grok_upstream_decode_failed"},
		{"empty", http.StatusOK, ``, "grok_upstream_decode_failed"},
		{"array", http.StatusOK, `[]`, "grok_upstream_decode_failed"},
		{"null", http.StatusOK, `null`, "grok_upstream_decode_failed"},
		{"trailing data", http.StatusOK, goodChat + `{}`, "grok_upstream_decode_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v.answer(tc.status, tc.reply)
			r := v.do(ann, "/v1/grok/chat", chatBody)
			require.Equal(t, http.StatusBadGateway, r.code, r.body)
			require.Equal(t, tc.want, message(t, r))
		})
	}
	t.Run("the container's error body stops at the proxy", func(t *testing.T) {
		v.answer(http.StatusUnauthorized, `{"error":"bad key parity-grok-key-ann"}`)
		r := v.do(ann, "/v1/grok/chat", chatBody)
		require.NotContains(t, r.body, "parity-grok-key-ann")
	})
	t.Run("x-search output of the wrong type", func(t *testing.T) {
		v.answer(http.StatusOK, `{"output":[{"type":"message"}]}`)
		r := v.do(ann, "/v1/grok/x-search", searchBody)
		require.Equal(t, "grok_upstream_decode_failed", message(t, r))
	})
	t.Run("citation with no url", func(t *testing.T) {
		v.answer(http.StatusOK, `{"citations":[{"title":"T"}]}`)
		r := v.do(ann, "/v1/grok/x-search", searchBody)
		require.Equal(t, "grok_upstream_decode_failed", message(t, r))
	})
	t.Run("bad citation date", func(t *testing.T) {
		v.answer(http.StatusOK, `{"citations":[{"url":"u","published_at":"yesterday"}]}`)
		r := v.do(ann, "/v1/grok/x-search", searchBody)
		require.Equal(t, "grok_upstream_decode_failed", message(t, r))
	})
	t.Run("vision with no choices", func(t *testing.T) {
		v.answer(http.StatusOK, `{"choices":[]}`)
		r := v.do(ann, "/v1/grok/vision", visionBody)
		require.Equal(t, "grok_upstream_missing_answer", message(t, r))
	})
}

func TestUpstreamBodyLimit(t *testing.T) {
	v := setup(t)
	const limit = 8 << 20
	exactly := `{"choices":[{"message":{"content":"a"}}]}`
	exactly += strings.Repeat(" ", limit-len(exactly))

	v.answer(http.StatusOK, exactly)
	r := v.do(ann, "/v1/grok/chat", chatBody)
	require.Equal(t, http.StatusOK, r.code, "exactly 8 MiB is allowed")

	v.answer(http.StatusOK, exactly+" ")
	r = v.do(ann, "/v1/grok/chat", chatBody)
	require.Equal(t, http.StatusRequestEntityTooLarge, r.code, "NIOTooManyBytesError passes through mapProxyError")
	require.Empty(t, r.body)

	v.answer(http.StatusInternalServerError, exactly+" ")
	r = v.do(ann, "/v1/grok/chat", chatBody)
	require.Equal(t, http.StatusRequestEntityTooLarge, r.code, "the body is collected before the status is checked")
}

func TestUpstreamTransportFailureIsPlain500(t *testing.T) {
	v := setup(t)
	v.mu.Lock()
	v.reply = func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
	v.mu.Unlock()
	r := v.do(ann, "/v1/grok/chat", chatBody)
	require.Equal(t, http.StatusInternalServerError, r.code)
	require.Empty(t, r.body, "not grok_upstream_*: mapProxyError only knows the proxy's own errors")
}

// 200s and the guard's 402. The spec's 409, 501 and 502 point at ErrorEnvelope, whose
// error is a string, like its 107 other uses; Hummingbird and wire.Error send
// {"error":{"message":...}}. Parity pins those bodies.
func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	full := `{"model":"up","output":"text","citations":[{"url":"u","title":"T","published_at":"2026-01-02T03:04:05Z"},{"url":"u2"}]}`
	for _, k := range []struct {
		user        uuid.UUID
		path, body  string
		reply       string
		replyStatus int
		want        int
	}{
		{ann, "/v1/grok/chat", chatBody, goodChat, http.StatusOK, http.StatusOK},
		{ann, "/v1/grok/x-search", searchBody, full, http.StatusOK, http.StatusOK},
		{ann, "/v1/grok/vision", visionBody, goodChat, http.StatusOK, http.StatusOK},
		{trial, "/v1/grok/chat", chatBody, "", 0, http.StatusPaymentRequired},
	} {
		if k.replyStatus != 0 {
			v.answer(k.replyStatus, k.reply)
		}
		r := v.do(k.user, k.path, k.body)
		require.Equal(t, k.want, r.code, "%s: %s", k.path, r.body)
		req := httptest.NewRequest(http.MethodPost, k.path, strings.NewReader(k.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, k.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: r.code,
			Header: http.Header{"Content-Type": {r.ct}},
		}
		out.SetBodyBytes([]byte(r.body))
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %d: %s", k.path, r.code, r.body)
	}
}
