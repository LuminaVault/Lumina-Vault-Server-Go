package llmrouter

import (
	"encoding/json"
	"lumina/internal/llm"
	"lumina/internal/wire"
	"reflect"
	"slices"
	"time"
)

// unmarshalEnum is a Swift String enum's init(from:): a non-string is a type
// mismatch against String, an unknown string is dataCorrupted naming the type.
func unmarshalEnum(b []byte, swiftType string, allowed ...string) (string, error) {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return "", &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	if !slices.Contains(allowed, s) {
		return "", &wire.CorruptedError{Message: "Cannot initialize " + swiftType + " from invalid String value " + s}
	}
	return s, nil
}

type providerID string

func (p *providerID) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "ProviderID", "xai", "anthropic", "openai", "ollama", "openRouter", "nvidia", "gemini", "nous", "custom")
	*p = providerID(s)
	return err
}

type brainMode string

func (m *brainMode) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "LLMBrainMode", "managed", "byok")
	*m = brainMode(s)
	return err
}

type actionKind string

func (k *actionKind) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "RouterActionKind", "sequential", "ensemble")
	*k = actionKind(s)
	return err
}

type retryPolicy string

func (r *retryPolicy) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "RouterRetryPolicy", "fast", "resilient")
	*r = retryPolicy(s)
	return err
}

type parallelStrategy string

func (p *parallelStrategy) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "ParallelStrategyDTO", "auto", "bestOfN", "debate", "consensus", "specialist")
	*p = parallelStrategy(s)
	return err
}

// taskTypes is RouterTaskType.allCases.
var taskTypes = []string{"general", "reasoning", "coding", "search", "summarization", "extraction", "creative", "automation"}

// surfaces is RouterSurface.allCases.
var surfaces = []string{"chat", "query", "knowledgeCompile", "job", "workflow", "skill", "memo", "system"}

type taskType string

func (t *taskType) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "RouterTaskType", taskTypes...)
	*t = taskType(s)
	return err
}

type surface string

func (t *surface) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "RouterSurface", surfaces...)
	*t = surface(s)
	return err
}

type routingPolicy string

func (p *routingPolicy) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "LLMRoutingPolicy", "locked", "autoSmart", "fastCheap", "balanced", "maxQuality")
	*p = routingPolicy(s)
	return err
}

// MARK: - Requests (synthesized Codable unless noted)

// routeReq is RouterModelRouteDTO.
type routeReq struct {
	Provider                  providerID  `json:"provider"`
	Model                     string      `json:"model"`
	InputPerMillionUsdMicros  *wire.Int64 `json:"inputPerMillionUsdMicros,omitempty"`
	OutputPerMillionUsdMicros *wire.Int64 `json:"outputPerMillionUsdMicros,omitempty"`
}

// participantReq is ParallelParticipantDTO.
type participantReq struct {
	ID           wire.UUID `json:"id"`
	Role         string    `json:"role"`
	Instructions *string   `json:"instructions,omitempty"`
	Route        routeReq  `json:"route"`
}

// actionReq is RouterActionDTO.
type actionReq struct {
	Kind                     actionKind        `json:"kind"`
	Routes                   []routeReq        `json:"routes"`
	SynthesisRoute           *routeReq         `json:"synthesisRoute,omitempty"`
	MinimumSuccessfulResults *int              `json:"minimumSuccessfulResults,omitempty"`
	RetryPolicy              retryPolicy       `json:"retryPolicy"`
	ParallelStrategy         *parallelStrategy `json:"parallelStrategy,omitempty"`
	Participants             *[]participantReq `json:"participants,omitempty"`
}

// ruleReq is RouterRuleDTO.
type ruleReq struct {
	ID        wire.UUID  `json:"id"`
	Name      string     `json:"name"`
	Enabled   bool       `json:"enabled"`
	Priority  int        `json:"priority"`
	TaskTypes []taskType `json:"taskTypes"`
	Surfaces  []surface  `json:"surfaces"`
	Action    actionReq  `json:"action"`
}

// objectiveReq is RouterObjectiveWeightsDTO.
type objectiveReq struct {
	Quality int `json:"quality"`
	Cost    int `json:"cost"`
	Latency int `json:"latency"`
}

// budgetReq is RouterBudgetPolicyDTO.
type budgetReq struct {
	SoftLimitUsdMicros *wire.Int64 `json:"softLimitUsdMicros,omitempty"`
	HardLimitUsdMicros *wire.Int64 `json:"hardLimitUsdMicros,omitempty"`
}

// writeReq is RouterProfileWriteRequest, whose init(from:) defaults the provider
// lists, rules and routing policy.
type writeReq struct {
	Name             string         `json:"name"`
	Mode             brainMode      `json:"mode"`
	Objective        objectiveReq   `json:"objective"`
	Budget           budgetReq      `json:"budget"`
	AllowedProviders []providerID   `json:"allowedProviders,omitempty"`
	BlockedProviders []providerID   `json:"blockedProviders,omitempty"`
	DefaultAction    actionReq      `json:"defaultAction"`
	Rules            []ruleReq      `json:"rules,omitempty"`
	RoutingPolicy    *routingPolicy `json:"routingPolicy,omitempty"`
	ExpectedRevision *int           `json:"expectedRevision,omitempty"`
}

// bindingPutReq is RouterBindingPutRequest.
type bindingPutReq struct {
	ProfileID wire.UUID `json:"profileID"`
}

// presetWriteReq is SynthesisPresetWriteRequest.
type presetWriteReq struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
}

// parallelReq is ParallelExecutionRequestDTO. Its synthesized init(from:) needs
// strategy: the memberwise default does not apply to decoding.
type parallelReq struct {
	Prompt            string           `json:"prompt"`
	Strategy          parallelStrategy `json:"strategy"`
	Participants      *[]participantReq `json:"participants,omitempty"`
	SynthesisRoute    *routeReq        `json:"synthesisRoute,omitempty"`
	SynthesisPrompt   *string          `json:"synthesisPrompt,omitempty"`
	SynthesisPresetID *wire.UUID       `json:"synthesisPresetID,omitempty"`
	SpaceID           *wire.UUID       `json:"spaceID,omitempty"`
}

// MARK: - Request to llm

func i64(p *wire.Int64) *int64 {
	if p == nil {
		return nil
	}
	v := int64(*p)
	return &v
}

func (r routeReq) wire() llm.WireRoute {
	return llm.WireRoute{
		Provider: llm.ProviderID(r.Provider), Model: r.Model,
		InputPerMillionUsdMicros: i64(r.InputPerMillionUsdMicros), OutputPerMillionUsdMicros: i64(r.OutputPerMillionUsdMicros),
	}
}

func (p participantReq) wire() llm.ParallelParticipant {
	return llm.ParallelParticipant{ID: p.ID.UUID, Role: p.Role, Instructions: p.Instructions, Route: p.Route.wire()}
}

func participants(ps *[]participantReq) []llm.ParallelParticipant {
	if ps == nil {
		return nil
	}
	out := make([]llm.ParallelParticipant, len(*ps))
	for i, p := range *ps {
		out[i] = p.wire()
	}
	return out
}

func (a actionReq) wire() llm.RouterAction {
	out := llm.RouterAction{
		Kind: llm.RouterActionKind(a.Kind), Routes: make([]llm.WireRoute, len(a.Routes)),
		MinimumSuccessfulResults: a.MinimumSuccessfulResults, RetryPolicy: llm.RouterRetryPolicy(a.RetryPolicy),
		Participants: participants(a.Participants),
	}
	if a.Participants != nil && out.Participants == nil {
		out.Participants = []llm.ParallelParticipant{}
	}
	for i, r := range a.Routes {
		out.Routes[i] = r.wire()
	}
	if a.SynthesisRoute != nil {
		s := a.SynthesisRoute.wire()
		out.SynthesisRoute = &s
	}
	if a.ParallelStrategy != nil {
		s := llm.ParallelStrategy(*a.ParallelStrategy)
		out.ParallelStrategy = &s
	}
	return out
}

func providers(ps []providerID) []llm.ProviderID {
	out := make([]llm.ProviderID, len(ps))
	for i, p := range ps {
		out[i] = llm.ProviderID(p)
	}
	return out
}

func (w *writeReq) write() llm.RouterProfileWrite {
	out := llm.RouterProfileWrite{
		Name: w.Name, Mode: llm.CredentialMode(w.Mode),
		Objective: llm.RouterObjective{Quality: w.Objective.Quality, Cost: w.Objective.Cost, Latency: w.Objective.Latency},
		Budget:    llm.RouterBudget{SoftLimitUsdMicros: i64(w.Budget.SoftLimitUsdMicros), HardLimitUsdMicros: i64(w.Budget.HardLimitUsdMicros)},
		AllowedProviders: providers(w.AllowedProviders), BlockedProviders: providers(w.BlockedProviders),
		DefaultAction: w.DefaultAction.wire(), Rules: make([]llm.RouterRule, len(w.Rules)),
		RoutingPolicy: llm.PolicyAutoSmart, ExpectedRevision: w.ExpectedRevision,
	}
	if w.RoutingPolicy != nil {
		out.RoutingPolicy = llm.RoutingPolicy(*w.RoutingPolicy)
	}
	for i, r := range w.Rules {
		rule := llm.RouterRule{
			ID: r.ID.UUID, Name: r.Name, Enabled: r.Enabled, Priority: r.Priority,
			TaskTypes: make([]llm.RouterTaskType, len(r.TaskTypes)), Surfaces: make([]llm.RouterSurface, len(r.Surfaces)),
			Action: r.Action.wire(),
		}
		for j, t := range r.TaskTypes {
			rule.TaskTypes[j] = llm.RouterTaskType(t)
		}
		for j, s := range r.Surfaces {
			rule.Surfaces[j] = llm.RouterSurface(s)
		}
		out.Rules[i] = rule
	}
	return out
}

// MARK: - Responses

// profileDTO is RouterProfileDTO. The synthesized encoder writes every
// non-optional field; createdAt and updatedAt are always set from the row.
type profileDTO struct {
	ID               wire.UUID          `json:"id"`
	Name             string             `json:"name"`
	Mode             string             `json:"mode"`
	IsPreset         bool               `json:"isPreset"`
	Objective        llm.RouterObjective `json:"objective"`
	Budget           llm.RouterBudget   `json:"budget"`
	AllowedProviders []llm.ProviderID   `json:"allowedProviders"`
	BlockedProviders []llm.ProviderID   `json:"blockedProviders"`
	DefaultAction    llm.RouterAction   `json:"defaultAction"`
	Rules            []llm.RouterRule   `json:"rules"`
	RoutingPolicy    llm.RoutingPolicy  `json:"routingPolicy"`
	Revision         int                `json:"revision"`
	CreatedAt        *wire.Time         `json:"createdAt,omitempty"`
	UpdatedAt        *wire.Time         `json:"updatedAt,omitempty"`
}

type profilesResponse struct {
	Profiles         []profileDTO `json:"profiles"`
	DefaultProfileID wire.UUID    `json:"defaultProfileID"`
}

// bindingDTO is RouterBindingDTO.
type bindingDTO struct {
	ID        wire.UUID `json:"id"`
	Scope     string    `json:"scope"`
	ScopeID   string    `json:"scopeID"`
	ProfileID wire.UUID `json:"profileID"`
}

type bindingsResponse struct {
	Bindings []bindingDTO `json:"bindings"`
}

// catalogEntryDTO is RouterModelCatalogEntryDTO.
type catalogEntryDTO struct {
	Provider                  llm.ProviderID `json:"provider"`
	Model                     string         `json:"model"`
	DisplayName               string         `json:"displayName"`
	TaskQuality               map[string]int `json:"taskQuality"`
	InputPerMillionUsdMicros  int64          `json:"inputPerMillionUsdMicros"`
	OutputPerMillionUsdMicros int64          `json:"outputPerMillionUsdMicros"`
	DefaultLatencyMs          int            `json:"defaultLatencyMs"`
	Capabilities              []string       `json:"capabilities"`
	Tier                      string         `json:"tier"`
}

// catalogResponse is RouterCatalogResponse.
type catalogResponse struct {
	Models                []catalogEntryDTO `json:"models"`
	TaskTypes             []string          `json:"taskTypes"`
	Surfaces              []string          `json:"surfaces"`
	CustomProfilesAllowed bool              `json:"customProfilesAllowed"`
	EnsemblesAllowed      bool              `json:"ensemblesAllowed"`
}

// dashboardResponse is RouterDashboardResponse.
type dashboardResponse struct {
	PeriodStart               wire.Time `json:"periodStart"`
	PeriodEnd                 wire.Time `json:"periodEnd"`
	Requests                  int64     `json:"requests"`
	SuccessfulRequests        int64     `json:"successfulRequests"`
	FallbackCount             int64     `json:"fallbackCount"`
	TokensIn                  int64     `json:"tokensIn"`
	TokensOut                 int64     `json:"tokensOut"`
	EstimatedCostUsdMicros    int64     `json:"estimatedCostUsdMicros"`
	AverageLatencyMs          int64     `json:"averageLatencyMs"`
	MonthlySoftLimitUsdMicros *int64    `json:"monthlySoftLimitUsdMicros,omitempty"`
	MonthlyHardLimitUsdMicros *int64    `json:"monthlyHardLimitUsdMicros,omitempty"`
}

// presetDTO is SynthesisPresetDTO.
type presetDTO struct {
	ID        wire.UUID `json:"id"`
	Name      string    `json:"name"`
	Prompt    string    `json:"prompt"`
	CreatedAt wire.Time `json:"createdAt"`
	UpdatedAt wire.Time `json:"updatedAt"`
}

type presetsResponse struct {
	Presets []presetDTO `json:"presets"`
}

// routeDTO is RouterModelRouteDTO on the way out.
type routeDTO struct {
	Provider llm.ProviderID `json:"provider"`
	Model    string         `json:"model"`
}

// executionSummaryDTO is ParallelExecutionSummaryDTO.
type executionSummaryDTO struct {
	ID                     wire.UUID `json:"id"`
	Strategy               string    `json:"strategy"`
	Status                 string    `json:"status"`
	PromptPreview          string    `json:"promptPreview"`
	ParticipantCount       int32     `json:"participantCount"`
	EstimatedCostUsdMicros int64     `json:"estimatedCostUsdMicros"`
	LatencyMs              int64     `json:"latencyMs"`
	CreatedAt              wire.Time `json:"createdAt"`
}

type executionsResponse struct {
	Executions []executionSummaryDTO `json:"executions"`
}

// outputDTO is ParallelOutputDTO.
type outputDTO struct {
	ID                     wire.UUID  `json:"id"`
	ParticipantID          *wire.UUID `json:"participantID,omitempty"`
	Role                   string     `json:"role"`
	Route                  routeDTO   `json:"route"`
	Stage                  string     `json:"stage"`
	Round                  int32      `json:"round"`
	Content                string     `json:"content"`
	Status                 string     `json:"status"`
	TokensIn               int64      `json:"tokensIn"`
	TokensOut              int64      `json:"tokensOut"`
	EstimatedCostUsdMicros int64      `json:"estimatedCostUsdMicros"`
	LatencyMs              int64      `json:"latencyMs"`
}

// executionDetailDTO is ParallelExecutionDetailDTO.
type executionDetailDTO struct {
	Summary           executionSummaryDTO `json:"summary"`
	Prompt            string              `json:"prompt"`
	Outputs           []outputDTO         `json:"outputs"`
	SynthesizedAnswer *string             `json:"synthesizedAnswer,omitempty"`
}

func wtime(t time.Time) *wire.Time { return &wire.Time{Time: t} }
