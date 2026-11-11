package llm

import (
	"context"
	"lumina/internal/hermesresolve"

	"github.com/labstack/echo/v5"

	"github.com/google/uuid"
)

// User is the part of Swift's User that routing reads.
type User struct {
	ID                uuid.UUID
	Tier              string
	TierOverride      string
	PrivacyNoCNOrigin bool
}

// Resolution is what hermesresolve.Resolver returns for a tenant. Only an override
// changes where a gateway call goes.
type Resolution = hermesresolve.Resolution

// WireRoute is RouterModelRouteDTO: a route as clients and Cerberus name it.
// The rates are optional, and win over the catalogue when present.
type WireRoute struct {
	Provider                  ProviderID `json:"provider"`
	Model                     string     `json:"model"`
	InputPerMillionUsdMicros  *int64     `json:"inputPerMillionUsdMicros,omitempty"`
	OutputPerMillionUsdMicros *int64     `json:"outputPerMillionUsdMicros,omitempty"`
}

// ID is RouterModelRouteDTO.id: rates do not distinguish two routes.
func (r WireRoute) ID() string { return string(r.Provider) + ":" + r.Model }

// ParallelParticipant is ParallelParticipantDTO.
type ParallelParticipant struct {
	ID           uuid.UUID `json:"id"`
	Role         string    `json:"role"`
	Instructions *string   `json:"instructions,omitempty"`
	Route        WireRoute `json:"route"`
}

// ParallelRequest is the part of ParallelExecutionRequestDTO routing reads. A nil
// Participants means none were named; an empty one means none, as in Swift.
type ParallelRequest struct {
	Strategy        ParallelStrategy
	Participants    []ParallelParticipant
	SynthesisRoute  *WireRoute
	SynthesisPrompt *string
}

// CerberusScope is CerberusRequestScope: which call this is, for rule matching
// and profile bindings. Nil on Values means a system call.
type CerberusScope struct {
	Surface        RouterSurface
	SpaceID        *uuid.UUID
	JobID          string // empty means none
	WorkflowID     string // empty means none
	ConversationID *uuid.UUID
}

// ModelProvenance is ModelProvenanceDTO: which route answered.
type ModelProvenance struct {
	Provider      string
	Model         string
	Reason        string
	RoutingPolicy string
	Complexity    string
	TaskType      string
}

// CerberusEvent is what the transport publishes to Values.CerberusSink. One of
// Routing or Usage is set. Callers map it onto their SSE events.
type CerberusEvent struct {
	Routing *RoutingEvent
	Usage   *UsageEvent
}

// RoutingEvent is RouterRoutingEventDTO, phase "selected".
type RoutingEvent struct {
	ExecutionID  uuid.UUID
	ProfileID    uuid.UUID
	ProfileName  string
	TaskType     string
	Strategy     string
	ActiveRoutes []WireRoute
	// DisplayLabel is set only by ScrubCerberusEvent ("Auto · <task>").
	DisplayLabel *string
	// PromptTokens and ContextWindowTokens feed the client's context gauge. Only
	// the routed stream service's managed Auto branch sets them; nil means "could
	// not tell", and the client then shows no gauge.
	PromptTokens        *int
	ContextWindowTokens *int
}

// UsageEvent is RouterUsageDTO.
type UsageEvent struct {
	ExecutionID            uuid.UUID
	Provider               ProviderID // empty when the route has no wire id
	Model                  string
	TokensIn               int
	TokensOut              int
	EstimatedCostUsdMicros int64
	LatencyMs              int
	UsageEstimated         bool
}

// Values is LLMRoutingContext.Values: what a caller binds around a routed call.
type Values struct {
	CurrentUser           *User
	CurrentResolution     *Resolution
	ForcedRoute           *WireRoute
	CerberusScope         *CerberusScope
	CerberusPrompt        string
	ParallelStrategy      ParallelStrategy // empty means none requested
	ParallelRequest       *ParallelRequest
	AnalyticsVaultID      *uuid.UUID
	BillingTenantID       *uuid.UUID
	CredentialMode        CredentialMode
	ConversationMessageID *uuid.UUID
	RouteOutcomeSink      func(ModelProvenance)
	CerberusSink          func(CerberusEvent)
	FailoverSink          func(FailoverNotice)
}

type valuesKey struct{}

// RoutingValues returns the values bound to ctx, or the zero Values.
func RoutingValues(ctx context.Context) Values {
	v, _ := ctx.Value(valuesKey{}).(Values)
	return v
}

// WithValues binds a subset of the routing values. Values mutate leaves alone
// are inherited from the enclosing scope.
func WithValues(ctx context.Context, mutate func(*Values)) context.Context {
	next := RoutingValues(ctx)
	mutate(&next)
	return context.WithValue(ctx, valuesKey{}, next)
}

// RequestContext is the request's context with the BYO Hermes middleware's resolution
// bound, as Swift's middleware does around next: a call made with it reaches the
// tenant's own Hermes when they have one.
func RequestContext(c *echo.Context) context.Context {
	ctx := c.Request().Context()
	if res, ok := hermesresolve.FromContext(c); ok {
		return WithValues(ctx, func(v *Values) { v.CurrentResolution = &res })
	}
	return ctx
}
