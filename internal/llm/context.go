package llm

import (
	"context"
	"lumina/internal/hermesresolve"

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
	Provider                  ProviderID
	Model                     string
	InputPerMillionUsdMicros  *int64
	OutputPerMillionUsdMicros *int64
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
	CerberusPrompt        string
	ParallelRequested     bool // LLMRoutingContext.parallelStrategy != nil
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
