package memoryagent

import (
	"context"
	"encoding/json"
	"lumina/internal/embedding"
	"lumina/internal/memorystore"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hermesService(pool *pgxpool.Pool, transport *scriptedTransport, iterations int) *Service {
	return NewService(ServiceOptions{
		Transport: transport, Memories: memorystore.New(pool, nil), Embeddings: embedding.Deterministic{},
		DefaultModel: "test", MaxToolIterations: iterations,
	})
}

func storedMemories(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID) []memorystore.Memory {
	t.Helper()
	rows, err := memorystore.New(pool, nil).List(context.Background(), tenant)
	require.NoError(t, err)
	return rows
}

func TestUpsertDispatchesMemoryUpsertAndPersists(t *testing.T) {
	pool := migratedPool(t)
	tenant := createTenant(t, pool)
	transport := &scriptedTransport{steps: []step{
		toolStep("memory_upsert", `{"content":"alice prefers tea over coffee"}`),
		plainStep("Saved that you prefer tea over coffee."),
	}}
	result, err := hermesService(pool, transport, 0).Upsert(context.Background(), tenant, "alice", "", "I prefer tea over coffee.")
	require.NoError(t, err)
	assert.Equal(t, "Saved that you prefer tea over coffee.", result.Summary)
	assert.Equal(t, "alice prefers tea over coffee", result.Memory.Content)

	rows := storedMemories(t, pool, tenant)
	require.Len(t, rows, 1)
	assert.Equal(t, "alice prefers tea over coffee", rows[0].Content)

	// HER-183: every call pins the session key, and no session id.
	require.Len(t, transport.calls, 2)
	for _, call := range transport.calls {
		assert.Equal(t, "alice", call.sessionKey)
		assert.Empty(t, call.session)
	}
}

func TestSearchDispatchesSessionSearchAndReturnsHits(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	tenant := createTenant(t, pool)
	repo := memorystore.New(pool, nil)
	for _, content := range []string{"favourite drink: jasmine tea", "weekly sleep average: 7.2 hours"} {
		vec, err := embedding.Deterministic{}.Embed(ctx, content, tenant)
		require.NoError(t, err)
		_, err = repo.CreateEmbedded(ctx, tenant, content, vec)
		require.NoError(t, err)
	}
	transport := &scriptedTransport{steps: []step{
		toolStep("session_search", `{"query":"what does the user drink","limit":3}`),
		plainStep("Your notes mention jasmine tea as the favourite drink."),
	}}
	answer, err := hermesService(pool, transport, 0).Search(ctx, tenant, "alice", "", "what does the user drink", 5)
	require.NoError(t, err)
	assert.Contains(t, answer.Summary, "jasmine")
	require.Len(t, answer.Hits, 2) // both rows return; ANN order depends on the embedding
	found := false
	for _, hit := range answer.Hits {
		found = found || strings.Contains(hit.Content, "jasmine")
	}
	assert.True(t, found)
}

func TestPlainAssistantContentSkipsToolDispatch(t *testing.T) {
	pool := migratedPool(t)
	tenant := createTenant(t, pool)
	transport := &scriptedTransport{steps: []step{plainStep("Got it.")}}
	_, err := hermesService(pool, transport, 0).Upsert(context.Background(), tenant, "bob", "", "remember anything")
	require.ErrorIs(t, err, errNoUpsert)
	assert.Empty(t, storedMemories(t, pool, tenant))
}

func TestUpsertWritesTagsAfterTagExtractToolCall(t *testing.T) {
	pool := migratedPool(t)
	tenant := createTenant(t, pool)
	transport := &scriptedTransport{steps: []step{
		toolStep("memory_upsert", `{"content":"finished a 5k run, felt great"}`),
		toolStep("tag_extract", `{"tags":["running","fitness","5k"]}`),
		plainStep("Stored your 5k run note."),
	}}
	result, err := hermesService(pool, transport, 0).Upsert(context.Background(), tenant, "alice", "", "I finished a 5k run.")
	require.NoError(t, err)
	assert.Equal(t, "Stored your 5k run note.", result.Summary)
	assert.Equal(t, []string{"running", "fitness", "5k"}, result.Memory.Tags)
	rows := storedMemories(t, pool, tenant)
	require.Len(t, rows, 1)
	assert.Equal(t, []string{"running", "fitness", "5k"}, rows[0].Tags)
}

func TestTagExtractNormalizesAndCapsAtFive(t *testing.T) {
	pool := migratedPool(t)
	tenant := createTenant(t, pool)
	transport := &scriptedTransport{steps: []step{
		toolStep("memory_upsert", `{"content":"work meeting about Q3 goals"}`),
		toolStep("tag_extract", `{"tags":["  Work ","WORK","meeting","Q3","goals","planning","quarterly"]}`),
		plainStep("Stored the meeting note."),
	}}
	_, err := hermesService(pool, transport, 0).Upsert(context.Background(), tenant, "alice", "", "Meeting about Q3.")
	require.NoError(t, err)
	rows := storedMemories(t, pool, tenant)
	require.Len(t, rows, 1)
	assert.Equal(t, []string{"work", "meeting", "q3", "goals", "planning"}, rows[0].Tags)
}

func TestUpsertSucceedsEvenIfTagExtractIsSkipped(t *testing.T) {
	pool := migratedPool(t)
	tenant := createTenant(t, pool)
	transport := &scriptedTransport{steps: []step{
		toolStep("memory_upsert", `{"content":"call mom on sunday"}`),
		plainStep("Saved."),
	}}
	result, err := hermesService(pool, transport, 0).Upsert(context.Background(), tenant, "alice", "", "Call mom Sunday.")
	require.NoError(t, err)
	assert.Equal(t, "Saved.", result.Summary)
	rows := storedMemories(t, pool, tenant)
	require.Len(t, rows, 1)
	assert.Nil(t, rows[0].Tags)
}

func TestNormalizeTags(t *testing.T) {
	assert.Equal(t, []string{"a", "b"}, NormalizeTags([]string{"A", "b", "A"}))
	assert.Equal(t, []string{"hi"}, NormalizeTags([]string{"", "  ", " hi "}))
	assert.Equal(t, []string{"1", "2", "3", "4", "5"}, NormalizeTags([]string{"1", "2", "3", "4", "5", "6", "7"}))
	assert.Equal(t, []string{}, NormalizeTags([]string{}))
}

func TestAgentLoopRespectsMaxIterations(t *testing.T) {
	pool := migratedPool(t)
	tenant := createTenant(t, pool)
	loop := toolStep("memory_upsert", `{"content":"stuck in a loop"}`)
	transport := &scriptedTransport{repeating: &loop}
	_, err := hermesService(pool, transport, 3).Upsert(context.Background(), tenant, "carol", "", "loop me")
	require.ErrorIs(t, err, errNotConverged)
	assert.Len(t, transport.calls, 3)
}

// Beyond Swift's suite: Go inserts telemetry directly instead of through the worker's inbox.
func TestPGTelemetryInsertsOneSample(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	tenant := createTenant(t, pool)
	PGTelemetry{Pool: pool}.Record(ctx, TelemetrySample{Tenant: tenant, SourcePath: "agentic_search", Distances: []float32{0.5, 0.25}, Limit: 3})
	PGTelemetry{Pool: pool}.Record(ctx, TelemetrySample{Tenant: tenant, SourcePath: "query", Limit: 5})
	type row struct {
		hits, limit int
		zero        bool
		top, mean   *float64
	}
	read := func(source string) (r row, err error) {
		err = pool.QueryRow(ctx, `SELECT hit_count, limit_requested, zero_hit, top_distance, mean_distance
			FROM retrieval_telemetry_events WHERE tenant_id = $1 AND source_path = $2`, tenant, source).
			Scan(&r.hits, &r.limit, &r.zero, &r.top, &r.mean)
		return r, err
	}
	require.Eventually(t, func() bool {
		_, a := read("agentic_search")
		_, b := read("query")
		return a == nil && b == nil
	}, 5*time.Second, 25*time.Millisecond)
	hit, err := read("agentic_search")
	require.NoError(t, err)
	assert.Equal(t, 2, hit.hits)
	assert.Equal(t, 3, hit.limit)
	assert.False(t, hit.zero)
	assert.InDelta(t, 0.25, *hit.top, 1e-6)
	assert.InDelta(t, 0.375, *hit.mean, 1e-6)
	zero, err := read("query")
	require.NoError(t, err)
	assert.True(t, zero.zero)
	assert.Nil(t, zero.top)
	assert.Nil(t, zero.mean)
}

// The model payload names the tools and temperature Swift sends.
func TestUpsertPayloadShape(t *testing.T) {
	pool := migratedPool(t)
	tenant := createTenant(t, pool)
	transport := &scriptedTransport{steps: []step{toolStep("memory_upsert", `{"content":"x"}`), plainStep("ok")}}
	_, err := hermesService(pool, transport, 0).Upsert(context.Background(), tenant, "k", "", "x")
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(transport.calls[0].payload, &body))
	assert.Equal(t, "auto", body["tool_choice"])
	assert.Len(t, body["tools"], 3)
}
