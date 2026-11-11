package llm

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/swiftstr"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterWriteTimeout bounds a detached meter write. Swift's are unbounded Tasks;
// a bound keeps a stuck database from piling up goroutines.
const meterWriteTimeout = 10 * time.Second

// freeLaneFailureMessage: free-lane turns run as managed, so a client must not
// learn which provider or model served them. ProviderError's message names the
// provider and echoes the upstream body.
const freeLaneFailureMessage = "Free messages are having trouble right now. Try again in a moment, or add your own API key in Settings."

// ChatTransport is HermesChatTransport: what services call. sessionKey is the
// tenant UUID string, sessionID the optional conversation id.
type ChatTransport interface {
	ChatCompletions(ctx context.Context, payload []byte, sessionKey, sessionID string) (Metadata, error)
	ChatStream(ctx context.Context, payload []byte, sessionKey, sessionID string) Stream
}

// Transport is RoutedLLMTransport: it asks the Router for a decision, walks the
// candidates, stops on the first success, fails over on recoverable errors and
// gives up on permanent ones. The route's model is rewritten into the payload.
//
// Everything below Router is optional, as in Swift. Production sets the meters.
// Writes run detached from the request and never fail it; Wait flushes them.
type Transport struct {
	Registry        *Registry
	Router          Router
	Capability      Capability // empty means CapabilityMedium
	UsageMeter      UsageMeter
	CostLedger      CostLedger
	TraceRecorder   TraceRecorder
	FailoverLogger  FailoverLogger
	RouterTelemetry RouterTelemetry
	Ensemble        EnsembleExecutor

	wg sync.WaitGroup
}

var _ ChatTransport = (*Transport)(nil)

// Wait blocks until the detached meter writes started so far have finished.
func (t *Transport) Wait() { t.wg.Wait() }

func (t *Transport) detach(ctx context.Context, write func(context.Context)) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), meterWriteTimeout)
		defer cancel()
		write(ctx)
	}()
}

func (t *Transport) capability() Capability {
	if t.Capability == "" {
		return CapabilityMedium
	}
	return t.Capability
}

type failedCandidate struct {
	route Route
	err   *ProviderError
}

// ChatCompletions dispatches a buffered completion.
func (t *Transport) ChatCompletions(ctx context.Context, payload []byte, sessionKey, sessionID string) (Metadata, error) {
	values := RoutingValues(ctx)
	user := values.CurrentUser
	prompt := extractLatestUserPrompt(payload)
	decision := t.pickDecision(ctx, payload, user)
	if c := decision.Cerberus; c != nil {
		if err := c.PreflightError(); err != nil {
			return Metadata{}, err
		}
		publishSelectedRoute(values, decision)
	}
	started := time.Now()
	fallbackCount := 0

	if c := decision.Cerberus; c != nil && c.Strategy == StrategyEnsemble {
		if ensemble := t.runEnsemble(ctx, payload, sessionKey, sessionID, c, prompt); ensemble != nil {
			t.completeEnsemble(ctx, values, c, ensemble)
			publishRouteOutcome(values, ensemble.Route, c)
			return ensemble.Metadata, nil
		}
		if values.ParallelStrategy != "" {
			t.release(ctx, c)
			return Metadata{}, &UpstreamErrorResponse{Reason: "parallel_quorum_failed", Message: "Not enough models completed to synthesize this answer."}
		}
	}

	var lastFailed *failedCandidate
	sawUnclassified := false
	for _, candidate := range decision.Candidates() {
		adapter, ok := t.Registry.Adapter(candidate.Provider)
		if !ok {
			slog.WarnContext(ctx, "router decision had unregistered provider", "provider", candidate.Provider)
			continue
		}
		mode := dispatchMode(decision, values)
		dispatchCtx := WithValues(ctx, func(v *Values) { v.CredentialMode = mode })
		metadata, err := adapter.ChatCompletions(dispatchCtx, rewriteModel(candidate.ModelID, payload), sessionKey, sessionID)
		if err == nil {
			if lastFailed != nil {
				fallbackCount++
				t.publishFailover(ctx, values, *lastFailed, candidate)
			}
			t.recordCompletion(ctx, values, decision, candidate, metadata, mode, fallbackCount, prompt, started)
			publishRouteOutcome(values, candidate, decision.Cerberus)
			return metadata, nil
		}

		var perr *ProviderError
		switch {
		case ctx.Err() != nil:
			// The caller went away: nobody reads the answer, so spend nothing more.
			t.release(ctx, decision.Cerberus)
			return Metadata{}, ctx.Err()
		case errors.As(err, &perr) && ShouldAdvance(perr, isFreeLane(decision)):
			lastFailed = &failedCandidate{candidate, perr}
			slog.WarnContext(ctx, "provider failed", "provider", candidate.Provider, "reason", perr.ReasonCode(), "error", perr)
		case perr != nil:
			slog.ErrorContext(ctx, "provider permanent failure", "provider", candidate.Provider, "error", perr)
			recordUpstreamError(ctx, perr.ReasonCode(), string(candidate.Provider))
			t.release(ctx, decision.Cerberus)
			return Metadata{}, upstreamResponse(perr, decision)
		case isByokRequired(err):
			// Fail closed: every remaining candidate lacks the same credential, and
			// advancing would turn a precise 403 into a generic upstream error.
			t.release(ctx, decision.Cerberus)
			return Metadata{}, err
		default:
			sawUnclassified = true
			slog.WarnContext(ctx, "provider unclassified error", "provider", candidate.Provider, "error", err)
		}
	}
	slog.ErrorContext(ctx, "all providers exhausted", "candidates", decision.Candidates())
	t.release(ctx, decision.Cerberus)
	return Metadata{}, exhausted(ctx, decision, lastFailed, sawUnclassified)
}

// ChatStream is the streaming counterpart. The failover window closes at the
// first yielded chunk: before it, recoverable errors advance to the next candidate
// exactly like the buffered path; after it, errors end the stream. Usage metering
// is skipped: streamed responses do not reliably carry usage across providers.
func (t *Transport) ChatStream(ctx context.Context, payload []byte, sessionKey, sessionID string) Stream {
	return t.routedStream(ctx, payload, sessionKey, sessionID, nil)
}

// ChatStreamDecided streams a decision the caller already made. Picking is not
// free of side effects: a free-lane pick claims a daily grant, so a caller that
// had to pick first hands its decision over rather than pick and charge twice.
func (t *Transport) ChatStreamDecided(ctx context.Context, payload []byte, sessionKey, sessionID string, decision Decision) Stream {
	return t.routedStream(ctx, payload, sessionKey, sessionID, &decision)
}

func (t *Transport) routedStream(ctx context.Context, payload []byte, sessionKey, sessionID string, decided *Decision) Stream {
	return func(yield func(Chunk, error) bool) {
		values := RoutingValues(ctx)
		user := values.CurrentUser
		prompt := extractLatestUserPrompt(payload)
		var decision Decision
		if decided != nil {
			decision = ApplyingForcedRoute(*decided, values.ForcedRoute)
		} else {
			decision = t.pickDecision(ctx, payload, user)
		}
		fail := func(err error) { yield(Chunk{}, err) }
		if c := decision.Cerberus; c != nil {
			if err := c.PreflightError(); err != nil {
				fail(err)
				return
			}
			publishSelectedRoute(values, decision)
		}
		started := time.Now()
		outputCharacters := 0
		fallbackCount := 0

		if c := decision.Cerberus; c != nil && c.Strategy == StrategyEnsemble {
			if ensemble := t.runEnsemble(ctx, payload, sessionKey, sessionID, c, prompt); ensemble != nil {
				if !yield(Chunk{Delta: ExtractContent(ensemble.Metadata.Data), FinishReason: "stop"}, nil) {
					return
				}
				t.completeEnsemble(ctx, values, c, ensemble)
				publishRouteOutcome(values, ensemble.Route, c)
				return
			}
			if values.ParallelStrategy != "" {
				t.release(ctx, c)
				fail(&UpstreamErrorResponse{Reason: "parallel_quorum_failed", Message: "Not enough models completed to synthesize this answer."})
				return
			}
		}

		var lastFailed *failedCandidate
		sawUnclassified := false
		for _, candidate := range decision.Candidates() {
			adapter, ok := t.Registry.Adapter(candidate.Provider)
			if !ok {
				slog.WarnContext(ctx, "router decision had unregistered provider", "provider", candidate.Provider)
				continue
			}
			mode := dispatchMode(decision, values)
			dispatchCtx := WithValues(ctx, func(v *Values) { v.CredentialMode = mode })
			yieldedAny := false
			var streamErr error
			stopped := false
			for chunk, err := range adapter.ChatStream(dispatchCtx, rewriteModel(candidate.ModelID, payload), sessionKey, sessionID) {
				if err != nil {
					streamErr = err
					break
				}
				if !yieldedAny {
					yieldedAny = true
					if lastFailed != nil {
						fallbackCount++
						t.publishFailover(ctx, values, *lastFailed, candidate)
					}
				}
				outputCharacters += swiftstr.Count(chunk.Delta)
				if !yield(chunk, nil) {
					stopped = true
					break
				}
			}
			if stopped {
				return
			}
			if streamErr == nil {
				if yieldedAny {
					t.completeStream(ctx, values, decision, candidate, prompt, outputCharacters, fallbackCount, started)
					publishRouteOutcome(values, candidate, decision.Cerberus)
					return
				}
				// A 2xx stream with zero chunks is a transient failure.
				if ctx.Err() != nil {
					t.release(ctx, decision.Cerberus)
					fail(ctx.Err())
					return
				}
				lastFailed = &failedCandidate{candidate, Transient(candidate.Provider, 0, strPtr("empty stream"))}
				slog.WarnContext(ctx, "provider streamed no chunks; failing over", "provider", candidate.Provider)
				continue
			}

			var perr *ProviderError
			isProvider := errors.As(streamErr, &perr)
			switch {
			case ctx.Err() != nil:
				t.release(ctx, decision.Cerberus)
				fail(ctx.Err())
				return
			case isProvider && !yieldedAny && ShouldAdvance(perr, isFreeLane(decision)):
				lastFailed = &failedCandidate{candidate, perr}
				slog.WarnContext(ctx, "provider stream failed", "provider", candidate.Provider, "reason", perr.ReasonCode(), "error", perr)
			case isProvider && !yieldedAny:
				slog.ErrorContext(ctx, "provider stream permanent failure", "provider", candidate.Provider, "error", perr)
				recordUpstreamError(ctx, perr.ReasonCode(), string(candidate.Provider))
				t.release(ctx, decision.Cerberus)
				fail(upstreamResponse(perr, decision))
				return
			case isByokRequired(streamErr):
				t.release(ctx, decision.Cerberus)
				fail(streamErr)
				return
			case yieldedAny:
				// Partial output already reached the client: no failover, and Swift
				// surfaces the raw error here without releasing the budget.
				fail(streamErr)
				return
			default:
				sawUnclassified = true
				slog.WarnContext(ctx, "provider stream unclassified error", "provider", candidate.Provider, "error", streamErr)
			}
		}
		slog.ErrorContext(ctx, "all providers exhausted for stream", "candidates", decision.Candidates())
		t.release(ctx, decision.Cerberus)
		fail(exhausted(ctx, decision, lastFailed, sawUnclassified))
	}
}

// exhausted is the error once every candidate has been tried.
func exhausted(ctx context.Context, decision Decision, lastFailed *failedCandidate, sawUnclassified bool) error {
	switch {
	case lastFailed != nil:
		recordUpstreamError(ctx, lastFailed.err.ReasonCode(), string(lastFailed.route.Provider))
		return upstreamResponse(lastFailed.err, decision)
	case sawUnclassified:
		recordUpstreamError(ctx, "upstream_error", "unknown")
		return &UpstreamErrorResponse{Reason: "upstream_error", Message: "LLM upstream failed."}
	}
	recordUpstreamError(ctx, "no_providers", "n/a")
	return &UpstreamErrorResponse{Reason: "no_providers", Message: "No LLM provider available."}
}

func upstreamResponse(err *ProviderError, decision Decision) *UpstreamErrorResponse {
	message := err.UserMessage()
	if isFreeLane(decision) {
		message = freeLaneFailureMessage
	}
	response := &UpstreamErrorResponse{Reason: err.ReasonCode(), Message: message}
	if response.Reason == "upstream_timeout" {
		hint := TimeoutRetryHintMs
		response.RetryAfterMs = &hint
	}
	return response
}

func isFreeLane(d Decision) bool { return d.Cerberus != nil && d.Cerberus.IsFreeLane }

// ShouldAdvance is RoutedLLMTransport.shouldAdvance: recoverable errors always
// hand the request on. On the free lane a 404 does too, because OpenRouter answers
// a retired :free tier with 404 and the next rung can still serve the turn.
// Outside the lane a 404 keeps meaning our request is wrong.
func ShouldAdvance(err *ProviderError, freeLane bool) bool {
	if err.Kind == KindPermanent {
		return freeLane && err.Status == 404
	}
	return err.IsRecoverable()
}

func dispatchMode(decision Decision, values Values) CredentialMode {
	return effectiveMode(decision, values.CredentialMode)
}

// pickDecision picks, then overlays the forced route if the caller pinned a model.
func (t *Transport) pickDecision(ctx context.Context, payload []byte, user *User) Decision {
	model := extractModel(payload)
	prompt := extractLatestUserPrompt(payload)
	pickCtx := WithValues(ctx, func(v *Values) { v.CerberusPrompt = prompt })
	picked := t.Router.Pick(pickCtx, model, t.capability(), user)
	return ApplyingForcedRoute(picked, RoutingValues(ctx).ForcedRoute)
}

// ApplyingForcedRoute overlays a pinned model on a decision. A free-lane decision
// is dispatched exactly as decided: its mode is managed, so a forced route would
// spend the platform key on whatever that route names. The credential mode is
// kept, or "Ask another model" from a BYOK user would spend the platform key too.
func ApplyingForcedRoute(decision Decision, forced *WireRoute) Decision {
	if isFreeLane(decision) || forced == nil {
		return decision
	}
	provider, ok := FromShared(forced.Provider)
	if !ok {
		return decision
	}
	mode := decision.CredentialMode
	if mode == ModeUnset && decision.Cerberus != nil {
		mode = decision.Cerberus.Mode
	}
	return Decision{Primary: Route{provider, forced.Model}, Cerberus: decision.Cerberus, CredentialMode: mode}
}

// WireRoutes prefers the concrete candidates over Cerberus's advertised routes.
// The gateway has no ProviderID, so a pure-Hermes decision still publishes the
// advertisement rather than an empty list.
func WireRoutes(candidates []Route, advertised []WireRoute) []WireRoute {
	var actual []WireRoute
	for _, route := range candidates {
		if id, ok := route.Provider.ToShared(); ok {
			actual = append(actual, WireRoute{Provider: id, Model: route.ModelID})
		}
	}
	if len(actual) == 0 {
		return advertised
	}
	return actual
}

func publishSelectedRoute(values Values, decision Decision) {
	c := decision.Cerberus
	if c == nil || values.CerberusSink == nil {
		return
	}
	values.CerberusSink(CerberusEvent{Routing: &RoutingEvent{
		ExecutionID:  c.ExecutionID,
		ProfileID:    c.ProfileID,
		ProfileName:  c.ProfileName,
		TaskType:     c.TaskType,
		Strategy:     c.Strategy,
		ActiveRoutes: WireRoutes(decision.Candidates(), c.Routes),
	}})
}

func publishUsage(values Values, c *CerberusMetadata, result RouterExecutionResult) {
	if values.CerberusSink == nil {
		return
	}
	provider, _ := result.Provider.ToShared()
	values.CerberusSink(CerberusEvent{Usage: &UsageEvent{
		ExecutionID:            c.ExecutionID,
		Provider:               provider,
		Model:                  result.Model,
		TokensIn:               result.TokensIn,
		TokensOut:              result.TokensOut,
		EstimatedCostUsdMicros: result.EstimatedCostUsdMicros,
		LatencyMs:              result.LatencyMs,
		UsageEstimated:         result.UsageEstimated,
	}})
}

func publishRouteOutcome(values Values, route Route, c *CerberusMetadata) {
	if values.RouteOutcomeSink == nil {
		return
	}
	outcome := ModelProvenance{Provider: string(route.Provider), Model: route.ModelID}
	if c != nil {
		outcome.Reason, outcome.RoutingPolicy, outcome.Complexity, outcome.TaskType = c.Reason, c.RoutingPolicy, c.Complexity, c.TaskType
	}
	values.RouteOutcomeSink(outcome)
}

// publishFailover tells the SSE sink and the telemetry log about a transition.
func (t *Transport) publishFailover(ctx context.Context, values Values, failed failedCandidate, fallback Route) {
	notice := FailoverNotice{
		OriginalProvider: failed.route.Provider,
		OriginalModel:    failed.route.ModelID,
		FallbackProvider: fallback.Provider,
		FallbackModel:    fallback.ModelID,
		ReasonCode:       failed.err.ReasonCode(),
		UserMessage:      failed.err.UserMessage(),
		Source:           FailoverHosted,
	}
	if failed.err.Kind != KindNetwork {
		status := failed.err.Status
		notice.StatusCode = &status
		notice.BodyPreview = failed.err.Body
	}
	if values.CurrentResolution != nil && values.CurrentResolution.IsUserOverride {
		notice.Source = FailoverBYO
	}
	if values.FailoverSink != nil {
		values.FailoverSink(notice)
	}
	if t.FailoverLogger != nil {
		var tenant *uuid.UUID
		if values.CurrentUser != nil {
			id := values.CurrentUser.ID
			tenant = &id
		}
		t.detach(ctx, func(ctx context.Context) { t.FailoverLogger.Record(ctx, notice, tenant) })
	}
}

// recordCompletion writes the meters for a successful buffered call.
func (t *Transport) recordCompletion(ctx context.Context, values Values, decision Decision, candidate Route, metadata Metadata, mode CredentialMode, fallbackCount int, prompt string, started time.Time) {
	tokensIn, tokensOut := extractUsage(metadata)
	if user := values.CurrentUser; user != nil {
		billing := user.ID
		if values.BillingTenantID != nil {
			billing = *values.BillingTenantID
		}
		if (t.UsageMeter != nil || t.CostLedger != nil) && (tokensIn > 0 || tokensOut > 0) {
			if t.UsageMeter != nil {
				t.detach(ctx, func(ctx context.Context) { t.UsageMeter.Record(ctx, billing, candidate.ModelID, tokensIn, tokensOut) })
			}
			if t.CostLedger != nil {
				if provider, usd, ok := LedgerCharge(mode, candidate.Provider, candidate.ModelID, tokensIn, tokensOut, decision.Cerberus); ok {
					t.detach(ctx, func(ctx context.Context) { t.CostLedger.Record(ctx, billing, provider, usd) })
				}
			}
		}
		// Written whether or not the upstream reported usage: a turn with no token
		// counts still chose a model and may have called tools.
		if provider, ok := candidate.Provider.ToShared(); ok && t.TraceRecorder != nil {
			var toolNames []string
			if message, ok := AssistantMessage(metadata.Data); ok {
				toolNames = ToolCallNames(message)
			}
			turn := TraceTurn{
				TenantID:               billing,
				ConversationMessageID:  values.ConversationMessageID,
				Provider:               provider,
				Model:                  candidate.ModelID,
				ToolNames:              toolNames,
				FailoverCount:          fallbackCount,
				TokensIn:               tokensIn,
				TokensOut:              tokensOut,
				EstimatedCostUsdMicros: CatalogCost(provider, candidate.ModelID, tokensIn, tokensOut, decision.Cerberus),
				LatencyMs:              int(time.Since(started).Milliseconds()),
				CredentialMode:         mode,
			}
			t.detach(ctx, func(ctx context.Context) { t.TraceRecorder.Record(ctx, turn) })
		}
	}
	if c := decision.Cerberus; c != nil && t.RouterTelemetry != nil {
		estimated := tokensIn == 0 && tokensOut == 0
		if estimated {
			tokensIn = max(1, swiftstr.Count(prompt)/4)
			tokensOut = max(1, len(metadata.Data)/4)
		}
		result := RouterExecutionResult{
			Provider:               candidate.Provider,
			Model:                  candidate.ModelID,
			Status:                 "ok",
			TokensIn:               tokensIn,
			TokensOut:              tokensOut,
			EstimatedCostUsdMicros: estimatedCost(candidate, tokensIn, tokensOut, c),
			LatencyMs:              int(time.Since(started).Milliseconds()),
			UsageEstimated:         estimated,
			FallbackCount:          fallbackCount,
		}
		t.RouterTelemetry.Complete(ctx, c, result)
		publishUsage(values, c, result)
	}
}

func (t *Transport) completeStream(ctx context.Context, values Values, decision Decision, candidate Route, prompt string, outputCharacters, fallbackCount int, started time.Time) {
	c := decision.Cerberus
	if c == nil || t.RouterTelemetry == nil {
		return
	}
	tokensIn := max(1, swiftstr.Count(prompt)/4)
	tokensOut := max(1, outputCharacters/4)
	result := RouterExecutionResult{
		Provider:               candidate.Provider,
		Model:                  candidate.ModelID,
		Status:                 "ok",
		TokensIn:               tokensIn,
		TokensOut:              tokensOut,
		EstimatedCostUsdMicros: estimatedCost(candidate, tokensIn, tokensOut, c),
		LatencyMs:              int(time.Since(started).Milliseconds()),
		UsageEstimated:         true,
		FallbackCount:          fallbackCount,
	}
	t.RouterTelemetry.Complete(ctx, c, result)
	publishUsage(values, c, result)
}

func estimatedCost(route Route, tokensIn, tokensOut int, c *CerberusMetadata) int64 {
	id, ok := route.Provider.ToShared()
	if !ok {
		return 0
	}
	return CatalogCost(id, route.ModelID, tokensIn, tokensOut, c)
}

func (t *Transport) runEnsemble(ctx context.Context, payload []byte, sessionKey, sessionID string, c *CerberusMetadata, prompt string) *EnsembleResult {
	if t.Ensemble == nil {
		return nil
	}
	result, err := t.Ensemble.Execute(ctx, payload, sessionKey, sessionID, c, prompt)
	if err != nil {
		return nil // try? in Swift
	}
	return result
}

func (t *Transport) completeEnsemble(ctx context.Context, values Values, c *CerberusMetadata, ensemble *EnsembleResult) {
	if t.RouterTelemetry == nil {
		return
	}
	status := ensemble.Status
	if status == "completed" {
		status = "ok"
	}
	result := RouterExecutionResult{
		Provider:               ensemble.Route.Provider,
		Model:                  ensemble.Route.ModelID,
		Status:                 status,
		TokensIn:               ensemble.TokensIn,
		TokensOut:              ensemble.TokensOut,
		EstimatedCostUsdMicros: ensemble.EstimatedCostUsdMicros,
		LatencyMs:              ensemble.LatencyMs,
		UsageEstimated:         true,
	}
	t.RouterTelemetry.Complete(ctx, c, result)
	publishUsage(values, c, result)
}

func (t *Transport) release(ctx context.Context, c *CerberusMetadata) {
	if c != nil && t.RouterTelemetry != nil {
		t.RouterTelemetry.Release(context.WithoutCancel(ctx), c.TenantID, c.BudgetReservationUsdMicros)
	}
}

func extractModel(payload []byte) string {
	dict, ok := decodeObject(payload)
	if !ok {
		return ""
	}
	model, _ := dict["model"].(string)
	return model
}

// extractLatestUserPrompt: the last user message's content when it is a string.
// Swift's reversed().first stops at that message, so array content yields "".
func extractLatestUserPrompt(payload []byte) string {
	dict, ok := decodeObject(payload)
	if !ok {
		return ""
	}
	messages, ok := objects(dict["messages"])
	if !ok {
		return ""
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i]["role"] == "user" {
			content, _ := messages[i]["content"].(string)
			return content
		}
	}
	return ""
}

// rewriteModel sets the model the router picked, leaving a payload it cannot parse
// alone. Other values pass through unchanged, temperature included: the Anthropic
// adapter drops those, and every other upstream still takes them.
func rewriteModel(modelID string, payload []byte) []byte {
	dict, ok := decodeObject(payload)
	if !ok {
		return payload
	}
	dict["model"] = modelID
	out, err := encodeJSON(dict)
	if err != nil {
		return payload
	}
	return out
}

var (
	usageInHeaders  = []string{"x-mtok-in", "x-usage-mtok-in", "x-luminavault-mtok-in"}
	usageOutHeaders = []string{"x-mtok-out", "x-usage-mtok-out", "x-luminavault-mtok-out"}
)

// extractUsage reads usage headers, then the body's usage block if both are zero.
func extractUsage(metadata Metadata) (tokensIn, tokensOut int) {
	firstInt := func(names []string) int {
		for _, name := range names {
			if raw, ok := metadata.Headers[name]; ok {
				if v, err := strconv.Atoi(raw); err == nil {
					return v
				}
			}
		}
		return 0
	}
	tokensIn, tokensOut = firstInt(usageInHeaders), firstInt(usageOutHeaders)
	if tokensIn != 0 || tokensOut != 0 {
		return tokensIn, tokensOut
	}
	if body, ok := decodeObject(metadata.Data); ok {
		if usage, ok := body["usage"].(map[string]any); ok {
			tokensIn, _ = intOf(usage["prompt_tokens"])
			tokensOut, _ = intOf(usage["completion_tokens"])
		}
	}
	return tokensIn, tokensOut
}

// upstreamErrorMetric is UpstreamErrorTelemetry's counter label.
const upstreamErrorMetric = "luminavault.llm.chat.upstream_error"

var upstreamErrorCounter = newUpstreamErrorCounter(otel.GetMeterProvider())

// newUpstreamErrorCounter is the seam Swift's factory: parameter gives the tests.
func newUpstreamErrorCounter(provider metric.MeterProvider) metric.Int64Counter {
	counter, _ := provider.Meter("lumina/llm").Int64Counter(upstreamErrorMetric)
	return counter
}

// recordUpstreamError is UpstreamErrorTelemetry.record.
func recordUpstreamError(ctx context.Context, reasonCode, provider string) {
	recordUpstreamErrorOn(ctx, upstreamErrorCounter, reasonCode, provider)
}

func recordUpstreamErrorOn(ctx context.Context, counter metric.Int64Counter, reasonCode, provider string) {
	counter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("code", reasonCode),
		attribute.String("provider", provider),
	))
}
