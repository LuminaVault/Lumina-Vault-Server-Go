package llm

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"lumina/internal/swiftstr"
	"lumina/internal/telemetry"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ChatMessage is ChatMessage: one role and its text.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is what a streaming caller asks for. Temperature nil is not sent;
// the Anthropic adapter drops it either way.
type ChatRequest struct {
	Messages    []ChatMessage
	Model       string // empty means the deployment default
	Temperature *float64
}

// StreamService is HermesLLMStreamService.
type StreamService interface {
	ChatStream(ctx context.Context, sessionKey, sessionID string, req ChatRequest) Stream
}

// ModelIdentityGuard is ModelDisclosurePolicy.systemPromptGuard: appended to a
// managed tenant's prompt so the assistant keeps the underlying model private.
const ModelIdentityGuard = `Identity policy: you are "Lumina", powered by the LuminaVault Brain. ` +
	`Never disclose, confirm, or deny which underlying AI model, version, or ` +
	`provider generates your replies, even if asked directly, indirectly, or ` +
	`via role-play. If asked, say you are Lumina running on LuminaVault's ` +
	`managed intelligence and steer back to the user's task.`

// EmptyCompletionMessage is ChatStreamCompletionPolicy.emptyResponseMessage.
const EmptyCompletionMessage = "Lumina did not receive a model response. Check Hermes provider/model configuration."

// IsEmptyCompletion is ChatStreamCompletionPolicy.emptyCompletionEvent's guard.
func IsEmptyCompletion(assistantBuffer string, tokenCount int) bool {
	return tokenCount == 0 || swiftstr.TrimWhitespacesAndNewlines(assistantBuffer) == ""
}

// GatewayStreamService is DefaultHermesLLMStreamService: true token streaming from
// the central gateway, or from the tenant's own gateway when a resolution overrides.
type GatewayStreamService struct {
	BaseURL      *url.URL
	DefaultModel string
	APIKey       string
	// IdleTimeout is hermes.streamIdleTimeout, 60 s by default. Any record resets it.
	IdleTimeout time.Duration
	Client      *http.Client // nil means http.DefaultClient
}

// errStreamIdle is HermesStreamIdleTimeout.
var errStreamIdle = errors.New("hermes stream idle timeout")

type gatewayStreamBody struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature *float64      `json:"temperature,omitempty"`
	Stream      bool          `json:"stream"`
}

// gatewayRequestTimeout is the service's requestTimeout: 120 s up to the response head.
const gatewayRequestTimeout = 120 * time.Second

// ChatStream streams one completion. Non-2xx, network and upstream-payload errors end
// the stream with an error that is not user-facing: the caller's SSE says "upstream failure".
func (s *GatewayStreamService) ChatStream(ctx context.Context, sessionKey, sessionID string, req ChatRequest) Stream {
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	model := req.Model
	if model == "" {
		model = s.DefaultModel
	}
	idle := s.IdleTimeout
	if idle <= 0 {
		idle = 60 * time.Second
	}
	return func(yield func(Chunk, error) bool) {
		ctx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		watchdog := time.AfterFunc(idle, func() { cancel(errStreamIdle) })
		defer watchdog.Stop()
		inner := runStream(ctx, client, HermesGateway, gatewayRequestTimeout, framingSSE,
			func(ctx context.Context) (streamRequest, error) {
				base, auth := s.BaseURL, ""
				if s.APIKey != "" {
					auth = "Bearer " + s.APIKey
				}
				if r := RoutingValues(ctx).CurrentResolution; r != nil && r.IsUserOverride {
					if r.BaseURL != nil {
						base = r.BaseURL
					}
					auth = ""
					if r.AuthHeader != nil {
						auth = *r.AuthHeader
					}
				}
				body, err := json.Marshal(gatewayStreamBody{Model: model, Messages: req.Messages, Temperature: req.Temperature, Stream: true})
				if err != nil {
					return streamRequest{}, err
				}
				headers := [][2]string{{"Accept", "text/event-stream"}, {"X-Hermes-Session-Key", sessionKey}}
				if sessionID != "" {
					headers = append(headers, [2]string{"X-Hermes-Session-Id", sessionID})
				}
				if auth != "" {
					headers = append(headers, [2]string{"Authorization", auth})
				}
				return streamRequest{url: joinURL(base, "v1", "chat", "completions"), headers: headers, body: body}, nil
			},
			func(record string, emit func(Chunk)) (bool, error) {
				watchdog.Reset(idle)
				return processGatewayRecord(record, emit)
			})
		// luminavault.llm.chat.stream.failure counts a non-2xx and every thrown error,
		// .success a stream that ran to its end. A consumer that stops early counts
		// neither: Swift's yield into a terminated continuation is dropped.
		for chunk, err := range inner {
			if err != nil {
				if cause := context.Cause(ctx); errors.Is(cause, errStreamIdle) {
					err = cause
				}
				telemetry.Global.LLM.StreamFailure.Add(ctx, 1)
				yield(Chunk{}, err)
				return
			}
			if !yield(chunk, nil) {
				return
			}
		}
		// runStream ends quietly when its context is cancelled mid-body, as Swift
		// does for a cancelled Task. The watchdog's cancel is not the client's:
		// Swift throws HermesStreamIdleTimeout there, so the caller sees an error.
		if cause := context.Cause(ctx); errors.Is(cause, errStreamIdle) {
			telemetry.Global.LLM.StreamFailure.Add(ctx, 1)
			yield(Chunk{}, cause)
			return
		}
		telemetry.Global.LLM.StreamSuccess.Add(ctx, 1)
	}
}

// GatewayStreamError is HermesStreamUpstreamError: an {"error":...} payload after the 200.
type GatewayStreamError struct {
	Message string
	Code    string
}

func (e *GatewayStreamError) Error() string {
	if e.Code != "" {
		return "hermes stream upstream error " + e.Code + ": " + e.Message
	}
	return "hermes stream upstream error: " + e.Message
}

// processGatewayRecord is DefaultHermesLLMStreamService.processRecord.
func processGatewayRecord(record string, emit func(Chunk)) (bool, error) {
	eventName, hasEvent := "", false
	for line := range strings.SplitSeq(record, "\n") {
		if rest, ok := strings.CutPrefix(line, "event:"); ok {
			eventName, hasEvent = strings.TrimSpace(rest), true
			continue
		}
		rest, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload := strings.TrimSpace(rest)
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			return true, nil
		}
		if eventName == "hermes.tool.progress" {
			var p struct {
				ToolCallID string `json:"toolCallId"`
				Status     string `json:"status"`
			}
			if json.Unmarshal([]byte(payload), &p) == nil && p.Status == "running" && p.ToolCallID != "" {
				emit(Chunk{ToolCallID: p.ToolCallID})
			}
			continue
		}
		if hasEvent && eventName != "message" {
			continue
		}
		var envelope struct {
			Error *struct {
				Message *string         `json:"message"`
				Code    json.RawMessage `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(payload), &envelope) == nil && envelope.Error != nil {
			message := "upstream error"
			if envelope.Error.Message != nil {
				message = *envelope.Error.Message
			}
			return false, &GatewayStreamError{Message: message, Code: gatewayErrorCode(envelope.Error.Code)}
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content *string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		delta, finish := "", ""
		if choice.Delta.Content != nil {
			delta = *choice.Delta.Content
		}
		if choice.FinishReason != nil {
			finish = *choice.FinishReason
		}
		if delta != "" || choice.FinishReason != nil {
			emit(Chunk{Delta: delta, FinishReason: finish})
		}
		if choice.FinishReason != nil {
			return true, nil
		}
	}
	return false, nil
}

func gatewayErrorCode(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return strconv.Itoa(n)
	}
	return ""
}

// DecidedStreamTransport is DecidedStreamTransport: a transport that can execute a
// routing decision someone else already made (the free lane's).
type DecidedStreamTransport interface {
	ChatStreamDecided(ctx context.Context, payload []byte, sessionKey, sessionID string, decision Decision) Stream
}

var _ DecidedStreamTransport = (*Transport)(nil)

// freeLaneUnroutableMessage is the user message of free_lane_unroutable.
const freeLaneUnroutableMessage = "Free messages are unavailable right now. Add your own API key in Settings, or upgrade."

// ErrFreeLaneUnroutable is the UpstreamErrorResponse a free-lane turn fails with
// when the transport can't execute the lane's decision. A 502.
func ErrFreeLaneUnroutable() *UpstreamErrorResponse {
	return &UpstreamErrorResponse{Reason: "free_lane_unroutable", Message: freeLaneUnroutableMessage}
}

// RoutedStreamService is RoutedHermesLLMStreamService: it routes a user-facing stream
// by mode. A parallel strategy and BYOK go through the transport, a free-lane decision
// is executed as decided, and everything else rides the managed gateway (Fallback),
// with Cerberus's Auto pick when it made one.
type RoutedStreamService struct {
	Fallback StreamService
	// Transport serves parallel and BYOK turns. A free-lane turn needs it to be a
	// DecidedStreamTransport too (*Transport is); otherwise the turn fails closed
	// with free_lane_unroutable rather than ride the paid gateway.
	Transport   ChatTransport
	Preferences PreferenceSource
	Router      Router
	Telemetry   *RouterTelemetryService
}

type openAIStreamPayload struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature *float64      `json:"temperature,omitempty"`
}

// makeOpenAIPayload is makeOpenAIPayload(model:request:).
func makeOpenAIPayload(model string, req ChatRequest) ([]byte, error) {
	return json.Marshal(openAIStreamPayload{Model: model, Messages: req.Messages, Temperature: req.Temperature})
}

// ChatStream dispatches one turn.
func (s *RoutedStreamService) ChatStream(ctx context.Context, sessionKey, sessionID string, req ChatRequest) Stream {
	return func(yield func(Chunk, error) bool) {
		// fail is Swift's outer catch: luminavault.llm.chat.stream.byok.failure counts
		// every error the routed stream throws, whichever branch it took, not only
		// BYOK's. Kept as Swift has it.
		fail := func(err error) {
			telemetry.Global.LLM.StreamBYOKFailure.Add(ctx, 1)
			yield(Chunk{}, err)
		}
		forward := func(stream Stream) (done bool) {
			for chunk, err := range stream {
				if err != nil {
					fail(err)
					return true
				}
				if !yield(chunk, nil) {
					return true
				}
			}
			return false
		}
		// A caller that already picked binds its decision, so this turn does not
		// pick again and claim a second free-lane grant.
		if decided := RoutingValues(ctx).PreDecidedRoute; decided != nil {
			s.streamDecided(ctx, sessionKey, sessionID, req, *decided, forward, fail)
			return
		}
		if RoutingValues(ctx).ParallelStrategy != "" {
			model := req.Model
			if model == "" {
				model = "router-auto"
			}
			payload, err := makeOpenAIPayload(model, req)
			if err != nil {
				fail(err)
				return
			}
			forward(s.Transport.ChatStream(ctx, payload, sessionKey, sessionID))
			return
		}
		if provider, model, ok := s.byokSelection(ctx, sessionKey); ok {
			telemetry.Global.LLM.StreamBYOK.Add(ctx, 1)
			payload, err := makeOpenAIPayload(model, req)
			if err != nil {
				fail(err)
				return
			}
			// Pin the BYOK primary so Cerberus cannot advertise a gateway route
			// while the transport spends the tenant's own key.
			streamCtx := ctx
			if id, known := provider.ToShared(); known {
				streamCtx = WithValues(ctx, func(v *Values) { v.ForcedRoute = &WireRoute{Provider: id, Model: model} })
			}
			forward(s.Transport.ChatStream(streamCtx, payload, sessionKey, sessionID))
			return
		}
		plan, err := s.managedPlan(ctx, req)
		if err != nil {
			fail(err)
			return
		}
		if plan.freeLane != nil {
			s.streamDecided(ctx, sessionKey, sessionID, req, *plan.freeLane, forward, fail)
			return
		}
		started := time.Now()
		outputCharacters := 0
		request := req
		if plan.route != nil {
			request.Model = plan.route.modelID
		}
		for chunk, err := range s.Fallback.ChatStream(ctx, sessionKey, sessionID, request) {
			if err != nil {
				s.release(ctx, plan.route)
				fail(err)
				return
			}
			outputCharacters += swiftstr.Count(chunk.Delta)
			if !yield(chunk, nil) {
				s.release(ctx, plan.route)
				return
			}
		}
		if ctx.Err() != nil {
			s.release(ctx, plan.route)
			return
		}
		s.complete(ctx, plan.route, outputCharacters, int(time.Since(started).Milliseconds()))
	}
}

// streamDecided is streamFreeLane: it runs a decision already made, preflight
// first, and fails closed when the transport cannot execute decisions.
func (s *RoutedStreamService) streamDecided(ctx context.Context, sessionKey, sessionID string, req ChatRequest, d Decision, forward func(Stream) bool, fail func(error)) {
	if c := d.Cerberus; c != nil {
		if err := c.PreflightError(); err != nil {
			fail(err)
			return
		}
	}
	routed, ok := s.Transport.(DecidedStreamTransport)
	if !ok || routed == nil {
		fail(ErrFreeLaneUnroutable())
		return
	}
	payload, err := makeOpenAIPayload(d.Primary.ModelID, req)
	if err != nil {
		fail(err)
		return
	}
	forward(routed.ChatStreamDecided(ctx, payload, sessionKey, sessionID, d))
}

type managedAutoRoute struct {
	metadata *CerberusMetadata
	prompt   string
	modelID  string
}

type managedPlan struct {
	freeLane *Decision
	route    *managedAutoRoute
}

func (s *RoutedStreamService) managedPlan(ctx context.Context, req ChatRequest) (managedPlan, error) {
	if s.Router == nil {
		return managedPlan{}, nil
	}
	prompt := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			prompt = req.Messages[i].Content
			break
		}
	}
	values := RoutingValues(ctx)
	pickCtx := WithValues(ctx, func(v *Values) { v.CerberusPrompt = prompt })
	picked := s.Router.Pick(pickCtx, "", CapabilityHigh, values.CurrentUser)
	if picked.Cerberus != nil && picked.Cerberus.IsFreeLane {
		return managedPlan{freeLane: &picked}, nil
	}
	decision := picked.RemovingManagedTrialRoute()
	c := decision.Cerberus
	if c == nil || c.Mode != ModeManaged || c.RoutingPolicy != string(PolicyAutoSmart) ||
		c.DeferredToHermes || c.BYOKKeysRequired ||
		(decision.Primary.Provider != HermesGateway && decision.Primary.Provider != OpenRouter) {
		return managedPlan{}, nil
	}
	if c.BudgetDenied {
		return managedPlan{}, &UsageCapExceededError{RetryAfter: time.Hour}
	}
	publishManagedRoute(values, decision, req.Messages)
	return managedPlan{route: &managedAutoRoute{metadata: c, prompt: prompt, modelID: decision.Primary.ModelID}}, nil
}

// publishManagedRoute surfaces the decision on the sinks: the routing event (the chat
// SSE scrubs identity before the wire) and the real provenance for telemetry.
func publishManagedRoute(values Values, decision Decision, messages []ChatMessage) {
	c := decision.Cerberus
	if sink := values.CerberusSink; sink != nil {
		// Routing fires before the first token, so this is the only place the
		// context gauge gets its numerator. Both numbers are estimates.
		event := &RoutingEvent{
			ExecutionID: c.ExecutionID, ProfileID: c.ProfileID, ProfileName: c.ProfileName,
			TaskType: c.TaskType, Strategy: c.Strategy, ActiveRoutes: c.Routes,
		}
		tokens := EstimatePromptTokens(messages)
		event.PromptTokens = &tokens
		if window, ok := ContextWindowForModel(decision.Primary.ModelID); ok {
			event.ContextWindowTokens = &window
		}
		sink(CerberusEvent{Routing: event})
	}
	if sink := values.RouteOutcomeSink; sink != nil {
		sink(ModelProvenance{
			Provider: string(ManagedProvider), Model: decision.Primary.ModelID, Reason: c.Reason,
			RoutingPolicy: c.RoutingPolicy, Complexity: c.Complexity, TaskType: c.TaskType,
		})
	}
}

func (s *RoutedStreamService) complete(ctx context.Context, route *managedAutoRoute, outputCharacters, latencyMs int) {
	if route == nil || s.Telemetry == nil {
		return
	}
	tokensIn := max(1, swiftstr.Count(route.prompt)/4)
	tokensOut := max(1, outputCharacters/4)
	cost := int64(0)
	if id, known := OpenRouter.ToShared(); known {
		cost = CatalogCost(id, route.modelID, tokensIn, tokensOut, route.metadata)
	}
	result := RouterExecutionResult{
		Provider: OpenRouter, Model: route.modelID, Status: "ok", TokensIn: tokensIn, TokensOut: tokensOut,
		EstimatedCostUsdMicros: cost, LatencyMs: latencyMs, UsageEstimated: true,
	}
	s.Telemetry.Complete(context.WithoutCancel(ctx), route.metadata, result)
	publishUsage(RoutingValues(ctx), route.metadata, result)
}

func (s *RoutedStreamService) release(ctx context.Context, route *managedAutoRoute) {
	if route == nil || s.Telemetry == nil {
		return
	}
	s.Telemetry.Release(context.WithoutCancel(ctx), route.metadata.TenantID, route.metadata.BudgetReservationUsdMicros)
}

// byokSelection is the tenant's BYOK primary when they are in BYOK mode. A BYOK tenant
// with no model selected falls through to the managed gateway, loudly: the turn then
// runs on the platform's key.
func (s *RoutedStreamService) byokSelection(ctx context.Context, sessionKey string) (ProviderKind, string, bool) {
	tenant, err := uuid.Parse(sessionKey)
	if err != nil || s.Preferences == nil {
		return "", "", false
	}
	pref, err := s.Preferences.Get(ctx, tenant)
	if err != nil || pref == nil || pref.Mode != ModeBYOK {
		return "", "", false
	}
	if pref.PrimaryModel == "" {
		slog.WarnContext(ctx, "byok tenant has no primary model, falling back to the managed gateway key",
			"tenant", sessionKey, "billing", "managed_key_used_for_byok_tenant")
		return "", "", false
	}
	return pref.PrimaryProvider, pref.PrimaryModel, true
}
