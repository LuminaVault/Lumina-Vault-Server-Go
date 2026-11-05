package chunkindex

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/embedding"

	"github.com/google/uuid"
)

// Indexer is DocumentChunkIndexer: the one place that owns chunk, embed, store, so
// ingest paths cannot drift into different chunking.
//
// Indexing is non-fatal at its call sites. The memories row and its whole-document
// embedding are written first and independently, and Hybrid unions the document arm
// with the chunk arm, so a document whose chunking failed is still findable, just
// without line-level citations.
type Indexer struct {
	chunks     *ChunkRepository
	embeddings embedding.Service
	links      *LinkRepository
}

// NewIndexer builds an Indexer. links may be nil: chunks are still written and links
// are not extracted.
func NewIndexer(chunks *ChunkRepository, embeddings embedding.Service, links *LinkRepository) *Indexer {
	return &Indexer{chunks: chunks, embeddings: embeddings, links: links}
}

// IndexParams are index's arguments.
type IndexParams struct {
	Tenant      uuid.UUID
	MemoryID    uuid.UUID
	VaultFileID *uuid.UUID
	SpaceID     *uuid.UUID
	// SourcePath is the vault-relative path used in citations. Nil for memories with
	// no backing file: those chunks are searchable but cite only heading and lines.
	SourcePath *string
	Content    string
}

// Index is index: replace the document's links (when it has a file), chunk it, embed
// each chunk, and replace the memory's chunks. It returns the number of chunks. An
// empty body clears stale chunks. A provider failure stores the chunks without vectors
// instead of dropping them; only a cancelled ctx comes back as an error from that step.
func (x *Indexer) Index(ctx context.Context, p IndexParams) (int, error) {
	if x.links != nil && p.VaultFileID != nil {
		if err := x.links.ReplaceLinks(ctx, p.Tenant, *p.VaultFileID, p.SourcePath, ExtractWikilinks(p.Content)); err != nil {
			return 0, err
		}
	}

	chunks := Chunk(p.Content)
	if len(chunks) == 0 {
		return 0, x.chunks.DeleteChunks(ctx, p.Tenant, p.MemoryID)
	}

	// With no file the path is the memory's uuidString, uppercase like the tenant's.
	path := swiftUUIDString(p.MemoryID)
	if p.SourcePath != nil {
		path = *p.SourcePath
	}
	documentID := DocumentID(p.Tenant, path)

	texts := make([]string, len(chunks))
	for i, chunk := range chunks {
		texts[i] = chunk.Text
	}
	vectors, err := embedding.EmbedBatchForWrite(ctx, x.embeddings, texts, p.Tenant, "chunk.index")
	if err != nil {
		return 0, fmt.Errorf("chunkindex: embed: %w", err)
	}
	if err := x.chunks.ReplaceChunks(ctx, ReplaceChunksParams{
		Tenant:      p.Tenant,
		MemoryID:    p.MemoryID,
		DocumentID:  documentID,
		VaultFileID: p.VaultFileID,
		SpaceID:     p.SpaceID,
		SourcePath:  p.SourcePath,
		Chunks:      chunks,
		Embeddings:  vectors,
	}); err != nil {
		return 0, err
	}
	return len(chunks), nil
}

// IndexBestEffort is indexBestEffort: Index with failures logged and swallowed, the
// shape every ingest path wants.
func (x *Indexer) IndexBestEffort(ctx context.Context, p IndexParams) {
	count, err := x.Index(ctx, p)
	if err != nil {
		path := "-"
		if p.SourcePath != nil {
			path = *p.SourcePath
		}
		slog.ErrorContext(ctx, fmt.Sprintf("chunk.index.failed tenant=%s memory=%s path=%s: %v", p.Tenant, p.MemoryID, path, err))
		return
	}
	slog.DebugContext(ctx, fmt.Sprintf("chunk.index tenant=%s memory=%s chunks=%d", p.Tenant, p.MemoryID, count))
}
