package llmchat_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"lumina/internal/auth"
	"lumina/internal/authservice"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/hermesprofile"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/migrations"
	"lumina/internal/modules/authroutes"
	"lumina/internal/modules/llmchat"
	"lumina/internal/modules/meproviders"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultpaths"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// LLMPreferencesChatE2ETests: sign up, sign in, set an LLM preference, then chat,
// all over HTTP through the auth, preferences and /v1/llm/chat modules on one
// router. The Swift suite backs the chat hop with StubChatAdapter; here every
// upstream (Hermes gateway and the OpenRouter-compatible managed route) is one
// httptest server answering a fixed reply. No key is real and nothing leaves
// the process. The free lane is off, as in Swift's dbTestReaderWithStubChat(freeLaneEnabled: false).

var e2eKeys = auth.NewKeyset(auth.Key{Kid: "test-kid", Secret: []byte("llm-e2e-secret-do-not-use-in-prod-32")})

const e2ePassword = "CorrectHorseBatteryStaple1!"

type nopSender struct{}

func (nopSender) Send(context.Context, string, string, string) error { return nil }

// redirect sends every outbound request to the stub server.
type redirect struct{ target *url.URL }

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.target.Scheme, r.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

type e2eEnv struct {
	t     *testing.T
	srv   http.Handler
	calls *atomic.Int32
	ip    *atomic.Int32
}

func newE2E(t *testing.T, reply string) e2eEnv {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))

	calls := &atomic.Int32{}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		out, _ := json.Marshal(map[string]any{
			"id": "stub-1", "model": "stub-model",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": reply}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
		})
		_, _ = w.Write(out)
	}))
	t.Cleanup(stub.Close)
	target, err := url.Parse(stub.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: redirect{target}}
	vars := map[string]string{
		"FREELANE_ENABLED":   "false",
		"OPENROUTER_API_KEY": "stub-not-a-real-key",
	}
	stack := llm.NewStack(llm.StackConfig{
		Getenv: func(k string) string { return vars[k] }, Pool: pool, Client: client,
		HermesURL: target, HermesDefaultModel: "hermes-3",
	})

	limits := ratelimit.NewStore()
	users := auth.PGUsers(pool)
	identify := auth.Identify(e2eKeys, users)
	svc := authservice.New(pool, authservice.Options{
		Keys:      e2eKeys,
		Profiles:  hermesprofile.New(pool, hermesprofile.LoggingGateway{}, vaultpaths.Paths{Root: t.TempDir()}),
		MFASender: nopSender{}, ResetSender: nopSender{}, VerifySender: nopSender{},
		PasswordCost: 4,
	})
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth: auth.Middleware(e2eKeys, users),
		Modules: []httpserver.Module{
			authroutes.New(pool, authroutes.Options{Service: svc, Limits: limits}),
			meproviders.New(pool, meproviders.Options{Identify: identify, Limits: limits, Stack: stack}),
			llmchat.New(pool, llmchat.Options{Identify: identify, Limits: limits, MaxStreams: 3, Chat: stack.ChatService("hermes-3")}),
		},
	})
	return e2eEnv{t: t, srv: e, calls: calls, ip: &atomic.Int32{}}
}

func (v e2eEnv) do(method, path, token, body string) (int, string) {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	n := v.ip.Add(1)
	req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.9.%d.%d", n>>8&255, n&255))
	rec := httptest.NewRecorder()
	v.srv.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (v e2eEnv) object(method, path, token, body string) (map[string]any, string) {
	v.t.Helper()
	code, raw := v.do(method, path, token, body)
	require.Equal(v.t, http.StatusOK, code, raw)
	var out map[string]any
	require.NoError(v.t, json.Unmarshal([]byte(raw), &out), raw)
	return out, raw
}

// signUpThenSignIn registers over HTTP, then logs in with the same credentials
// and returns the login's access token, as the Swift helper does.
func (v e2eEnv) signUpThenSignIn() string {
	v.t.Helper()
	suffix := strings.ToLower(uuid.NewString()[:8])
	email, username := "llm-e2e-"+suffix+"@test.luminavault", "llm-e2e-"+suffix
	code, raw := v.do(http.MethodPost, "/v1/auth/register", "", fmt.Sprintf(`{"email":%q,"username":%q,"password":%q}`, email, username, e2ePassword))
	require.Equal(v.t, http.StatusOK, code, raw)
	login, _ := v.object(http.MethodPost, "/v1/auth/login", "", fmt.Sprintf(`{"email":%q,"password":%q}`, email, e2ePassword))
	token, _ := login["accessToken"].(string)
	require.NotEmpty(v.t, token)
	return token
}

func (v e2eEnv) chat(token, content, want string) {
	v.t.Helper()
	before := v.calls.Load()
	reply, raw := v.object(http.MethodPost, "/v1/llm/chat", token, fmt.Sprintf(`{"messages":[{"role":"user","content":%q}]}`, content))
	msg, ok := reply["message"].(map[string]any)
	require.True(v.t, ok, raw)
	assert.Equal(v.t, "assistant", msg["role"])
	assert.Equal(v.t, want, msg["content"])
	assert.Greater(v.t, v.calls.Load(), before, "the chat reached the stub upstream")
}

// LLMPreferencesChatE2ETests: signup, signin, set managed brain, then chat gets a reply.
func TestLLMPreferencesE2EManagedBrainThenChatReplies(t *testing.T) {
	const reply = "Hello from the LuminaVault default brain."
	v := newE2E(t, reply)
	token := v.signUpThenSignIn()

	put, _ := v.object(http.MethodPut, "/v1/me/preferences/llm", token, `{"mode":"managed","primaryProvider":"custom","primaryModel":"","fallbackChain":[]}`)
	assert.Equal(t, "managed", put["mode"])
	assert.Equal(t, "openRouter", put["primaryProvider"])
	assert.Equal(t, llm.GenericBrainName, put["primaryModel"])
	assert.NotEqual(t, llm.ManagedModel, put["primaryModel"])
	assert.Empty(t, put["fallbackChain"])

	got, _ := v.object(http.MethodGet, "/v1/me/preferences/llm", token, "")
	assert.Equal(t, "managed", got["mode"])
	assert.Equal(t, "openRouter", got["primaryProvider"])

	v.chat(token, "Hello, who are you?", reply)
}

// LLMPreferencesChatE2ETests: a BYOK preference with a non-empty fallback chain
// persists and round-trips (the jsonb bind Swift once broke).
func TestLLMPreferencesE2EByokFallbackChainRoundTrips(t *testing.T) {
	v := newE2E(t, "unused")
	token := v.signUpThenSignIn()

	put, raw := v.object(http.MethodPut, "/v1/me/preferences/llm", token, `{"mode":"byok","primaryProvider":"anthropic","primaryModel":"claude-opus-4-7","fallbackChain":[{"provider":"openai","model":"gpt-4o"}]}`)
	assert.Equal(t, "byok", put["mode"])
	assert.Equal(t, "anthropic", put["primaryProvider"])
	chain, _ := put["fallbackChain"].([]any)
	require.Len(t, chain, 1, raw)
	step, _ := chain[0].(map[string]any)
	assert.Equal(t, "openai", step["provider"])
	assert.Equal(t, "gpt-4o", step["model"])

	got, raw := v.object(http.MethodGet, "/v1/me/preferences/llm", token, "")
	chain, _ = got["fallbackChain"].([]any)
	require.Len(t, chain, 1, raw)
	step, _ = chain[0].(map[string]any)
	assert.Equal(t, "openai", step["provider"])
}

// LLMPreferencesChatE2ETests: a model chosen in managed mode is not disclosed
// back, and chat still replies.
func TestLLMPreferencesE2EManagedModelIsNotDisclosedAndChatReplies(t *testing.T) {
	const reply = "Reply from stepfun/step-3.7-flash:free via Nous."
	const model = "stepfun/step-3.7-flash:free"
	v := newE2E(t, reply)
	token := v.signUpThenSignIn()

	put, raw := v.object(http.MethodPut, "/v1/me/preferences/llm", token, fmt.Sprintf(`{"mode":"managed","primaryProvider":"openRouter","primaryModel":%q,"fallbackChain":[]}`, model))
	assert.Equal(t, "managed", put["mode"])
	assert.Equal(t, "openRouter", put["primaryProvider"])
	assert.Equal(t, llm.GenericBrainName, put["primaryModel"])
	assert.NotContains(t, raw, model)

	got, raw := v.object(http.MethodGet, "/v1/me/preferences/llm", token, "")
	assert.Equal(t, llm.GenericBrainName, got["primaryModel"])
	assert.NotContains(t, raw, model)

	v.chat(token, "Hello from the stepfun e2e test.", reply)
}
