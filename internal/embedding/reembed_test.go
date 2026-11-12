package embedding

import (
	"context"
	"lumina/internal/dbtest"
	"lumina/internal/jobs"
	"lumina/internal/migrations"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// EmbeddingReembedServiceTests and the heal-pass tests of EmbeddingDegradationTests.
// The embedders are stubs (no keys, no network). The database is shared by every
// subtest, so assertions look only at each subtest's own rows, and each re-embed
// subtest starts from an empty progress table.
func TestReembedAgainstPostgres(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))

	for name, fn := range map[string]func(*testing.T, *pgxpool.Pool){
		// EmbeddingReembedServiceTests
		"RewritesEveryTargetThenMarksComplete": testReembedAllTargets,
		"CursorPersistsAndRestartResumes":      testReembedResume,
		"CompletedTargetNeverWalkedAgain":      testReembedComplete,
		"RefusesUnderDeterministic":            testReembedRefuses,
		"FailingRowSkippedAndCounted":          testReembedFailingRow,
		"ProviderDownKeepsCursorAndBacksOff":   testReembedProviderDown,
		"RowEditedAfterReadKeepsItsVector":     testReembedRace,
		// EmbeddingDegradationTests (heal pass)
		"HealProbesOncePerWindowThenFills": testHealProbes,
		"HealSkipsRejectedRow":             testHealSkipsRejected,
		"HealRefusesUnderDeterministic":    testHealRefuses,
	} {
		t.Run(name, func(t *testing.T) { fn(t, pool) })
	}
}

func unitVector(axis int) []float32 {
	v := make([]float32, Dimensions)
	v[axis] = 1
	return v
}

// realVector is what the stub provider returns; fakeVector is what the
// deterministic stub would have stored.
var (
	realVector = unitVector(1)
	fakeVector = unitVector(0)
)

// reembedStub turns every text into realVector; a text containing FAIL (or every
// text, when failAll) throws a transient error. It counts calls per text.
type reembedStub struct {
	mu      sync.Mutex
	calls   map[string]int
	failAll bool
}

func (s *reembedStub) Embed(_ context.Context, text string, _ uuid.UUID) ([]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[text]++
	if s.failAll || strings.Contains(text, "FAIL") {
		return nil, &Error{Kind: FailureTransient, Reason: "stub down"}
	}
	return realVector, nil
}

func (s *reembedStub) count(text string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[text]
}

func (s *reembedStub) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		n += c
	}
	return n
}

func (s *reembedStub) setFailAll(v bool) {
	s.mu.Lock()
	s.failAll = v
	s.mu.Unlock()
}

func noSleep(context.Context, time.Duration) error { return nil }

func reembedService(pool *pgxpool.Pool, emb Service, kind Kind, cfg ReembedConfig) *ReembedService {
	s := NewReembedService(pool, emb, kind, true, true, cfg, nil)
	s.Sleep = noSleep
	return s
}

func testConfig(batch int) ReembedConfig {
	cfg := DefaultReembedConfig()
	cfg.BatchSize, cfg.Pause = batch, 0
	return cfg
}

func resetProgress(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `DELETE FROM embedding_reembed_progress`)
	require.NoError(t, err)
}

func reembedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	slug := "re" + id.String()[:8]
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, username, password_hash, created_at, updated_at) VALUES ($1, $2, $3, 'stub', now(), now())`,
		id, slug+"@test.luminavault", slug)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'v', now(), now())`, id)
	require.NoError(t, err)
	// The re-embed walk covers every tenant's rows, so a subtest's leftovers (a
	// FAIL row stays un-embedded) would fail every batch of a later subtest whose
	// test sleep never waits: an endless loop. Each subtest removes its own rows.
	t.Cleanup(func() {
		for _, table := range []string{"memory_chunks", "memories_archive", "memories"} {
			_, err := pool.Exec(context.Background(), `DELETE FROM `+table+` WHERE tenant_id = $1`, id)
			require.NoError(t, err)
		}
	})
	return id
}

// memoryRow stores a memory with vector (nil stores NULL).
func memoryRow(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, content string, vector []float32) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var literal *string
	if vector != nil {
		v := FormatVector(vector)
		literal = &v
	}
	_, err := pool.Exec(context.Background(), `INSERT INTO memories (id, tenant_id, content, embedding, created_at, updated_at) VALUES ($1, $2, $3, $4::vector, now(), now())`,
		id, tenant, content, literal)
	require.NoError(t, err)
	return id
}

func chunkRow(t *testing.T, pool *pgxpool.Pool, tenant, memoryID uuid.UUID, text string, vector []float32) string {
	t.Helper()
	chunkID := "test-chunk-" + uuid.NewString()
	var literal *string
	if vector != nil {
		v := FormatVector(vector)
		literal = &v
	}
	_, err := pool.Exec(context.Background(), `
INSERT INTO memory_chunks (chunk_id, document_id, tenant_id, memory_id, ordinal, start_line, end_line, text, content_sha256, embedding)
VALUES ($1, $2, $3, $4, 0, 1, 1, $5, 'sha', $6::vector)`, chunkID, "doc-"+memoryID.String(), tenant, memoryID, text, literal)
	require.NoError(t, err)
	return chunkID
}

func archivedRow(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, content string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `INSERT INTO memories_archive (id, tenant_id, content, embedding, created_at) VALUES ($1, $2, $3, $4::vector, now())`,
		id, tenant, content, FormatVector(fakeVector))
	require.NoError(t, err)
	return id
}

// axis is the axis the stored unit vector points along (0 still fake, 1
// rewritten), -1 when there is no vector.
func axis(t *testing.T, pool *pgxpool.Pool, table, idColumn, id string) int {
	t.Helper()
	var text *string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT embedding::text FROM `+table+` WHERE `+idColumn+`::text = $1`, id).Scan(&text))
	if text == nil {
		return -1
	}
	for i, part := range strings.Split(strings.Trim(*text, "[]"), ",") {
		if f, err := strconv.ParseFloat(part, 32); err == nil && f == 1 {
			return i
		}
	}
	return -1
}

func embeddingMissing(t *testing.T, pool *pgxpool.Pool, table, idColumn, id string) bool {
	t.Helper()
	var missing bool
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT embedding IS NULL FROM `+table+` WHERE `+idColumn+`::text = $1`, id).Scan(&missing))
	return missing
}

// MARK: EmbeddingReembedServiceTests

func testReembedAllTargets(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	resetProgress(t, pool)
	tenant := reembedUser(t, pool)
	memoryID := memoryRow(t, pool, tenant, "Flight to Lisbon on Friday", fakeVector)
	chunkID := chunkRow(t, pool, tenant, memoryID, "Flight to Lisbon", fakeVector)
	archiveID := archivedRow(t, pool, tenant, "Old note about Porto")
	_, err := pool.Exec(ctx, `UPDATE memories SET graph_x = 1, graph_y = 2, graph_z = 3, graph_layout_at = now() WHERE id = $1`, memoryID)
	require.NoError(t, err)
	var createdBefore time.Time
	var stateBefore string
	require.NoError(t, pool.QueryRow(ctx, `SELECT created_at, review_state FROM memories WHERE id = $1`, memoryID).Scan(&createdBefore, &stateBefore))

	stub := &reembedStub{}
	ran, err := reembedService(pool, stub, KindOpenRouter, testConfig(50)).RunToCompletion(ctx)
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, 1, axis(t, pool, "memories", "id", memoryID.String()))
	require.Equal(t, 1, axis(t, pool, "memory_chunks", "chunk_id", chunkID))
	require.Equal(t, 1, axis(t, pool, "memories_archive", "id", archiveID.String()))

	// Embedding column only.
	var createdAfter time.Time
	var stateAfter string
	require.NoError(t, pool.QueryRow(ctx, `SELECT created_at, review_state FROM memories WHERE id = $1`, memoryID).Scan(&createdAfter, &stateAfter))
	require.True(t, createdBefore.Equal(createdAfter))
	require.Equal(t, stateBefore, stateAfter)

	// The layout projected from the fake vectors is cleared for the worker.
	var graphX *float64
	require.NoError(t, pool.QueryRow(ctx, `SELECT graph_x FROM memories WHERE id = $1`, memoryID).Scan(&graphX))
	require.Nil(t, graphX)

	service := reembedService(pool, stub, KindOpenRouter, testConfig(50))
	for _, target := range AllReembedTargets() {
		p, err := service.Progress(ctx, target)
		require.NoError(t, err)
		require.NotNil(t, p, target.Name)
		require.NotNil(t, p.CompletedAt, target.Name)
		require.GreaterOrEqual(t, p.RowsDone, int64(1), target.Name)
	}
}

func testReembedResume(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	resetProgress(t, pool)
	tenant := reembedUser(t, pool)
	var contents []string
	for i := range 5 {
		content := "resume note " + strconv.Itoa(i) + " " + uuid.NewString()
		contents = append(contents, content)
		memoryRow(t, pool, tenant, content, fakeVector)
	}
	stub := &reembedStub{}
	first := reembedService(pool, stub, KindOpenRouter, testConfig(2))
	result, err := first.RunBatch(ctx, TargetMemories)
	require.NoError(t, err)
	require.Equal(t, BatchProgressed, result.Kind)
	afterOne, err := first.Progress(ctx, TargetMemories)
	require.NoError(t, err)
	require.NotNil(t, afterOne.LastID)
	require.Equal(t, int64(result.Reembedded), afterOne.RowsDone)
	require.Nil(t, afterOne.CompletedAt)

	// "Restart": a fresh instance reads the cursor back.
	second := reembedService(pool, stub, KindOpenRouter, testConfig(2))
	_, err = second.RunToCompletion(ctx)
	require.NoError(t, err)
	for _, content := range contents {
		require.Equal(t, 1, stub.count(content), "re-embedded more than once or never: %s", content)
	}
	done, err := second.Progress(ctx, TargetMemories)
	require.NoError(t, err)
	require.NotNil(t, done.CompletedAt)
	require.GreaterOrEqual(t, done.RowsDone, int64(len(contents)))
}

func testReembedComplete(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	resetProgress(t, pool)
	tenant := reembedUser(t, pool)
	content := "complete once " + uuid.NewString()
	memoryRow(t, pool, tenant, content, fakeVector)
	stub := &reembedStub{}
	_, err := reembedService(pool, stub, KindOpenRouter, testConfig(50)).RunToCompletion(ctx)
	require.NoError(t, err)
	again, err := reembedService(pool, stub, KindOpenRouter, testConfig(50)).RunBatch(ctx, TargetMemories)
	require.NoError(t, err)
	require.Equal(t, BatchAlreadyComplete, again.Kind)
	_, err = reembedService(pool, stub, KindOpenRouter, testConfig(50)).RunToCompletion(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stub.count(content))
}

func testReembedRefuses(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	resetProgress(t, pool)
	tenant := reembedUser(t, pool)
	memoryID := memoryRow(t, pool, tenant, "should stay fake", fakeVector)
	stub := &reembedStub{}
	service := reembedService(pool, stub, KindDeterministic, testConfig(50))
	ran, err := service.RunToCompletion(ctx)
	require.NoError(t, err)
	require.False(t, ran)
	require.Zero(t, stub.total())
	require.Equal(t, 0, axis(t, pool, "memories", "id", memoryID.String()))
	p, err := service.Progress(ctx, TargetMemories)
	require.NoError(t, err)
	require.Nil(t, p)
}

func testReembedFailingRow(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	resetProgress(t, pool)
	tenant := reembedUser(t, pool)
	good := memoryRow(t, pool, tenant, "good note "+uuid.NewString(), fakeVector)
	bad := memoryRow(t, pool, tenant, "FAIL note "+uuid.NewString(), fakeVector)
	service := reembedService(pool, &reembedStub{}, KindOpenRouter, testConfig(50))
	_, err := service.RunToCompletion(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, axis(t, pool, "memories", "id", good.String()))
	require.Equal(t, 0, axis(t, pool, "memories", "id", bad.String()))
	p, err := service.Progress(ctx, TargetMemories)
	require.NoError(t, err)
	require.NotNil(t, p.CompletedAt)
	require.GreaterOrEqual(t, p.RowsFailed, int64(1))
}

func testReembedProviderDown(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	resetProgress(t, pool)
	tenant := reembedUser(t, pool)
	memoryRow(t, pool, tenant, "provider down "+uuid.NewString(), fakeVector)
	stub := &reembedStub{}
	stub.setFailAll(true)
	service := reembedService(pool, stub, KindOpenRouter, ReembedConfig{
		BatchSize: 50, Pause: 2 * time.Second, FailedBatchesBeforeBackoff: 2,
		BackoffBase: 30 * time.Second, MaxBackoff: 60 * time.Second,
		HealInterval: 600 * time.Second, HealMaxBackoff: 3600 * time.Second,
	})
	for range 3 {
		result, err := service.RunBatch(ctx, TargetMemories)
		require.NoError(t, err)
		require.Equal(t, BatchProviderFailed, result.Kind)
	}
	// Three failed batches, threshold two: 30s · 2^1, capped at 60s.
	require.Equal(t, 60*time.Second, service.DelayAfterFailedBatch())
	p, err := service.Progress(ctx, TargetMemories)
	require.NoError(t, err)
	if p != nil {
		require.Nil(t, p.LastID)
		require.Nil(t, p.CompletedAt)
	}

	// Provider back: the same rows are picked up, nothing was skipped.
	stub.setFailAll(false)
	recovered, err := service.RunBatch(ctx, TargetMemories)
	require.NoError(t, err)
	require.Equal(t, BatchProgressed, recovered.Kind)
	require.GreaterOrEqual(t, recovered.Reembedded, 1)
}

// editingEmbedder changes the row's content while "embedding", simulating an
// edit between read and write.
type editingEmbedder struct {
	pool     *pgxpool.Pool
	memoryID uuid.UUID
}

func (e *editingEmbedder) Embed(ctx context.Context, _ string, _ uuid.UUID) ([]float32, error) {
	if _, err := e.pool.Exec(ctx, `UPDATE memories SET content = 'edited by the user' WHERE id = $1`, e.memoryID); err != nil {
		return nil, err
	}
	return realVector, nil
}

func testReembedRace(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	resetProgress(t, pool)
	tenant := reembedUser(t, pool)
	memoryID := memoryRow(t, pool, tenant, "original "+uuid.NewString(), fakeVector)
	service := reembedService(pool, &editingEmbedder{pool: pool, memoryID: memoryID}, KindOpenRouter, testConfig(50))
	service.Targets = []ReembedTarget{TargetMemories}
	_, err := service.RunToCompletion(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, axis(t, pool, "memories", "id", memoryID.String()))
}

// MARK: EmbeddingDegradationTests (heal pass)

type switchMode int

const (
	modePaymentRequired switchMode = iota
	modeOutage
	modeWorking
)

// switchableEmbedder fails like a provider in the chosen state and counts calls.
type switchableEmbedder struct {
	mu    sync.Mutex
	mode  switchMode
	calls int
}

func (s *switchableEmbedder) set(m switchMode) {
	s.mu.Lock()
	s.mode = m
	s.mu.Unlock()
}

func (s *switchableEmbedder) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *switchableEmbedder) Embed(_ context.Context, text string, _ uuid.UUID) ([]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	switch s.mode {
	case modePaymentRequired:
		return nil, &Error{Kind: FailurePermanent, Permanent: ReasonPaymentRequired, Response: &ProviderResponse{Status: 402, Message: "Insufficient credits"}}
	case modeOutage:
		return nil, &Error{Kind: FailureTransient, Reason: "stub http 503", Response: &ProviderResponse{Status: 503, Message: "upstream unavailable"}}
	default:
		// A text marked REJECT is a bad input, not an outage.
		if strings.Contains(text, "REJECT") {
			return nil, &Error{Kind: FailurePermanent, Permanent: ReasonRequestRejected, Response: &ProviderResponse{Status: 400, Message: "input rejected"}}
		}
		return realVector, nil
	}
}

func healer(pool *pgxpool.Pool, emb Service, kind Kind) *ReembedService {
	cfg := testConfig(50)
	cfg.HealInterval, cfg.HealMaxBackoff = 600*time.Second, 1800*time.Second
	s := NewReembedService(pool, emb, kind, false, true, cfg, nil)
	s.Sleep = noSleep
	return s
}

func testHealProbes(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := reembedUser(t, pool)
	memoryID := memoryRow(t, pool, tenant, "Saved while OpenRouter was out of credit "+uuid.NewString(), nil)
	chunkID := chunkRow(t, pool, tenant, memoryID, "Chunk saved during the outage", nil)

	stub := &switchableEmbedder{mode: modePaymentRequired}
	h := healer(pool, stub, KindOpenRouter)
	unavailable := HealResult{Kind: HealProviderUnavailable}

	// One call, then the pass stops: not one 402 per row.
	result, err := h.HealMissing(ctx)
	require.NoError(t, err)
	require.Equal(t, unavailable, result)
	require.Equal(t, 1, stub.count())
	require.Equal(t, 600*time.Second, h.DelayAfterHeal(unavailable))
	result, err = h.HealMissing(ctx)
	require.NoError(t, err)
	require.Equal(t, unavailable, result)
	require.Equal(t, 2, stub.count())
	require.Equal(t, 1200*time.Second, h.DelayAfterHeal(unavailable))
	require.Equal(t, 1800*time.Second, h.DelayAfterHeal(unavailable))
	require.True(t, embeddingMissing(t, pool, "memories", "id", memoryID.String()))

	// Provider back. The cursor moved past the probed rows, so it may take the
	// wrap-around pass to reach them; never more than two.
	stub.set(modeWorking)
	for range 3 {
		_, err := h.HealMissing(ctx)
		require.NoError(t, err)
	}
	require.False(t, embeddingMissing(t, pool, "memories", "id", memoryID.String()))
	require.False(t, embeddingMissing(t, pool, "memory_chunks", "chunk_id", chunkID))
	// A caught-up pass resets the backoff.
	require.Equal(t, 600*time.Second, h.DelayAfterHeal(HealResult{Kind: HealCaughtUp}))
}

func testHealSkipsRejected(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := reembedUser(t, pool)
	bad := memoryRow(t, pool, tenant, "REJECT this one "+uuid.NewString(), nil)
	good := memoryRow(t, pool, tenant, "heal this one "+uuid.NewString(), nil)
	h := healer(pool, &switchableEmbedder{mode: modeWorking}, KindOpenRouter)
	for range 2 {
		result, err := h.HealMissing(ctx)
		require.NoError(t, err)
		require.Equal(t, HealCaughtUp, result.Kind)
	}
	require.False(t, embeddingMissing(t, pool, "memories", "id", good.String()))
	require.True(t, embeddingMissing(t, pool, "memories", "id", bad.String()))
}

func testHealRefuses(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := reembedUser(t, pool)
	memoryID := memoryRow(t, pool, tenant, "stays unembedded "+uuid.NewString(), nil)
	stub := &switchableEmbedder{mode: modeWorking}
	result, err := healer(pool, stub, KindDeterministic).HealMissing(ctx)
	require.NoError(t, err)
	require.Equal(t, HealResult{Kind: HealRefused}, result)
	require.Zero(t, stub.count())
	require.True(t, embeddingMissing(t, pool, "memories", "id", memoryID.String()))
}

// The job wrapper: deterministic with only heal on stops after the refused pass;
// a refused provider makes the run fail and skips runs until the backoff is up.
func TestReembedJobRunsSwiftLoop(t *testing.T) {
	ctx := context.Background()
	s := NewReembedService(nil, &reembedStub{}, KindDeterministic, false, true, DefaultReembedConfig(), nil)
	require.NoError(t, s.runOnce(ctx))
	require.ErrorIs(t, s.runOnce(ctx), jobs.ErrSkipped)
	require.Equal(t, "EmbeddingReembedService", s.Job().Name)
}
