package llm

import "context"

// DecidedCompletionTransport is DecidedCompletionTransport: the buffered
// counterpart of DecidedStreamTransport, for callers that run several
// completions (tool rounds) per question on one route decision.
type DecidedCompletionTransport interface {
	PickDecision(ctx context.Context, payload []byte) Decision
	ChatCompletionsDecided(ctx context.Context, payload []byte, sessionKey, sessionID string, decision Decision) (Metadata, error)
	// DispatchesToProviderEndpoint reports whether ChatCompletionsDecided
	// would send decision's primary to its provider's own endpoint, as opposed
	// to a base URL the user stored with their credential.
	DispatchesToProviderEndpoint(ctx context.Context, decision Decision) bool
}

var _ DecidedCompletionTransport = (*Transport)(nil)

// PickDecision is pickToolDecision: a route for a turn the caller completes
// itself, over several ChatCompletionsDecided rounds. Picking can claim a
// free-lane grant, so a caller does it once per question.
func (t *Transport) PickDecision(ctx context.Context, payload []byte) Decision {
	return t.pickDecision(ctx, payload, RoutingValues(ctx).CurrentUser)
}

// ChatCompletionsDecided is chatCompletions(decision:): a buffered completion on
// a decision the caller already made. It never picks and never takes the
// ensemble path: it walks the candidates only, so Removing really does keep a
// provider out. The reservation is settled on every call; see
// WithoutBudgetReservation.
func (t *Transport) ChatCompletionsDecided(ctx context.Context, payload []byte, sessionKey, sessionID string, decision Decision) (Metadata, error) {
	return t.completeDecided(ctx, payload, sessionKey, sessionID, ApplyingForcedRoute(decision, RoutingValues(ctx).ForcedRoute), false)
}

// providerEndpointChecker is an adapter that can say where a request under ctx
// would go without sending it.
type providerEndpointChecker interface {
	usesProviderEndpoint(ctx context.Context) bool
}

// DispatchesToProviderEndpoint resolves the primary exactly as
// ChatCompletionsDecided would dispatch it: the same forced route, the same
// credential mode, the adapter's own credential resolution. It answers false
// whenever it cannot tell, so a caller that must keep data on the provider's
// endpoint fails closed. Only the Anthropic adapter can answer true.
func (t *Transport) DispatchesToProviderEndpoint(ctx context.Context, decision Decision) bool {
	values := RoutingValues(ctx)
	decision = ApplyingForcedRoute(decision, values.ForcedRoute)
	adapter, ok := t.Registry.Adapter(decision.Primary.Provider)
	if !ok {
		return false
	}
	checker, ok := adapter.(providerEndpointChecker)
	if !ok {
		return false
	}
	mode := dispatchMode(decision, values)
	return checker.usesProviderEndpoint(WithValues(ctx, func(v *Values) { v.CredentialMode = mode }))
}

// Removing is RouteDecision.removing(_:): the same decision without kind's
// candidates, order kept, or false when none remain. The trial flag survives
// only while the route in front is still the one that was prepended.
func (d Decision) Removing(kind ProviderKind) (Decision, bool) {
	var kept []Route
	for _, route := range d.Candidates() {
		if route.Provider != kind {
			kept = append(kept, route)
		}
	}
	if len(kept) == 0 {
		return Decision{}, false
	}
	first := kept[0]
	return Decision{
		Primary:               first,
		Fallbacks:             kept[1:],
		Cerberus:              d.Cerberus,
		CredentialMode:        d.CredentialMode,
		ManagedTrialPrepended: d.ManagedTrialPrepended && first == d.Primary,
	}, true
}

// OnlyProvider pins a decision to one provider: the first of kind's candidates
// becomes the primary and there are no fallbacks, so a failing step fails
// instead of moving to another provider. False when kind has no candidate.
//
// It takes the first route, never "the best" one: after ApplyTrial that is the
// trial route, the only model the platform trial key may pay for
// (guardTrialKeyModel). The trial flag survives on the same terms as Removing.
func (d Decision) OnlyProvider(kind ProviderKind) (Decision, bool) {
	for _, route := range d.Candidates() {
		if route.Provider != kind {
			continue
		}
		return Decision{
			Primary:               route,
			Cerberus:              d.Cerberus,
			CredentialMode:        d.CredentialMode,
			ManagedTrialPrepended: d.ManagedTrialPrepended && route == d.Primary,
		}, true
	}
	return Decision{}, false
}

// WithoutBudgetReservation is RouteDecision.withoutBudgetReservation(): the
// same decision with the budget reservation zeroed. The transport settles a
// decision's reservation on every completion, against the tenant's one monthly
// row that also holds its other in-flight requests' reservations. A caller that
// runs several rounds on one decision passes the original on round 1 and this
// on every later round, so the reservation is subtracted once per question.
// Each round still records its own cost.
func (d Decision) WithoutBudgetReservation() Decision {
	if d.Cerberus != nil {
		c := *d.Cerberus
		c.BudgetReservationUsdMicros = 0
		d.Cerberus = &c
	}
	return d
}
