package chunkindex

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultBackfillBatchSize is ChunkBackfillService.defaultBatchSize.
const DefaultBackfillBatchSize = 50

// BackfillResult is ChunkBackfillResult.
type BackfillResult struct {
	TenantID       string `json:"tenantID"`
	Scanned        int    `json:"scanned"`
	Indexed        int    `json:"indexed"`
	Chunks         int    `json:"chunks"`
	AlreadyIndexed int    `json:"alreadyIndexed"`
	Failed         int    `json:"failed"`
}

// Backfiller is ChunkBackfillService: it chunks memories that have no chunk
// rows yet, a bounded batch per call, ordered by memory id so a rerun resumes.
type Backfiller struct {
	pool    *pgxpool.Pool
	paths   vaultpaths.Paths
	indexer *Indexer
}

// NewBackfiller returns a Backfiller.
func NewBackfiller(pool *pgxpool.Pool, paths vaultpaths.Paths, indexer *Indexer) *Backfiller {
	return &Backfiller{pool: pool, paths: paths, indexer: indexer}
}

// Backfill is ChunkBackfillService.backfill.
func (b *Backfiller) Backfill(ctx context.Context, tenant uuid.UUID, batchSize int) (BackfillResult, error) {
	limit := max(1, min(batchSize, 500))
	rows, err := b.pool.Query(ctx, `
		SELECT m.id, m.content, m.space_id, m.source_vault_file_id, v.path
		FROM memories m
		LEFT JOIN vault_files v
		    ON v.id = m.source_vault_file_id
		   AND v.tenant_id = m.tenant_id
		LEFT JOIN memory_chunks c
		    ON c.memory_id = m.id
		   AND c.tenant_id = m.tenant_id
		WHERE m.tenant_id = $1
		  AND c.chunk_id IS NULL
		  AND m.review_state <> 'rejected'
		ORDER BY m.id
		LIMIT $2`, tenant, limit)
	if err != nil {
		return BackfillResult{}, fmt.Errorf("chunkindex: backfill candidates: %w", err)
	}
	type candidate struct {
		memoryID    uuid.UUID
		content     string
		spaceID     *uuid.UUID
		vaultFileID *uuid.UUID
		sourcePath  *string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.memoryID, &c.content, &c.spaceID, &c.vaultFileID, &c.sourcePath); err != nil {
			rows.Close()
			return BackfillResult{}, fmt.Errorf("chunkindex: backfill candidates: %w", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return BackfillResult{}, fmt.Errorf("chunkindex: backfill candidates: %w", err)
	}

	result := BackfillResult{TenantID: ids.Upper(tenant), Scanned: len(candidates)}
	for _, c := range candidates {
		source, ok := b.readSource(tenant, c.sourcePath)
		if !ok {
			source = c.content
		}
		if swiftstr.TrimWhitespacesAndNewlines(source) == "" {
			continue
		}
		written, err := b.indexer.Index(ctx, IndexParams{
			Tenant: tenant, MemoryID: c.memoryID, VaultFileID: c.vaultFileID,
			SpaceID: c.spaceID, SourcePath: c.sourcePath, Content: source,
		})
		if err != nil {
			result.Failed++
			slog.WarnContext(ctx, fmt.Sprintf("chunk.backfill.failed tenant=%s memory=%s: %v", ids.Upper(tenant), ids.Upper(c.memoryID), err))
			continue
		}
		if written > 0 {
			result.Indexed++
			result.Chunks += written
		}
	}
	slog.InfoContext(ctx, fmt.Sprintf("chunk.backfill tenant=%s scanned=%d indexed=%d chunks=%d failed=%d",
		ids.Upper(tenant), result.Scanned, result.Indexed, result.Chunks, result.Failed))
	return result, nil
}

// readSource reads the raw vault file inside the tenant's root, or reports false.
func (b *Backfiller) readSource(tenant uuid.UUID, path *string) (string, bool) {
	if path == nil {
		return "", false
	}
	target, err := vaultfiles.ResolveInside(b.paths.Raw(tenant), *path)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(target) //nolint:gosec // resolved inside the tenant's raw root
	if err != nil {
		return "", false
	}
	return vaultfiles.DecodeUTF8(data)
}
