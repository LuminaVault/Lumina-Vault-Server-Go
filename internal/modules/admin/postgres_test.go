package admin

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/dbtest"
	"lumina/internal/embedding"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/wire"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	return pool
}

// newTenant is a user with the personal vault memories.tenant_id references (M90).
func newTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at) VALUES ($1, $2, 'x', true, $3, NOW(), NOW())`,
		id, id.String()+"@test.luminavault", "u"+id.String()[:8])
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(),
		`INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'v', NOW(), NOW())`, id)
	require.NoError(t, err)
	return id
}

func insertMemory(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, content string, createdAt time.Time, access, hits int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `INSERT INTO memories (id, tenant_id, content, created_at, access_count, query_hit_count, score)
		VALUES ($1, $2, $3, $4, $5, $6, 0)`, id, tenant, content, createdAt, access, hits)
	require.NoError(t, err)
	return id
}

func memoryScore(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (float64, bool) {
	t.Helper()
	var score float64
	err := pool.QueryRow(context.Background(), `SELECT score FROM memories WHERE id = $1`, id).Scan(&score)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	require.NoError(t, err)
	return score, true
}

// Ports MemoryPruningServiceTests.swift (4).
func TestMemoryPruningService(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	scoring := Scoring{Pool: pool, Config: DefaultScoringConfig}
	pruning := Pruning{Pool: pool, Config: DefaultPruningConfig}
	day := 24 * time.Hour

	t.Run("recompute updates score to formula", func(t *testing.T) {
		tenant := newTenant(t, pool)
		now := time.Now()
		fresh := insertMemory(t, pool, tenant, "fresh", now, 0, 0)
		oldQuiet := insertMemory(t, pool, tenant, "old quiet", now.Add(-180*day), 0, 0)
		oldPopular := insertMemory(t, pool, tenant, "old popular", now.Add(-180*day), 100, 30)

		updated, err := scoring.RecomputeForTenant(ctx, tenant, now)
		require.NoError(t, err)
		require.Equal(t, 3, updated)
		f, _ := memoryScore(t, pool, fresh)
		require.InDelta(t, 1.0, f, 1e-6)
		q, _ := memoryScore(t, pool, oldQuiet)
		require.Less(t, q, 0.05)
		p, _ := memoryScore(t, pool, oldPopular)
		require.Greater(t, p, 10.0)
		created := now.Add(-180 * day)
		require.InDelta(t, ComputeScore(100, 30, &created, now, DefaultScoringConfig), p, 1e-6)
	})
	t.Run("prune archives low score old rows only", func(t *testing.T) {
		tenant := newTenant(t, pool)
		now := time.Now()
		recent := insertMemory(t, pool, tenant, "recent low-score", now, 0, 0)
		oldLow := insertMemory(t, pool, tenant, "old & quiet", now.Add(-180*day), 0, 0)
		oldHigh := insertMemory(t, pool, tenant, "old & loved", now.Add(-180*day), 100, 30)

		_, err := scoring.RecomputeForTenant(ctx, tenant, now)
		require.NoError(t, err)
		result, err := pruning.PruneForTenant(ctx, tenant, now)
		require.NoError(t, err)
		require.Equal(t, 1, result.Archived)
		require.Equal(t, 1, result.CandidatesScanned)

		_, ok := memoryScore(t, pool, recent)
		require.True(t, ok)
		_, ok = memoryScore(t, pool, oldLow)
		require.False(t, ok, "low-score old row must be archived")
		_, ok = memoryScore(t, pool, oldHigh)
		require.True(t, ok)
		var archived int
		require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM memories_archive WHERE tenant_id = $1 AND id = $2 AND content = 'old & quiet'`,
			tenant, oldLow).Scan(&archived))
		require.Equal(t, 1, archived)
	})
	t.Run("prune is idempotent", func(t *testing.T) {
		tenant := newTenant(t, pool)
		now := time.Now()
		insertMemory(t, pool, tenant, "drop me", now.Add(-180*day), 0, 0)
		_, err := scoring.RecomputeForTenant(ctx, tenant, now)
		require.NoError(t, err)
		first, err := pruning.PruneForTenant(ctx, tenant, now)
		require.NoError(t, err)
		require.Equal(t, 1, first.Archived)
		second, err := pruning.PruneForTenant(ctx, tenant, now)
		require.NoError(t, err)
		require.Equal(t, PruneResult{TenantID: first.TenantID}, second, "second prune over same data must be a no-op")
	})
	t.Run("tenant isolation during prune", func(t *testing.T) {
		alice, mallory := newTenant(t, pool), newTenant(t, pool)
		now := time.Now()
		aliceMem := insertMemory(t, pool, alice, "alice old", now.Add(-180*day), 0, 0)
		malloryMem := insertMemory(t, pool, mallory, "mallory old", now.Add(-180*day), 0, 0)
		_, err := scoring.RecomputeAll(ctx, now)
		require.NoError(t, err)
		_, err = pruning.PruneForTenant(ctx, alice, now)
		require.NoError(t, err)
		_, ok := memoryScore(t, pool, aliceMem)
		require.False(t, ok)
		_, ok = memoryScore(t, pool, malloryMem)
		require.True(t, ok)
	})
	t.Run("the sweep recomputes then prunes every user", func(t *testing.T) {
		tenant := newTenant(t, pool)
		now := time.Now()
		insertMemory(t, pool, tenant, "sweep me", now.Add(-180*day), 0, 0)
		s, err := RunPruningSweep(ctx, pool, scoring, pruning, now)
		require.NoError(t, err)
		require.Positive(t, s.UsersScanned)
		require.Empty(t, s.Failures)
		require.Contains(t, s.PerTenant, PruneResult{TenantID: wireUUID(tenant), CandidatesScanned: 1, Archived: 1})
	})
}

func TestUsageEventsRetentionSweep(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	tenant := newTenant(t, pool)
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		_, err := pool.Exec(ctx, `INSERT INTO usage_events (tenant_id, occurred_at, metric, amount, source) VALUES ($1, $2, 'voice_transcribe', 1, 't')`,
			tenant, now.Add(-time.Duration(100+i)*24*time.Hour))
		require.NoError(t, err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO usage_events (tenant_id, occurred_at, metric, amount, source) VALUES ($1, $2, 'voice_transcribe', 1, 't')`,
		tenant, now.Add(-time.Hour))
	require.NoError(t, err)

	off, err := Retention{Pool: pool, Policy: NewRetentionPolicy(0, 2)}.Sweep(ctx, now)
	require.NoError(t, err)
	require.Equal(t, RetentionSummary{}, off)

	s, err := Retention{Pool: pool, Policy: NewRetentionPolicy(90, 2)}.Sweep(ctx, now)
	require.NoError(t, err)
	require.True(t, s.Enabled)
	require.Equal(t, 5, s.Deleted)
	require.False(t, s.MoreRemaining)
	require.Equal(t, "2026-06-14T00:00:00Z", s.Cutoff.UTC().Format(time.RFC3339))
	var left int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM usage_events WHERE tenant_id = $1`, tenant).Scan(&left))
	require.Equal(t, 1, left)
}

// stubTransport is StubHermesTransport: one canned assistant message.
type stubTransport struct {
	mu      sync.Mutex
	content string
	calls   []string
}

func (s *stubTransport) ChatCompletions(_ context.Context, payload []byte, sessionKey, _ string) (llm.Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, sessionKey)
	body, err := json.Marshal(map[string]any{
		"id": "stub", "model": "stub-model",
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": s.content}, "finish_reason": "stop"}},
	})
	return llm.Metadata{Data: body}, err
}

func (s *stubTransport) ChatStream(context.Context, []byte, string, string) llm.Stream {
	return func(yield func(llm.Chunk, error) bool) { yield(llm.Chunk{}, errors.New("no stream")) }
}

func (s *stubTransport) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// seedHealthEvents spreads count rows evenly from start to end, as the Swift helper.
func seedHealthEvents(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, count int, start, end time.Time) {
	t.Helper()
	step := max(1, int(end.Sub(start).Seconds())/max(1, count))
	for i := range count {
		eventType, unit := "steps", "count"
		if i%2 == 1 {
			eventType, unit = "sleep_hours", "hours"
		}
		_, err := pool.Exec(context.Background(), `INSERT INTO health_events (id, tenant_id, event_type, value_numeric, unit, recorded_at, source, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'test', NOW())`, uuid.New(), tenant, eventType, float64(1000+i*37), unit,
			start.Add(time.Duration(i*step)*time.Second))
		require.NoError(t, err)
	}
}

// Ports HealthCorrelationServiceTests.swift's five database tests.
func TestHealthCorrelationService(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	day := 24 * time.Hour
	harness := func(content string) (*Correlation, *stubTransport) {
		tr := &stubTransport{content: content}
		return &Correlation{
			Pool: pool, Transport: tr, Embeddings: embedding.Deterministic{}, Memories: memorystore.New(pool, nil),
			DefaultModel: "stub",
		}, tr
	}
	const canned = "Stub synthesis.\n- When step count dropped, the user's notes mentioned fatigue."

	t.Run("skips when insufficient history", func(t *testing.T) {
		svc, tr := harness(canned)
		tenant := newTenant(t, pool)
		now := time.Now()
		seedHealthEvents(t, pool, tenant, 5, now.Add(-5*day), now)
		out, err := svc.Correlate(ctx, tenant, now)
		require.NoError(t, err)
		require.Equal(t, StatusInsufficientHistory, out.Status)
		require.Zero(t, tr.count(), "must not call Hermes when history is short")
	})
	t.Run("skips when no events in window", func(t *testing.T) {
		svc, _ := harness(canned)
		tenant := newTenant(t, pool)
		now := time.Now()
		seedHealthEvents(t, pool, tenant, 30, now.Add(-60*day), now.Add(-10*day))
		out, err := svc.Correlate(ctx, tenant, now)
		require.NoError(t, err)
		require.Equal(t, StatusNoRecentEvents, out.Status)
	})
	t.Run("saves on happy path", func(t *testing.T) {
		svc, tr := harness(canned)
		tenant := newTenant(t, pool)
		now := time.Now()
		seedHealthEvents(t, pool, tenant, 40, now.Add(-35*day), now.Add(-time.Minute))
		insertMemory(t, pool, tenant, "felt tired today", now.Add(-time.Hour), 0, 0)
		out, err := svc.Correlate(ctx, tenant, now)
		require.NoError(t, err)
		require.Equal(t, StatusSaved, out.Status)
		require.NotNil(t, out.MemoryID)
		var tags []string
		var content string
		require.NoError(t, pool.QueryRow(ctx, `SELECT tags, content FROM memories WHERE id = $1`, *out.MemoryID).Scan(&tags, &content))
		require.Equal(t, []string{"correlation", "weekly"}, tags)
		require.Equal(t, canned, content)
		require.Equal(t, 1, tr.count())
		require.Equal(t, []string{wireUUID(tenant).String()}, tr.calls, "the session key is the UPPERCASE tenant id")
	})
	t.Run("second run same week is skipped", func(t *testing.T) {
		svc, tr := harness(canned)
		tenant := newTenant(t, pool)
		now := time.Now()
		seedHealthEvents(t, pool, tenant, 40, now.Add(-35*day), now.Add(-time.Minute))
		first, err := svc.Correlate(ctx, tenant, now)
		require.NoError(t, err)
		require.Equal(t, StatusSaved, first.Status)
		second, err := svc.Correlate(ctx, tenant, now)
		require.NoError(t, err)
		require.Equal(t, StatusAlreadyRan, second.Status)
		require.Equal(t, 1, tr.count(), "idempotent — Hermes must be hit at most once per week")
	})
	t.Run("skips when synthesis empty", func(t *testing.T) {
		svc, _ := harness("   \n  ")
		tenant := newTenant(t, pool)
		now := time.Now()
		seedHealthEvents(t, pool, tenant, 40, now.Add(-35*day), now.Add(-time.Minute))
		out, err := svc.Correlate(ctx, tenant, now)
		require.NoError(t, err)
		require.Equal(t, StatusNoSynthesis, out.Status)
	})
}

// Swift saves a NO_CORRELATION reply as a memory: its system prompt calls that
// line a skip, but only an empty reply is one. Kept until after the archive.
func TestSwiftWeaknessNoCorrelationIsSaved(t *testing.T) {
	pool := pgPool(t)
	tr := &stubTransport{content: "NO_CORRELATION"}
	svc := &Correlation{Pool: pool, Transport: tr, Embeddings: embedding.Deterministic{}, Memories: memorystore.New(pool, nil)}
	tenant := newTenant(t, pool)
	now := time.Now()
	seedHealthEvents(t, pool, tenant, 40, now.Add(-35*24*time.Hour), now.Add(-time.Minute))
	out, err := svc.Correlate(context.Background(), tenant, now)
	require.NoError(t, err)
	require.Equal(t, StatusSaved, out.Status)
}

func wireUUID(id uuid.UUID) wire.UUID { return wire.UUID{UUID: id} }
