package chunkindex

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/memorystore"

	"github.com/google/uuid"
)

// Hybrid is HybridMemorySearch, the retrieval entry point: chunk-level hybrid search
// unioned with whole-memory search. Keeping the document arm is what makes chunk
// indexing safe to fail: a memory whose chunks are missing is still retrievable.
type Hybrid struct {
	memories *memorystore.Repository
	chunks   *ChunkRepository
}

// NewHybrid builds the search.
func NewHybrid(memories *memorystore.Repository, chunks *ChunkRepository) *Hybrid {
	return &Hybrid{memories: memories, chunks: chunks}
}

// Search runs both arms at once and merges them. query is the raw user text. A nil
// queryEmbedding (embedding.EmbedForQuery could not embed it) makes both arms search
// lexically, so an outage costs ranking quality instead of failing the request. Only
// the document arm counts hits. A chunk arm failure degrades to empty and is logged; a
// document arm failure is returned.
func (h *Hybrid) Search(ctx context.Context, tenant uuid.UUID, query string, queryEmbedding []float32, limit int, opts memorystore.SearchOptions) ([]memorystore.SearchResult, error) {
	chunkHits := make(chan []memorystore.SearchResult, 1)
	go func() {
		hits, err := h.chunks.HybridSearch(ctx, tenant, query, queryEmbedding, limit, opts.SpaceID)
		if err != nil {
			slog.ErrorContext(ctx, fmt.Sprintf("memory.search.chunkArmFailed tenant=%s: %v", tenant, err))
			hits = nil
		}
		chunkHits <- hits
	}()
	documents, err := h.memories.SearchDocuments(ctx, tenant, query, queryEmbedding, limit, opts)
	chunks := <-chunkHits
	if err != nil {
		return nil, err
	}
	return MergeHits(chunks, documents, limit), nil
}

// MergeHits is merge: de-duplicates by memory id, preferring the citation-bearing hit.
// Chunk hits keep their fused order and document hits fill the remaining slots.
func MergeHits(chunks, documents []memorystore.SearchResult, limit int) []memorystore.SearchResult {
	out := make([]memorystore.SearchResult, 0, max(limit, 0))
	seen := make(map[uuid.UUID]struct{}, max(limit, 0))
	for _, arm := range [][]memorystore.SearchResult{chunks, documents} {
		for _, hit := range arm {
			if len(out) >= limit {
				return out
			}
			if _, dup := seen[hit.ID]; dup {
				continue
			}
			seen[hit.ID] = struct{}{}
			out = append(out, hit)
		}
	}
	return out
}
