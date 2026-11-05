package memorycompile

import (
	"context"
	"encoding/json"
	"lumina/internal/memorystore"
	"lumina/internal/swiftstr"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExtractedMemory is MemoryCompileService.ExtractedMemory. Kind and Source are nil on a
// bare string, or a kind this server doesn't know.
type ExtractedMemory struct {
	Text   string
	Kind   *string
	Source *string
}

// ParseExtractedMemories is parseExtractedMemories.
func ParseExtractedMemories(raw string) []string {
	items := ParseExtractedItems(raw)
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Text
	}
	return out
}

func stripFences(raw string) string {
	s := swiftstr.TrimWhitespacesAndNewlines(raw)
	if strings.Contains(s, "```") {
		s = strings.ReplaceAll(s, "```json", "")
		s = strings.ReplaceAll(s, "```", "")
		s = swiftstr.TrimWhitespacesAndNewlines(s)
	}
	return s
}

func decodeAny(s string) (any, bool) {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return nil, false
	}
	return v, true
}

func outerBlock(s, open, closing string) (string, bool) {
	lo, hi := strings.Index(s, open), strings.LastIndex(s, closing)
	if lo < 0 || hi < 0 || lo >= hi {
		return "", false
	}
	return s[lo : hi+len(closing)], true
}

func extractedFrom(v any) (ExtractedMemory, bool) {
	switch t := v.(type) {
	case string:
		return ExtractedMemory{Text: t}, true
	case map[string]any:
		raw, ok := t["text"]
		if !ok || raw == nil {
			raw = t["content"]
		}
		text, ok := raw.(string)
		if !ok {
			return ExtractedMemory{}, false
		}
		out := ExtractedMemory{Text: text}
		if k, ok := t["kind"].(string); ok && knownKind(k) {
			out.Kind = &k
		}
		if src, ok := t["source"].(string); ok {
			out.Source = &src
		}
		return out, true
	}
	return ExtractedMemory{}, false
}

func knownKind(k string) bool {
	switch k {
	case memorystore.KindPreference, memorystore.KindDecision, memorystore.KindFact, memorystore.KindNote:
		return true
	}
	return false
}

func memoriesFrom(v any) ([]ExtractedMemory, bool) {
	var arr []any
	switch t := v.(type) {
	case map[string]any:
		a, ok := t["memories"].([]any)
		if !ok {
			return nil, false
		}
		arr = a
	case []any:
		arr = t
	default:
		return nil, false
	}
	out := []ExtractedMemory{}
	for _, e := range arr {
		if m, ok := extractedFrom(e); ok {
			out = append(out, m)
		}
	}
	return out, true
}

// ParseExtractedItems is parseExtractedItems: the canonical object, an array of
// objects, a bare array; code fences and surrounding prose are tolerated.
func ParseExtractedItems(raw string) []ExtractedMemory {
	s := stripFences(raw)
	if v, ok := decodeAny(s); ok {
		if out, ok := memoriesFrom(v); ok {
			return out
		}
	}
	for _, pair := range [][2]string{{"{", "}"}, {"[", "]"}} {
		if slice, ok := outerBlock(s, pair[0], pair[1]); ok {
			if v, ok := decodeAny(slice); ok {
				if out, ok := memoriesFrom(v); ok {
					return out
				}
			}
		}
	}
	return []ExtractedMemory{}
}

// CommitmentsService's limits.
const (
	maxCommitmentChars = 500
	maxPerCompile      = 20
)

// ExtractedCommitment is CommitmentsService.Extracted.
type ExtractedCommitment struct {
	Text   string
	Due    *time.Time
	Source *string
}

// NewCommitment is CommitmentsService.NewCommitment.
type NewCommitment struct {
	Text              string
	Due               *time.Time
	MemoryID          *uuid.UUID
	SourceVaultFileID *uuid.UUID
}

func commitmentsFrom(v any) ([]ExtractedCommitment, bool) {
	dict, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	arr, ok := dict["commitments"].([]any)
	if !ok {
		return []ExtractedCommitment{}, true
	}
	out := []ExtractedCommitment{}
	for _, entry := range arr {
		var text string
		var item map[string]any
		switch t := entry.(type) {
		case string:
			text = t
		case map[string]any:
			item = t
			raw, present := t["text"]
			if !present || raw == nil {
				raw = t["content"]
			}
			s, isString := raw.(string)
			if !isString {
				continue
			}
			text = s
		default:
			continue
		}
		trimmed := swiftstr.TrimWhitespacesAndNewlines(text)
		if trimmed == "" {
			continue
		}
		c := ExtractedCommitment{Text: swiftstr.Prefix(trimmed, maxCommitmentChars)}
		if item != nil {
			c.Due = ParseDue(item["due"])
			if src, ok := item["source"].(string); ok {
				c.Source = &src
			}
		}
		out = append(out, c)
	}
	return out, true
}

// ParseExtractedCommitments is parseExtractedCommitments: a missing or malformed
// commitments key is no commitments.
func ParseExtractedCommitments(raw string) []ExtractedCommitment {
	s := stripFences(raw)
	if v, ok := decodeAny(s); ok {
		if out, ok := commitmentsFrom(v); ok {
			return out
		}
	}
	if slice, ok := outerBlock(s, "{", "}"); ok {
		if v, ok := decodeAny(slice); ok {
			if out, ok := commitmentsFrom(v); ok {
				return out
			}
		}
	}
	return []ExtractedCommitment{}
}

// ParseDue is CommitmentsService.parseDue: a calendar day lands at noon UTC so it stays
// the same day in every timezone. Otherwise ISO-8601. Anything else is nil, and the
// commitment is kept.
func ParseDue(raw any) *time.Time {
	s, ok := raw.(string)
	if !ok {
		return nil
	}
	trimmed := swiftstr.TrimWhitespacesAndNewlines(s)
	if trimmed == "" {
		return nil
	}
	if swiftstr.Count(trimmed) == 10 {
		day, err := time.ParseInLocation("2006-01-02", trimmed, time.UTC)
		if err != nil {
			return nil
		}
		noon := day.Add(12 * time.Hour)
		return &noon
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999Z07:00", time.RFC3339} {
		if t, err := time.Parse(layout, trimmed); err == nil {
			return &t
		}
	}
	return nil
}

// NormalizeCommitment is CommitmentsService.normalize: lowercase, whitespace collapsed,
// and trailing or leading ".!;:," and spaces trimmed.
func NormalizeCommitment(text string) string {
	collapsed := strings.Join(strings.FieldsFunc(strings.ToLower(text), swiftstr.IsWhitespaceOrNewline), " ")
	return strings.TrimFunc(collapsed, func(r rune) bool {
		return strings.ContainsRune(".!;:,", r) || swiftstr.IsWhitespace(r)
	})
}

// RecordCommitments is CommitmentsService.record: open commitments, at most 20 from one
// compile. One already open with the same normalized text is left alone, so a recompile
// adds nothing. It returns how many were new.
func RecordCommitments(ctx context.Context, pool *pgxpool.Pool, tenant uuid.UUID, items []NewCommitment, now time.Time) (int, error) {
	inserted := 0
	for i, item := range items {
		if i == maxPerCompile {
			break
		}
		normalized := NormalizeCommitment(item.Text)
		if normalized == "" {
			continue
		}
		var id uuid.UUID
		err := pool.QueryRow(ctx, `
			INSERT INTO commitments (tenant_id, memory_id, source_vault_file_id, text, normalized_text, due_at, state, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'open', $7)
			ON CONFLICT (tenant_id, normalized_text) WHERE state = 'open' DO NOTHING
			RETURNING id`, tenant, item.MemoryID, item.SourceVaultFileID, item.Text, normalized, item.Due, now).Scan(&id)
		if err == pgx.ErrNoRows { //nolint:errorlint // pgx returns the sentinel itself
			continue
		}
		if err != nil {
			return inserted, err
		}
		inserted++
	}
	return inserted, nil
}

// RecordMemoryCompile is UsageMetricsService.recordMemoryCompile: a run and its file
// count, keyed by the run so a retry can't double count. Failures are logged by the caller.
func RecordMemoryCompile(ctx context.Context, pool *pgxpool.Pool, tenant, runID uuid.UUID, files int) error {
	run := strings.ToUpper(runID.String())
	metadata := `{"run_id":"` + run + `"}`
	const stmt = `
		INSERT INTO usage_events (tenant_id, metric, amount, source, idempotency_key, metadata)
		VALUES ($1, $2, $3, 'memory_compile', $4, $5::jsonb)
		ON CONFLICT (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL
		DO UPDATE SET amount = EXCLUDED.amount`
	if _, err := pool.Exec(ctx, stmt, tenant, "memory_compile_run", int64(1), "memory_compile_run:"+run, metadata); err != nil {
		return err
	}
	_, err := pool.Exec(ctx, stmt, tenant, "memory_compile_file", int64(max(0, files)), "memory_compile_file:"+run, metadata)
	return err
}
