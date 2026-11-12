package memorystore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// distantPast is Date.distantPast, the created_at a contribution reports for a
// memory with none.
var distantPast = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)

// ModelRef is ModelProvenanceDTO: provider and model are present together or not
// at all.
type ModelRef struct {
	Provider string
	Model    string
}

// ContributionRow is one memory_contributions row, read the way
// MemoryContributionDTO reads it: unknown stored kinds fall back rather than fail.
type ContributionRow struct {
	ID              uuid.UUID
	Operation       string
	Actor           string
	Source          string
	Model           *ModelRef
	SourceReference *string
	CreatedAt       time.Time
}

// ProvenanceSummary is MemoryProvenanceSummaryDTO.
type ProvenanceSummary struct {
	CreatedBy     *ContributionRow
	LastUpdatedBy *ContributionRow
	Contributors  []ModelRef
}

// Facet is MemoryFacetDTO.
type Facet struct {
	Value string
	Count int
}

// Facets is MemoryFacetsResponse.
type Facets struct {
	Providers []Facet
	Models    []Facet
	Sources   []Facet
	OldestAt  *time.Time
	NewestAt  *time.Time
}

func modelRef(provider, model *string) *ModelRef {
	if provider == nil || model == nil {
		return nil
	}
	return &ModelRef{Provider: *provider, Model: *model}
}

func newContributionRow(id uuid.UUID, operation, actor, source string, provider, model, reference *string, createdAt time.Time) ContributionRow {
	if operation != OperationCreate && operation != OperationUpdate {
		operation = OperationUpdate
	}
	if actor != ActorUser && actor != ActorModel && actor != ActorSystem {
		actor = ActorSystem
	}
	return ContributionRow{
		ID:              id,
		Operation:       operation,
		Actor:           actor,
		Source:          ParseSourceKind(source),
		Model:           modelRef(provider, model),
		SourceReference: reference,
		CreatedAt:       createdAt,
	}
}

// OriginSummary is Memory.originSummary and MemoryNodeRow.provenance: what the origin
// columns say when there are no contribution rows to read. A memory with no
// created_at reports Date.distantPast.
func OriginSummary(id uuid.UUID, originKind string, originSourceID, originProvider, originModel *string, createdAt *time.Time) ProvenanceSummary {
	source := ParseSourceKind(originKind)
	actor := ActorSystem
	switch {
	case originProvider != nil:
		actor = ActorModel
	case source == SourceManual:
		actor = ActorUser
	}
	created := distantPast
	if createdAt != nil {
		created = *createdAt
	}
	model := modelRef(originProvider, originModel)
	summary := ProvenanceSummary{
		CreatedBy: &ContributionRow{
			ID: id, Operation: OperationCreate, Actor: actor, Source: source,
			Model: model, SourceReference: originSourceID, CreatedAt: created,
		},
		Contributors: []ModelRef{},
	}
	if model != nil {
		summary.Contributors = []ModelRef{*model}
	}
	return summary
}

// Timeline is MemoryProvenanceRepository.timeline, oldest first. exists is false when
// the tenant has no such memory.
func (r *Repository) Timeline(ctx context.Context, tenant, memory uuid.UUID) (rows []ContributionRow, exists bool, err error) {
	found, err := r.Find(ctx, tenant, memory)
	if err != nil || found == nil {
		return nil, false, err
	}
	q, err := r.pool.Query(ctx, `
		SELECT id, operation, actor_kind, source_kind, provider, model, source_reference, created_at
		FROM memory_contributions
		WHERE tenant_id = $1 AND memory_id = $2
		ORDER BY created_at ASC, id ASC`, tenant, memory)
	if err != nil {
		return nil, false, fmt.Errorf("memorystore: timeline: %w", err)
	}
	defer q.Close()
	rows = []ContributionRow{}
	for q.Next() {
		var (
			id                         uuid.UUID
			operation, actor, source   string
			provider, model, reference *string
			createdAt                  time.Time
		)
		if err := q.Scan(&id, &operation, &actor, &source, &provider, &model, &reference, &createdAt); err != nil {
			return nil, false, err
		}
		rows = append(rows, newContributionRow(id, operation, actor, source, provider, model, reference, createdAt))
	}
	return rows, true, q.Err()
}

// Summaries is MemoryProvenanceRepository.summaries: a memory with no contribution
// rows has no entry. Contributors are distinct, sorted by provider then model.
func (r *Repository) Summaries(ctx context.Context, tenant uuid.UUID, memories []uuid.UUID) (map[uuid.UUID]ProvenanceSummary, error) {
	out := map[uuid.UUID]ProvenanceSummary{}
	if len(memories) == 0 {
		return out, nil
	}
	q, err := r.pool.Query(ctx, `
		SELECT memory_id, id, operation, actor_kind, source_kind, provider, model, source_reference, created_at
		FROM memory_contributions
		WHERE tenant_id = $1 AND memory_id = ANY($2::uuid[])
		ORDER BY memory_id, created_at ASC, id ASC`, tenant, memories)
	if err != nil {
		return nil, fmt.Errorf("memorystore: summaries: %w", err)
	}
	defer q.Close()
	grouped := map[uuid.UUID][]ContributionRow{}
	for q.Next() {
		var (
			memoryID, id               uuid.UUID
			operation, actor, source   string
			provider, model, reference *string
			createdAt                  time.Time
		)
		if err := q.Scan(&memoryID, &id, &operation, &actor, &source, &provider, &model, &reference, &createdAt); err != nil {
			return nil, err
		}
		grouped[memoryID] = append(grouped[memoryID], newContributionRow(id, operation, actor, source, provider, model, reference, createdAt))
	}
	if err := q.Err(); err != nil {
		return nil, err
	}
	for id, rows := range grouped {
		out[id] = summarize(rows)
	}
	return out, nil
}

func summarize(rows []ContributionRow) ProvenanceSummary {
	summary := ProvenanceSummary{Contributors: []ModelRef{}}
	seen := map[ModelRef]bool{}
	for i := range rows {
		if m := rows[i].Model; m != nil && !seen[*m] {
			seen[*m] = true
			summary.Contributors = append(summary.Contributors, *m)
		}
	}
	sort.Slice(summary.Contributors, func(i, j int) bool {
		a, b := summary.Contributors[i], summary.Contributors[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
	if len(rows) > 0 {
		summary.CreatedBy = &rows[0]
	}
	if len(rows) > 1 {
		summary.LastUpdatedBy = &rows[len(rows)-1]
	}
	return summary
}

// Facets is MemoryProvenanceRepository.facets: counts over the origin columns, most
// common first, plus the tenant's created_at bounds. Rejected memories count.
func (r *Repository) Facets(ctx context.Context, tenant uuid.UUID) (*Facets, error) {
	out := &Facets{}
	var err error
	if out.Providers, err = r.facet(ctx, tenant, "origin_provider"); err != nil {
		return nil, err
	}
	if out.Models, err = r.facet(ctx, tenant, "origin_model"); err != nil {
		return nil, err
	}
	if out.Sources, err = r.facet(ctx, tenant, "origin_kind"); err != nil {
		return nil, err
	}
	err = r.pool.QueryRow(ctx, `SELECT MIN(created_at), MAX(created_at) FROM memories WHERE tenant_id = $1`, tenant).Scan(&out.OldestAt, &out.NewestAt)
	if err != nil {
		return nil, fmt.Errorf("memorystore: facet bounds: %w", err)
	}
	return out, nil
}

// facet takes column from the three fixed names above, never from a request.
func (r *Repository) facet(ctx context.Context, tenant uuid.UUID, column string) ([]Facet, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+column+` AS value, COUNT(*)::int AS count
		FROM memories
		WHERE tenant_id = $1 AND `+column+` IS NOT NULL
		GROUP BY `+column+`
		ORDER BY count DESC, value ASC`, tenant)
	if err != nil {
		return nil, fmt.Errorf("memorystore: facet %s: %w", column, err)
	}
	defer rows.Close()
	out := []Facet{}
	for rows.Next() {
		var f Facet
		if err := rows.Scan(&f.Value, &f.Count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// EnqueueOutputParams are enqueueOutput's arguments.
type EnqueueOutputParams struct {
	Tenant                uuid.UUID
	Source                string
	SourceID              string
	ConversationMessageID *uuid.UUID
	Content               string
	Provider              *string
	Model                 *string
}

// EnqueueOutput is enqueueOutput: an assistant answer for MemoryIndexWorker
// (internal/workers/memoryindex.go). A second answer for the same source is dropped.
func (r *Repository) EnqueueOutput(ctx context.Context, p EnqueueOutputParams) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO memory_index_jobs
			(id, tenant_id, source_kind, source_id, conversation_message_id,
			 content, provider, model, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', NOW(), NOW())
		ON CONFLICT (tenant_id, source_kind, source_id) DO NOTHING`,
		uuid.New(), p.Tenant, p.Source, p.SourceID, p.ConversationMessageID, p.Content, p.Provider, p.Model)
	if err != nil {
		return fmt.Errorf("memorystore: enqueue output: %w", err)
	}
	return nil
}

// SuppressJob is suppressJob: the source job of a memory the user removed, so it is
// not re-learned. A memory with no source id has none.
func (r *Repository) SuppressJob(ctx context.Context, tenant uuid.UUID, m *Memory) error {
	if m.OriginSourceID == nil {
		return nil
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE memory_index_jobs SET status = 'suppressed', updated_at = NOW()
		WHERE tenant_id = $1 AND source_kind = $2 AND source_id = $3`,
		tenant, m.OriginKind, *m.OriginSourceID)
	if err != nil {
		return fmt.Errorf("memorystore: suppress job: %w", err)
	}
	return nil
}

// FindMany is Memory.query(...).filter(\.$id ~~ ids).all(): the tenant's rows among
// ids, in no particular order.
func (r *Repository) FindMany(ctx context.Context, tenant uuid.UUID, ids []uuid.UUID) ([]Memory, error) {
	return r.queryMemories(ctx, `SELECT `+memoryColumns+` FROM memories WHERE tenant_id = $1 AND id = ANY($2::uuid[])`, tenant, ids)
}

// StampCreator is the upsert route's Fluent update after create: it names the actor
// as creator and last editor. It updates m.
func (r *Repository) StampCreator(ctx context.Context, m *Memory, actor uuid.UUID) error {
	now := time.Now()
	_, err := r.pool.Exec(ctx, `UPDATE memories SET created_by_user_id = $2, updated_by_user_id = $2, updated_at = $3 WHERE id = $1`, m.ID, actor, now)
	if err != nil {
		return fmt.Errorf("memorystore: stamp creator: %w", err)
	}
	m.CreatedByUserID, m.UpdatedByUserID, m.UpdatedAt = &actor, &actor, &now
	return nil
}

// Geo is the upsert body's optional anchor. Each field is independent.
type Geo struct {
	Lat, Lng, AccuracyM *float64
	PlaceName           *string
}

// SetGeo is the upsert route's Fluent save of the geo columns. It updates m.
func (r *Repository) SetGeo(ctx context.Context, m *Memory, g Geo) error {
	now := time.Now()
	_, err := r.pool.Exec(ctx, `UPDATE memories SET lat = $2, lng = $3, accuracy_m = $4, place_name = $5, updated_at = $6 WHERE id = $1`,
		m.ID, g.Lat, g.Lng, g.AccuracyM, g.PlaceName, now)
	if err != nil {
		return fmt.Errorf("memorystore: set geo: %w", err)
	}
	m.Lat, m.Lng, m.AccuracyM, m.PlaceName, m.UpdatedAt = g.Lat, g.Lng, g.AccuracyM, g.PlaceName, &now
	return nil
}

// SaveReview is the review route's Fluent update: the columns the caller changed on m,
// from its in-memory values, so two concurrent reviews can count as one, as in Swift.
func (r *Repository) SaveReview(ctx context.Context, m *Memory) error {
	now := time.Now()
	_, err := r.pool.Exec(ctx, `
		UPDATE memories
		SET last_reviewed_at = $2, review_count = $3, next_review_at = $4, resurface_muted = $5,
		    updated_by_user_id = $6, updated_at = $7
		WHERE id = $1`,
		m.ID, m.LastReviewedAt, m.ReviewCount, m.NextReviewAt, m.ResurfaceMuted, m.UpdatedByUserID, now)
	if err != nil {
		return fmt.Errorf("memorystore: save review: %w", err)
	}
	m.UpdatedAt = &now
	return nil
}

// SaveEdit is the patch route's closing Fluent update: the last editor, and the kind
// when the patch set one. It always moves updated_at, because assigning the editor
// marks the row changed even when the value is the same. It updates m.
func (r *Repository) SaveEdit(ctx context.Context, m *Memory, actor uuid.UUID, kind *string) error {
	now := time.Now()
	_, err := r.pool.Exec(ctx, `UPDATE memories SET updated_by_user_id = $2, kind = COALESCE($3, kind), updated_at = $4 WHERE id = $1`, m.ID, actor, kind, now)
	if err != nil {
		return fmt.Errorf("memorystore: save edit: %w", err)
	}
	m.UpdatedByUserID, m.UpdatedAt = &actor, &now
	if kind != nil {
		m.Kind = kind
	}
	return nil
}

// ContentHash is MemoryCompileService.contentHash: the SHA-256 hex of the UTF-8 text,
// what kb_compile_reject_list stores.
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// RecordRejected is KBCompileRejectListRepository.record: an idempotent insert, so
// the next kb-compile run skips content the user rejected.
func (r *Repository) RecordRejected(ctx context.Context, tenant uuid.UUID, contentHash string, vaultFile *uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO kb_compile_reject_list (id, tenant_id, content_hash, vault_file_id, rejected_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (tenant_id, content_hash) DO NOTHING`, uuid.New(), tenant, contentHash, vaultFile)
	if err != nil {
		return fmt.Errorf("memorystore: record rejected: %w", err)
	}
	return nil
}

// CompareIDs orders UUIDs as Swift compares uuidString: by byte, since hex digits
// sort below hex letters in either case.
func CompareIDs(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }
