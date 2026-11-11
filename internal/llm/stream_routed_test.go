package llm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auditCFailingFallback is Swift's FailingManagedFallback: the paid managed
// gateway. Reaching it fails the turn.
type auditCFailingFallback struct{}

var errAuditCManagedFallback = errors.New("managed fallback should not be used")

func (auditCFailingFallback) ChatStream(context.Context, string, string, ChatRequest) Stream {
	return func(yield func(Chunk, error) bool) { yield(Chunk{}, errAuditCManagedFallback) }
}

// auditCPayloadModelRouter is Swift's FixedRouter in the streaming suite: openai,
// with the payload's model.
type auditCPayloadModelRouter struct{}

func (auditCPayloadModelRouter) Pick(_ context.Context, model string, _ Capability, _ *User) Decision {
	if model == "" {
		model = "gpt-test"
	}
	return Decision{Primary: Route{OpenAI, model}}
}

func auditCBYOKPreference() stubPreferences {
	return stubPreferences{pref: &Preference{Mode: ModeBYOK, PrimaryProvider: OpenAI, PrimaryModel: "gpt-stream"}}
}

func auditCFreeLaneDecision(exhausted bool) Decision {
	route := WireRoute{Provider: ProviderID(OpenRouter), Model: "free/lane-model:free"}
	routes, reason, retry := []WireRoute{route}, "Free lane: openRouterFree", 0
	if exhausted {
		routes, reason, retry = []WireRoute{}, "Free lane exhausted", 3600
	}
	return Decision{
		Primary: Route{OpenRouter, route.Model},
		Cerberus: &CerberusMetadata{
			ExecutionID: uuid.New(), ProfileName: "Free lane", Strategy: "sequential", Routes: routes,
			Mode: ModeManaged, RoutingPolicy: string(PolicyLocked), Reason: reason,
			IsFreeLane: true, FreeLaneExhausted: exhausted, FreeLaneRetryAfterSeconds: retry,
		},
		CredentialMode: ModeManaged,
	}
}

// Ports RoutedLLMTransportStreamingTests: the cases that drive the routed stream
// service (Swift's RoutedHermesLLMStreamService) over a RoutedLLMTransport.
func TestAuditCRoutedStreamService(t *testing.T) {
	tenant := uuid.New()
	request := ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "Hello"}}}
	run := func(ctx context.Context, service *RoutedStreamService) ([]Chunk, error) {
		return collect(service.ChatStream(ctx, tenant.String(), "conversation-1", request))
	}
	byokService := func(adapter Adapter) *RoutedStreamService {
		return &RoutedStreamService{
			Fallback:    auditCFailingFallback{},
			Transport:   &Transport{Registry: NewRegistry(nil, adapter), Router: auditCPayloadModelRouter{}},
			Preferences: auditCBYOKPreference(),
		}
	}
	payloadOf := func(t *testing.T, adapter *scriptedAdapter) map[string]any {
		t.Helper()
		require.NotEmpty(t, adapter.calls)
		dict, ok := decodeObject(adapter.calls[0])
		require.True(t, ok)
		return dict
	}

	t.Run("BYOK uses native provider streaming when selected adapter supports it", func(t *testing.T) {
		adapter := &scriptedAdapter{kind: OpenAI, native: true, outcomes: []outcome{{chunks: []Chunk{{Delta: "Hel"}, {Delta: "lo", FinishReason: "stop"}}}}}
		chunks, err := run(context.Background(), byokService(adapter))
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "Hel"}, {Delta: "lo", FinishReason: "stop"}}, chunks)
		assert.Equal(t, "gpt-stream", payloadOf(t, adapter)["model"])
	})
	t.Run("BYOK keeps one chunk fallback when selected adapter does not support streaming", func(t *testing.T) {
		adapter := &scriptedAdapter{kind: OpenAI, outcomes: []outcome{reply(`{"choices":[{"message":{"role":"assistant","content":"complete answer"}}]}`)}}
		chunks, err := run(context.Background(), byokService(adapter))
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "complete answer", FinishReason: "stop"}}, chunks)
		payload := payloadOf(t, adapter)
		assert.Equal(t, "gpt-stream", payload["model"])
		assert.NotContains(t, payload, "stream")
	})
	t.Run("managed Auto streaming fails closed when router budget is denied", func(t *testing.T) {
		route := WireRoute{Provider: ProviderID(OpenRouter), Model: "openrouter/auto"}
		denied := Decision{
			Primary: Route{HermesGateway, route.Model},
			Cerberus: &CerberusMetadata{
				ExecutionID: uuid.New(), TenantID: tenant, ProfileName: "Managed Auto", Strategy: "sequential",
				Routes: []WireRoute{route}, PredictedCostUsdMicros: 100, BudgetDenied: true, Mode: ModeManaged,
				RoutingPolicy: string(PolicyAutoSmart), Complexity: "medium", Reason: "hard budget exceeded",
			},
		}
		service := &RoutedStreamService{
			Fallback:  auditCFailingFallback{},
			Transport: &Transport{Registry: NewRegistry(nil), Router: fixedRouter{denied}},
			Router:    fixedRouter{denied},
		}
		_, err := run(context.Background(), service)
		var capped *UsageCapExceededError
		require.ErrorAs(t, err, &capped)
	})
	t.Run("BYOK stream pins the preference model when Cerberus advertised Hermes", func(t *testing.T) {
		adapter := &scriptedAdapter{kind: OpenAI, native: true, outcomes: []outcome{{chunks: []Chunk{{Delta: "Hel"}, {Delta: "lo", FinishReason: "stop"}}}}}
		service := &RoutedStreamService{
			Fallback:    auditCFailingFallback{},
			Transport:   &Transport{Registry: NewRegistry(nil, adapter), Router: fixedRouter{byoHermesDecision()}},
			Preferences: auditCBYOKPreference(),
		}
		var events []CerberusEvent
		ctx := WithValues(context.Background(), func(v *Values) {
			v.CerberusSink = func(e CerberusEvent) { events = append(events, e) }
			v.CurrentUser = &User{ID: tenant}
		})
		chunks, err := run(ctx, service)
		require.NoError(t, err)
		require.NotEmpty(t, chunks)
		assert.NotEmpty(t, chunks[0].Delta)
		var routing *RoutingEvent
		for _, e := range events {
			if e.Routing != nil {
				routing = e.Routing
				break
			}
		}
		require.NotNil(t, routing)
		assert.Equal(t, []WireRoute{{Provider: ProviderID(OpenAI), Model: "gpt-stream"}}, routing.ActiveRoutes)
		assert.Equal(t, "BYO Hermes", routing.ProfileName)
		assert.Equal(t, "gpt-stream", payloadOf(t, adapter)["model"])
	})
	t.Run("a granted free lane streams through the routed transport, never the paid gateway", func(t *testing.T) {
		lane := &scriptedAdapter{kind: OpenRouter, native: true, outcomes: []outcome{{chunks: []Chunk{{Delta: "free "}, {Delta: "reply", FinishReason: "stop"}}}}}
		decision := auditCFreeLaneDecision(false)
		service := &RoutedStreamService{
			Fallback:  auditCFailingFallback{},
			Transport: &Transport{Registry: NewRegistry(nil, lane), Router: fixedRouter{decision}},
			Router:    fixedRouter{decision},
		}
		chunks, err := run(context.Background(), service)
		require.NoError(t, err)
		text := ""
		for _, c := range chunks {
			text += c.Delta
		}
		assert.Equal(t, "free reply", text)
		assert.Equal(t, 1, lane.callCount())
	})
	t.Run("an exhausted free lane ends the stream instead of falling back to the gateway", func(t *testing.T) {
		decision := auditCFreeLaneDecision(true)
		service := &RoutedStreamService{
			Fallback:  auditCFailingFallback{},
			Transport: &Transport{Registry: NewRegistry(nil), Router: fixedRouter{decision}},
			Router:    fixedRouter{decision},
		}
		_, err := run(context.Background(), service)
		var exhausted *FreeLaneExhaustedError
		require.ErrorAs(t, err, &exhausted)
		assert.NotErrorIs(t, err, errAuditCManagedFallback)
	})
	t.Run("a forced route never overrides a free-lane decision", func(t *testing.T) {
		applied := ApplyingForcedRoute(auditCFreeLaneDecision(false), &WireRoute{Provider: ProviderID(Anthropic), Model: "claude-opus-4-7"})
		assert.Equal(t, Route{OpenRouter, "free/lane-model:free"}, applied.Primary)
	})
	t.Run("a BYOK tenant without keys is served by the lane, never their pinned model", func(t *testing.T) {
		pinned := &scriptedAdapter{kind: OpenAI, native: true}
		lane := &scriptedAdapter{kind: OpenRouter, native: true, outcomes: []outcome{{chunks: []Chunk{{Delta: "free "}, {Delta: "reply", FinishReason: "stop"}}}}}
		decision := auditCFreeLaneDecision(false)
		service := &RoutedStreamService{
			Fallback:    auditCFailingFallback{},
			Transport:   &Transport{Registry: NewRegistry(nil, pinned, lane), Router: fixedRouter{decision}},
			Preferences: auditCBYOKPreference(),
			Router:      fixedRouter{decision},
		}
		_, err := run(context.Background(), service)
		require.NoError(t, err)
		assert.Zero(t, pinned.callCount(), "the pinned model was dispatched on the platform key")
		assert.Equal(t, 1, lane.callCount())
	})
}

// Ports FreeLaneModelUnavailableFailoverTests over the path Swift drives: a decided
// stream on the catalogue's free-lane routes, with OpenRouter's real 404 body.
func TestAuditCFreeLaneDecidedStream(t *testing.T) {
	unavailableForFree := []byte(`{"error":{"message":"nvidia/nemotron-3-ultra-550b-a55b:free: This model is unavailable for free","code":404}}`)
	notFound := func(p ProviderKind) outcome { return failing(Classify(p, 404, unavailableForFree)) }
	decision := func(freeLane bool) Decision {
		var routes []Route
		for _, r := range FreeLaneRoutes(FreeLaneOpenRouterModel, FreeLaneOpenRouterSecondaryModel, FreeLaneNvidiaModel, "") {
			kind, ok := FromShared(r.Provider)
			require.True(t, ok)
			routes = append(routes, Route{kind, r.Model})
		}
		tenant := uuid.New()
		return Decision{
			Primary:   routes[0],
			Fallbacks: routes[1:],
			Cerberus: &CerberusMetadata{
				ExecutionID: uuid.New(), TenantID: tenant, ProfileName: "Free lane", Strategy: "sequential",
				Mode: ModeManaged, RoutingPolicy: string(PolicyLocked), IsFreeLane: freeLane,
			},
		}
	}
	content := func(s string) outcome {
		return reply(`{"choices":[{"message":{"role":"assistant","content":"` + s + `"}}]}`)
	}
	stream := func(d Decision, adapters ...Adapter) (string, error) {
		transport := &Transport{Registry: NewRegistry(nil, adapters...), Router: panicRouter{}}
		payload := []byte(`{"model":"lumina","messages":[{"role":"user","content":"hi"}]}`)
		chunks, err := collect(transport.ChatStreamDecided(context.Background(), payload, "alice", "", d))
		text := ""
		for _, c := range chunks {
			text += c.Delta
		}
		return text, err
	}

	t.Run("the classifier still reads a 404 as permanent", func(t *testing.T) {
		err := Classify(OpenRouter, 404, unavailableForFree)
		assert.False(t, err.IsRecoverable())
		assert.False(t, ShouldAdvance(err, false))
		assert.True(t, ShouldAdvance(err, true))
	})
	t.Run("other permanent errors stop the free lane", func(t *testing.T) {
		assert.False(t, ShouldAdvance(Permanent(OpenRouter, 400, nil), true))
		assert.False(t, ShouldAdvance(Permanent(OpenRouter, 401, nil), true))
	})
	t.Run("a retired primary free slug falls through to the secondary", func(t *testing.T) {
		openRouter := &scriptedAdapter{kind: OpenRouter, outcomes: []outcome{notFound(OpenRouter), content("from super")}}
		nvidia := &scriptedAdapter{kind: Nvidia, outcomes: []outcome{content("from nim")}}
		text, err := stream(decision(true), openRouter, nvidia)
		require.NoError(t, err)
		assert.Equal(t, "from super", text)
		assert.Equal(t, []string{FreeLaneOpenRouterModel, FreeLaneOpenRouterSecondaryModel}, openRouter.models(t))
		assert.Zero(t, nvidia.callCount())
	})
	t.Run("both free slugs retired falls through to the NIM reserve", func(t *testing.T) {
		openRouter := &scriptedAdapter{kind: OpenRouter, outcomes: []outcome{notFound(OpenRouter), notFound(OpenRouter)}}
		nvidia := &scriptedAdapter{kind: Nvidia, outcomes: []outcome{content("from nim")}}
		text, err := stream(decision(true), openRouter, nvidia)
		require.NoError(t, err)
		assert.Equal(t, "from nim", text)
		assert.Equal(t, []string{FreeLaneNvidiaModel}, nvidia.models(t))
	})
	t.Run("outside the free lane a 404 still ends the chain", func(t *testing.T) {
		openRouter := &scriptedAdapter{kind: OpenRouter, outcomes: []outcome{notFound(OpenRouter), content("from super")}}
		nvidia := &scriptedAdapter{kind: Nvidia, outcomes: []outcome{content("from nim")}}
		_, err := stream(decision(false), openRouter, nvidia)
		var response *UpstreamErrorResponse
		require.ErrorAs(t, err, &response)
		assert.Equal(t, 1, openRouter.callCount())
	})
	t.Run("a fully failed free lane names no provider or model", func(t *testing.T) {
		openRouter := &scriptedAdapter{kind: OpenRouter, outcomes: []outcome{notFound(OpenRouter), notFound(OpenRouter)}}
		nvidia := &scriptedAdapter{kind: Nvidia, outcomes: []outcome{notFound(Nvidia)}}
		_, err := stream(decision(true), openRouter, nvidia)
		var response *UpstreamErrorResponse
		require.ErrorAs(t, err, &response)
		assert.Equal(t, freeLaneFailureMessage, response.UserMessage())
		lower := strings.ToLower(response.UserMessage())
		for _, leak := range []string{"openrouter", "nvidia", "nemotron", ":free"} {
			assert.NotContains(t, lower, leak, "free-lane error leaks %s", leak)
		}
	})
}
