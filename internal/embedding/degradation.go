package embedding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// EmbedForWrite is embedForWrite: nil vector and nil error when the provider
// fails after its own retry and fallback chain, so the caller stores the row
// with embedding = NULL and Swift's heal pass fills it in later. A note saved
// without a vector is still listed and still found lexically; a note never
// saved is gone. operation names the write path in the log line (memory.upsert,
// vault.upload, ...). A cancelled ctx is returned as an error, never as a
// degraded save.
func EmbedForWrite(ctx context.Context, svc Service, text string, tenant uuid.UUID, operation string) ([]float32, error) {
	vec, err := svc.Embed(ctx, text, tenant)
	if err == nil {
		return vec, nil
	}
	if cancelled := cancellation(ctx, err); cancelled != nil {
		return nil, cancelled
	}
	slog.WarnContext(ctx, fmt.Sprintf("embedding.degraded op=%s tenant=%s: saved without an embedding, the heal pass fills it in later. %v", operation, tenant, err))
	return nil, nil
}

// EmbedForQuery is embedForQuery: nil vector and nil error when the provider
// fails, so the caller runs a lexical-only search.
func EmbedForQuery(ctx context.Context, svc Service, query string, tenant uuid.UUID, operation string) ([]float32, error) {
	vec, err := svc.Embed(ctx, query, tenant)
	if err == nil {
		return vec, nil
	}
	if cancelled := cancellation(ctx, err); cancelled != nil {
		return nil, cancelled
	}
	slog.WarnContext(ctx, fmt.Sprintf("embedding.degraded op=%s tenant=%s: query not embedded, searching lexically. %v", operation, tenant, err))
	return nil, nil
}

// EmbedBatchForWrite is embedBatchForWrite: a nil vector for every text when the
// batch fails. A provider that refused one chunk has almost always refused them
// all.
func EmbedBatchForWrite(ctx context.Context, svc Service, texts []string, tenant uuid.UUID, operation string) ([][]float32, error) {
	vecs, err := EmbedBatch(ctx, svc, texts, tenant)
	if err == nil {
		return vecs, nil
	}
	if cancelled := cancellation(ctx, err); cancelled != nil {
		return nil, cancelled
	}
	slog.WarnContext(ctx, fmt.Sprintf("embedding.degraded op=%s tenant=%s texts=%d: saved without embeddings, the heal pass fills them in later. %v", operation, tenant, len(texts), err))
	return make([][]float32, len(texts)), nil
}

// cancellation is EmbeddingDegradation.rethrowIfCancelled. A cancelled HTTP call
// surfaces as a network error, so ctx is checked as well as err. Deadlines on ctx
// count too; the adapters' own client timeout does not touch ctx and degrades.
func cancellation(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	return ctx.Err()
}
