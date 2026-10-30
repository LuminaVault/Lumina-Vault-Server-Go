package apple

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/modules/apple/db"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Embedder is Swift's EmbeddingService as PhotoIndexController uses it.
type Embedder interface {
	Embed(ctx context.Context, text string, tenant uuid.UUID) ([]float32, error)
}

// errProviderNotPorted is returned for every embed when `embedding.provider`
// names a real provider. Only the deterministic stub is ported; writing its
// hash vectors into a production index would silently break photo recall, so
// Go fails the request (a 500, as Swift answers a failed embed) instead.
var errProviderNotPorted = errors.New("embedding provider not ported to Go")

// newEmbedder mirrors EmbeddingProviderRegistry.bootstrap for what Go has:
// EmbeddingUsageTracker (monthly cap + usage rows) around the active provider.
// An unset or unknown provider is "deterministic", as in Swift.
func newEmbedder(q *db.Queries, opts Options, now func() time.Time) Embedder {
	var inner Embedder = deterministic{}
	switch kind := strings.ToLower(strings.TrimSpace(opts.EmbeddingProvider)); kind {
	case "openai", "openrouter", "open_router", "open-router", "nomic",
		"hermeslocal", "hermes_local", "hermes-local", "local", "localhermes":
		slog.Warn("embedding provider is not ported to Go; /v1/photos/index embeds will fail", "provider", kind)
		inner = notPorted{}
	}
	return &usageTracker{inner: inner, q: q, cap: opts.EmbeddingMonthlyCap, now: now}
}

// deterministic is DeterministicEmbeddingService: 1536 dims, byte i%len
// mapped to b/127.5 - 1.
type deterministic struct{}

func (deterministic) Embed(_ context.Context, text string, _ uuid.UUID) ([]float32, error) {
	v := make([]float32, 1536)
	b := []byte(text)
	if len(b) == 0 {
		return v, nil
	}
	for i := range v {
		v[i] = float32(b[i%len(b)])/127.5 - 1.0
	}
	return v, nil
}

type notPorted struct{}

func (notPorted) Embed(context.Context, string, uuid.UUID) ([]float32, error) {
	return nil, errProviderNotPorted
}

// usageTracker is EmbeddingUsageTracker: refuse when this month's usage has
// reached the cap, embed, then add an estimate (chars/4) to embedding_usage.
// A failed usage write is logged, never returned.
type usageTracker struct {
	inner Embedder
	q     *db.Queries
	cap   int64
	now   func() time.Time
}

func (u *usageTracker) Embed(ctx context.Context, text string, tenant uuid.UUID) ([]float32, error) {
	if u.cap > 0 {
		used := int64(0)
		if row, err := u.q.EmbeddingUsageFor(ctx, db.EmbeddingUsageForParams{TenantID: tenant, YearMonth: yearMonth(u.now())}); err == nil {
			used = row.TokensUsed
		}
		if used >= u.cap {
			slog.WarnContext(ctx, "embedding cap exceeded", "tenant", tenant, "used", used, "cap", u.cap)
			return nil, fmt.Errorf("embedding cap exceeded: tenant=%s used=%d cap=%d", tenant, used, u.cap)
		}
	}
	vec, err := u.inner.Embed(ctx, text, tenant)
	if err != nil {
		return nil, err
	}
	if err := u.increment(ctx, tenant, EstimateTokens(text)); err != nil {
		slog.WarnContext(ctx, "embedding usage increment failed", "error", err)
	}
	return vec, nil
}

// increment is EmbeddingUsageRepository.increment: update the month's row, or
// insert it, retrying once if a concurrent insert won.
func (u *usageTracker) increment(ctx context.Context, tenant uuid.UUID, tokens int64) error {
	if tokens <= 0 {
		return nil
	}
	now := u.now()
	ym := yearMonth(now)
	for attempt := range 2 {
		row, err := u.q.EmbeddingUsageFor(ctx, db.EmbeddingUsageForParams{TenantID: tenant, YearMonth: ym})
		switch {
		case err == nil:
			return u.q.SetEmbeddingUsage(ctx, db.SetEmbeddingUsageParams{ID: row.ID, TokensUsed: row.TokensUsed + tokens, UpdatedAt: &now})
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		err = u.q.InsertEmbeddingUsage(ctx, db.InsertEmbeddingUsageParams{ID: uuid.New(), TenantID: tenant, YearMonth: ym, TokensUsed: tokens, CreatedAt: &now})
		if err == nil || attempt == 1 {
			return err
		}
	}
	return nil
}

// EstimateTokens is EmbeddingUsageTracker.estimateTokens: max(1, (chars+3)/4).
// Swift counts grapheme clusters; Go counts runes, which differs only for
// combining sequences and multi-scalar emoji.
func EstimateTokens(text string) int64 {
	return max(1, (int64(utf8.RuneCountInString(text))+3)/4)
}

func yearMonth(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%04d-%02d", t.Year(), int(t.Month()))
}

// EmbedSource is PhotoIndexController.embedSource: trimmed OCR text, then
// "Scene: a, b" on its own line. ok is false when there is nothing to embed.
func EmbedSource(ocr *string, tags []string) (string, bool) {
	var parts []string
	if ocr != nil {
		if t := trimSpace(*ocr); t != "" {
			parts = append(parts, t)
		}
	}
	if joined := strings.Join(tags, ", "); joined != "" {
		parts = append(parts, "Scene: "+joined)
	}
	combined := strings.Join(parts, "\n")
	return combined, combined != ""
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
