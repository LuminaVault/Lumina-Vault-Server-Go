package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// outcome is one scripted reply: data or err for a buffered call, chunks then
// streamErr for a native stream.
type outcome struct {
	data      string
	headers   map[string]string
	err       error
	chunks    []Chunk
	streamErr error
}

// scriptedAdapter plays back outcomes and records what it was handed. Without
// native it keeps the protocol default: one chunk from the buffered path.
type scriptedAdapter struct {
	kind     ProviderKind
	native   bool
	mu       sync.Mutex
	outcomes []outcome
	calls    [][]byte
	modes    []CredentialMode
}

func (a *scriptedAdapter) Kind() ProviderKind           { return a.kind }
func (a *scriptedAdapter) AcceptsUserCredentials() bool { return false }

func (a *scriptedAdapter) next(ctx context.Context, payload []byte) outcome {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, payload)
	a.modes = append(a.modes, RoutingValues(ctx).CredentialMode)
	if len(a.outcomes) == 0 {
		return outcome{err: Transient(a.kind, 0, strPtr("stub: outcomes exhausted"))}
	}
	out := a.outcomes[0]
	a.outcomes = a.outcomes[1:]
	return out
}

func (a *scriptedAdapter) ChatCompletions(ctx context.Context, payload []byte, _, _ string) (Metadata, error) {
	out := a.next(ctx, payload)
	if out.err != nil {
		return Metadata{}, out.err
	}
	return Metadata{Data: []byte(out.data), Headers: out.headers}, nil
}

func (a *scriptedAdapter) ChatStream(ctx context.Context, payload []byte, key, id string) Stream {
	if !a.native {
		return BufferedChatStream(ctx, a, payload, key, id)
	}
	return func(yield func(Chunk, error) bool) {
		out := a.next(ctx, payload)
		if out.err != nil {
			yield(Chunk{}, out.err)
			return
		}
		for _, chunk := range out.chunks {
			if !yield(chunk, nil) {
				return
			}
		}
		if out.streamErr != nil {
			yield(Chunk{}, out.streamErr)
		}
	}
}

func (a *scriptedAdapter) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func (a *scriptedAdapter) models(t *testing.T) []string {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.calls))
	for _, call := range a.calls {
		dict, ok := decodeObject(call)
		require.True(t, ok)
		out = append(out, dict["model"].(string))
	}
	return out
}

func reply(s string) outcome { return outcome{data: s} }

func failing(err error) outcome { return outcome{err: err} }

const emptyPayload = `{"model":"stub","messages":[]}`

func transportFor(decision Decision, adapters ...Adapter) *Transport {
	return &Transport{Registry: NewRegistry(nil, adapters...), Router: fixedRouter{decision}}
}

func call(t *Transport) (Metadata, error) {
	return t.ChatCompletions(context.Background(), []byte(emptyPayload), "alice", "")
}

// RoutedLLMTransportTests.
func TestTransportFailover(t *testing.T) {
	stub := func(p ProviderKind) Route { return Route{p, "stub"} }
	t.Run("primary success returns data", func(t *testing.T) {
		primary := &scriptedAdapter{kind: HermesGateway, outcomes: []outcome{reply("OK")}}
		result, err := call(transportFor(Decision{Primary: stub(HermesGateway)}, primary))
		require.NoError(t, err)
		assert.Equal(t, "OK", string(result.Data))
		assert.Equal(t, 1, primary.callCount())
	})
	t.Run("transient failovers to next", func(t *testing.T) {
		primary := &scriptedAdapter{kind: Together, outcomes: []outcome{failing(Transient(Together, 429, nil))}}
		fallback := &scriptedAdapter{kind: Groq, outcomes: []outcome{reply("FALLBACK")}}
		result, err := call(transportFor(Decision{Primary: stub(Together), Fallbacks: []Route{stub(Groq)}}, primary, fallback))
		require.NoError(t, err)
		assert.Equal(t, "FALLBACK", string(result.Data))
		assert.Equal(t, 1, primary.callCount())
		assert.Equal(t, 1, fallback.callCount())
	})
	t.Run("network error failovers to next", func(t *testing.T) {
		primary := &scriptedAdapter{kind: Together, outcomes: []outcome{failing(Network(Together, errors.New("x")))}}
		fallback := &scriptedAdapter{kind: Groq, outcomes: []outcome{reply("OK")}}
		_, err := call(transportFor(Decision{Primary: stub(Together), Fallbacks: []Route{stub(Groq)}}, primary, fallback))
		require.NoError(t, err)
		assert.Equal(t, 1, fallback.callCount(), "network errors must be treated as recoverable")
	})
	t.Run("permanent does not failover", func(t *testing.T) {
		primary := &scriptedAdapter{kind: Together, outcomes: []outcome{failing(Permanent(Together, 400, nil))}}
		fallback := &scriptedAdapter{kind: Groq, outcomes: []outcome{reply("WRONG")}}
		_, err := call(transportFor(Decision{Primary: stub(Together), Fallbacks: []Route{stub(Groq)}}, primary, fallback))
		require.Error(t, err)
		assert.Zero(t, fallback.callCount(), "permanent failures must not touch the fallback")
	})
	t.Run("all candidates exhausted throws", func(t *testing.T) {
		primary := &scriptedAdapter{kind: Together, outcomes: []outcome{failing(Transient(Together, 503, nil))}}
		fallback := &scriptedAdapter{kind: Groq, outcomes: []outcome{failing(Transient(Groq, 502, nil))}}
		_, err := call(transportFor(Decision{Primary: stub(Together), Fallbacks: []Route{stub(Groq)}}, primary, fallback))
		require.Error(t, err)
		assert.Equal(t, 1, primary.callCount())
		assert.Equal(t, 1, fallback.callCount())
	})
	t.Run("unregistered provider is skipped", func(t *testing.T) {
		groq := &scriptedAdapter{kind: Groq, outcomes: []outcome{reply("OK")}}
		result, err := call(transportFor(Decision{Primary: stub(Together), Fallbacks: []Route{stub(Groq)}}, groq))
		require.NoError(t, err)
		assert.Equal(t, "OK", string(result.Data))
	})
	t.Run("byok with zero keys throws before calling providers", func(t *testing.T) {
		primary := &scriptedAdapter{kind: OpenRouter, outcomes: []outcome{reply("SHOULD_NOT_RUN")}}
		_, err := call(transportFor(Decision{
			Primary:  stub(OpenRouter),
			Cerberus: &CerberusMetadata{ProfileName: "BYOK", Mode: ModeBYOK, BYOKKeysRequired: true},
		}, primary))
		require.ErrorAs(t, err, &BYOKKeysRequiredError{})
		assert.Zero(t, primary.callCount(), "BYOK fail-closed must not debit platform keys")
	})
	t.Run("routed model id is rewritten into the payload", func(t *testing.T) {
		primary := &scriptedAdapter{kind: HermesGateway, outcomes: []outcome{reply("OK")}}
		_, err := call(transportFor(Decision{Primary: Route{HermesGateway, "hermes-3-large"}}, primary))
		require.NoError(t, err)
		assert.Equal(t, []string{"hermes-3-large"}, primary.models(t))
	})
	t.Run("a BYOK adapter failure stops the walk", func(t *testing.T) {
		primary := &scriptedAdapter{kind: XAI, outcomes: []outcome{failing(BYOKKeysRequiredError{})}}
		fallback := &scriptedAdapter{kind: OpenRouter, outcomes: []outcome{reply("WRONG")}}
		_, err := call(transportFor(Decision{Primary: stub(XAI), Fallbacks: []Route{stub(OpenRouter)}, CredentialMode: ModeBYOK}, primary, fallback))
		require.ErrorAs(t, err, &BYOKKeysRequiredError{})
		assert.Zero(t, fallback.callCount())
	})
	t.Run("the dispatch mode reaches the adapter", func(t *testing.T) {
		primary := &scriptedAdapter{kind: OpenRouter, outcomes: []outcome{reply("OK")}}
		_, err := call(transportFor(Decision{Primary: stub(OpenRouter), Cerberus: &CerberusMetadata{Mode: ModeBYOK}}, primary))
		require.NoError(t, err)
		assert.Equal(t, []CredentialMode{ModeBYOK}, primary.modes)
	})
	t.Run("a cancelled caller spends nothing more", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		primary := &cancellingAdapter{cancel: cancel}
		fallback := &scriptedAdapter{kind: Groq, outcomes: []outcome{reply("WRONG")}}
		transport := transportFor(Decision{Primary: stub(Together), Fallbacks: []Route{stub(Groq)}}, primary, fallback)
		_, err := transport.ChatCompletions(ctx, []byte(emptyPayload), "alice", "")
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, fallback.callCount())
	})
}

type cancellingAdapter struct{ cancel context.CancelFunc }

func (a *cancellingAdapter) Kind() ProviderKind           { return Together }
func (a *cancellingAdapter) AcceptsUserCredentials() bool { return false }
func (a *cancellingAdapter) ChatCompletions(context.Context, []byte, string, string) (Metadata, error) {
	a.cancel()
	return Metadata{}, Network(Together, context.Canceled)
}

func (a *cancellingAdapter) ChatStream(ctx context.Context, p []byte, k, id string) Stream {
	return BufferedChatStream(ctx, a, p, k, id)
}

func TestWireRoutes(t *testing.T) {
	advertised := []WireRoute{{Provider: ProviderID(OpenRouter), Model: "hermes-3"}}
	t.Run("prefers the dispatched BYOK candidate over a Hermes advertisement", func(t *testing.T) {
		assert.Equal(t, []WireRoute{{Provider: ProviderID(XAI), Model: "grok-4.3"}}, WireRoutes([]Route{{XAI, "grok-4.3"}}, advertised))
	})
	t.Run("keeps the Hermes advertisement when the candidate has no wire provider", func(t *testing.T) {
		assert.Equal(t, advertised, WireRoutes([]Route{{HermesGateway, "hermes-3"}}, advertised))
	})
}

func byoHermesDecision() Decision {
	return Decision{
		Primary: hermesRoute,
		Cerberus: &CerberusMetadata{
			ProfileName: "BYO Hermes", Strategy: "sequential", Mode: ModeBYOK, RoutingPolicy: "locked", DeferredToHermes: true,
			Routes: []WireRoute{{Provider: ProviderID(OpenRouter), Model: "hermes-3"}},
		},
		CredentialMode: ModeBYOK,
	}
}

func TestForcedRoute(t *testing.T) {
	forced := &WireRoute{Provider: ProviderID(XAI), Model: "grok-4.3"}
	t.Run("forced BYOK route keeps Cerberus metadata but replaces the primary", func(t *testing.T) {
		out := ApplyingForcedRoute(byoHermesDecision(), forced)
		assert.Equal(t, Route{XAI, "grok-4.3"}, out.Primary)
		assert.Empty(t, out.Fallbacks)
		assert.Equal(t, "BYO Hermes", out.Cerberus.ProfileName)
		assert.Equal(t, ModeBYOK, out.CredentialMode)
	})
	t.Run("streaming routing SSE reports the forced BYOK model not the Hermes advertisement", func(t *testing.T) {
		adapter := &scriptedAdapter{kind: XAI, outcomes: []outcome{reply(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)}}
		transport := transportFor(byoHermesDecision(), adapter)
		var events []CerberusEvent
		ctx := WithValues(context.Background(), func(v *Values) {
			v.ForcedRoute = forced
			v.CerberusSink = func(e CerberusEvent) { events = append(events, e) }
		})
		chunks, err := collect(transport.ChatStream(ctx, []byte(`{"model":"grok-4.3","messages":[]}`), "alice", ""))
		require.NoError(t, err)
		assert.NotEmpty(t, chunks)
		require.NotEmpty(t, events)
		require.NotNil(t, events[0].Routing)
		assert.Equal(t, []WireRoute{*forced}, events[0].Routing.ActiveRoutes)
		assert.Equal(t, "BYO Hermes", events[0].Routing.ProfileName)
		assert.Equal(t, []string{"grok-4.3"}, adapter.models(t))
	})
	t.Run("a forced route never overrides a free-lane decision", func(t *testing.T) {
		lane := Decision{Primary: Route{OpenRouter, "free/lane-model:free"}, Cerberus: &CerberusMetadata{Mode: ModeManaged, IsFreeLane: true}}
		assert.Equal(t, Route{OpenRouter, "free/lane-model:free"}, ApplyingForcedRoute(lane, forced).Primary)
	})
	t.Run("an unknown forced provider changes nothing", func(t *testing.T) {
		d := Decision{Primary: hermesRoute}
		assert.Equal(t, d, ApplyingForcedRoute(d, &WireRoute{Provider: "nope", Model: "x"}))
	})
}

func collectNotices(ctx context.Context) (context.Context, func() []FailoverNotice) {
	var mu sync.Mutex
	var notices []FailoverNotice
	return WithValues(ctx, func(v *Values) {
			v.FailoverSink = func(n FailoverNotice) {
				mu.Lock()
				defer mu.Unlock()
				notices = append(notices, n)
			}
		}), func() []FailoverNotice {
			mu.Lock()
			defer mu.Unlock()
			return notices
		}
}

// RoutedLLMTransportFallbackTests.
func TestFailoverNotices(t *testing.T) {
	t.Run("credit exhaustion fires failover notice and falls over", func(t *testing.T) {
		primary := &scriptedAdapter{kind: XAI, outcomes: []outcome{failing(CreditExhausted(XAI, 403, strPtr("insufficient_quota")))}}
		fallback := &scriptedAdapter{kind: OpenRouter, outcomes: []outcome{reply("FALLBACK")}}
		transport := transportFor(Decision{Primary: Route{XAI, "grok-4"}, Fallbacks: []Route{{OpenRouter, "qwen-2.5-72b"}}}, primary, fallback)
		ctx, notices := collectNotices(context.Background())
		result, err := transport.ChatCompletions(ctx, []byte(`{"model":"grok-4","messages":[]}`), "alice", "")
		require.NoError(t, err)
		assert.Equal(t, "FALLBACK", string(result.Data))
		require.Len(t, notices(), 1, "exactly one notice per failover")
		notice := notices()[0]
		assert.Equal(t, XAI, notice.OriginalProvider)
		assert.Equal(t, "grok-4", notice.OriginalModel)
		assert.Equal(t, OpenRouter, notice.FallbackProvider)
		assert.Equal(t, "qwen-2.5-72b", notice.FallbackModel)
		assert.Equal(t, "credit_exhausted", notice.ReasonCode)
		assert.Contains(t, notice.UserMessage, "Grok")
		assert.Equal(t, 403, *notice.StatusCode)
	})
	t.Run("no notice when primary succeeds", func(t *testing.T) {
		transport := transportFor(Decision{Primary: Route{Anthropic, "claude-sonnet-4.6"}}, &scriptedAdapter{kind: Anthropic, outcomes: []outcome{reply("OK")}})
		ctx, notices := collectNotices(context.Background())
		_, err := transport.ChatCompletions(ctx, []byte(emptyPayload), "alice", "")
		require.NoError(t, err)
		assert.Empty(t, notices())
	})
	t.Run("chained failover only notices the last transition", func(t *testing.T) {
		transport := transportFor(
			Decision{Primary: Route{XAI, "grok-4"}, Fallbacks: []Route{{Anthropic, "claude-sonnet-4.6"}, {OpenRouter, "qwen-2.5-72b"}}},
			&scriptedAdapter{kind: XAI, outcomes: []outcome{failing(CreditExhausted(XAI, 402, nil))}},
			&scriptedAdapter{kind: Anthropic, outcomes: []outcome{failing(Transient(Anthropic, 429, nil))}},
			&scriptedAdapter{kind: OpenRouter, outcomes: []outcome{reply("LAST")}},
		)
		ctx, notices := collectNotices(context.Background())
		_, err := transport.ChatCompletions(ctx, []byte(emptyPayload), "alice", "")
		require.NoError(t, err)
		require.Len(t, notices(), 1, "only the successful transition emits a notice")
		assert.Equal(t, Anthropic, notices()[0].OriginalProvider)
		assert.Equal(t, OpenRouter, notices()[0].FallbackProvider)
		assert.Equal(t, "rate_limit", notices()[0].ReasonCode)
	})
}

// RoutedLLMTransportTimeoutEnvelopeTests.
func TestExhaustionEnvelopes(t *testing.T) {
	gateway := Decision{Primary: hermesRoute}
	run := func(t *testing.T, err error) *UpstreamErrorResponse {
		t.Helper()
		_, got := call(transportFor(gateway, &scriptedAdapter{kind: HermesGateway, outcomes: []outcome{failing(err)}}))
		var response *UpstreamErrorResponse
		require.ErrorAs(t, got, &response)
		return response
	}
	refused := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}

	t.Run("exhausted timeout has upstream_timeout", func(t *testing.T) {
		response := run(t, Network(HermesGateway, context.DeadlineExceeded))
		assert.Equal(t, "upstream_timeout", response.Reason)
		assert.Equal(t, http.StatusGatewayTimeout, response.HTTPStatus())
		require.NotNil(t, response.RetryAfterMs, "upstream_timeout envelope must include the hint")
		assert.Equal(t, 2000, *response.RetryAfterMs)
	})
	t.Run("non-timeout exhaustion has no retry_after_ms", func(t *testing.T) {
		response := run(t, Network(HermesGateway, refused))
		assert.Equal(t, "upstream_unreachable", response.Reason)
		assert.Nil(t, response.RetryAfterMs)
		assert.Equal(t, http.StatusBadGateway, response.HTTPStatus())
	})
	t.Run("permanent error has upstream_rejected", func(t *testing.T) {
		response := run(t, Permanent(HermesGateway, 400, strPtr("bad json")))
		assert.Equal(t, "upstream_rejected", response.Reason)
		assert.Equal(t, http.StatusBadGateway, response.HTTPStatus())
	})
	t.Run("no providers", func(t *testing.T) {
		_, err := call(transportFor(gateway))
		var response *UpstreamErrorResponse
		require.ErrorAs(t, err, &response)
		assert.Equal(t, "no_providers", response.Reason)
		assert.Equal(t, http.StatusBadGateway, response.HTTPStatus())
	})
	t.Run("unclassified errors have upstream_error", func(t *testing.T) {
		response := run(t, errors.New("opaque"))
		assert.Equal(t, "upstream_error", response.Reason)
		assert.Equal(t, http.StatusBadGateway, response.HTTPStatus())
	})
}

// FreeLaneModelUnavailableFailoverTests.
func TestFreeLaneNotFound(t *testing.T) {
	notFound := func(p ProviderKind) *ProviderError { return Permanent(p, 404, strPtr("model not found")) }
	lane := func() Decision {
		return Decision{
			Primary:   Route{OpenRouter, FreeLaneOpenRouterModel},
			Fallbacks: []Route{{OpenRouter, "qwen/secondary:free"}, {Nvidia, FreeLaneNvidiaModel}},
			Cerberus:  &CerberusMetadata{Mode: ModeManaged, IsFreeLane: true},
		}
	}
	content := func(s string) string {
		return `{"choices":[{"message":{"role":"assistant","content":"` + s + `"}}]}`
	}
	t.Run("the classifier still reads a 404 as permanent", func(t *testing.T) {
		err := Classify(OpenRouter, 404, []byte("model not found"))
		assert.False(t, err.IsRecoverable())
		assert.False(t, ShouldAdvance(err, false))
		assert.True(t, ShouldAdvance(err, true))
	})
	t.Run("other permanent errors stop the free lane", func(t *testing.T) {
		assert.False(t, ShouldAdvance(Classify(OpenRouter, 400, []byte("bad")), true))
		assert.False(t, ShouldAdvance(Classify(OpenRouter, 401, []byte("nope")), true))
	})
	t.Run("a retired primary free slug falls through to the secondary", func(t *testing.T) {
		openRouter := &scriptedAdapter{kind: OpenRouter, outcomes: []outcome{failing(notFound(OpenRouter)), reply(content("from super"))}}
		nvidia := &scriptedAdapter{kind: Nvidia, outcomes: []outcome{reply(content("from nim"))}}
		result, err := call(transportFor(lane(), openRouter, nvidia))
		require.NoError(t, err)
		assert.Equal(t, "from super", ExtractContent(result.Data))
		assert.Equal(t, []string{FreeLaneOpenRouterModel, "qwen/secondary:free"}, openRouter.models(t))
		assert.Zero(t, nvidia.callCount())
	})
	t.Run("both free slugs retired falls through to the NIM reserve", func(t *testing.T) {
		openRouter := &scriptedAdapter{kind: OpenRouter, outcomes: []outcome{failing(notFound(OpenRouter)), failing(notFound(OpenRouter))}}
		nvidia := &scriptedAdapter{kind: Nvidia, outcomes: []outcome{reply(content("from nim"))}}
		result, err := call(transportFor(lane(), openRouter, nvidia))
		require.NoError(t, err)
		assert.Equal(t, "from nim", ExtractContent(result.Data))
		assert.Equal(t, []string{FreeLaneNvidiaModel}, nvidia.models(t))
	})
	t.Run("outside the free lane a 404 still ends the chain", func(t *testing.T) {
		openRouter := &scriptedAdapter{kind: OpenRouter, outcomes: []outcome{failing(notFound(OpenRouter)), reply(content("from super"))}}
		nvidia := &scriptedAdapter{kind: Nvidia, outcomes: []outcome{reply(content("from nim"))}}
		d := lane()
		d.Cerberus = nil
		_, err := call(transportFor(d, openRouter, nvidia))
		var response *UpstreamErrorResponse
		require.ErrorAs(t, err, &response)
		assert.Equal(t, 1, openRouter.callCount())
	})
	t.Run("a fully failed free lane names no provider or model", func(t *testing.T) {
		openRouter := &scriptedAdapter{kind: OpenRouter, outcomes: []outcome{failing(notFound(OpenRouter)), failing(notFound(OpenRouter))}}
		nvidia := &scriptedAdapter{kind: Nvidia, outcomes: []outcome{failing(notFound(Nvidia))}}
		_, err := call(transportFor(lane(), openRouter, nvidia))
		var response *UpstreamErrorResponse
		require.ErrorAs(t, err, &response)
		assert.Equal(t, freeLaneFailureMessage, response.UserMessage())
		lower := strings.ToLower(response.UserMessage())
		for _, leak := range []string{"openrouter", "nvidia", "nim", "qwen", "deepseek", "model not found"} {
			assert.NotContains(t, lower, leak)
		}
	})
}

// RoutedLLMTransportStreamingTests. The cases driving Swift's
// RoutedHermesLLMStreamService and CerberusRouterService move with those; the
// assertions that belong to the transport are kept with a fixed router.
func TestTransportStream(t *testing.T) {
	gptStream := Decision{Primary: Route{OpenAI, "gpt-stream"}, CredentialMode: ModeBYOK}
	payload := []byte(`{"model":"ignored","messages":[{"role":"user","content":"Hello"}]}`)
	run := func(transport *Transport) ([]Chunk, error) {
		return collect(transport.ChatStream(context.Background(), payload, "alice", "conversation-1"))
	}
	t.Run("BYOK uses native provider streaming when the adapter supports it", func(t *testing.T) {
		adapter := &scriptedAdapter{kind: OpenAI, native: true, outcomes: []outcome{{chunks: []Chunk{{Delta: "Hel"}, {Delta: "lo", FinishReason: "stop"}}}}}
		chunks, err := run(transportFor(gptStream, adapter))
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "Hel"}, {Delta: "lo", FinishReason: "stop"}}, chunks)
		assert.Equal(t, []string{"gpt-stream"}, adapter.models(t))
	})
	t.Run("BYOK keeps one chunk fallback when the adapter does not stream", func(t *testing.T) {
		adapter := &scriptedAdapter{kind: OpenAI, outcomes: []outcome{reply(`{"choices":[{"message":{"role":"assistant","content":"complete answer"}}]}`)}}
		chunks, err := run(transportFor(gptStream, adapter))
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "complete answer", FinishReason: "stop"}}, chunks)
		assert.Equal(t, []string{"gpt-stream"}, adapter.models(t))
		dict, _ := decodeObject(adapter.calls[0])
		assert.NotContains(t, dict, "stream")
	})
	t.Run("managed streaming fails closed when the router budget is denied", func(t *testing.T) {
		adapter := &scriptedAdapter{kind: OpenAI, native: true}
		_, err := run(transportFor(Decision{Primary: gptStream.Primary, Cerberus: &CerberusMetadata{Mode: ModeManaged, BudgetDenied: true}}, adapter))
		var capped *UsageCapExceededError
		require.ErrorAs(t, err, &capped)
		assert.Zero(t, adapter.callCount())
	})
	t.Run("a granted free lane streams through the routed transport, never the paid gateway", func(t *testing.T) {
		lane := &scriptedAdapter{kind: OpenRouter, native: true, outcomes: []outcome{{chunks: []Chunk{{Delta: "free "}, {Delta: "reply"}}}}}
		gateway := &scriptedAdapter{kind: HermesGateway, native: true}
		chunks, err := run(transportFor(Decision{
			Primary:  Route{OpenRouter, "free/lane-model:free"},
			Cerberus: &CerberusMetadata{Mode: ModeManaged, IsFreeLane: true},
		}, lane, gateway))
		require.NoError(t, err)
		assert.Equal(t, "free reply", chunks[0].Delta+chunks[1].Delta)
		assert.Equal(t, 1, lane.callCount())
		assert.Zero(t, gateway.callCount())
	})
	t.Run("an exhausted free lane ends the stream instead of falling back to the gateway", func(t *testing.T) {
		gateway := &scriptedAdapter{kind: HermesGateway, native: true}
		_, err := run(transportFor(Decision{
			Primary:   Route{OpenRouter, "free/lane-model:free"},
			Fallbacks: []Route{hermesRoute},
			Cerberus:  &CerberusMetadata{Mode: ModeManaged, FreeLaneExhausted: true, FreeLaneRetryAfterSeconds: 60},
		}, gateway))
		var exhausted *FreeLaneExhaustedError
		require.ErrorAs(t, err, &exhausted)
		assert.Equal(t, 60, exhausted.RetryAfterSeconds)
		assert.Zero(t, gateway.callCount())
	})
	t.Run("a pinned model is not dispatched on a free-lane decision", func(t *testing.T) {
		pinned := &scriptedAdapter{kind: XAI, native: true}
		lane := &scriptedAdapter{kind: OpenRouter, native: true, outcomes: []outcome{{chunks: []Chunk{{Delta: "free"}}}}}
		ctx := WithValues(context.Background(), func(v *Values) { v.ForcedRoute = &WireRoute{Provider: ProviderID(XAI), Model: "grok-4"} })
		_, err := collect(transportFor(Decision{
			Primary:  Route{OpenRouter, "free/lane-model:free"},
			Cerberus: &CerberusMetadata{Mode: ModeManaged, IsFreeLane: true},
		}, pinned, lane).ChatStream(ctx, payload, "alice", ""))
		require.NoError(t, err)
		assert.Zero(t, pinned.callCount(), "the pinned model was dispatched on the platform key")
		assert.Equal(t, 1, lane.callCount())
	})
	t.Run("a recoverable error before the first chunk advances", func(t *testing.T) {
		first := &scriptedAdapter{kind: Together, native: true, outcomes: []outcome{failing(Transient(Together, 429, nil))}}
		second := &scriptedAdapter{kind: Groq, native: true, outcomes: []outcome{{chunks: []Chunk{{Delta: "ok"}}}}}
		ctx, notices := collectNotices(context.Background())
		chunks, err := collect(transportFor(Decision{Primary: Route{Together, "a"}, Fallbacks: []Route{{Groq, "b"}}}, first, second).ChatStream(ctx, payload, "alice", ""))
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "ok"}}, chunks)
		require.Len(t, notices(), 1)
		assert.Equal(t, "rate_limit", notices()[0].ReasonCode)
	})
	t.Run("the failover window closes at the first chunk", func(t *testing.T) {
		broken := Transient(Together, 500, nil)
		first := &scriptedAdapter{kind: Together, native: true, outcomes: []outcome{{chunks: []Chunk{{Delta: "par"}}, streamErr: broken}}}
		second := &scriptedAdapter{kind: Groq, native: true, outcomes: []outcome{{chunks: []Chunk{{Delta: "WRONG"}}}}}
		chunks, err := run(transportFor(Decision{Primary: Route{Together, "a"}, Fallbacks: []Route{{Groq, "b"}}}, first, second))
		assert.Equal(t, []Chunk{{Delta: "par"}}, chunks)
		require.ErrorIs(t, err, broken, "after partial output Swift surfaces the raw error")
		assert.Zero(t, second.callCount())
	})
	t.Run("a 2xx stream with zero chunks counts as a failure", func(t *testing.T) {
		first := &scriptedAdapter{kind: Together, native: true, outcomes: []outcome{{}}}
		second := &scriptedAdapter{kind: Groq, native: true, outcomes: []outcome{{chunks: []Chunk{{Delta: "ok"}}}}}
		chunks, err := run(transportFor(Decision{Primary: Route{Together, "a"}, Fallbacks: []Route{{Groq, "b"}}}, first, second))
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "ok"}}, chunks)
	})
	t.Run("a permanent stream failure becomes the envelope", func(t *testing.T) {
		first := &scriptedAdapter{kind: Together, native: true, outcomes: []outcome{failing(Permanent(Together, 400, nil))}}
		_, err := run(transportFor(Decision{Primary: Route{Together, "a"}}, first))
		var response *UpstreamErrorResponse
		require.ErrorAs(t, err, &response)
		assert.Equal(t, "upstream_rejected", response.Reason)
	})
	t.Run("a decided stream does not pick again", func(t *testing.T) {
		adapter := &scriptedAdapter{kind: OpenAI, native: true, outcomes: []outcome{{chunks: []Chunk{{Delta: "ok"}}}}}
		transport := &Transport{Registry: NewRegistry(nil, adapter), Router: panicRouter{}}
		_, err := collect(transport.ChatStreamDecided(context.Background(), payload, "alice", "", gptStream))
		require.NoError(t, err)
		assert.Equal(t, 1, adapter.callCount())
	})
}

type panicRouter struct{}

func (panicRouter) Pick(context.Context, string, Capability, *User) Decision {
	panic("a decided stream must not pick")
}

// CostLedgerWiringTests.
func TestLedger(t *testing.T) {
	haiku := "claude-3-5-haiku-20241022"
	assert.Equal(t, int64(4_800_000), CatalogCost(ProviderID(Anthropic), haiku, 1_000_000, 1_000_000, nil))
	assert.Equal(t, int64(80000), CatalogCost(ProviderID(Anthropic), haiku, 100_000, 0, nil))
	assert.Equal(t, int64(15000), CatalogCost(ProviderID(OpenAI), "gpt-4o-mini", 100_000, 0, nil))
	assert.Zero(t, CatalogCost(ProviderID(OpenAI), "some-model-we-have-never-priced", 1_000_000, 1_000_000, nil))
	assert.Zero(t, CatalogCost(ProviderID(Anthropic), haiku, 0, 0, nil))
	assert.Zero(t, CatalogCost(ProviderID(Anthropic), haiku, -5, -5, nil))

	_, _, ok := LedgerCharge(ModeBYOK, Anthropic, haiku, 1_000_000, 1_000_000, nil)
	assert.False(t, ok, "BYOK traffic is never charged")

	provider, usd, ok := LedgerCharge(ModeManaged, Anthropic, haiku, 1_000_000, 1_000_000, nil)
	require.True(t, ok)
	assert.Equal(t, ProviderID(Anthropic), provider)
	assert.Equal(t, int64(4_800_000), usd)

	_, usd, ok = LedgerCharge(ModeUnset, OpenAI, "gpt-4o-mini", 1_000_000, 0, nil)
	require.True(t, ok, "an unknown mode is managed")
	assert.Equal(t, int64(150_000), usd)

	_, _, ok = LedgerCharge(ModeManaged, OpenAI, "some-model-we-have-never-priced", 1_000_000, 1_000_000, nil)
	assert.False(t, ok)

	rate := int64(2_000_000)
	cerberus := &CerberusMetadata{Routes: []WireRoute{{
		Provider: ProviderID(OpenAI), Model: "custom", InputPerMillionUsdMicros: &rate, OutputPerMillionUsdMicros: &rate,
	}}}
	assert.Equal(t, int64(4_000_000), CatalogCost(ProviderID(OpenAI), "custom", 1_000_000, 1_000_000, cerberus), "a route's own rates win")
}

type fakeMeters struct {
	mu      sync.Mutex
	usage   []string
	ledger  []string
	traces  []TraceTurn
	notices []FailoverNotice
	tenants []*uuid.UUID
}

func (f *fakeMeters) UsageMeter() UsageMeter { return fakeUsage{f} }
func (f *fakeMeters) CostLedger() CostLedger { return fakeLedger{f} }

type (
	fakeUsage  struct{ *fakeMeters }
	fakeLedger struct{ *fakeMeters }
)

func (f fakeUsage) Record(_ context.Context, tenant uuid.UUID, model string, in, out int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = append(f.usage, fmt.Sprintf("%s %s %d %d", tenant, model, in, out))
}

func (f fakeLedger) Record(_ context.Context, tenant uuid.UUID, provider ProviderID, usd int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ledger = append(f.ledger, fmt.Sprintf("%s %s %d", tenant, provider, usd))
}

func (f *fakeMeters) RecordTurn(turn TraceTurn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.traces = append(f.traces, turn)
}

type fakeTrace struct{ *fakeMeters }

func (f fakeTrace) Record(_ context.Context, turn TraceTurn) { f.RecordTurn(turn) }

type fakeFailover struct{ *fakeMeters }

func (f fakeFailover) Record(_ context.Context, notice FailoverNotice, tenant *uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notices = append(f.notices, notice)
	f.tenants = append(f.tenants, tenant)
}

func TestTransportMeters(t *testing.T) {
	const usageBody = `{"usage":{"prompt_tokens":1000000,"completion_tokens":1000000},"choices":[{"message":{"role":"assistant","content":"ok","tool_calls":[{"type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}`
	user := &User{ID: uuid.New()}
	wired := func(decision Decision, adapters ...Adapter) (*Transport, *fakeMeters) {
		f := &fakeMeters{}
		transport := transportFor(decision, adapters...)
		transport.UsageMeter, transport.CostLedger = f.UsageMeter(), f.CostLedger()
		transport.TraceRecorder, transport.FailoverLogger = fakeTrace{f}, fakeFailover{f}
		return transport, f
	}
	asUser := WithValues(context.Background(), func(v *Values) { v.CurrentUser = user })

	t.Run("a managed call meters usage, charges the ledger and traces the turn", func(t *testing.T) {
		transport, f := wired(Decision{Primary: Route{Anthropic, "claude-3-5-haiku-20241022"}},
			&scriptedAdapter{kind: Anthropic, outcomes: []outcome{reply(usageBody)}})
		_, err := transport.ChatCompletions(asUser, []byte(emptyPayload), "alice", "")
		require.NoError(t, err)
		transport.Wait()
		assert.Equal(t, []string{fmt.Sprintf("%s claude-3-5-haiku-20241022 1000000 1000000", user.ID)}, f.usage)
		assert.Equal(t, []string{fmt.Sprintf("%s anthropic 4800000", user.ID)}, f.ledger)
		require.Len(t, f.traces, 1)
		assert.Equal(t, []string{"lookup"}, f.traces[0].ToolNames)
		assert.Equal(t, int64(4_800_000), f.traces[0].EstimatedCostUsdMicros)
		assert.Equal(t, ModeUnset, f.traces[0].CredentialMode)
	})
	t.Run("BYOK meters tokens but not the ledger", func(t *testing.T) {
		transport, f := wired(Decision{Primary: Route{Anthropic, "claude-3-5-haiku-20241022"}, CredentialMode: ModeBYOK},
			&scriptedAdapter{kind: Anthropic, outcomes: []outcome{reply(usageBody)}})
		_, err := transport.ChatCompletions(asUser, []byte(emptyPayload), "alice", "")
		require.NoError(t, err)
		transport.Wait()
		assert.Len(t, f.usage, 1)
		assert.Empty(t, f.ledger)
		assert.Equal(t, ModeBYOK, f.traces[0].CredentialMode)
	})
	t.Run("the billing tenant takes the charge", func(t *testing.T) {
		owner := uuid.New()
		ctx := WithValues(asUser, func(v *Values) { v.BillingTenantID = &owner })
		transport, f := wired(Decision{Primary: Route{Anthropic, "claude-3-5-haiku-20241022"}},
			&scriptedAdapter{kind: Anthropic, outcomes: []outcome{reply(usageBody)}})
		_, err := transport.ChatCompletions(ctx, []byte(emptyPayload), "alice", "")
		require.NoError(t, err)
		transport.Wait()
		assert.Contains(t, f.usage[0], owner.String())
		assert.Contains(t, f.ledger[0], owner.String())
		assert.Equal(t, owner, f.traces[0].TenantID)
	})
	t.Run("internal calls with no user meter nothing", func(t *testing.T) {
		transport, f := wired(Decision{Primary: Route{Anthropic, "claude-3-5-haiku-20241022"}},
			&scriptedAdapter{kind: Anthropic, outcomes: []outcome{reply(usageBody)}})
		_, err := call(transport)
		require.NoError(t, err)
		transport.Wait()
		assert.Empty(t, f.usage)
		assert.Empty(t, f.ledger)
		assert.Empty(t, f.traces)
	})
	t.Run("the gateway has no wire id, so no trace", func(t *testing.T) {
		transport, f := wired(Decision{Primary: hermesRoute}, &scriptedAdapter{kind: HermesGateway, outcomes: []outcome{reply(usageBody)}})
		_, err := transport.ChatCompletions(asUser, []byte(emptyPayload), "alice", "")
		require.NoError(t, err)
		transport.Wait()
		assert.Len(t, f.usage, 1)
		assert.Empty(t, f.traces)
	})
	t.Run("headers win over the body", func(t *testing.T) {
		transport, f := wired(Decision{Primary: hermesRoute}, &scriptedAdapter{kind: HermesGateway, outcomes: []outcome{{
			data: usageBody, headers: map[string]string{"x-usage-mtok-in": "7", "x-luminavault-mtok-out": "9"},
		}}})
		_, err := transport.ChatCompletions(asUser, []byte(emptyPayload), "alice", "")
		require.NoError(t, err)
		transport.Wait()
		assert.Contains(t, f.usage[0], "hermes-3 7 9")
	})
	t.Run("a failover is logged with the tenant and counted on the trace", func(t *testing.T) {
		transport, f := wired(Decision{Primary: Route{XAI, "grok-4"}, Fallbacks: []Route{{OpenAI, "gpt-4o-mini"}}},
			&scriptedAdapter{kind: XAI, outcomes: []outcome{failing(CreditExhausted(XAI, 402, nil))}},
			&scriptedAdapter{kind: OpenAI, outcomes: []outcome{reply(usageBody)}})
		_, err := transport.ChatCompletions(asUser, []byte(emptyPayload), "alice", "")
		require.NoError(t, err)
		transport.Wait()
		require.Len(t, f.notices, 1)
		assert.Equal(t, FailoverHosted, f.notices[0].Source)
		assert.Equal(t, user.ID, *f.tenants[0])
		assert.Equal(t, 1, f.traces[0].FailoverCount)
	})
	t.Run("streams are not metered", func(t *testing.T) {
		transport, f := wired(Decision{Primary: Route{OpenAI, "gpt-4o-mini"}},
			&scriptedAdapter{kind: OpenAI, native: true, outcomes: []outcome{{chunks: []Chunk{{Delta: "ok"}}}}})
		_, err := collect(transport.ChatStream(asUser, []byte(emptyPayload), "alice", ""))
		require.NoError(t, err)
		transport.Wait()
		assert.Empty(t, f.usage)
		assert.Empty(t, f.ledger)
		assert.Empty(t, f.traces)
	})
}

type fakeTelemetry struct {
	mu       sync.Mutex
	complete []RouterExecutionResult
	released []int64
}

func (f *fakeTelemetry) Complete(_ context.Context, _ *CerberusMetadata, result RouterExecutionResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.complete = append(f.complete, result)
}

func (f *fakeTelemetry) Release(_ context.Context, _ uuid.UUID, reserved int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, reserved)
}

func TestRouterTelemetryHook(t *testing.T) {
	metadata := func() *CerberusMetadata {
		return &CerberusMetadata{ExecutionID: uuid.New(), Strategy: "sequential", BudgetReservationUsdMicros: 500, Mode: ModeManaged}
	}
	t.Run("success completes with usage estimated when the upstream reports none", func(t *testing.T) {
		telemetry := &fakeTelemetry{}
		var events []CerberusEvent
		ctx := WithValues(context.Background(), func(v *Values) { v.CerberusSink = func(e CerberusEvent) { events = append(events, e) } })
		transport := transportFor(Decision{Primary: Route{OpenAI, "gpt-4o-mini"}, Cerberus: metadata()},
			&scriptedAdapter{kind: OpenAI, outcomes: []outcome{reply("0123456789abcdef")}})
		transport.RouterTelemetry = telemetry
		_, err := transport.ChatCompletions(ctx, []byte(`{"messages":[{"role":"user","content":"12345678"}]}`), "alice", "")
		require.NoError(t, err)
		require.Len(t, telemetry.complete, 1)
		assert.True(t, telemetry.complete[0].UsageEstimated)
		assert.Equal(t, 2, telemetry.complete[0].TokensIn)
		assert.Equal(t, 4, telemetry.complete[0].TokensOut)
		require.Len(t, events, 2)
		assert.NotNil(t, events[0].Routing)
		assert.NotNil(t, events[1].Usage)
	})
	t.Run("exhaustion releases the reservation", func(t *testing.T) {
		telemetry := &fakeTelemetry{}
		transport := transportFor(Decision{Primary: Route{OpenAI, "m"}, Cerberus: metadata()},
			&scriptedAdapter{kind: OpenAI, outcomes: []outcome{failing(Transient(OpenAI, 500, nil))}})
		transport.RouterTelemetry = telemetry
		_, err := call(transport)
		require.Error(t, err)
		assert.Equal(t, []int64{500}, telemetry.released)
	})
	t.Run("a mid-stream failure does not release, as in Swift", func(t *testing.T) {
		telemetry := &fakeTelemetry{}
		transport := transportFor(Decision{Primary: Route{OpenAI, "m"}, Cerberus: metadata()},
			&scriptedAdapter{kind: OpenAI, native: true, outcomes: []outcome{{chunks: []Chunk{{Delta: "x"}}, streamErr: Transient(OpenAI, 500, nil)}}})
		transport.RouterTelemetry = telemetry
		_, err := collect(transport.ChatStream(context.Background(), []byte(emptyPayload), "alice", ""))
		require.Error(t, err)
		assert.Empty(t, telemetry.released)
	})
	t.Run("a failed ensemble under a parallel request is a quorum failure", func(t *testing.T) {
		telemetry := &fakeTelemetry{}
		m := metadata()
		m.Strategy = StrategyEnsemble
		adapter := &scriptedAdapter{kind: OpenAI, outcomes: []outcome{reply("WRONG")}}
		transport := transportFor(Decision{Primary: Route{OpenAI, "m"}, Cerberus: m}, adapter)
		transport.RouterTelemetry = telemetry
		ctx := WithValues(context.Background(), func(v *Values) { v.ParallelRequested = true })
		_, err := transport.ChatCompletions(ctx, []byte(emptyPayload), "alice", "")
		var response *UpstreamErrorResponse
		require.ErrorAs(t, err, &response)
		assert.Equal(t, "parallel_quorum_failed", response.Reason)
		assert.Zero(t, adapter.callCount())
		assert.Equal(t, []int64{500}, telemetry.released)
	})
}

func TestPayloadHelpers(t *testing.T) {
	assert.Equal(t, "last", extractLatestUserPrompt([]byte(`{"messages":[{"role":"user","content":"first"},{"role":"user","content":"last"},{"role":"assistant","content":"x"}]}`)))
	assert.Empty(t, extractLatestUserPrompt([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"x"}]}]}`)), "Swift's reversed().first stops at array content")
	garbage := []byte("not json")
	assert.Equal(t, garbage, rewriteModel("m", garbage))
	big, ok := decodeObject(rewriteModel("m", []byte(`{"model":"a","max_tokens":12345678901234567}`)))
	require.True(t, ok)
	assert.Equal(t, "12345678901234567", fmt.Sprint(big["max_tokens"]), "numbers survive the round trip")
}
