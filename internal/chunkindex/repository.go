package chunkindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"lumina/internal/memorystore"
	"lumina/internal/telemetry"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ChunkRepository is MemoryChunkRepository. Every statement binds tenant_id
// explicitly: there is no inherited row filter.
type ChunkRepository struct {
	pool *pgxpool.Pool
}

// NewChunkRepository builds a repository.
func NewChunkRepository(pool *pgxpool.Pool) *ChunkRepository {
	return &ChunkRepository{pool: pool}
}

// armDepthMultiplier is how deep each arm reaches before fusion: wider than the
// limit, so a result one arm ranks poorly can be rescued by the other. The Reciprocal
// Rank Fusion constant, 60, is inline in the query.
const armDepthMultiplier = 4

// ReplaceChunksParams are replaceChunks' arguments. Embeddings is positional and a
// nil entry is a deliberate NULL: the provider failed, the chunk is still found
// lexically, and Swift's heal pass embeds it later.
type ReplaceChunksParams struct {
	Tenant      uuid.UUID
	MemoryID    uuid.UUID
	DocumentID  string
	VaultFileID *uuid.UUID
	SpaceID     *uuid.UUID
	SourcePath  *string
	Chunks      []DocumentChunk
	Embeddings  [][]float32
}

// ReplaceChunks is replaceChunks: delete then insert in one transaction, so a
// re-chunk that gives fewer chunks leaves no orphans and a half-rewritten document is
// never searchable. A chunk_id another memory already holds is skipped, as Swift's
// ON CONFLICT DO NOTHING does: two memories at one path share a document ID.
func (r *ChunkRepository) ReplaceChunks(ctx context.Context, p ReplaceChunksParams) error {
	if len(p.Chunks) != len(p.Embeddings) {
		return errors.New("chunkindex: chunk/embedding count mismatch")
	}
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM memory_chunks WHERE tenant_id = $1 AND memory_id = $2`, p.Tenant, p.MemoryID); err != nil {
			return err
		}
		for i, chunk := range p.Chunks {
			headingPath, err := EncodeHeadingPath(chunk.HeadingPath)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO memory_chunks (
					chunk_id, document_id, tenant_id, memory_id, vault_file_id, space_id,
					source_path, ordinal, heading_path, start_line, end_line, text,
					content_sha256, embedding, created_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::text::jsonb, $10, $11, $12, $13, $14::vector, NOW())
				ON CONFLICT (chunk_id) DO NOTHING`,
				ChunkID(p.DocumentID, chunk.Ordinal, chunk.ContentSHA256), p.DocumentID, p.Tenant, p.MemoryID,
				p.VaultFileID, p.SpaceID, p.SourcePath, chunk.Ordinal, headingPath, chunk.StartLine, chunk.EndLine,
				chunk.Text, chunk.ContentSHA256, memorystore.VectorArg(p.Embeddings[i])); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("chunkindex: replace chunks: %w", err)
	}
	return nil
}

// DeleteChunks is deleteChunks.
func (r *ChunkRepository) DeleteChunks(ctx context.Context, tenant, memory uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM memory_chunks WHERE tenant_id = $1 AND memory_id = $2`, tenant, memory)
	return err
}

// HasChunks is hasChunks: true when the tenant has at least one chunk.
func (r *ChunkRepository) HasChunks(ctx context.Context, tenant uuid.UUID) (bool, error) {
	var present bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memory_chunks WHERE tenant_id = $1)`, tenant).Scan(&present)
	return present, err
}

// The dense arm has two spellings, chosen in HybridSearch. With no query vector
// (the provider failed to embed the query) it selects nothing and the fusion is
// lexical-only, spelled as Swift spells it so the plans match.
const (
	hybridHead = `
		WITH tsq AS (
			SELECT websearch_to_tsquery('english', $1::text) AS query
		),
		dense AS (`
	denseWithVector = `
			SELECT chunk_id,
			       ROW_NUMBER() OVER (ORDER BY embedding <=> $6::vector ASC) AS rank,
			       embedding <=> $6::vector AS distance
			FROM memory_chunks
			WHERE tenant_id = $2
			  AND ($3::uuid IS NULL OR space_id = $3)
			  AND embedding IS NOT NULL
			ORDER BY embedding <=> $6::vector ASC
			LIMIT $4`
	denseWithoutVector = `
			SELECT chunk_id,
			       ROW_NUMBER() OVER (ORDER BY NULL::float8 ASC) AS rank,
			       NULL::float8 AS distance
			FROM memory_chunks
			WHERE tenant_id = $2
			  AND ($3::uuid IS NULL OR space_id = $3)
			  AND embedding IS NOT NULL
			  AND FALSE
			ORDER BY NULL::float8 ASC
			LIMIT $4`
	hybridTail = `
		),
		lexical AS (
			SELECT c.chunk_id,
			       ROW_NUMBER() OVER (ORDER BY ts_rank_cd(c.content_tsv, tsq.query) DESC) AS rank,
			       ts_rank_cd(c.content_tsv, tsq.query) AS score
			FROM memory_chunks c
			CROSS JOIN tsq
			WHERE c.tenant_id = $2
			  AND ($3::uuid IS NULL OR c.space_id = $3)
			  AND c.content_tsv @@ tsq.query
			ORDER BY ts_rank_cd(c.content_tsv, tsq.query) DESC
			LIMIT $4
		),
		fused AS (
			SELECT COALESCE(d.chunk_id, l.chunk_id) AS chunk_id,
			       COALESCE(1.0 / (60 + d.rank), 0)
			     + COALESCE(1.0 / (60 + l.rank), 0) AS rrf,
			       d.distance AS distance,
			       l.score AS lexical_score
			FROM dense d
			FULL OUTER JOIN lexical l ON d.chunk_id = l.chunk_id
		)
		SELECT c.chunk_id,
		       c.document_id,
		       c.memory_id,
		       c.tenant_id,
		       c.source_path,
		       c.heading_path::text,
		       c.start_line,
		       c.end_line,
		       c.text,
		       c.created_at,
		       m.origin_kind,
		       m.origin_provider,
		       m.origin_model,
		       f.distance,
		       f.lexical_score,
		       ts_headline('english', c.text, tsq.query,
		                   'MaxFragments=2, MinWords=8, MaxWords=32, StartSel=[, StopSel=]')
		FROM fused f
		JOIN memory_chunks c ON c.chunk_id = f.chunk_id
		JOIN memories m ON m.id = c.memory_id
		CROSS JOIN tsq
		WHERE c.tenant_id = $2
		  AND m.tenant_id = $2
		  AND m.review_state <> 'rejected'
		ORDER BY f.rrf DESC, f.distance ASC NULLS LAST
		LIMIT $5`

	hybridVectorSQL  = hybridHead + denseWithVector + hybridTail
	hybridLexicalSQL = hybridHead + denseWithoutVector + hybridTail
)

// HybridSearch is hybridSearch: dense pgvector recall fused with lexical tsquery
// recall by Reciprocal Rank Fusion. websearch_to_tsquery is deliberate: it cannot
// raise on user input. Read-only: no hit count bump. A nil queryEmbedding runs the
// lexical arm alone. Chunks with a NULL embedding are never in the dense arm but are
// in the lexical one.
//
// Every call is observed as memory.hybridSearch under luminavault.memory.*: Swift's
// only MemoryChunkRepository is built with RouteTelemetry(labelPrefix: "memory").
func (r *ChunkRepository) HybridSearch(ctx context.Context, tenant uuid.UUID, query string, queryEmbedding []float32, limit int, spaceID *uuid.UUID) ([]memorystore.SearchResult, error) {
	return telemetry.Observe(ctx, telemetry.Global.Route("memory"), "memory.hybridSearch",
		func(ctx context.Context) ([]memorystore.SearchResult, error) {
			return r.hybridSearch(ctx, tenant, query, queryEmbedding, limit, spaceID)
		})
}

func (r *ChunkRepository) hybridSearch(ctx context.Context, tenant uuid.UUID, query string, queryEmbedding []float32, limit int, spaceID *uuid.UUID) ([]memorystore.SearchResult, error) {
	limit = max(1, limit)
	sql, args := hybridLexicalSQL, []any{query, tenant, spaceID, limit * armDepthMultiplier, limit}
	if queryEmbedding != nil {
		sql, args = hybridVectorSQL, append(args, memorystore.VectorArg(queryEmbedding))
	}
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("chunkindex: hybrid search: %w", err)
	}
	defer rows.Close()

	out := []memorystore.SearchResult{}
	for rows.Next() {
		var (
			hit        memorystore.SearchResult
			citation   memorystore.Citation
			headings   *string
			originKind string
			distance   *float64
			lexical    *float64
		)
		if err := rows.Scan(&citation.ChunkID, &citation.DocumentID, &hit.ID, &hit.TenantID, &citation.Path, &headings,
			&citation.StartLine, &citation.EndLine, &hit.Content, &hit.CreatedAt, &originKind, &hit.Provider, &hit.Model,
			&distance, &lexical, &hit.Snippet); err != nil {
			return nil, fmt.Errorf("chunkindex: hybrid search: %w", err)
		}
		// Absent when only the lexical arm matched. 1.0 is "maximally far" on the
		// cosine scale: this hit was earned lexically, not semantically.
		hit.Distance = 1.0
		if distance != nil {
			hit.Distance = float32(*distance)
		}
		if lexical != nil {
			score := float32(*lexical)
			hit.LexicalScore = &score
		}
		hit.Source = memorystore.ParseSourceKind(originKind)
		citation.HeadingPath = DecodeHeadingPath(headings)
		hit.Citation = &citation
		out = append(out, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chunkindex: hybrid search: %w", err)
	}
	return out, nil
}

// EncodeHeadingPath is encodeHeadingPath. Swift's JSONEncoder and Go's escape "/" and
// "<" differently, but the column is jsonb, which stores the value and not the
// spelling. Nil encodes as [].
func EncodeHeadingPath(path []string) (string, error) {
	if path == nil {
		return "[]", nil
	}
	raw, err := json.Marshal(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// DecodeHeadingPath is decodeHeadingPath: anything unreadable is an empty path.
func DecodeHeadingPath(raw *string) []string {
	if raw == nil {
		return []string{}
	}
	var path []string
	if err := json.Unmarshal([]byte(*raw), &path); err != nil || path == nil {
		return []string{}
	}
	return path
}
