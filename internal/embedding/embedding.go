// Package embedding is the Go port of LuminaVaultServer Sources/App/Services/Embedding
// and Services/EmbeddingService.swift: the provider registry, the OpenAI,
// OpenRouter, Nomic and per-tenant Hermes adapters, the fallback chain, the
// monthly usage tracker, the degrade-to-NULL helpers and BYOK routing.
//
// Callers depend on Service and get one from New. EmbeddingReembedService (the
// heal and re-embed pass) stays in Swift (jobs.md): rows Go saves with a NULL
// vector are healed by the Swift worker.
package embedding

import (
	"context"
	"lumina/internal/swiftstr"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
)

// Dimensions is the pgvector column width every adapter answers with.
const Dimensions = 1536

// maxConcurrentEmbeds is embedBatch's default maxConcurrent. No Swift caller
// passes another value.
const maxConcurrentEmbeds = 4

// Service is Swift's EmbeddingService.
type Service interface {
	Embed(ctx context.Context, text string, tenant uuid.UUID) ([]float32, error)
}

// EmbedBatch is embedBatch: at most four embeds in flight, results in input
// order, and any single failure fails the batch (a document indexed with a hole
// in its chunk list would silently miss content). The first failure cancels the
// others, as leaving Swift's task group does.
func EmbedBatch(ctx context.Context, svc Service, texts []string, tenant uuid.UUID) ([][]float32, error) {
	out := make([][]float32, len(texts))
	if len(texts) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		next     atomic.Int64
		once     sync.Once
		firstErr error
	)
	for range min(maxConcurrentEmbeds, len(texts)) {
		wg.Go(func() {
			for ctx.Err() == nil {
				i := int(next.Add(1) - 1)
				if i >= len(texts) {
					return
				}
				vec, err := svc.Embed(ctx, texts[i], tenant)
				if err != nil {
					once.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
				out[i] = vec
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Deterministic is DeterministicEmbeddingService: Dimensions floats, byte
// i%len mapped to b/127.5 - 1. Empty text is the zero vector.
type Deterministic struct{}

func (Deterministic) Embed(_ context.Context, text string, _ uuid.UUID) ([]float32, error) {
	v := make([]float32, Dimensions)
	b := []byte(text)
	if len(b) == 0 {
		return v, nil
	}
	for i := range v {
		v[i] = float32(b[i%len(b)])/127.5 - 1.0
	}
	return v, nil
}

// EstimateTokens is EmbeddingUsageTracker.estimateTokens: max(1, (chars+3)/4),
// where chars is Swift's text.count (grapheme clusters).
func EstimateTokens(text string) int64 {
	return max(1, (int64(swiftstr.Count(text))+3)/4)
}

// FormatVector is MemoryRepository.formatVector: "[" + String(Float) joined by
// "," + "]", with Swift's shortest round-trip digits and its ".0" on whole
// numbers. pgvector parses either spelling to the same float4, so only the
// stored value has to agree.
func FormatVector(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		s := strconv.FormatFloat(float64(f), 'g', -1, 32)
		if !strings.ContainsAny(s, ".eIN") {
			s += ".0"
		}
		b.WriteString(s)
	}
	b.WriteByte(']')
	return b.String()
}
