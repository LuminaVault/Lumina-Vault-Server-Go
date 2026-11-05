package llm

import (
	"context"
	"lumina/internal/entitlement"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Capability is LLMCapabilityLevel: the tier a service surface asks for.
type Capability string

const (
	CapabilityLow    Capability = "low"
	CapabilityMedium Capability = "medium"
	CapabilityHigh   Capability = "high"
)

// Route is ModelRoute: a concrete pair the transport will try. The model is
// rewritten into the payload before dispatch.
type Route struct {
	Provider ProviderKind
	ModelID  string
}

// CerberusMetadata is CerberusDecisionMetadata, filled in by CerberusRouter. Nil
// on Decision means no Cerberus involvement.
type CerberusMetadata struct {
	ExecutionID                uuid.UUID
	TenantID                   uuid.UUID
	VaultID                    uuid.UUID
	ActorUserID                uuid.UUID
	ProfileID                  uuid.UUID
	ProfileName                string
	RuleID                     *uuid.UUID
	TaskType                   string
	Surface                    RouterSurface
	SpaceID                    *uuid.UUID
	ConversationID             *uuid.UUID
	Strategy                   string // "sequential" or StrategyEnsemble
	ParallelStrategy           ParallelStrategy
	Participants               []ParallelParticipant
	Routes                     []WireRoute
	SynthesisRoute             *WireRoute
	MinimumSuccessfulResults   int
	RetryPolicy                RouterRetryPolicy
	PredictedCostUsdMicros     int64
	BudgetReservationUsdMicros int64
	BudgetDenied               bool
	Mode                       CredentialMode
	RoutingPolicy              string
	Complexity                 string
	Reason                     string
	BYOKKeysRequired           bool
	DeferredToHermes           bool
	IsFreeLane                 bool
	FreeLaneUnavailable        bool
	FreeLaneActions            []string
	FreeLaneExhausted          bool
	FreeLaneRetryAfterSeconds  int
}

// StrategyEnsemble is RouterActionKind.ensemble.
const StrategyEnsemble = "ensemble"

// PreflightError is the error a decision must fail with before anything is
// dispatched, or nil when it may proceed.
func (m *CerberusMetadata) PreflightError() error {
	switch {
	case m.BYOKKeysRequired:
		return BYOKKeysRequiredError{}
	case m.FreeLaneUnavailable:
		return &FreeLaneUnavailableError{Actions: m.FreeLaneActions}
	case m.FreeLaneExhausted:
		return &FreeLaneExhaustedError{RetryAfterSeconds: m.FreeLaneRetryAfterSeconds, Actions: m.FreeLaneActions}
	case m.BudgetDenied:
		return &UsageCapExceededError{RetryAfter: time.Hour}
	}
	return nil
}

// Decision is RouteDecision: the preferred upstream and the ordered cascade the
// transport walks when one fails recoverably. CredentialMode says who pays;
// ModeUnset means nobody declared an intent. ManagedTrialPrepended marks a
// Primary put in front by TrialRouter, with Fallbacks the original decision.
type Decision struct {
	Primary               Route
	Fallbacks             []Route
	Cerberus              *CerberusMetadata
	CredentialMode        CredentialMode
	ManagedTrialPrepended bool
}

// Candidates is the primary followed by the fallbacks.
func (d Decision) Candidates() []Route { return append([]Route{d.Primary}, d.Fallbacks...) }

// Router is ModelRouter. model is the caller's hint, empty for none.
type Router interface {
	Pick(ctx context.Context, model string, capability Capability, user *User) Decision
}

// cnOriginSubstrings is ModelOriginRegistry: lowercased substrings of model ids
// whose weights originated in China, whatever host serves them.
var cnOriginSubstrings = []string{"deepseek", "qwen", "kimi", "moonshot", "yi-"}

// IsCNOrigin is ModelOriginRegistry.isCNOrigin.
func IsCNOrigin(modelID string) bool {
	lower := strings.ToLower(modelID)
	return slices.ContainsFunc(cnOriginSubstrings, func(s string) bool { return strings.Contains(lower, s) })
}

// TableRouter is TableModelRouter: routes from a static table keyed on tier and
// capability, honouring privacy_no_cn_origin, dropping providers without keys,
// and always appending the gateway so the chat path never has zero candidates.
type TableRouter struct {
	Registry           *Registry
	HermesDefaultModel string
}

func (r *TableRouter) Pick(_ context.Context, _ string, capability Capability, user *User) Decision {
	var enabled []Route
	for _, route := range tableRoutes(capability, isProRouting(user)) {
		if user != nil && user.PrivacyNoCNOrigin && IsCNOrigin(route.ModelID) {
			continue
		}
		if r.Registry.IsEnabled(route.Provider) {
			enabled = append(enabled, route)
		}
	}
	hermes := Route{Provider: HermesGateway, ModelID: r.HermesDefaultModel}
	if len(enabled) == 0 {
		return Decision{Primary: hermes}
	}
	return Decision{Primary: enabled[0], Fallbacks: append(enabled[1:], hermes)}
}

func isProRouting(user *User) bool {
	if user == nil {
		return false
	}
	switch entitlement.EffectiveTier(user.Tier, user.TierOverride) {
	case "pro", "ultimate":
		return true
	}
	return false
}

func tableRoutes(capability Capability, pro bool) []Route {
	if !pro {
		// Non-entitled users get the free lane at every capability level. This
		// path has no FreeLaneGate metering; neither leg is billable.
		return []Route{
			{OpenRouter, FreeLaneOpenRouterModel},
			{OpenRouter, FreeLaneOpenRouterSecondaryModel},
			{Nvidia, FreeLaneNvidiaModel},
		}
	}
	if capability == CapabilityHigh {
		return []Route{{Anthropic, "claude-sonnet-4-6"}, {Anthropic, "claude-opus-4-7"}, {OpenAI, "gpt-5"}}
	}
	return []Route{{Anthropic, "claude-sonnet-4-6"}, {Gemini, "gemini-2.5-pro"}}
}

// SingleGatewayRouter routes every call to the gateway.
type SingleGatewayRouter struct{ HermesDefaultModel string }

func (r SingleGatewayRouter) Pick(context.Context, string, Capability, *User) Decision {
	return Decision{Primary: Route{HermesGateway, r.HermesDefaultModel}}
}

// RoutingModelRouter is the model-hint router: gemini* to Gemini, everything
// else to the gateway.
type RoutingModelRouter struct {
	HermesDefaultModel string
	FallbackKinds      []ProviderKind
}

func (r RoutingModelRouter) Pick(_ context.Context, model string, _ Capability, _ *User) Decision {
	fallbacks := make([]Route, 0, len(r.FallbackKinds))
	for _, k := range r.FallbackKinds {
		fallbacks = append(fallbacks, Route{k, r.HermesDefaultModel})
	}
	if model == "" {
		return Decision{Primary: Route{HermesGateway, r.HermesDefaultModel}, Fallbacks: fallbacks}
	}
	if strings.HasPrefix(strings.ToLower(model), "gemini") {
		return Decision{
			Primary:   Route{Gemini, model},
			Fallbacks: append([]Route{{HermesGateway, r.HermesDefaultModel}}, fallbacks...),
		}
	}
	return Decision{Primary: Route{HermesGateway, model}, Fallbacks: fallbacks}
}
