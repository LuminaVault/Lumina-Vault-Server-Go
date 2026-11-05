package memory

import (
	"encoding/json"
	"lumina/internal/memorystore"
	"lumina/internal/wire"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The spec is wrong or thin on several memory schemas (MemoryDTO has no geo or actor
// fields, MemoryLineageResponse describes another shape, the upsert body has no geo),
// and generated float64 fields can't print a Double as Foundation does. These DTOs are
// LuminaVaultShared 5.32.0's. The rest of the wire types come from models.gen.go. Nil
// optionals are omitted, as Swift's encoding does.

// kindValue is MemoryKindDTO on the way in.
type kindValue string

func (k *kindValue) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	if !validKind(s) {
		return &wire.CorruptedError{Message: "Cannot initialize MemoryKindDTO from invalid String value " + s}
	}
	*k = kindValue(s)
	return nil
}

func validKind(s string) bool {
	switch s {
	case memorystore.KindPreference, memorystore.KindDecision, memorystore.KindFact, memorystore.KindNote:
		return true
	}
	return false
}

// upsertRequest is MemoryUpsertRequest. Field order is Swift's decode order.
type upsertRequest struct {
	Content   string     `json:"content"`
	Lat       *float64   `json:"lat,omitempty"`
	Lng       *float64   `json:"lng,omitempty"`
	AccuracyM *float64   `json:"accuracyM,omitempty"`
	PlaceName *string    `json:"placeName,omitempty"`
	Kind      *kindValue `json:"kind,omitempty"`
}

type upsertResponse struct {
	MemoryID wire.UUID `json:"memoryId"`
	Content  string    `json:"content"`
	Summary  string    `json:"summary"`
}

// patchRequest is MemoryPatchRequest. SpaceID null and absent differ; patch reads that
// from the raw body, because the decoder treats both as nil.
type patchRequest struct {
	Content     *string    `json:"content,omitempty"`
	Tags        *[]string  `json:"tags,omitempty"`
	ReviewState *string    `json:"reviewState,omitempty"`
	Kind        *kindValue `json:"kind,omitempty"`
	SpaceID     *wire.UUID `json:"spaceId,omitempty"`
}

// memoryDTO is MemoryDTO.
type memoryDTO struct {
	ID              wire.UUID                   `json:"id"`
	Content         string                      `json:"content"`
	Tags            []string                    `json:"tags"`
	CreatedAt       *wire.Time                  `json:"createdAt,omitempty"`
	Lat             *wire.Double                `json:"lat,omitempty"`
	Lng             *wire.Double                `json:"lng,omitempty"`
	AccuracyM       *wire.Double                `json:"accuracyM,omitempty"`
	PlaceName       *string                     `json:"placeName,omitempty"`
	ReviewState     string                      `json:"reviewState"`
	Provenance      *MemoryProvenanceSummaryDTO `json:"provenance,omitempty"`
	CreatedByUserID *wire.UUID                  `json:"createdByUserId,omitempty"`
	UpdatedByUserID *wire.UUID                  `json:"updatedByUserId,omitempty"`
	Kind            *string                     `json:"kind,omitempty"`
	SpaceID         *wire.UUID                  `json:"spaceId,omitempty"`
}

type listResponse struct {
	Memories []memoryDTO `json:"memories"`
	Limit    int         `json:"limit"`
	Offset   int         `json:"offset"`
}

type lineageSource struct {
	VaultFileID wire.UUID  `json:"vaultFileId"`
	Path        string     `json:"path"`
	CreatedAt   *wire.Time `json:"createdAt,omitempty"`
}

type lineageResponse struct {
	MemoryID wire.UUID      `json:"memoryId"`
	Source   *lineageSource `json:"source,omitempty"`
	Trace    string         `json:"trace"`
}

type relatedItem struct {
	ID         wire.UUID   `json:"id"`
	Snippet    string      `json:"snippet"`
	Kind       *string     `json:"kind,omitempty"`
	CreatedAt  wire.Time   `json:"createdAt"`
	SpaceID    *wire.UUID  `json:"spaceId,omitempty"`
	Similarity wire.Double `json:"similarity"`
}

type relatedResponse struct {
	MemoryID wire.UUID     `json:"memoryId"`
	Items    []relatedItem `json:"items"`
}

type position3D struct {
	X wire.Double `json:"x"`
	Y wire.Double `json:"y"`
	Z wire.Double `json:"z"`
}

type graphNode struct {
	ID         wire.UUID                   `json:"id"`
	Title      string                      `json:"title"`
	Tags       []string                    `json:"tags"`
	CreatedAt  wire.Time                   `json:"createdAt"`
	Score      wire.Double                 `json:"score"`
	Kind       string                      `json:"kind"`
	SpaceID    *wire.UUID                  `json:"spaceID,omitempty"`
	Activity   *wire.Double                `json:"activity,omitempty"`
	Position   *position3D                 `json:"position,omitempty"`
	Provenance *MemoryProvenanceSummaryDTO `json:"provenance,omitempty"`
}

type graphEdge struct {
	From       wire.UUID    `json:"from"`
	To         wire.UUID    `json:"to"`
	Kind       string       `json:"kind"`
	Tag        *string      `json:"tag,omitempty"`
	Similarity *wire.Double `json:"similarity,omitempty"`
	Weight     wire.Double  `json:"weight"`
}

type graphResponse struct {
	Nodes       []graphNode `json:"nodes"`
	Edges       []graphEdge `json:"edges"`
	GeneratedAt wire.Time   `json:"generatedAt"`
}

func wireUUIDPtr(v *uuid.UUID) *wire.UUID {
	if v == nil {
		return nil
	}
	return &wire.UUID{UUID: *v}
}

func wireTimePtr(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}

func doublePtr(f *float64) *wire.Double {
	if f == nil {
		return nil
	}
	d := wire.Double(*f)
	return &d
}

// knownKind is memory.kind.flatMap(MemoryKindDTO.init(rawValue:)).
func knownKind(k *string) *string {
	if k == nil || !validKind(*k) {
		return nil
	}
	return k
}

func contributionDTO(c *memorystore.ContributionRow) *MemoryContributionDTO {
	if c == nil {
		return nil
	}
	out := &MemoryContributionDTO{
		Id:              wire.UUID{UUID: c.ID},
		Operation:       MemoryContributionOperationDTO(c.Operation),
		Actor:           MemoryActorKindDTO(c.Actor),
		Source:          MemorySourceKindDTO(c.Source),
		SourceReference: c.SourceReference,
		CreatedAt:       wire.Time{Time: c.CreatedAt},
	}
	if c.Model != nil {
		out.Model = &ModelProvenanceDTO{Provider: c.Model.Provider, Model: c.Model.Model}
	}
	return out
}

func summaryDTO(s memorystore.ProvenanceSummary) *MemoryProvenanceSummaryDTO {
	out := &MemoryProvenanceSummaryDTO{
		CreatedBy:     contributionDTO(s.CreatedBy),
		LastUpdatedBy: contributionDTO(s.LastUpdatedBy),
		Contributors:  make([]ModelProvenanceDTO, 0, len(s.Contributors)),
	}
	for _, m := range s.Contributors {
		out.Contributors = append(out.Contributors, ModelProvenanceDTO{Provider: m.Provider, Model: m.Model})
	}
	return out
}

// toDTO is MemoryDTO.fromMemory: provenance is the contribution summary, or what the
// origin columns say when there is none.
func toDTO(m *memorystore.Memory, summary *memorystore.ProvenanceSummary) memoryDTO {
	s := memorystore.OriginSummary(m.ID, m.OriginKind, m.OriginSourceID, m.OriginProvider, m.OriginModel, m.CreatedAt)
	if summary != nil {
		s = *summary
	}
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	return memoryDTO{
		ID:              wire.UUID{UUID: m.ID},
		Content:         m.Content,
		Tags:            tags,
		CreatedAt:       wireTimePtr(m.CreatedAt),
		Lat:             doublePtr(m.Lat),
		Lng:             doublePtr(m.Lng),
		AccuracyM:       doublePtr(m.AccuracyM),
		PlaceName:       m.PlaceName,
		ReviewState:     m.ReviewState,
		Provenance:      summaryDTO(s),
		CreatedByUserID: wireUUIDPtr(m.CreatedByUserID),
		UpdatedByUserID: wireUUIDPtr(m.UpdatedByUserID),
		Kind:            knownKind(m.Kind),
		SpaceID:         wireUUIDPtr(m.SpaceID),
	}
}

// ModelDisclosurePolicy.scrub: a managed tenant never learns which model wrote what.

const mcpProviderPrefix = "mcp"

// isAgentAttribution is MCPCaller.isAgentAttribution: the user's own agent connection,
// not the upstream model the policy hides.
func isAgentAttribution(m *ModelProvenanceDTO) bool {
	return m.Provider == mcpProviderPrefix || strings.HasPrefix(m.Provider, mcpProviderPrefix+":")
}

func scrubContribution(c *MemoryContributionDTO) {
	if c != nil && c.Model != nil && !isAgentAttribution(c.Model) {
		c.Model = nil
	}
}

func scrubSummary(s *MemoryProvenanceSummaryDTO) {
	if s == nil {
		return
	}
	scrubContribution(s.CreatedBy)
	scrubContribution(s.LastUpdatedBy)
	s.Contributors = []ModelProvenanceDTO{}
}
