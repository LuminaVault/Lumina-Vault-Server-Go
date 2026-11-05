package memorystore

import (
	"context"
	"errors"
	"fmt"
	"lumina/internal/embedding"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is MemoryRepository. Swift's optional RouteTelemetry wrapper is not
// ported: otelpgx already spans every query, and nothing reads the timer on a route
// Go serves yet.
type Repository struct {
	pool *pgxpool.Pool
	// hits coalesces bumps when set; without it the search awaits one small UPDATE,
	// as a Swift repository with no queue wired does.
	hits *HitCountQueue
}

// New builds a Repository. hits may be nil.
func New(pool *pgxpool.Pool, hits *HitCountQueue) *Repository {
	return &Repository{pool: pool, hits: hits}
}

const memoryColumns = `id, tenant_id, content, created_by_user_id, updated_by_user_id, tags, created_at, updated_at,
	score, access_count, query_hit_count, last_accessed_at, last_reviewed_at, review_count, next_review_at,
	resurface_muted, source_vault_file_id, space_id, kind, lat, lng, accuracy_m, place_name, review_state,
	origin_kind, origin_source_id, origin_provider, origin_model, origin_conversation_message_id`

func scanMemory(row pgx.Row) (*Memory, error) {
	var m Memory
	if err := row.Scan(&m.ID, &m.TenantID, &m.Content, &m.CreatedByUserID, &m.UpdatedByUserID, &m.Tags, &m.CreatedAt, &m.UpdatedAt,
		&m.Score, &m.AccessCount, &m.QueryHitCount, &m.LastAccessedAt, &m.LastReviewedAt, &m.ReviewCount, &m.NextReviewAt,
		&m.ResurfaceMuted, &m.SourceVaultFileID, &m.SpaceID, &m.Kind, &m.Lat, &m.Lng, &m.AccuracyM, &m.PlaceName, &m.ReviewState,
		&m.OriginKind, &m.OriginSourceID, &m.OriginProvider, &m.OriginModel, &m.OriginConversationMessageID); err != nil {
		return nil, err
	}
	return &m, nil
}

// VectorArg is the parameter for a `$n::vector`: Swift's literal as text, or NULL
// when the provider failed. Strings go over pgx's text format whether or not the
// connection has the vector type registered.
func VectorArg(v []float32) *string {
	if v == nil {
		return nil
	}
	s := embedding.FormatVector(v)
	return &s
}

// CreateBare is create(content:context:), a Fluent save: it stamps updated_at as
// well as created_at, which the raw inserts below leave NULL, and writes no
// provenance row.
func (r *Repository) CreateBare(ctx context.Context, tenant uuid.UUID, content string) (*Memory, error) {
	return scanMemory(r.pool.QueryRow(ctx, `
		INSERT INTO memories (id, tenant_id, content, created_at, updated_at)
		VALUES ($1, $2, $3, NOW(), NOW())
		RETURNING `+memoryColumns, uuid.New(), tenant, content))
}

// CreateEmbedded is create(content:embedding:context:): a raw insert with no
// provenance row and updated_at NULL. A nil embedding stores NULL.
func (r *Repository) CreateEmbedded(ctx context.Context, tenant uuid.UUID, content string, vec []float32) (*Memory, error) {
	return scanMemory(r.pool.QueryRow(ctx, `
		INSERT INTO memories (id, tenant_id, content, embedding, created_at)
		VALUES ($1, $2, $3, $4::vector, NOW())
		RETURNING `+memoryColumns, uuid.New(), tenant, content, VectorArg(vec)))
}

// CreateParams are create(tenantID:content:embedding:...)'s arguments. Tenant and
// Content are required. A zero ReviewState means ReviewAuto; a zero Contribution
// means LegacyContribution.
type CreateParams struct {
	Tenant  uuid.UUID
	Content string
	// Embedding nil stores NULL, see embedding.EmbedForWrite.
	Embedding []float32
	// Tags empty stores NULL.
	Tags                        []string
	SourceVaultFileID           *uuid.UUID
	SpaceID                     *uuid.UUID
	ReviewState                 string
	Contribution                *Contribution
	OriginConversationMessageID *uuid.UUID
	// Kind is nil when unclassified.
	Kind *string
}

// Create is the tenant-direct create: the memory, then a provenance row. They are
// not one transaction, so a failed provenance insert returns an error with the
// memory already saved, as in Swift. updated_at stays NULL.
func (r *Repository) Create(ctx context.Context, p CreateParams) (*Memory, error) {
	contribution := LegacyContribution()
	if p.Contribution != nil {
		contribution = *p.Contribution
	}
	reviewState := p.ReviewState
	if reviewState == "" {
		reviewState = ReviewAuto
	}
	var tags []string
	if len(p.Tags) > 0 {
		tags = p.Tags
	}
	id := uuid.New()
	m, err := scanMemory(r.pool.QueryRow(ctx, `
		INSERT INTO memories (
			id, tenant_id, content, embedding, tags, source_vault_file_id,
			space_id, review_state, origin_kind, origin_source_id,
			origin_provider, origin_model, origin_conversation_message_id, kind, created_at
		) VALUES ($1, $2, $3, $4::vector, $5::text[], $6, $7, $8, $9, $10, $11, $12, $13, $14, NOW())
		RETURNING `+memoryColumns,
		id, p.Tenant, p.Content, VectorArg(p.Embedding), tags, p.SourceVaultFileID, p.SpaceID, reviewState,
		contribution.Source, contribution.SourceReference, contribution.Provider, contribution.Model,
		p.OriginConversationMessageID, p.Kind))
	if err != nil {
		return nil, fmt.Errorf("memorystore: create: %w", err)
	}
	if err := r.RecordContribution(ctx, p.Tenant, id, contribution); err != nil {
		return nil, err
	}
	return m, nil
}

// RecordContribution is MemoryProvenanceRepository.record.
func (r *Repository) RecordContribution(ctx context.Context, tenant, memory uuid.UUID, c Contribution) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO memory_contributions
			(id, tenant_id, memory_id, operation, actor_kind, source_kind, provider, model, source_reference, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())`,
		uuid.New(), tenant, memory, c.Operation, c.Actor, c.Source, c.Provider, c.Model, c.SourceReference)
	if err != nil {
		return fmt.Errorf("memorystore: record contribution: %w", err)
	}
	return nil
}

// List is list(context:): every memory of the tenant, in no particular order,
// rejected ones included.
func (r *Repository) List(ctx context.Context, tenant uuid.UUID) ([]Memory, error) {
	return r.queryMemories(ctx, `SELECT `+memoryColumns+` FROM memories WHERE tenant_id = $1`, tenant)
}

func (r *Repository) queryMemories(ctx context.Context, sql string, args ...any) ([]Memory, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Memory{}
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// SearchOptions are the optional arguments of the tenant-direct searches.
type SearchOptions struct {
	// SpaceID scopes the search to one Space.
	SpaceID *uuid.UUID
	// SkipHitCount is Swift's countHits: false, for lookups that are not recall.
	SkipHitCount bool
}

type searchRow struct {
	id           uuid.UUID
	tenant       uuid.UUID
	content      string
	createdAt    *time.Time
	originKind   string
	provider     *string
	model        *string
	distance     float32
	lexicalScore *float32
}

// SemanticSearch is semanticSearch(tenantID:queryEmbedding:limit:spaceID:countHits:):
// cosine search over embedded, non-rejected memories. The older
// semanticSearch(queryEmbedding:limit:context:) is this call with SkipHitCount and no
// Space.
func (r *Repository) SemanticSearch(ctx context.Context, tenant uuid.UUID, queryEmbedding []float32, limit int, opts SearchOptions) ([]SearchResult, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, content, created_at, origin_kind, origin_provider, origin_model,
		       embedding <=> $1::vector AS distance, NULL::float8 AS lexical_score
		FROM memories
		WHERE tenant_id = $2
		  AND ($3::uuid IS NULL OR space_id = $3)
		  AND embedding IS NOT NULL
		  AND review_state <> $4
		ORDER BY distance ASC
		LIMIT $5`,
		VectorArg(queryEmbedding), tenant, opts.SpaceID, ReviewRejected, max(limit, limit*3))
	if err != nil {
		return nil, fmt.Errorf("memorystore: semantic search: %w", err)
	}
	found, err := collectSearchRows(rows)
	if err != nil {
		return nil, fmt.Errorf("memorystore: semantic search: %w", err)
	}
	return r.finishSearch(ctx, found, limit, opts), nil
}

// LexicalSearch is lexicalSearch: full text over content_tsv, the document arm's
// fallback when the query could not be embedded. It finds a memory saved with
// embedding NULL during the same outage. Distance is reported as 1.0.
func (r *Repository) LexicalSearch(ctx context.Context, tenant uuid.UUID, query string, limit int, opts SearchOptions) ([]SearchResult, error) {
	rows, err := r.pool.Query(ctx, `
		WITH tsq AS (
			SELECT websearch_to_tsquery('english', $1::text) AS query
		)
		SELECT m.id, m.tenant_id, m.content, m.created_at, m.origin_kind, m.origin_provider, m.origin_model,
		       1.0::float8 AS distance,
		       ts_rank_cd(m.content_tsv, tsq.query)::float8 AS lexical_score
		FROM memories m
		CROSS JOIN tsq
		WHERE m.tenant_id = $2
		  AND ($3::uuid IS NULL OR m.space_id = $3)
		  AND m.review_state <> $4
		  AND m.content_tsv @@ tsq.query
		ORDER BY ts_rank_cd(m.content_tsv, tsq.query) DESC, m.created_at DESC
		LIMIT $5`,
		query, tenant, opts.SpaceID, ReviewRejected, max(limit, limit*3))
	if err != nil {
		return nil, fmt.Errorf("memorystore: lexical search: %w", err)
	}
	found, err := collectSearchRows(rows)
	if err != nil {
		return nil, fmt.Errorf("memorystore: lexical search: %w", err)
	}
	return r.finishSearch(ctx, found, limit, opts), nil
}

// SearchDocuments is searchDocuments: by vector, or by words when there is no vector
// (embedding.EmbedForQuery returned nil).
func (r *Repository) SearchDocuments(ctx context.Context, tenant uuid.UUID, query string, queryEmbedding []float32, limit int, opts SearchOptions) ([]SearchResult, error) {
	if queryEmbedding == nil {
		return r.LexicalSearch(ctx, tenant, query, limit, opts)
	}
	return r.SemanticSearch(ctx, tenant, queryEmbedding, limit, opts)
}

func collectSearchRows(rows pgx.Rows) ([]searchRow, error) {
	defer rows.Close()
	var out []searchRow
	for rows.Next() {
		var (
			row      searchRow
			distance float64
			lexical  *float64
		)
		if err := rows.Scan(&row.id, &row.tenant, &row.content, &row.createdAt, &row.originKind, &row.provider, &row.model, &distance, &lexical); err != nil {
			return nil, err
		}
		row.distance = float32(distance)
		if lexical != nil {
			score := float32(*lexical)
			row.lexicalScore = &score
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// finishSearch balances, bumps hit counts and maps. A bump failure never fails the
// search.
func (r *Repository) finishSearch(ctx context.Context, rows []searchRow, limit int, opts SearchOptions) []SearchResult {
	selected := balanced(rows, limit)
	if !opts.SkipHitCount && len(selected) > 0 {
		ids := make([]uuid.UUID, len(selected))
		for i, row := range selected {
			ids[i] = row.id
		}
		if r.hits != nil {
			r.hits.Enqueue(ids)
		} else {
			_ = BumpQueryHits(ctx, r.pool, ids)
		}
	}
	out := make([]SearchResult, len(selected))
	for i, row := range selected {
		out[i] = SearchResult{
			ID:           row.id,
			TenantID:     row.tenant,
			Content:      row.content,
			CreatedAt:    row.createdAt,
			Distance:     row.distance,
			Source:       ParseSourceKind(row.originKind),
			Provider:     row.provider,
			Model:        row.model,
			LexicalScore: row.lexicalScore,
		}
	}
	return out
}

// balanced prefers direct memories while still admitting prior model answers. The
// 40% model-output quota is soft: unused fact slots are backfilled so sparse vaults
// still benefit from continuity. The sort is stable, so on equal distance (every
// lexical hit is 1.0) facts stay ahead of outputs.
func balanced(rows []searchRow, limit int) []searchRow {
	isOutput := func(kind string) bool { return kind == SourceChat || kind == SourceQuery }
	outputLimit := max(1, int(float64(limit)*0.4))
	factsLeft := max(0, limit-outputLimit)
	outputsLeft := outputLimit

	selected := make([]searchRow, 0, min(limit, len(rows)))
	for _, row := range rows {
		if !isOutput(row.originKind) && factsLeft > 0 {
			selected = append(selected, row)
			factsLeft--
		}
	}
	for _, row := range rows {
		if isOutput(row.originKind) && outputsLeft > 0 {
			selected = append(selected, row)
			outputsLeft--
		}
	}
	chosen := make(map[uuid.UUID]struct{}, len(selected))
	for _, row := range selected {
		chosen[row.id] = struct{}{}
	}
	for _, row := range rows {
		if len(selected) >= limit {
			break
		}
		if _, taken := chosen[row.id]; !taken {
			selected = append(selected, row)
		}
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].distance < selected[j].distance })
	return selected
}

// SpaceTopology is spaceTopology: the tenant's Spaces, most populated first. It
// backs the agentic vault_map tool and resolves a slug to an id.
func (r *Repository) SpaceTopology(ctx context.Context, tenant uuid.UUID) ([]SpaceTopologyRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, slug, note_count, last_compiled_at FROM spaces
		WHERE tenant_id = $1 ORDER BY note_count DESC`, tenant)
	if err != nil {
		return nil, fmt.Errorf("memorystore: space topology: %w", err)
	}
	defer rows.Close()
	out := []SpaceTopologyRow{}
	for rows.Next() {
		var s SpaceTopologyRow
		if err := rows.Scan(&s.ID, &s.Name, &s.Slug, &s.NoteCount, &s.LastCompiledAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListParams are listPaginated's arguments.
type ListParams struct {
	Tenant uuid.UUID
	// Tag, when non-empty, takes the tag path below.
	Tag string
	// ReviewStates nil means every state except rejected. Non-nil is an in-set, so an
	// empty slice matches nothing.
	ReviewStates []string
	Health       HealthFilter
	// Kind is nil for any kind.
	Kind *string
	// Space is nil for any Space.
	Space  *SpaceFilter
	Limit  int
	Offset int
}

// SpaceFilter is MemorySpaceAssignment: one Space, or the unfiled ones.
type SpaceFilter struct {
	Unfiled bool
	ID      uuid.UUID
}

// ListPaginated is listPaginated, ordered (created_at DESC, id DESC).
//
// A non-empty Tag ignores every other filter, rejected rows included, and returns
// partial rows: id, tenant, content, tags, created_at and review_state. The rest of
// each Memory holds the model's defaults, as Swift's does.
func (r *Repository) ListPaginated(ctx context.Context, p ListParams) ([]Memory, error) {
	if p.Tag != "" {
		return r.listByTag(ctx, p)
	}
	where := []string{"tenant_id = $1"}
	args := []any{p.Tenant}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if p.ReviewStates != nil {
		where = append(where, "review_state = ANY("+arg(p.ReviewStates)+"::text[])")
	} else {
		where = append(where, "review_state <> "+arg(ReviewRejected))
	}
	switch p.Health {
	case HealthReviewOverdue:
		cutoff := arg(time.Now().Add(-ReviewOverdueAge))
		where = append(where, "(last_reviewed_at < "+cutoff+
			" OR (last_reviewed_at IS NULL AND last_accessed_at < "+cutoff+")"+
			" OR (last_reviewed_at IS NULL AND last_accessed_at IS NULL AND created_at < "+cutoff+"))")
	case HealthPending:
		where = append(where, "review_state = "+arg(ReviewPending))
	case HealthUnorganized:
		where = append(where, "space_id IS NULL", "source_vault_file_id IS NULL", "(tags IS NULL OR tags = '{}'::text[])")
	case HealthUnused:
		where = append(where, "query_hit_count = 0")
	}
	if p.Kind != nil {
		where = append(where, "kind = "+arg(*p.Kind))
	}
	if p.Space != nil {
		if p.Space.Unfiled {
			where = append(where, "space_id IS NULL")
		} else {
			where = append(where, "space_id = "+arg(p.Space.ID))
		}
	}
	limit, offset := arg(p.Limit), arg(p.Offset)
	sql := `SELECT ` + memoryColumns + ` FROM memories WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY created_at DESC, id DESC LIMIT ` + limit + ` OFFSET ` + offset
	out, err := r.queryMemories(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("memorystore: list: %w", err)
	}
	return out, nil
}

func (r *Repository) listByTag(ctx context.Context, p ListParams) ([]Memory, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, content, tags, created_at, review_state
		FROM memories
		WHERE tenant_id = $1 AND $2 = ANY(tags)
		ORDER BY created_at DESC, id DESC
		LIMIT $3 OFFSET $4`, p.Tenant, p.Tag, p.Limit, p.Offset)
	if err != nil {
		return nil, fmt.Errorf("memorystore: list by tag: %w", err)
	}
	defer rows.Close()
	out := []Memory{}
	for rows.Next() {
		m := Memory{OriginKind: SourceLegacy}
		if err := rows.Scan(&m.ID, &m.TenantID, &m.Content, &m.Tags, &m.CreatedAt, &m.ReviewState); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpdateReviewState is updateReviewState. The caller validates the transition. It
// does not touch updated_at, and a missing row is not an error.
func (r *Repository) UpdateReviewState(ctx context.Context, tenant, id uuid.UUID, reviewState string) error {
	_, err := r.pool.Exec(ctx, `UPDATE memories SET review_state = $1 WHERE tenant_id = $2 AND id = $3`, reviewState, tenant, id)
	return err
}

// Find is the tenant-scoped fetch. Nil when not found.
func (r *Repository) Find(ctx context.Context, tenant, id uuid.UUID) (*Memory, error) {
	m, err := scanMemory(r.pool.QueryRow(ctx, `SELECT `+memoryColumns+` FROM memories WHERE tenant_id = $1 AND id = $2`, tenant, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// StoredEmbedding is storedEmbedding. exists is false when the tenant has no such
// memory; a nil vector with exists true means it was never embedded.
func (r *Repository) StoredEmbedding(ctx context.Context, tenant, id uuid.UUID) (vec []float32, exists bool, err error) {
	var text *string
	err = r.pool.QueryRow(ctx, `SELECT embedding::text FROM memories WHERE tenant_id = $1 AND id = $2`, tenant, id).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if text == nil {
		return nil, true, nil
	}
	return ParseVector(*text), true, nil
}

// ParseVector is parseVector: pgvector's text form, "[0.1,0.2,...]". An element that
// does not parse is dropped, as compactMap does.
func ParseVector(text string) []float32 {
	body := strings.Trim(text, "[] ")
	out := []float32{}
	for piece := range strings.SplitSeq(body, ",") {
		f, err := strconv.ParseFloat(strings.TrimSpace(piece), 32)
		if err == nil {
			out = append(out, float32(f))
		}
	}
	return out
}

// Delete is the tenant-scoped delete. It reports whether a row was removed. Chunks
// and contributions go by foreign key.
func (r *Repository) Delete(ctx context.Context, tenant, id uuid.UUID) (bool, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM memories WHERE tenant_id = $1 AND id = $2`, tenant, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetSpace is setSpace: re-files a memory and its chunks together, so Space-scoped
// search agrees with the list. False when the tenant has no such memory.
func (r *Repository) SetSpace(ctx context.Context, tenant, id uuid.UUID, spaceID *uuid.UUID) (bool, error) {
	found := false
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE memories SET space_id = $1, updated_at = NOW() WHERE tenant_id = $2 AND id = $3`, spaceID, tenant, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		found = true
		_, err = tx.Exec(ctx, `UPDATE memory_chunks SET space_id = $1 WHERE tenant_id = $2 AND memory_id = $3`, spaceID, tenant, id)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("memorystore: set space: %w", err)
	}
	return found, nil
}

// Forget is forget, the user's own delete: the source job is suppressed so it is not
// re-learned, and a tombstone tells local-sync clients to drop their copy. False when
// the tenant has no such memory. A tombstone failure does not fail the delete.
func (r *Repository) Forget(ctx context.Context, tenant, id uuid.UUID) (bool, error) {
	m, err := r.Find(ctx, tenant, id)
	if err != nil {
		return false, err
	}
	if m != nil {
		if err := r.SuppressJob(ctx, tenant, m); err != nil {
			return false, err
		}
	}
	deleted, err := r.Delete(ctx, tenant, id)
	if err != nil || !deleted {
		return false, err
	}
	_, _ = r.pool.Exec(ctx, `
		INSERT INTO memory_sync_tombstones (id, tenant_id, memory_id, deleted_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (tenant_id, memory_id) DO NOTHING`, uuid.New(), tenant, id)
	return true, nil
}

// UpdateContent is updateContent: a user's edit. The chunks go in the same
// transaction, because they hold the old text and the chunk arm would keep returning
// it after a correction; the document arm finds the memory until they are rebuilt.
// updated_at is bumped because the local-sync cursor reads it. A nil embedding (the
// provider failed) stores NULL rather than keep a vector of the old text. contribution
// is written after the commit and may be nil.
func (r *Repository) UpdateContent(ctx context.Context, tenant, id uuid.UUID, content string, vec []float32, contribution *Contribution) (bool, error) {
	found := false
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE memories SET content = $1, embedding = $2::vector, updated_at = NOW()
			WHERE tenant_id = $3 AND id = $4`, content, VectorArg(vec), tenant, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		found = true
		_, err = tx.Exec(ctx, `DELETE FROM memory_chunks WHERE tenant_id = $1 AND memory_id = $2`, tenant, id)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("memorystore: update content: %w", err)
	}
	if found && contribution != nil {
		if err := r.RecordContribution(ctx, tenant, id, *contribution); err != nil {
			return false, err
		}
	}
	return found, nil
}

// IDBySourceVaultFileID is idBySourceVaultFileID: the memory compiled from a vault
// file, so the note write path can re-embed in place. Nil when there is none. With
// several, which one comes back is up to Postgres, as in Swift.
func (r *Repository) IDBySourceVaultFileID(ctx context.Context, tenant, sourceVaultFileID uuid.UUID) (*uuid.UUID, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `SELECT id FROM memories WHERE tenant_id = $1 AND source_vault_file_id = $2 LIMIT 1`, tenant, sourceVaultFileID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// DeleteBySourceVaultFileID is deleteBySourceVaultFileID, so a note delete does not
// leave a dangling recall. It reports whether any row went.
func (r *Repository) DeleteBySourceVaultFileID(ctx context.Context, tenant, sourceVaultFileID uuid.UUID) (bool, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM memories WHERE tenant_id = $1 AND source_vault_file_id = $2`, tenant, sourceVaultFileID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateTags is updateTags. Nil or empty clears the column to NULL. It is a Fluent
// save in Swift, so updated_at moves, even when the tags did not change. contribution
// may be nil.
func (r *Repository) UpdateTags(ctx context.Context, tenant, id uuid.UUID, tags []string, contribution *Contribution) (bool, error) {
	if len(tags) == 0 {
		tags = nil
	}
	tag, err := r.pool.Exec(ctx, `UPDATE memories SET tags = $1::text[], updated_at = NOW() WHERE tenant_id = $2 AND id = $3`, tags, tenant, id)
	if err != nil {
		return false, fmt.Errorf("memorystore: update tags: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if contribution != nil {
		if err := r.RecordContribution(ctx, tenant, id, *contribution); err != nil {
			return false, err
		}
	}
	return true, nil
}

// FindLineage is findLineage. It still returns the memory when no source is linked.
// Nil when the tenant has no such memory.
func (r *Repository) FindLineage(ctx context.Context, tenant, memoryID uuid.UUID) (*LineageRow, error) {
	var l LineageRow
	err := r.pool.QueryRow(ctx, `
		SELECT m.id, m.content, m.created_at, m.source_vault_file_id, v.path, v.created_at
		FROM memories m
		LEFT JOIN vault_files v
			ON v.id = m.source_vault_file_id
		   AND v.tenant_id = m.tenant_id
		WHERE m.tenant_id = $1 AND m.id = $2
		LIMIT 1`, tenant, memoryID).Scan(&l.MemoryID, &l.MemoryContent, &l.MemoryCreatedAt, &l.SourceVaultFileID, &l.SourcePath, &l.SourceCreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}
