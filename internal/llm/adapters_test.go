package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	completionOK  = `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`
	chatPayload   = `{"model":"hermes","messages":[{"role":"user","content":"hi"}]}`
	platformKey   = "PLATFORM-ENV-KEY-DO-NOT-SPEND"
	completionURL = "/v1/chat/completions"
)

// HermesGatewayAdapterTests: an httptest server per host stands in for the
// URLProtocol stub. Which server received the call is what "host" asserted.
func TestHermesDispatch(t *testing.T) {
	adapter := func(stub *wireStub, auth string) *HermesGatewayAdapter {
		return &HermesGatewayAdapter{BaseURL: stub.url(t), DefaultAuthHeader: auth}
	}
	withOverride := func(u *url.URL, auth string, override bool) context.Context {
		return WithValues(context.Background(), func(v *Values) {
			v.CurrentResolution = &Resolution{BaseURL: u, AuthHeader: auth, IsUserOverride: override}
		})
	}
	t.Run("no resolution dispatches to managed baseURL without authorization", func(t *testing.T) {
		managed := newWireStub(t, 200, completionOK)
		_, err := adapter(managed, "").ChatCompletions(context.Background(), []byte(chatPayload), "alice", "")
		require.NoError(t, err)
		sent := managed.last(t)
		assert.Equal(t, completionURL, sent.path)
		assert.Empty(t, sent.header.Get("Authorization"))
		assert.Equal(t, "alice", sent.header.Get("X-Hermes-Session-Key"))
		assert.Empty(t, sent.header.Values("X-Hermes-Session-Id"))
		assert.Empty(t, sent.header.Values("X-Hermes-Profile"))
	})
	t.Run("defaultAuthHeader sends Authorization when no override is in scope", func(t *testing.T) {
		managed := newWireStub(t, 200, completionOK)
		_, err := adapter(managed, "Bearer her254-key").ChatCompletions(context.Background(), []byte(chatPayload), "elise", "")
		require.NoError(t, err)
		assert.Equal(t, "Bearer her254-key", managed.last(t).header.Get("Authorization"))
	})
	t.Run("user override masks the defaultAuthHeader and moves the call", func(t *testing.T) {
		managed, vps := newWireStub(t, 200, completionOK), newWireStub(t, 200, completionOK)
		_, err := adapter(managed, "Bearer central-key").ChatCompletions(withOverride(vps.url(t), "Bearer user-token", true), []byte(chatPayload), "bob", "")
		require.NoError(t, err)
		assert.Zero(t, managed.count())
		sent := vps.last(t)
		assert.Equal(t, completionURL, sent.path)
		assert.Equal(t, "Bearer user-token", sent.header.Get("Authorization"))
		assert.Equal(t, "bob", sent.header.Get("X-Hermes-Session-Key"))
		assert.Empty(t, sent.header.Values("X-Hermes-Profile"))
	})
	t.Run("non-override resolution falls back to managed baseURL", func(t *testing.T) {
		managed, other := newWireStub(t, 200, completionOK), newWireStub(t, 200, completionOK)
		_, err := adapter(managed, "").ChatCompletions(withOverride(other.url(t), "Bearer should-not-be-used", false), []byte(chatPayload), "c", "")
		require.NoError(t, err)
		assert.Zero(t, other.count())
		assert.Empty(t, managed.last(t).header.Get("Authorization"))
	})
	t.Run("user override with an empty authHeader skips the header", func(t *testing.T) {
		managed, vps := newWireStub(t, 200, completionOK), newWireStub(t, 200, completionOK)
		_, err := adapter(managed, "Bearer central-key").ChatCompletions(withOverride(vps.url(t), "", true), []byte(chatPayload), "d", "")
		require.NoError(t, err)
		assert.Empty(t, vps.last(t).header.Values("Authorization"))
	})
	t.Run("sessionID sets X-Hermes-Session-Id", func(t *testing.T) {
		managed := newWireStub(t, 200, completionOK)
		_, err := adapter(managed, "").ChatCompletions(context.Background(), []byte(chatPayload), "alice", "conv-1")
		require.NoError(t, err)
		assert.Equal(t, "alice", managed.last(t).header.Get("X-Hermes-Session-Key"))
		assert.Equal(t, "conv-1", managed.last(t).header.Get("X-Hermes-Session-Id"))
	})
	t.Run("the stream honours the same override, with stream forced on", func(t *testing.T) {
		managed := newWireStub(t, 200, "")
		vps := newWireStub(t, 200, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
		chunks, err := collect(adapter(managed, "Bearer central-key").ChatStream(withOverride(vps.url(t), "Bearer user-token", true), []byte(chatPayload), "bob", "conv"))
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "hi"}}, chunks)
		assert.Zero(t, managed.count())
		sent := vps.last(t)
		assert.Equal(t, "Bearer user-token", sent.header.Get("Authorization"))
		assert.Equal(t, "text/event-stream", sent.header.Get("Accept"))
		assert.Equal(t, "conv", sent.header.Get("X-Hermes-Session-Id"))
		assert.Equal(t, true, sent.body["stream"])
	})
	t.Run("a BYO gateway cannot redirect the buffered call", func(t *testing.T) {
		var followed atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Add(1) }))
		t.Cleanup(target.Close)
		bounce := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
		t.Cleanup(bounce.Close)
		u, err := url.Parse(bounce.URL)
		require.NoError(t, err)
		_, err = (&HermesGatewayAdapter{BaseURL: u}).ChatCompletions(context.Background(), []byte(chatPayload), "a", "")
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, 307, perr.Status)
		assert.Zero(t, followed.Load())
	})
}

// HermesGatewayTimeoutTests. Short timeouts stand in for the 90 s and 2 s.
func TestHermesTimeouts(t *testing.T) {
	assert.Equal(t, 90*time.Second, HermesRequestTimeout)

	stall := func(t *testing.T, stalls int32) (*url.URL, *atomic.Int32) {
		t.Helper()
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The server only notices a client hang-up once the body is drained.
			_, _ = io.Copy(io.Discard, r.Body)
			if attempts.Add(1) <= stalls {
				<-r.Context().Done()
				return
			}
			_, _ = w.Write([]byte(completionOK))
		}))
		t.Cleanup(srv.Close)
		u, err := url.Parse(srv.URL)
		require.NoError(t, err)
		return u, &attempts
	}
	quick := func(u *url.URL) *HermesGatewayAdapter {
		return &HermesGatewayAdapter{BaseURL: u, RequestTimeout: 100 * time.Millisecond, RetryDelay: time.Millisecond}
	}
	t.Run("non-stream payload retries once on a timeout", func(t *testing.T) {
		u, attempts := stall(t, 1)
		meta, err := quick(u).ChatCompletions(context.Background(), []byte(chatPayload), "a", "")
		require.NoError(t, err)
		assert.EqualValues(t, 2, attempts.Load(), "must retry exactly once after timeout")
		assert.Equal(t, completionOK, string(meta.Data))
	})
	t.Run("non-stream payload throws after the second timeout", func(t *testing.T) {
		u, attempts := stall(t, 99)
		_, err := quick(u).ChatCompletions(context.Background(), []byte(chatPayload), "a", "")
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, "upstream_timeout", perr.ReasonCode())
		assert.EqualValues(t, 2, attempts.Load())
	})
	t.Run("stream payload does not retry", func(t *testing.T) {
		u, attempts := stall(t, 99)
		_, err := quick(u).ChatCompletions(context.Background(), []byte(`{"model":"m","stream":true,"messages":[]}`), "a", "")
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.EqualValues(t, 1, attempts.Load(), "streamed requests must not retry")
	})
	t.Run("a non-timeout failure does not retry", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}))
		t.Cleanup(srv.Close)
		u, err := url.Parse(srv.URL)
		require.NoError(t, err)
		_, err = quick(u).ChatCompletions(context.Background(), []byte(chatPayload), "a", "")
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, KindNetwork, perr.Kind)
		assert.EqualValues(t, 1, attempts.Load(), "only a timeout triggers the retry")
	})
}

type stubCredentials struct {
	creds *ResolvedCredential
	err   error
}

func (s stubCredentials) Credential(context.Context, ProviderKind, uuid.UUID) (*ResolvedCredential, error) {
	return s.creds, s.err
}

func modeCtx(mode CredentialMode, user *User) context.Context {
	return WithValues(context.Background(), func(v *Values) {
		v.CredentialMode = mode
		v.CurrentUser = user
	})
}

// BYOKFailClosedTests. The Gemini and Ollama cases stay with those adapters.
// The assertion that matters is not that it throws but that nothing was spent.
func TestBYOKFailClosed(t *testing.T) {
	user := &User{ID: uuid.New()}
	byoKey := "user-key"
	build := func(t *testing.T, kind ProviderKind, stub *wireStub, source CredentialSource) Adapter {
		t.Helper()
		if kind == Anthropic {
			return &AnthropicAdapter{APIKey: platformKey, BaseURL: stub.url(t), Credentials: source}
		}
		return &OpenAICompatibleAdapter{ProviderKind: kind, APIKey: platformKey, BaseURL: stub.url(t), Credentials: source}
	}
	for _, kind := range []ProviderKind{OpenRouter, XAI, Anthropic} {
		t.Run(string(kind)+" byok with no resolvable credential spends nothing", func(t *testing.T) {
			for name, tc := range map[string]struct {
				source CredentialSource
				user   *User
			}{
				"no store":      {nil, user},
				"no tenant":     {stubCredentials{creds: &ResolvedCredential{APIKey: &byoKey}}, nil},
				"no row":        {stubCredentials{}, user},
				"lookup failed": {stubCredentials{err: errors.New("db")}, user},
				"empty key":     {stubCredentials{creds: &ResolvedCredential{APIKey: strPtr("")}}, user},
			} {
				stub := newWireStub(t, 200, completionOK)
				_, err := build(t, kind, stub, tc.source).ChatCompletions(modeCtx(ModeBYOK, tc.user), []byte(chatPayload), "a", "")
				require.ErrorAs(t, err, &BYOKKeysRequiredError{}, name)
				assert.Zero(t, stub.count(), name)

				_, err = collect(build(t, kind, stub, tc.source).ChatStream(modeCtx(ModeBYOK, tc.user), []byte(chatPayload), "a", ""))
				require.ErrorAs(t, err, &BYOKKeysRequiredError{}, name)
				assert.Zero(t, stub.count(), name)
			}
		})
	}
	authOf := func(sent capturedRequest, kind ProviderKind) string {
		if kind == Anthropic {
			return sent.header.Get("x-api-key")
		}
		return sent.header.Get("Authorization")
	}
	want := func(kind ProviderKind, key string) string {
		if kind == Anthropic {
			return key
		}
		return "Bearer " + key
	}
	reply := func(kind ProviderKind) string {
		if kind == Anthropic {
			return anthropicOK
		}
		return completionOK
	}
	for _, kind := range []ProviderKind{OpenRouter, Anthropic} {
		t.Run(string(kind)+" managed mode spends the platform key, ignoring a stored one", func(t *testing.T) {
			stub := newWireStub(t, 200, reply(kind))
			_, err := build(t, kind, stub, stubCredentials{creds: &ResolvedCredential{APIKey: &byoKey}}).ChatCompletions(modeCtx(ModeManaged, user), []byte(chatPayload), "a", "")
			require.NoError(t, err)
			assert.Equal(t, want(kind, platformKey), authOf(stub.last(t), kind))
		})
		t.Run(string(kind)+" an undeclared mode keeps managed semantics", func(t *testing.T) {
			stub := newWireStub(t, 200, reply(kind))
			_, err := build(t, kind, stub, nil).ChatCompletions(modeCtx(ModeUnset, nil), []byte(chatPayload), "a", "")
			require.NoError(t, err)
			assert.Equal(t, want(kind, platformKey), authOf(stub.last(t), kind))
		})
		t.Run(string(kind)+" an undeclared mode prefers the tenant's key", func(t *testing.T) {
			stub := newWireStub(t, 200, reply(kind))
			_, err := build(t, kind, stub, stubCredentials{creds: &ResolvedCredential{APIKey: &byoKey}}).ChatCompletions(modeCtx(ModeUnset, user), []byte(chatPayload), "a", "")
			require.NoError(t, err)
			assert.Equal(t, want(kind, byoKey), authOf(stub.last(t), kind))
		})
		t.Run(string(kind)+" an undeclared mode survives a failed lookup on the platform key", func(t *testing.T) {
			stub := newWireStub(t, 200, reply(kind))
			_, err := build(t, kind, stub, stubCredentials{err: errors.New("db")}).ChatCompletions(modeCtx(ModeUnset, user), []byte(chatPayload), "a", "")
			require.NoError(t, err)
			assert.Equal(t, want(kind, platformKey), authOf(stub.last(t), kind))
		})
		t.Run(string(kind)+" byok spends the tenant's key at the tenant's base URL", func(t *testing.T) {
			platform, tenant := newWireStub(t, 200, reply(kind)), newWireStub(t, 200, reply(kind))
			source := stubCredentials{creds: &ResolvedCredential{APIKey: &byoKey, BaseURL: tenant.url(t)}}
			_, err := build(t, kind, platform, source).ChatCompletions(modeCtx(ModeBYOK, user), []byte(chatPayload), "a", "")
			require.NoError(t, err)
			assert.Zero(t, platform.count())
			assert.Equal(t, want(kind, byoKey), authOf(tenant.last(t), kind))
		})
	}
}

// ProviderCredentialWiringTests, without Gemini and Ollama, which are not ported.
func TestCredentialWiring(t *testing.T) {
	source := stubCredentials{}
	ported := []ProviderKind{Anthropic, OpenAI, XAI, OpenRouter, Nous, Nvidia, Deepseek, Kimi, Custom}
	for _, kind := range ported {
		if !UserCredentialTarget(kind) {
			continue
		}
		var bare, stored Adapter
		if kind == Anthropic {
			bare, stored = &AnthropicAdapter{}, &AnthropicAdapter{Credentials: source}
		} else {
			bare, stored = &OpenAICompatibleAdapter{ProviderKind: kind}, &OpenAICompatibleAdapter{ProviderKind: kind, Credentials: source}
		}
		assert.False(t, bare.AcceptsUserCredentials(), kind)
		assert.True(t, stored.AcceptsUserCredentials(), kind)
	}
	groq := &OpenAICompatibleAdapter{ProviderKind: Groq, APIKey: "platform-key", BaseURL: DefaultBaseURL(Groq)}
	assert.False(t, groq.AcceptsUserCredentials(), "a platform-only adapter reports no credential support")
}

// NvidiaAdapterRegistrationTests.
func TestNvidiaRegistration(t *testing.T) {
	registry := func(key string) *Registry {
		var configs []ProviderConfig
		if key != "" {
			configs = []ProviderConfig{{Kind: Nvidia, APIKey: key}}
		}
		return NewRegistry(configs, &OpenAICompatibleAdapter{ProviderKind: Nvidia, APIKey: key, BaseURL: DefaultBaseURL(Nvidia)})
	}
	_, ok := registry("nvapi-test").Adapter(Nvidia)
	assert.True(t, ok)
	assert.False(t, registry("").IsEnabled(Nvidia), "isEnabled tracks the platform key, not adapter registration")
	assert.True(t, registry("nvapi-test").IsEnabled(Nvidia))
	assert.Equal(t, "https://integrate.api.nvidia.com", DefaultBaseURL(Nvidia).String())
	assert.Equal(t, "https://integrate.api.nvidia.com/v1/chat/completions", Endpoint(Nvidia, DefaultBaseURL(Nvidia)))
	assert.Equal(t, "llm.provider.openRouter.apiKey", apiKeyConfigKey("openRouter"))
	assert.Equal(t, "llm.provider.openrouter.apikey", legacyAPIKeyConfigKey("openRouter"))
	assert.Equal(t, "llm.provider.deepinfra.apikey", legacyAPIKeyConfigKey("deepInfra"))
	assert.Equal(t, "llm.provider.nvidia.apikey", legacyAPIKeyConfigKey("nvidia"))
}

func TestOpenAIAdapter(t *testing.T) {
	t.Run("the payload passes through byte for byte, temperature included", func(t *testing.T) {
		stub := newWireStub(t, 200, completionOK)
		payload := `{"model":"m","temperature":0.2,"messages":[]}`
		_, err := (&OpenAICompatibleAdapter{ProviderKind: Groq, APIKey: "k", BaseURL: stub.url(t)}).ChatCompletions(context.Background(), []byte(payload), "a", "")
		require.NoError(t, err)
		sent := stub.last(t)
		assert.Equal(t, payload, string(sent.raw))
		assert.Equal(t, completionURL, sent.path)
	})
	t.Run("deepinfra's base already carries the prefix", func(t *testing.T) {
		assert.Equal(t, "https://api.deepinfra.com/v1/openai/chat/completions", Endpoint(DeepInfra, DefaultBaseURL(DeepInfra)))
	})
	t.Run("only openrouter gets identity headers", func(t *testing.T) {
		router, groq := newWireStub(t, 200, completionOK), newWireStub(t, 200, completionOK)
		_, err := (&OpenAICompatibleAdapter{ProviderKind: OpenRouter, APIKey: "k", BaseURL: router.url(t)}).ChatCompletions(context.Background(), []byte(chatPayload), "a", "")
		require.NoError(t, err)
		_, err = (&OpenAICompatibleAdapter{ProviderKind: Groq, APIKey: "k", BaseURL: groq.url(t)}).ChatCompletions(context.Background(), []byte(chatPayload), "a", "")
		require.NoError(t, err)
		assert.Equal(t, "https://luminavault.app", router.last(t).header.Get("HTTP-Referer"))
		assert.Equal(t, "LuminaVault", router.last(t).header.Get("X-Title"))
		assert.Empty(t, groq.last(t).header.Get("X-Title"))
	})
	t.Run("the stream forces the flag on and frames SSE", func(t *testing.T) {
		stub := newWireStub(t, 200, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		chunks, err := collect((&OpenAICompatibleAdapter{ProviderKind: Groq, APIKey: "k", BaseURL: stub.url(t)}).ChatStream(context.Background(), []byte(chatPayload), "a", ""))
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "a"}, {FinishReason: "stop"}}, chunks)
		assert.Equal(t, true, stub.last(t).body["stream"])
		assert.Equal(t, "Bearer k", stub.last(t).header.Get("Authorization"))
	})
	t.Run("a keyless custom endpoint sends no Authorization", func(t *testing.T) {
		stub := newWireStub(t, 200, completionOK)
		_, err := (&OpenAICompatibleAdapter{ProviderKind: Custom, BaseURL: stub.url(t)}).ChatCompletions(context.Background(), []byte(chatPayload), "a", "")
		require.NoError(t, err)
		assert.Empty(t, stub.last(t).header.Values("Authorization"))
	})
	t.Run("a linked xAI account talks to the tenant's gateway", func(t *testing.T) {
		platform, gateway := newWireStub(t, 200, completionOK), newWireStub(t, 200, completionOK)
		connected := time.Now()
		a := &OpenAICompatibleAdapter{
			ProviderKind: XAI, APIKey: platformKey, BaseURL: platform.url(t), Credentials: stubCredentials{},
			XAIResolver: func(context.Context, uuid.UUID) *ContainerHandle {
				return &ContainerHandle{BaseURL: gateway.server.URL, APIServerKey: "container-key", XAIConnectedAt: &connected}
			},
		}
		_, err := a.ChatCompletions(modeCtx(ModeBYOK, &User{ID: uuid.New()}), []byte(chatPayload), "a", "")
		require.NoError(t, err)
		assert.Zero(t, platform.count())
		assert.Equal(t, "Bearer container-key", gateway.last(t).header.Get("Authorization"))
	})
}

func TestStackAssembly(t *testing.T) {
	hermes, err := url.Parse("http://hermes.test")
	require.NoError(t, err)
	env := envOf(map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-trial", "ANTHROPIC_FIRST": "true",
		"LLM_PROVIDER_GROQ_API_KEY": "groq-key", "OPENROUTER_API_KEY": "or-key", "NVIDIA_API_KEY": "nv-key",
	})
	stack := NewStack(StackConfig{Getenv: env, HermesURL: hermes, HermesDefaultModel: "hermes-3"})

	assert.ElementsMatch(t, []ProviderKind{HermesGateway, Groq, XAI, OpenAI, OpenRouter, Nous, Nvidia, Custom, Anthropic}, stack.Registry.Registered())
	assert.True(t, stack.Registry.IsEnabled(OpenRouter))
	assert.True(t, stack.Registry.IsEnabled(Nvidia))
	assert.False(t, stack.Registry.IsEnabled(Anthropic), "the trial key is not a registry key")
	assert.False(t, stack.Registry.IsEnabled(Together))
	assert.Nil(t, stack.Credentials, "no Box, no BYOK store")

	// Internal work has no user, so preferences are never read.
	d := stack.Router.Pick(context.Background(), "", CapabilityMedium, nil)
	assert.Equal(t, Route{Anthropic, TrialDefaultModel}, d.Primary, "the trial comes first")
	assert.Equal(t, Route{OpenRouter, FreeLaneOpenRouterModel}, d.Fallbacks[0], "the old chain is the fallback")

	wrapped := NewStack(StackConfig{Getenv: envOf(nil), HermesURL: hermes, HermesDefaultModel: "hermes-3", WrapRouter: func(Router) Router {
		return fixedRouter{Decision{Primary: Route{Groq, "wrapped"}}}
	}})
	assert.Equal(t, Route{Groq, "wrapped"}, wrapped.Router.Pick(context.Background(), "", CapabilityMedium, nil).Primary)
}
