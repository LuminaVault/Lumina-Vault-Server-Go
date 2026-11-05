// Package memorystore is MemoryRepository and MemoryProvenanceRepository: the memories
// table, its searches, the hit count queue, and the provenance rows.
//
// Raw pgx rather than sqlc, as Swift's repository is raw SQL: the embedding column,
// optional filters and text arrays don't fit generated queries. Fluent's paths are
// spelled out too, and where Fluent and raw SQL write differently (updated_at, for
// one) each method says so. The rest of MemoryProvenanceRepository is in provenance.go.
package memorystore

import (
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Review states (MemoryReviewState).
const (
	ReviewAuto     = "auto"
	ReviewPending  = "pending"
	ReviewApproved = "approved"
	ReviewRejected = "rejected"
)

// Source kinds (MemorySourceKindDTO), stored in memories.origin_kind and
// memory_contributions.source_kind.
const (
	SourceManual           = "manual"
	SourceChat             = "chat"
	SourceQuery            = "query"
	SourceKnowledgeCompile = "knowledge_compile"
	SourceSkill            = "skill"
	SourceVault            = "vault"
	SourceImport           = "import"
	SourceLink             = "link"
	SourceLegacy           = "legacy"
)

// Actor kinds (MemoryActorKindDTO).
const (
	ActorUser   = "user"
	ActorModel  = "model"
	ActorSystem = "system"
)

// Contribution operations (MemoryContributionOperationDTO).
const (
	OperationCreate = "create"
	OperationUpdate = "update"
)

// Memory kinds (MemoryKindDTO).
const (
	KindPreference = "preference"
	KindDecision   = "decision"
	KindFact       = "fact"
	KindNote       = "note"
)

// ParseSourceKind is MemorySourceKindDTO(rawValue:) ?? .legacy.
func ParseSourceKind(raw string) string {
	switch raw {
	case SourceManual, SourceChat, SourceQuery, SourceKnowledgeCompile, SourceSkill, SourceVault, SourceImport, SourceLink:
		return raw
	}
	return SourceLegacy
}

// HealthFilter is MemoryHealthFilter, the `health` list filter.
type HealthFilter string

const (
	HealthReviewOverdue HealthFilter = "review-overdue"
	HealthPending       HealthFilter = "pending"
	HealthUnorganized   HealthFilter = "unorganized"
	HealthUnused        HealthFilter = "unused"
)

// ReviewOverdueAge is how long since a memory was last looked at before it counts
// as overdue, shared with the fading lane of GET /v1/me/resurface.
const ReviewOverdueAge = 30 * 24 * time.Hour

// Memory is one row of memories. It carries what Fluent's Memory model decodes;
// embedding and content_tsv are not on the model either.
type Memory struct {
	ID                          uuid.UUID
	TenantID                    uuid.UUID
	Content                     string
	CreatedByUserID             *uuid.UUID
	UpdatedByUserID             *uuid.UUID
	Tags                        []string
	CreatedAt                   *time.Time
	UpdatedAt                   *time.Time
	Score                       float64
	AccessCount                 int64
	QueryHitCount               int64
	LastAccessedAt              *time.Time
	LastReviewedAt              *time.Time
	ReviewCount                 int64
	NextReviewAt                *time.Time
	ResurfaceMuted              bool
	SourceVaultFileID           *uuid.UUID
	SpaceID                     *uuid.UUID
	Kind                        *string
	Lat                         *float64
	Lng                         *float64
	AccuracyM                   *float64
	PlaceName                   *string
	ReviewState                 string
	OriginKind                  string
	OriginSourceID              *string
	OriginProvider              *string
	OriginModel                 *string
	OriginConversationMessageID *uuid.UUID
}

// Citation is MemoryCitation: where a hit physically came from, precise enough to
// re-read by hand. Present on chunk hits, nil on the whole-memory path.
type Citation struct {
	// ChunkID is stable while the chunk's text and position are unchanged.
	ChunkID string
	// DocumentID is stable across edits and reindexing.
	DocumentID string
	// Path is nil for memories with no source file.
	Path *string
	// HeadingPath is the heading ancestry, outermost first.
	HeadingPath []string
	// StartLine and EndLine are a 1-based inclusive range within Path.
	StartLine int
	EndLine   int
}

// DisplayTrail is MemoryCitationDTO.displayTrail, the label a source chip shows and
// the trail the grounding prompt cites:
// "projects/hermes.md › Routing › Fallbacks (L40-58)".
func (c Citation) DisplayTrail() string {
	parts := make([]string, 0, len(c.HeadingPath)+1)
	if c.Path != nil {
		parts = append(parts, *c.Path)
	}
	parts = append(parts, c.HeadingPath...)
	trail := strings.Join(parts, " › ")
	lines := "L" + strconv.Itoa(c.StartLine)
	if c.StartLine != c.EndLine {
		lines += "-" + strconv.Itoa(c.EndLine)
	}
	if trail == "" {
		return lines
	}
	return trail + " (" + lines + ")"
}

// SearchResult is MemorySearchResult.
type SearchResult struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Content   string
	CreatedAt *time.Time
	// Distance is the cosine distance. 1.0 ("maximally far") on a hit that only the
	// lexical arm found. float32 as in Swift: the balancing sort runs on the narrowed
	// value, so two rows that tie as Floats keep their order.
	Distance float32
	// Source is a source kind; unknown stored values read as SourceLegacy.
	Source   string
	Provider *string
	Model    *string
	// Snippet is the lexical arm's match-highlighted excerpt, "[" and "]" around
	// terms. Nil on a path that produces none.
	Snippet *string
	// Citation is nil on the whole-memory path.
	Citation *Citation
	// LexicalScore is ts_rank_cd from a lexical arm; nil or 0 when no term matched.
	// Snippet cannot tell you this: ts_headline writes one regardless.
	LexicalScore *float32
}

// Contribution is MemoryContributionInput.
type Contribution struct {
	Operation       string
	Actor           string
	Source          string
	Provider        *string
	Model           *string
	SourceReference *string
}

// UserContribution is MemoryContributionInput.user. Swift's default source is
// SourceManual.
func UserContribution(operation, source string, reference *string) Contribution {
	return Contribution{Operation: operation, Actor: ActorUser, Source: source, SourceReference: reference}
}

// ModelContribution is MemoryContributionInput.model.
func ModelContribution(operation, source string, provider, model, reference *string) Contribution {
	return Contribution{Operation: operation, Actor: ActorModel, Source: source, Provider: provider, Model: model, SourceReference: reference}
}

// LegacyContribution is MemoryContributionInput.legacy, the default on create.
func LegacyContribution() Contribution {
	return Contribution{Operation: OperationCreate, Actor: ActorSystem, Source: SourceLegacy}
}

// SpaceTopologyRow is one Space with its note count.
type SpaceTopologyRow struct {
	ID             uuid.UUID
	Name           string
	Slug           string
	NoteCount      int
	LastCompiledAt *time.Time
}

// LineageRow is one memory with its source file. Source fields are nil when the
// memory has none, or the file row is gone.
type LineageRow struct {
	MemoryID          uuid.UUID
	MemoryContent     string
	MemoryCreatedAt   *time.Time
	SourceVaultFileID *uuid.UUID
	SourcePath        *string
	SourceCreatedAt   *time.Time
}
