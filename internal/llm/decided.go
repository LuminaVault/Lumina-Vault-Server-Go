package llm

import "context"

// DecidedCompletionTransport is DecidedCompletionTransport: the buffered
// counterpart of DecidedStreamTransport, for callers that run several
// completions (tool rounds) per question on one route decision.
type DecidedCompletionTransport interface {
	PickDecision(ctx context.Context, payload []byte) Decision
	ChatCompletionsDecided(ctx context.Context, payload []byte, sessionKey, sessionID string, decision Decision) (Metadata, error)
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
