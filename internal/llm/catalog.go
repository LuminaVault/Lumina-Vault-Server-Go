package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Managed defaults are ManagedLLMDefaults: what a managed tenant routes to.
const (
	ManagedProvider ProviderID = "openRouter"
	ManagedModel    string     = "deepseek/deepseek-v4-flash"
)

// allProviderIDs is ProviderID.allCases, the order the pool expands in. Swift
// walks a Set there, so its order varies per process; a fixed order only settles
// exact score ties.
var allProviderIDs = []ProviderID{"xai", "anthropic", "openai", "ollama", "openRouter", "nvidia", "gemini", "nous", "custom"}

// allProviderKinds is ProviderKind.allCases.
var allProviderKinds = []ProviderKind{
	HermesGateway, Anthropic, OpenAI, Gemini, Together, Groq, Fireworks, DeepInfra, DeepseekDirect,
	OpenRouter, Deepseek, Kimi, Ollama, XAI, Nvidia, Nous, Custom,
}

// RouterTaskType is what a prompt is asking for.
type RouterTaskType string

const (
	TaskGeneral       RouterTaskType = "general"
	TaskReasoning     RouterTaskType = "reasoning"
	TaskCoding        RouterTaskType = "coding"
	TaskSearch        RouterTaskType = "search"
	TaskSummarization RouterTaskType = "summarization"
	TaskExtraction    RouterTaskType = "extraction"
	TaskCreative      RouterTaskType = "creative"
	TaskAutomation    RouterTaskType = "automation"
)

// RouterSurface is where a routed call came from.
type RouterSurface string

const (
	SurfaceChat             RouterSurface = "chat"
	SurfaceQuery            RouterSurface = "query"
	SurfaceKnowledgeCompile RouterSurface = "knowledgeCompile"
	SurfaceJob              RouterSurface = "job"
	SurfaceWorkflow         RouterSurface = "workflow"
	SurfaceSkill            RouterSurface = "skill"
	SurfaceMemo             RouterSurface = "memo"
	SurfaceSystem           RouterSurface = "system"
)

// RoutingPolicy is LLMRoutingPolicy.
type RoutingPolicy string

const (
	PolicyLocked     RoutingPolicy = "locked"
	PolicyAutoSmart  RoutingPolicy = "autoSmart"
	PolicyFastCheap  RoutingPolicy = "fastCheap"
	PolicyBalanced   RoutingPolicy = "balanced"
	PolicyMaxQuality RoutingPolicy = "maxQuality"
)

// presetObjective is LLMRoutingPolicy.presetObjective: nil means the profile's
// stored weights apply.
func (p RoutingPolicy) presetObjective() *RouterObjective {
	switch p {
	case PolicyFastCheap:
		return &RouterObjective{Quality: 20, Cost: 50, Latency: 30}
	case PolicyBalanced:
		return &RouterObjective{Quality: 50, Cost: 25, Latency: 25}
	case PolicyMaxQuality:
		return &RouterObjective{Quality: 80, Cost: 10, Latency: 10}
	}
	return nil
}

// RouterComplexity is how hard a turn is. It sets the tier floor Auto may pick.
type RouterComplexity string

const (
	ComplexityLow    RouterComplexity = "low"
	ComplexityMedium RouterComplexity = "medium"
	ComplexityHigh   RouterComplexity = "high"
)

func (c RouterComplexity) rank() int {
	switch c {
	case ComplexityMedium:
		return 1
	case ComplexityHigh:
		return 2
	}
	return 0
}

func maxComplexity(a, b RouterComplexity) RouterComplexity {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// RouterModelTier is a model's capability band.
type RouterModelTier string

const (
	TierFast     RouterModelTier = "fast"
	TierBalanced RouterModelTier = "balanced"
	TierMax      RouterModelTier = "max"
)

func (t RouterModelTier) rank() int {
	switch t {
	case TierBalanced:
		return 1
	case TierMax:
		return 2
	}
	return 0
}

// MinimumTier is RouterModelTier.minimum(for:).
func MinimumTier(c RouterComplexity) RouterModelTier {
	switch c {
	case ComplexityMedium:
		return TierBalanced
	case ComplexityHigh:
		return TierMax
	}
	return TierFast
}

// RouterActionKind is sequential, or StrategyEnsemble for parallel execution.
type RouterActionKind string

const (
	ActionSequential RouterActionKind = "sequential"
	ActionEnsemble   RouterActionKind = StrategyEnsemble
)

// RouterRetryPolicy is RouterRetryPolicy.
type RouterRetryPolicy string

const (
	RetryFast      RouterRetryPolicy = "fast"
	RetryResilient RouterRetryPolicy = "resilient"
)

// ParallelStrategy is ParallelStrategyDTO. Empty means none was requested.
type ParallelStrategy string

const (
	ParallelAuto       ParallelStrategy = "auto"
	ParallelBestOfN    ParallelStrategy = "bestOfN"
	ParallelDebate     ParallelStrategy = "debate"
	ParallelConsensus  ParallelStrategy = "consensus"
	ParallelSpecialist ParallelStrategy = "specialist"
)

// decodeEnum is a synthesized Codable enum: an unknown value fails the decode,
// so a stored document Swift could not read is one Go will not route on either.
func decodeEnum[T ~string](data []byte, target *T, allowed ...T) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	for _, a := range allowed {
		if T(s) == a {
			*target = a
			return nil
		}
	}
	return fmt.Errorf("unknown value %q for %T", s, *target)
}

func (t *RouterTaskType) UnmarshalJSON(data []byte) error {
	return decodeEnum(data, t, TaskGeneral, TaskReasoning, TaskCoding, TaskSearch, TaskSummarization, TaskExtraction, TaskCreative, TaskAutomation)
}

func (s *RouterSurface) UnmarshalJSON(data []byte) error {
	return decodeEnum(data, s, SurfaceChat, SurfaceQuery, SurfaceKnowledgeCompile, SurfaceJob, SurfaceWorkflow, SurfaceSkill, SurfaceMemo, SurfaceSystem)
}

func (p *RoutingPolicy) UnmarshalJSON(data []byte) error {
	return decodeEnum(data, p, PolicyLocked, PolicyAutoSmart, PolicyFastCheap, PolicyBalanced, PolicyMaxQuality)
}

func (k *RouterActionKind) UnmarshalJSON(data []byte) error {
	return decodeEnum(data, k, ActionSequential, ActionEnsemble)
}

func (r *RouterRetryPolicy) UnmarshalJSON(data []byte) error {
	return decodeEnum(data, r, RetryFast, RetryResilient)
}

func (p *ParallelStrategy) UnmarshalJSON(data []byte) error {
	return decodeEnum(data, p, ParallelAuto, ParallelBestOfN, ParallelDebate, ParallelConsensus, ParallelSpecialist)
}

func (p *ProviderID) UnmarshalJSON(data []byte) error {
	return decodeEnum(data, p, allProviderIDs...)
}

// RouterCatalogEntry is RouterModelCatalogEntryDTO.
type RouterCatalogEntry struct {
	Provider                  ProviderID
	Model                     string
	DisplayName               string
	TaskQuality               map[RouterTaskType]int
	InputPerMillionUsdMicros  int64
	OutputPerMillionUsdMicros int64
	DefaultLatencyMs          int
	Capabilities              []string
	Tier                      RouterModelTier
}

func ratings(general, reasoning, coding, search, summarization int) map[RouterTaskType]int {
	return map[RouterTaskType]int{
		TaskGeneral:       general,
		TaskReasoning:     reasoning,
		TaskCoding:        coding,
		TaskSearch:        search,
		TaskSummarization: summarization,
		TaskExtraction:    summarization,
		TaskCreative:      general,
		TaskAutomation:    coding,
	}
}

// RouterCatalogEntries returns RouterModelCatalog.entries, for GET /v1/router/catalog.
func RouterCatalogEntries() []RouterCatalogEntry { return routerCatalogEntries }

// CatalogEntry is RouterModelCatalog.entry(provider:model:): routable models only.
func CatalogEntry(provider ProviderID, model string) (RouterCatalogEntry, bool) {
	for _, e := range routerCatalogEntries {
		if e.Provider == provider && e.Model == model {
			return e, true
		}
	}
	return RouterCatalogEntry{}, false
}

// catalogPricing is RouterModelCatalog.pricing(provider:model:), for meters: the
// routable catalogue, then the pricing-only rows.
func catalogPricing(provider ProviderID, model string) (RouterCatalogEntry, bool) {
	if e, ok := CatalogEntry(provider, model); ok {
		return e, true
	}
	for _, e := range routerPricingOnlyEntries {
		if e.Provider == provider && e.Model == model {
			return e, true
		}
	}
	return RouterCatalogEntry{}, false
}

// LLMModelInfo is LuminaVaultShared's LLMModelInfo.
type LLMModelInfo struct {
	ID            string
	DisplayName   string
	ContextWindow int
	Tier          RouterModelTier
}

// ModelsFor is LLMModelCatalog.models(for:).
func ModelsFor(provider ProviderID) []LLMModelInfo { return llmModelCatalog[provider] }

// modelCatalogTier is LLMModelCatalog.tier(for:model:).
func modelCatalogTier(provider ProviderID, model string) RouterModelTier {
	for _, m := range llmModelCatalog[provider] {
		if m.ID == model {
			return m.Tier
		}
	}
	return inferredTier(model)
}

// inferredTier is LLMModelCatalog.inferredTier(from:), order included: a model
// matching both a fast and a max marker is fast.
func inferredTier(model string) RouterModelTier {
	lower := strings.ToLower(model)
	for _, marker := range []string{"haiku", "flash", "mini", "lite", "8b", "deepseek-chat"} {
		if strings.Contains(lower, marker) {
			return TierFast
		}
	}
	for _, marker := range []string{"opus", "o3", "r1", "fable", "ultra", "gpt-5"} {
		if strings.Contains(lower, marker) {
			return TierMax
		}
	}
	return TierBalanced
}
