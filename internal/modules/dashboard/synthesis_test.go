package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/dbtest"
	"lumina/internal/llm"
	"lumina/internal/migrations"
	"lumina/internal/modules/dashboard/db"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// InsightTests: insight persistence, SynthesisWorker's window math, parsing and
// idempotency. The transport is a stub; no request leaves the process.
func TestInsightsAgainstPostgres(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for name, fn := range map[string]func(*testing.T, *pgxpool.Pool){
		"SaveRetrieveRoundTrips":         testInsightRoundTrip,
		"StoresPeriodBoundaries":         testInsightPeriod,
		"QueryIsTenantScoped":            testInsightTenantScoped,
		"WeeklyJobNoOpWhenRowExists":     testWeeklyIdempotent,
		"PatternJobSkipsRecentRow":       testPatternCooldown,
		"PatternJobCapsAtMax":            testPatternCap,
		"ContradictionJobSkipsRecentRow": testContradictionCooldown,
		"ContradictionJobInsertsUpToCap": testContradictionCap,
		"WeeklyJobFalseWithoutMemories":  testWeeklyEmpty,
	} {
		t.Run(name, func(t *testing.T) { fn(t, pool) })
	}
}

func insightTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	slug := "ins-" + id.String()[:8]
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, username, password_hash, created_at, updated_at) VALUES ($1, $2, $3, 'stub', now(), now())`,
		id, slug+"@test.luminavault", slug)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'v', now(), now())`, id)
	require.NoError(t, err)
	return id
}

func insightMemory(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, content string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `INSERT INTO memories (id, tenant_id, content, created_at, updated_at) VALUES ($1, $2, $3, now(), now())`, id, tenant, content)
	require.NoError(t, err)
	return id
}

func listInsightRows(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, section *string) []db.ListInsightsRow {
	t.Helper()
	rows, err := db.New(pool).ListInsights(context.Background(), db.ListInsightsParams{TenantID: tenant, Section: section, RowLimit: 100})
	require.NoError(t, err)
	return rows
}

// stubSynthTransport returns a canned completion with content and counts calls.
type stubSynthTransport struct {
	mu      sync.Mutex
	content string
	calls   int
}

func (s *stubSynthTransport) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubSynthTransport) ChatCompletions(context.Context, []byte, string, string) (llm.Metadata, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	body, err := json.Marshal(map[string]any{
		"id": "test", "model": "test",
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": s.content}}},
	})
	return llm.Metadata{Data: body}, err
}

func (s *stubSynthTransport) ChatStream(context.Context, []byte, string, string) llm.Stream {
	return func(func(llm.Chunk, error) bool) {}
}

func synthWorker(pool *pgxpool.Pool, transport llm.ChatTransport, maxPatterns int) *SynthesisWorker {
	w := NewSynthesisWorker(pool, transport, "test-model", nil)
	if maxPatterns > 0 {
		w.MaxPatterns = maxPatterns
	}
	return w
}

// MARK: persistence

func testInsightRoundTrip(t *testing.T, pool *pgxpool.Pool) {
	tenant := insightTenant(t, pool)
	_, err := SaveInsight(context.Background(), pool, InsightInput{
		Tenant: tenant, Section: "patterns", Headline: "You run on Mondays",
		Summary: "Of 4 runs this month, 3 were Mondays.", Sources: []uuid.UUID{uuid.New(), uuid.New()},
	})
	require.NoError(t, err)
	rows := listInsightRows(t, pool, tenant, nil)
	require.Len(t, rows, 1)
	require.Equal(t, "You run on Mondays", rows[0].Headline)
	require.Equal(t, "patterns", rows[0].Section)
	require.Len(t, rows[0].SourceMemoryIds, 2)
	require.Nil(t, rows[0].DismissedAt)
}

func testInsightPeriod(t *testing.T, pool *pgxpool.Pool) {
	tenant := insightTenant(t, pool)
	start := time.Unix(1_700_000_000, 0).UTC()
	end := start.Add(7 * 24 * time.Hour)
	_, err := SaveInsight(context.Background(), pool, InsightInput{
		Tenant: tenant, Section: "this_week", Headline: "Your week, in two lines", Summary: "Lots of writing.",
		PeriodStart: &start, PeriodEnd: &end,
	})
	require.NoError(t, err)
	r := listInsightRows(t, pool, tenant, nil)[0]
	dto := insightRow{
		ID: r.ID, Section: r.Section, Headline: r.Headline, Summary: r.Summary, Sources: r.SourceMemoryIds,
		PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, Dismissed: r.DismissedAt, CreatedAt: r.CreatedAt,
	}.toDTO()
	require.Equal(t, "this_week", dto.Section)
	require.True(t, dto.PeriodStart.Equal(start))
	require.True(t, dto.PeriodEnd.Equal(end))
}

func testInsightTenantScoped(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	a, b := insightTenant(t, pool), insightTenant(t, pool)
	_, err := SaveInsight(ctx, pool, InsightInput{Tenant: a, Section: "patterns", Headline: "A", Summary: "a"})
	require.NoError(t, err)
	_, err = SaveInsight(ctx, pool, InsightInput{Tenant: b, Section: "patterns", Headline: "B", Summary: "b"})
	require.NoError(t, err)
	rows := listInsightRows(t, pool, a, nil)
	require.Len(t, rows, 1)
	require.Equal(t, "A", rows[0].Headline)
}

// MARK: window math (no DB)

func TestSynthesisWeeklyWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	window := WeeklyWindow(now)
	require.False(t, window.End.After(now))
	require.Equal(t, 7*24*time.Hour, window.End.Sub(window.Start))
}

func TestSynthesisHourAndWeekdayInUTC(t *testing.T) {
	// 1970-01-04 02:00:00 UTC, the first Sunday after the epoch.
	date := time.Unix(266_400, 0).In(time.FixedZone("X", -5*3600))
	require.Equal(t, 2, HourComponent(date))
	require.Equal(t, 1, WeekdayComponent(date))
}

// MARK: parsing

func envelope(content string) []byte {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
	return b
}

func TestSynthesisParseSynthesis(t *testing.T) {
	parsed, ok := ParseSynthesis(envelope(`{"headline": "H", "summary": "S"}`))
	require.True(t, ok)
	require.Equal(t, Synthesised{"H", "S"}, parsed)
}

func TestSynthesisParseSynthesisMissingFields(t *testing.T) {
	_, ok := ParseSynthesis(envelope(`{"headline": "", "summary": "S"}`))
	require.False(t, ok)
}

func TestSynthesisParsePatterns(t *testing.T) {
	parsed, ok := ParsePatterns(envelope(`{"patterns": [{"headline": "  A  ", "summary": "a"}, {"headline": "", "summary": "x"}, {"headline": "B", "summary": ""}]}`))
	require.True(t, ok)
	require.Len(t, parsed, 1)
	require.Equal(t, "A", parsed[0].Headline)
}

func TestSynthesisParseContradictions(t *testing.T) {
	parsed, ok := ParseContradictions(envelope(`{"contradictions": [{"headline": "  A  ", "summary": "a"}, {"headline": "", "summary": "x"}, {"headline": "B", "summary": ""}]}`))
	require.True(t, ok)
	require.Len(t, parsed, 1)
	require.Equal(t, "A", parsed[0].Headline)
}

// MARK: worker idempotency

func testWeeklyIdempotent(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := insightTenant(t, pool)
	insightMemory(t, pool, tenant, "ran 5k")
	transport := &stubSynthTransport{content: `{"headline": "Week 21", "summary": "Lots of running."}`}
	w := synthWorker(pool, transport, 0)
	window := WeeklyWindow(time.Now())
	first, err := w.RunWeeklyJob(ctx, tenant, "u", window)
	require.NoError(t, err)
	require.True(t, first)
	again, err := w.RunWeeklyJob(ctx, tenant, "u", window)
	require.NoError(t, err)
	require.False(t, again)
	require.Equal(t, 1, transport.count())
}

func testPatternCooldown(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := insightTenant(t, pool)
	for range 5 {
		insightMemory(t, pool, tenant, "memo")
	}
	_, err := SaveInsight(ctx, pool, InsightInput{Tenant: tenant, Section: "patterns", Headline: "recent", Summary: "still fresh"})
	require.NoError(t, err)
	transport := &stubSynthTransport{content: `{"patterns": [{"headline": "X", "summary": "x"}]}`}
	inserted, err := synthWorker(pool, transport, 0).RunPatternJob(ctx, tenant, "u", time.Now())
	require.NoError(t, err)
	require.Zero(t, inserted)
	require.Zero(t, transport.count())
}

func testPatternCap(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := insightTenant(t, pool)
	for range 5 {
		insightMemory(t, pool, tenant, "memo")
	}
	transport := &stubSynthTransport{content: `{"patterns": [{"headline": "A", "summary": "a"}, {"headline": "B", "summary": "b"}, {"headline": "C", "summary": "c"}, {"headline": "D", "summary": "d"}, {"headline": "E", "summary": "e"}]}`}
	inserted, err := synthWorker(pool, transport, 2).RunPatternJob(ctx, tenant, "u", time.Now())
	require.NoError(t, err)
	require.Equal(t, 2, inserted)
	require.Len(t, listInsightRows(t, pool, tenant, nil), 2)
}

func testContradictionCooldown(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := insightTenant(t, pool)
	for range 5 {
		insightMemory(t, pool, tenant, "memo")
	}
	_, err := SaveInsight(ctx, pool, InsightInput{Tenant: tenant, Section: "contradictions", Headline: "recent", Summary: "still fresh"})
	require.NoError(t, err)
	transport := &stubSynthTransport{content: `{"contradictions": [{"headline": "X", "summary": "x"}]}`}
	inserted, err := synthWorker(pool, transport, 0).RunContradictionJob(ctx, tenant, time.Now())
	require.NoError(t, err)
	require.Zero(t, inserted)
	require.Zero(t, transport.count())
}

func testContradictionCap(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := insightTenant(t, pool)
	var memoryIDs []uuid.UUID
	for i := range 5 {
		memoryIDs = append(memoryIDs, insightMemory(t, pool, tenant, fmt.Sprintf("memo %d", i)))
	}
	for i := range 3 {
		left, right, edge := uuid.New(), uuid.New(), uuid.New()
		_, err := pool.Exec(ctx, `
INSERT INTO knowledge_nodes (id, tenant_id, kind, canonical_key, label, confidence)
VALUES ($1, $3, 'claim', $4, $5, 1), ($2, $3, 'claim', $6, $7, 1)`,
			left, right, tenant, fmt.Sprintf("left-%d", i), fmt.Sprintf("Position %d", i), fmt.Sprintf("right-%d", i), fmt.Sprintf("Not position %d", i))
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `
INSERT INTO knowledge_edges (id, tenant_id, from_node_id, to_node_id, predicate, state, confidence, rationale, evidence_fingerprint)
VALUES ($1, $2, $3, $4, 'contradicts', 'suggested', $5, $6, $7)`,
			edge, tenant, left, right, 0.9-float64(i)*0.1, fmt.Sprintf("Exact conflict %d", i), fmt.Sprintf("conflict-%d", i))
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO knowledge_evidence (id, tenant_id, edge_id, memory_id, quote) VALUES ($1, $2, $3, $4, $5)`,
			uuid.New(), tenant, edge, memoryIDs[i], fmt.Sprintf("evidence %d", i))
		require.NoError(t, err)
	}
	transport := &stubSynthTransport{content: `{"contradictions": [{"headline": "A", "summary": "a"}, {"headline": "B", "summary": "b"}, {"headline": "C", "summary": "c"}]}`}
	inserted, err := synthWorker(pool, transport, 2).RunContradictionJob(ctx, tenant, time.Now())
	require.NoError(t, err)
	require.Equal(t, 2, inserted)
	section := "contradictions"
	rows := listInsightRows(t, pool, tenant, &section)
	require.Len(t, rows, 2)
	for _, r := range rows {
		require.Len(t, r.SourceMemoryIds, 1)
		require.Contains(t, memoryIDs, r.SourceMemoryIds[0])
		var edge *uuid.UUID
		require.NoError(t, pool.QueryRow(ctx, `SELECT knowledge_edge_id FROM insights WHERE id = $1`, r.ID).Scan(&edge))
		require.NotNil(t, edge)
	}
	require.Zero(t, transport.count())
}

func testWeeklyEmpty(t *testing.T, pool *pgxpool.Pool) {
	tenant := insightTenant(t, pool)
	transport := &stubSynthTransport{content: `{"headline": "x", "summary": "y"}`}
	ok, err := synthWorker(pool, transport, 0).RunWeeklyJob(context.Background(), tenant, "u", WeeklyWindow(time.Now()))
	require.NoError(t, err)
	require.False(t, ok)
}
