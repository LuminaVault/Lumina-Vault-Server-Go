package chat

import (
	"encoding/json"
	"lumina/internal/llm"
	"lumina/internal/modules/memoryagent"
	"lumina/internal/wire"
	"reflect"
	"slices"
)

// The spec drifts from LuminaVaultShared in places (ConversationCommitResponse,
// optional keys); these DTOs are written by hand from LuminaVaultShared 5.32.0, which
// is what Swift serves.

// unmarshalEnum decodes a Swift String raw-value enum: an unknown value is
// Foundation's dataCorrupted.
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

// ProviderID is LuminaVaultShared's ProviderID on a request.
type ProviderID string

func (p *ProviderID) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "ProviderID", "xai", "anthropic", "openai", "ollama", "openRouter", "nvidia", "gemini", "nous", "custom")
	*p = ProviderID(s)
	return err
}

// RouteDTO is RouterModelRouteDTO.
type RouteDTO struct {
	Provider                  ProviderID `json:"provider"`
	Model                     string     `json:"model"`
	InputPerMillionUsdMicros  *int64     `json:"inputPerMillionUsdMicros,omitempty"`
	OutputPerMillionUsdMicros *int64     `json:"outputPerMillionUsdMicros,omitempty"`
}

// ConversationDTO is ConversationDTO.
type ConversationDTO struct {
	ID              wire.UUID   `json:"id"`
	Title           string      `json:"title"`
	SpaceID         *wire.UUID  `json:"spaceId,omitempty"`
	CreatedAt       wire.Time   `json:"createdAt"`
	UpdatedAt       wire.Time   `json:"updatedAt"`
	PinnedMemoryIDs []wire.UUID `json:"pinnedMemoryIDs"`
	RouteOverride   *RouteDTO   `json:"routeOverride,omitempty"`
}

// MessageDTO is ConversationMessageDTO.
type MessageDTO struct {
	ID                  wire.UUID   `json:"id"`
	ConversationID      wire.UUID   `json:"conversationId"`
	Role                string      `json:"role"`
	Content             string      `json:"content"`
	SourceMemoryIDs     []wire.UUID `json:"sourceMemoryIDs"`
	ParallelExecutionID *wire.UUID  `json:"parallelExecutionID,omitempty"`
	Provider            *string     `json:"provider,omitempty"`
	Model               *string     `json:"model,omitempty"`
	ToolCallCount       *int        `json:"toolCallCount,omitempty"`
	Origin              string      `json:"origin"`
	SourceLabel         *string     `json:"sourceLabel,omitempty"`
	CreatedAt           wire.Time   `json:"createdAt"`
}

// TraceDTO is AgentTurnTraceDTO.
type TraceDTO struct {
	ID                     wire.UUID  `json:"id"`
	ConversationMessageID  *wire.UUID `json:"conversationMessageId,omitempty"`
	Provider               string     `json:"provider"`
	Model                  string     `json:"model"`
	ToolNames              []string   `json:"toolNames"`
	ToolCallCount          int        `json:"toolCallCount"`
	FailoverCount          int        `json:"failoverCount"`
	TokensIn               int64      `json:"tokensIn"`
	TokensOut              int64      `json:"tokensOut"`
	EstimatedCostUsdMicros int64      `json:"estimatedCostUsdMicros"`
	LatencyMs              int64      `json:"latencyMs"`
	CredentialMode         *string    `json:"credentialMode,omitempty"`
	OccurredAt             wire.Time  `json:"occurredAt"`
}

type tracesResponse struct {
	Traces []TraceDTO `json:"traces"`
}

// createRequest is ConversationCreateRequest; pinnedMemoryIDs may be absent or null.
type createRequest struct {
	Title           *string      `json:"title,omitempty"`
	SpaceID         *wire.UUID   `json:"spaceId,omitempty"`
	PinnedMemoryIDs *[]wire.UUID `json:"pinnedMemoryIDs,omitempty"`
	RouteOverride   *RouteDTO    `json:"routeOverride,omitempty"`
}

type listResponse struct {
	Conversations []ConversationDTO `json:"conversations"`
}

type detailResponse struct {
	Conversation ConversationDTO `json:"conversation"`
	Messages     []MessageDTO    `json:"messages"`
}

// AttachmentKind is ChatAttachmentDTO.Kind.
type AttachmentKind string

func (k *AttachmentKind) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "Kind", "text", "vault_file", "link")
	*k = AttachmentKind(s)
	return err
}

// Attachment is ChatAttachmentDTO.
type Attachment struct {
	Kind      AttachmentKind `json:"kind"`
	Name      string         `json:"name"`
	Text      *string        `json:"text,omitempty"`
	VaultPath *string        `json:"vaultPath,omitempty"`
	URL       *string        `json:"url,omitempty"`
}

type agentMode string

func (m *agentMode) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "ChatAgentModeDTO", "off", "auto", "force")
	*m = agentMode(s)
	return err
}

type parallelStrategy string

func (p *parallelStrategy) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "ParallelStrategyDTO", "auto", "bestOfN", "debate", "consensus", "specialist")
	*p = parallelStrategy(s)
	return err
}

// multiModel is ChatMultiModelOptionsDTO. Swift's synthesized decoder requires both.
type multiModel struct {
	Enabled  bool             `json:"enabled"`
	Strategy parallelStrategy `json:"strategy"`
}

// streamRequest is MessageStreamRequest.
type streamRequest struct {
	Content     string        `json:"content"`
	MultiModel  *multiModel   `json:"multiModel,omitempty"`
	AgentMode   *agentMode    `json:"agentMode,omitempty"`
	Attachments *[]Attachment `json:"attachments,omitempty"`
	// QuickAssistant is QuickAssistantContextDTO, kept raw: it is decoded only
	// when Quick Assistant is on, so with it off any value is ignored, as
	// Swift production ignores a key it does not know.
	QuickAssistant json.RawMessage `json:"quickAssistant,omitempty"`
}

// HitDTO is QueryHitDTO, shared with /v1/query/stream.
type HitDTO = memoryagent.HitDTO

// StreamEvent is QueryStreamEvent on the wire.
type StreamEvent = memoryagent.StreamEvent

// hermesRunRef is ChatHermesRunRefDTO.
type hermesRunRef struct {
	RunID     wire.UUID `json:"runID"`
	SessionID *string   `json:"sessionID,omitempty"`
	AfterSeq  int       `json:"afterSeq"`
	StartedAt wire.Time `json:"startedAt"`
}

// linkSavedDTO is LinkSavedDTO.
type linkSavedDTO struct {
	URL             string    `json:"url"`
	VaultPath       string    `json:"vaultPath"`
	CapturedAt      wire.Time `json:"capturedAt"`
	FromUserMessage bool      `json:"fromUserMessage"`
}

// routeWire is RouterModelRouteDTO on a routing frame.
type routeWire = llm.WireRoute

// routingDTO is RouterRoutingEventDTO. droppedHistoryTurns is never set by Swift
// (no producer at fdbed9b0), so it is always absent.
type routingDTO struct {
	ExecutionID         wire.UUID   `json:"executionID"`
	Phase               string      `json:"phase"`
	ProfileID           wire.UUID   `json:"profileID"`
	ProfileName         string      `json:"profileName"`
	TaskType            string      `json:"taskType"`
	Strategy            string      `json:"strategy"`
	ActiveRoutes        []routeWire `json:"activeRoutes"`
	DisplayLabel        *string     `json:"displayLabel,omitempty"`
	PromptTokens        *int        `json:"promptTokens,omitempty"`
	ContextWindowTokens *int        `json:"contextWindowTokens,omitempty"`
}

// usageDTO is RouterUsageDTO. contextWindowTokens and toolCallCount have no producer
// in Swift at fdbed9b0, so they are always absent.
type usageDTO struct {
	ExecutionID            wire.UUID `json:"executionID"`
	Provider               *string   `json:"provider,omitempty"`
	Model                  *string   `json:"model,omitempty"`
	TokensIn               int       `json:"tokensIn"`
	TokensOut              int       `json:"tokensOut"`
	EstimatedCostUsdMicros int64     `json:"estimatedCostUsdMicros"`
	LatencyMs              int       `json:"latencyMs"`
	UsageEstimated         bool      `json:"usageEstimated"`
}

// fallbackDTO is ProviderFallbackNoticeDTO.
type fallbackDTO struct {
	OriginalProvider llm.ProviderID `json:"originalProvider"`
	OriginalModel    string         `json:"originalModel"`
	FallbackProvider llm.ProviderID `json:"fallbackProvider"`
	FallbackModel    string         `json:"fallbackModel"`
	ReasonCode       string         `json:"reasonCode"`
	UserMessage      string         `json:"userMessage"`
}

// MARK: - hybrid execution

// ChatMessageDTO is ChatMessage (tool_calls is never set on a prepared prompt).
type ChatMessageDTO struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type prepareRequest struct {
	Content string `json:"content"`
}

type localToolDTO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type prepareResponse struct {
	ExecutionID  wire.UUID        `json:"executionID"`
	Messages     []ChatMessageDTO `json:"messages"`
	Sources      []HitDTO         `json:"sources"`
	ExpiresAt    wire.Time        `json:"expiresAt"`
	AllowedTools []localToolDTO   `json:"allowedTools"`
}

type toolName string

func (n *toolName) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "LocalExecutionToolName", "memory_search")
	*n = toolName(s)
	return err
}

type toolRequest struct {
	Name  toolName `json:"name"`
	Query string   `json:"query"`
	Limit *int     `json:"limit,omitempty"`
}

type toolResponse struct {
	Name    string   `json:"name"`
	Content string   `json:"content"`
	Sources []HitDTO `json:"sources"`
}

type executionLocation string

func (l *executionLocation) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "ExecutionLocation", "on_device", "local_endpoint", "cloud")
	*l = executionLocation(s)
	return err
}

// usageReport is LocalExecutionUsageDTO; Swift's synthesized decoder requires all three.
type usageReport struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	LatencyMs    int `json:"latencyMs"`
}

type commitRequest struct {
	ExecutionID wire.UUID         `json:"executionID"`
	Content     string            `json:"content"`
	Location    executionLocation `json:"location"`
	Provider    string            `json:"provider"`
	Model       string            `json:"model"`
	Usage       *usageReport      `json:"usage,omitempty"`
}

type commitResponse struct {
	Message   MessageDTO `json:"message"`
	FollowUps []string   `json:"followUps"`
}

// MARK: - ChatExperienceController

type inboxItem struct {
	ID            wire.UUID  `json:"id"`
	Title         string     `json:"title"`
	Preview       string     `json:"preview"`
	MessageCount  int        `json:"messageCount"`
	LastMessageAt wire.Time  `json:"lastMessageAt"`
	WorkspaceID   *wire.UUID `json:"workspaceID,omitempty"`
	SourceLabel   *string    `json:"sourceLabel,omitempty"`
	Pinned        bool       `json:"pinned"`
	Archived      bool       `json:"archived"`
}

type inboxResponse struct {
	Items []inboxItem `json:"items"`
}

type chatPreferences struct {
	AutoExpandThinking bool `json:"autoExpandThinking"`
	SendOnReturn       bool `json:"sendOnReturn"`
}

type chatPreferencesResponse struct {
	Preferences chatPreferences `json:"preferences"`
}

type chatPreferencesPut struct {
	Preferences chatPreferences `json:"preferences"`
}

type hybridProfile string

func (p *hybridProfile) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "HybridExecutionProfile", "private", "balanced", "quality")
	*p = hybridProfile(s)
	return err
}

// hybridPreferences is HybridRoutingPreferencesDTO; all four keys are required.
type hybridPreferences struct {
	Profile                hybridProfile `json:"profile"`
	LocalFallbackEnabled   bool          `json:"localFallbackEnabled"`
	CloudFallbackEnabled   bool          `json:"cloudFallbackEnabled"`
	SyncLocalConversations bool          `json:"syncLocalConversations"`
}
