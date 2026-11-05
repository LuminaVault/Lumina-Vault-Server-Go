package memorystore

import (
	"context"
	"lumina/internal/dbtest"
	"lumina/internal/embedding"
	"lumina/internal/migrations"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// One container for the package; every subtest uses its own tenants.
func TestAgainstPostgres(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))

	for name, fn := range map[string]func(*testing.T, *pgxpool.Pool){
		// MemoryHitCountUpdateQueueTests.
		"QueueIncrementsQueryHitCount":     testQueueIncrements,
		"SemanticSearchEnqueuesHits":       testSearchEnqueues,
		"QueueCoalesces":                   testQueueCoalesces,
		"RunDrainsOnShutdown":              testRunDrains,
		"CreateVariantsAndUpdatedAt":       testCreateVariants,
		"CreateWritesEveryColumn":          testCreateColumns,
		"SemanticSearchFilters":            testSemanticFilters,
		"LexicalSearch":                    testLexical,
		"SearchDocumentsPicksThePath":      testSearchDocuments,
		"ListPaginatedDefaults":            testListDefaults,
		"ListPaginatedFilters":             testListFilters,
		"ListHealthFilters":                testListHealth,
		"ListTagPathIgnoresOtherFilters":   testListByTag,
		"FindIsTenantScoped":               testFindScoped,
		"StoredEmbedding":                  testStoredEmbedding,
		"UpdateContentDropsChunks":         testUpdateContent,
		"UpdateTags":                       testUpdateTags,
		"UpdateReviewStateLeavesUpdatedAt": testReviewState,
		"SetSpace":                         testSetSpace,
		"SetEmbedding":                     testSetEmbedding,
		"Forget":                           testForget,
		"SourceVaultFileLookups":           testSourceVaultFile,
		"Lineage":                          testLineage,
		"SpaceTopology":                    testSpaceTopology,
	} {
		t.Run(name, func(t *testing.T) { fn(t, pool) })
	}
}

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

func vec(t *testing.T, text string) []float32 {
	t.Helper()
	v, err := embedding.Deterministic{}.Embed(context.Background(), text, uuid.Nil)
	require.NoError(t, err)
	return v
}

func hitCount(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int64 {
	t.Helper()
	var n int64
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT query_hit_count FROM memories WHERE id = $1`, id).Scan(&n))
	return n
}

func create(t *testing.T, r *Repository, p CreateParams) *Memory {
	t.Helper()
	m, err := r.Create(context.Background(), p)
	require.NoError(t, err)
	return m
}

func strPtr(s string) *string { return &s }

func testQueueIncrements(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	m := create(t, New(pool, nil), CreateParams{Tenant: tenant, Content: "project alpha", Embedding: vec(t, "project alpha")})
	queue := NewHitCountQueue(pool)
	queue.Enqueue([]uuid.UUID{m.ID})
	queue.Drain(ctx)
	assert.Equal(t, int64(1), hitCount(t, pool, m.ID))
}

func testSearchEnqueues(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	queue := NewHitCountQueue(pool)
	r := New(pool, queue)
	m := create(t, r, CreateParams{Tenant: tenant, Content: "alpha search target", Embedding: vec(t, "alpha search target")})

	hits, err := r.SemanticSearch(ctx, tenant, vec(t, "alpha"), 1, SearchOptions{})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, m.ID, hits[0].ID)
	queue.Drain(ctx)
	assert.Equal(t, int64(1), hitCount(t, pool, m.ID))
	var accessed *time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT last_accessed_at FROM memories WHERE id = $1`, m.ID).Scan(&accessed))
	assert.NotNil(t, accessed)
}

// Ids in one batch are a set. Hold the worker off by filling pending before it can
// run is racy, so drive the batch directly.
func testQueueCoalesces(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	m := create(t, New(pool, nil), CreateParams{Tenant: tenant, Content: "x"})
	queue := NewHitCountQueue(pool)
	queue.pending = []uuid.UUID{m.ID, m.ID, m.ID}
	ids := queue.takeBatch()
	assert.Equal(t, []uuid.UUID{m.ID}, ids)
	require.NoError(t, BumpQueryHits(ctx, pool, ids))
	assert.Equal(t, int64(1), hitCount(t, pool, m.ID))
	assert.Nil(t, queue.takeBatch())
	queue.Enqueue(nil)
	queue.Drain(ctx)
	require.NoError(t, BumpQueryHits(ctx, pool, nil))
}

func testRunDrains(t *testing.T, pool *pgxpool.Pool) {
	tenant := newTenant(t, pool)
	m := create(t, New(pool, nil), CreateParams{Tenant: tenant, Content: "x"})
	queue := NewHitCountQueue(pool)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- queue.Run(ctx) }()
	queue.Enqueue([]uuid.UUID{m.ID})
	cancel()
	require.NoError(t, <-done)
	assert.Equal(t, int64(1), hitCount(t, pool, m.ID), "Run returns after the work it started")
}

// Fluent's save stamps updated_at; the raw inserts leave it NULL. The local-sync cursor
// reads that column, so the difference is visible.
func testCreateVariants(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)

	bare, err := r.CreateBare(ctx, tenant, "bare")
	require.NoError(t, err)
	assert.NotNil(t, bare.UpdatedAt)
	assert.NotNil(t, bare.CreatedAt)
	assert.Equal(t, ReviewAuto, bare.ReviewState)
	assert.Equal(t, SourceLegacy, bare.OriginKind)

	embedded, err := r.CreateEmbedded(ctx, tenant, "embedded", vec(t, "embedded"))
	require.NoError(t, err)
	assert.Nil(t, embedded.UpdatedAt)
	nullVec, err := r.CreateEmbedded(ctx, tenant, "null vec", nil)
	require.NoError(t, err)
	_, exists, err := r.StoredEmbedding(ctx, tenant, nullVec.ID)
	require.NoError(t, err)
	assert.True(t, exists)

	direct := create(t, r, CreateParams{Tenant: tenant, Content: "direct"})
	assert.Nil(t, direct.UpdatedAt)

	// Only the tenant-direct create records provenance.
	var contributions int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM memory_contributions WHERE tenant_id = $1`, tenant).Scan(&contributions))
	assert.Equal(t, 1, contributions)
	var op, actor, source string
	require.NoError(t, pool.QueryRow(ctx, `SELECT operation, actor_kind, source_kind FROM memory_contributions WHERE memory_id = $1`, direct.ID).Scan(&op, &actor, &source))
	assert.Equal(t, []string{OperationCreate, ActorSystem, SourceLegacy}, []string{op, actor, source})
}

func testCreateColumns(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	space := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at) VALUES ($1, $2, 'S', 's', 0, NOW(), NOW())`, space, tenant)
	require.NoError(t, err)
	contribution := ModelContribution(OperationCreate, SourceKnowledgeCompile, strPtr("anthropic"), strPtr("claude"), strPtr("ref-1"))
	m := create(t, r, CreateParams{
		Tenant: tenant, Content: "o'brien", Embedding: vec(t, "x"), Tags: []string{"it's", `back\slash`, "日本"},
		SpaceID: &space, ReviewState: ReviewPending, Contribution: &contribution, Kind: strPtr(KindDecision),
	})
	assert.Equal(t, "o'brien", m.Content)
	assert.Equal(t, []string{"it's", `back\slash`, "日本"}, m.Tags)
	assert.Equal(t, &space, m.SpaceID)
	assert.Equal(t, ReviewPending, m.ReviewState)
	assert.Equal(t, SourceKnowledgeCompile, m.OriginKind)
	assert.Equal(t, "ref-1", *m.OriginSourceID)
	assert.Equal(t, "anthropic", *m.OriginProvider)
	assert.Equal(t, "claude", *m.OriginModel)
	assert.Equal(t, KindDecision, *m.Kind)

	empty := create(t, r, CreateParams{Tenant: tenant, Content: "no tags", Tags: []string{}})
	assert.Nil(t, empty.Tags, "an empty list stores NULL, not {}")

	// memories_origin_source_unique: the same origin twice is an error, and the
	// provenance row is not written for it.
	_, err = r.Create(ctx, CreateParams{Tenant: tenant, Content: "dup", Contribution: &contribution})
	assert.Error(t, err)
}

func testSemanticFilters(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant, other := newTenant(t, pool), newTenant(t, pool)
	r := New(pool, nil)
	v := vec(t, "same")
	space := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at) VALUES ($1, $2, 'S', 's', 0, NOW(), NOW())`, space, tenant)
	require.NoError(t, err)

	kept := create(t, r, CreateParams{Tenant: tenant, Content: "kept", Embedding: v})
	filed := create(t, r, CreateParams{Tenant: tenant, Content: "filed", Embedding: v, SpaceID: &space})
	create(t, r, CreateParams{Tenant: tenant, Content: "rejected", Embedding: v, ReviewState: ReviewRejected})
	create(t, r, CreateParams{Tenant: tenant, Content: "no vector"})
	create(t, r, CreateParams{Tenant: other, Content: "other tenant", Embedding: v})

	hits, err := r.SemanticSearch(ctx, tenant, v, 10, SearchOptions{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{kept.ID, filed.ID}, []uuid.UUID{hits[0].ID, hits[1].ID})
	assert.Len(t, hits, 2)
	assert.InDelta(t, 0, hits[0].Distance, 1e-5)
	assert.Equal(t, SourceLegacy, hits[0].Source)
	assert.Nil(t, hits[0].LexicalScore, "the semantic path has no lexical score")
	assert.Equal(t, int64(1), hitCount(t, pool, kept.ID))

	scoped, err := r.SemanticSearch(ctx, tenant, v, 10, SearchOptions{SpaceID: &space, SkipHitCount: true})
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	assert.Equal(t, filed.ID, scoped[0].ID)
	assert.Equal(t, int64(1), hitCount(t, pool, filed.ID), "SkipHitCount leaves the count alone")
}

func testLexical(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	fact := create(t, r, CreateParams{Tenant: tenant, Content: "the zebra report"})
	contribution := ModelContribution(OperationCreate, SourceChat, strPtr("p"), strPtr("m"), nil)
	output := create(t, r, CreateParams{Tenant: tenant, Content: "zebra zebra zebra said the model", Contribution: &contribution})
	create(t, r, CreateParams{Tenant: tenant, Content: "rejected zebra", ReviewState: ReviewRejected})

	hits, err := r.LexicalSearch(ctx, tenant, "zebra", 5, SearchOptions{})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	// Every distance is 1.0, so the stable sort leaves facts ahead of outputs, even
	// though the output ranks higher.
	assert.Equal(t, fact.ID, hits[0].ID)
	assert.Equal(t, output.ID, hits[1].ID)
	assert.InDelta(t, 1.0, hits[0].Distance, 0)
	assert.Positive(t, *hits[0].LexicalScore)
	assert.Equal(t, SourceChat, hits[1].Source)
	assert.Equal(t, "p", *hits[1].Provider)
	assert.Equal(t, "m", *hits[1].Model)
	assert.Equal(t, int64(1), hitCount(t, pool, fact.ID))

	for _, query := range []string{"AND", `"`, "a & b", "!!!"} {
		_, err := r.LexicalSearch(ctx, tenant, query, 5, SearchOptions{SkipHitCount: true})
		assert.NoError(t, err, query)
	}
}

func testSearchDocuments(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	v := vec(t, "words")
	m := create(t, r, CreateParams{Tenant: tenant, Content: "words", Embedding: v})

	byVector, err := r.SearchDocuments(ctx, tenant, "unrelated", v, 5, SearchOptions{})
	require.NoError(t, err)
	require.Len(t, byVector, 1)
	assert.Nil(t, byVector[0].LexicalScore)

	byWords, err := r.SearchDocuments(ctx, tenant, "words", nil, 5, SearchOptions{})
	require.NoError(t, err)
	require.Len(t, byWords, 1)
	assert.Equal(t, m.ID, byWords[0].ID)
	assert.NotNil(t, byWords[0].LexicalScore)
}

func listIDs(t *testing.T, r *Repository, p ListParams) []uuid.UUID {
	t.Helper()
	if p.Limit == 0 {
		p.Limit = 50
	}
	rows, err := r.ListPaginated(context.Background(), p)
	require.NoError(t, err)
	out := make([]uuid.UUID, len(rows))
	for i, m := range rows {
		out[i] = m.ID
	}
	return out
}

func testListDefaults(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	first := create(t, r, CreateParams{Tenant: tenant, Content: "1"})
	second := create(t, r, CreateParams{Tenant: tenant, Content: "2"})
	third := create(t, r, CreateParams{Tenant: tenant, Content: "3"})
	rejected := create(t, r, CreateParams{Tenant: tenant, Content: "r", ReviewState: ReviewRejected})
	for i, id := range []uuid.UUID{first.ID, second.ID, third.ID, rejected.ID} {
		_, err := pool.Exec(ctx, `UPDATE memories SET created_at = NOW() + make_interval(mins => $1) WHERE id = $2`, i, id)
		require.NoError(t, err)
	}

	assert.Equal(t, []uuid.UUID{third.ID, second.ID, first.ID}, listIDs(t, r, ListParams{Tenant: tenant}), "newest first, no rejected")
	assert.Equal(t, []uuid.UUID{second.ID}, listIDs(t, r, ListParams{Tenant: tenant, Limit: 1, Offset: 1}), "exactly limit rows")
	assert.Equal(t, []uuid.UUID{rejected.ID}, listIDs(t, r, ListParams{Tenant: tenant, ReviewStates: []string{ReviewRejected}}))
	assert.Empty(t, listIDs(t, r, ListParams{Tenant: tenant, ReviewStates: []string{}}), "an empty in-set matches nothing")

	all, err := r.List(ctx, tenant)
	require.NoError(t, err)
	assert.Len(t, all, 4, "list has no filter of its own")
}

func testListFilters(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	space := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at) VALUES ($1, $2, 'S', 's', 0, NOW(), NOW())`, space, tenant)
	require.NoError(t, err)
	decision := create(t, r, CreateParams{Tenant: tenant, Content: "d", Kind: strPtr(KindDecision), SpaceID: &space})
	loose := create(t, r, CreateParams{Tenant: tenant, Content: "l"})

	assert.Equal(t, []uuid.UUID{decision.ID}, listIDs(t, r, ListParams{Tenant: tenant, Kind: strPtr(KindDecision)}))
	assert.Equal(t, []uuid.UUID{decision.ID}, listIDs(t, r, ListParams{Tenant: tenant, Space: &SpaceFilter{ID: space}}))
	assert.Equal(t, []uuid.UUID{loose.ID}, listIDs(t, r, ListParams{Tenant: tenant, Space: &SpaceFilter{Unfiled: true}}))
}

func testListHealth(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	backdate := func(id uuid.UUID, column string, days int) {
		t.Helper()
		_, err := pool.Exec(ctx, `UPDATE memories SET `+column+` = NOW() - make_interval(days => $1) WHERE id = $2`, days, id)
		require.NoError(t, err)
	}
	reviewedLongAgo := create(t, r, CreateParams{Tenant: tenant, Content: "a", Tags: []string{"t"}})
	backdate(reviewedLongAgo.ID, "last_reviewed_at", 31)
	reviewedRecently := create(t, r, CreateParams{Tenant: tenant, Content: "b", Tags: []string{"t"}})
	backdate(reviewedRecently.ID, "last_reviewed_at", 2)
	backdate(reviewedRecently.ID, "created_at", 90)
	accessedLongAgo := create(t, r, CreateParams{Tenant: tenant, Content: "c", Tags: []string{"t"}})
	backdate(accessedLongAgo.ID, "last_accessed_at", 31)
	accessedRecently := create(t, r, CreateParams{Tenant: tenant, Content: "d", Tags: []string{"t"}})
	backdate(accessedRecently.ID, "last_accessed_at", 2)
	backdate(accessedRecently.ID, "created_at", 90)
	untouchedOld := create(t, r, CreateParams{Tenant: tenant, Content: "e", Tags: []string{"t"}})
	backdate(untouchedOld.ID, "created_at", 31)
	fresh := create(t, r, CreateParams{Tenant: tenant, Content: "f"})

	assert.ElementsMatch(t, []uuid.UUID{reviewedLongAgo.ID, accessedLongAgo.ID, untouchedOld.ID},
		listIDs(t, r, ListParams{Tenant: tenant, Health: HealthReviewOverdue}))

	pending := create(t, r, CreateParams{Tenant: tenant, Content: "p", Tags: []string{"t"}, ReviewState: ReviewPending})
	assert.Equal(t, []uuid.UUID{pending.ID}, listIDs(t, r, ListParams{Tenant: tenant, Health: HealthPending}))

	assert.Equal(t, []uuid.UUID{fresh.ID}, listIDs(t, r, ListParams{Tenant: tenant, Health: HealthUnorganized}))
	_, err := pool.Exec(ctx, `UPDATE memories SET tags = '{}' WHERE id = $1`, reviewedRecently.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{fresh.ID, reviewedRecently.ID}, listIDs(t, r, ListParams{Tenant: tenant, Health: HealthUnorganized}), "NULL and {} both count")

	require.NoError(t, BumpQueryHits(ctx, pool, []uuid.UUID{reviewedLongAgo.ID, accessedLongAgo.ID, untouchedOld.ID, accessedRecently.ID, pending.ID}))
	assert.ElementsMatch(t, []uuid.UUID{fresh.ID, reviewedRecently.ID}, listIDs(t, r, ListParams{Tenant: tenant, Health: HealthUnused}))
}

func testListByTag(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	contribution := UserContribution(OperationCreate, SourceManual, nil)
	tagged := create(t, r, CreateParams{Tenant: tenant, Content: "t", Tags: []string{"x"}, Embedding: vec(t, "t"), Contribution: &contribution, Kind: strPtr(KindNote)})
	rejected := create(t, r, CreateParams{Tenant: tenant, Content: "r", Tags: []string{"x"}, ReviewState: ReviewRejected})
	create(t, r, CreateParams{Tenant: tenant, Content: "other"})
	_, err := pool.Exec(ctx, `UPDATE memories SET created_at = NOW() + interval '1 minute' WHERE id = $1`, rejected.ID)
	require.NoError(t, err)

	rows, err := r.ListPaginated(ctx, ListParams{Tenant: tenant, Tag: "x", Kind: strPtr(KindFact), Health: HealthPending, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 2, "every other filter is ignored, and rejected rows are not hidden")
	assert.Equal(t, rejected.ID, rows[0].ID)
	assert.Equal(t, ReviewRejected, rows[0].ReviewState)
	got := rows[1]
	assert.Equal(t, tagged.ID, got.ID)
	assert.Equal(t, []string{"x"}, got.Tags)
	assert.NotNil(t, got.CreatedAt)
	// A partial row: the rest is the model's defaults, whatever the column holds.
	assert.Nil(t, got.Kind)
	assert.Equal(t, SourceLegacy, got.OriginKind)
	assert.Nil(t, got.UpdatedAt)
}

func testFindScoped(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	a, b := newTenant(t, pool), newTenant(t, pool)
	r := New(pool, nil)
	m := create(t, r, CreateParams{Tenant: a, Content: "x"})

	got, err := r.Find(ctx, a, m.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "x", got.Content)
	got, err = r.Find(ctx, b, m.ID)
	require.NoError(t, err)
	assert.Nil(t, got)

	deleted, err := r.Delete(ctx, b, m.ID)
	require.NoError(t, err)
	assert.False(t, deleted)
	deleted, err = r.Delete(ctx, a, m.ID)
	require.NoError(t, err)
	assert.True(t, deleted)
}

func testStoredEmbedding(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	v := vec(t, "round trip")
	with := create(t, r, CreateParams{Tenant: tenant, Content: "with", Embedding: v})
	without := create(t, r, CreateParams{Tenant: tenant, Content: "without"})

	got, exists, err := r.StoredEmbedding(ctx, tenant, with.ID)
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, v, got, "the vector survives the text format exactly")

	got, exists, err = r.StoredEmbedding(ctx, tenant, without.ID)
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Nil(t, got)

	_, exists, err = r.StoredEmbedding(ctx, tenant, uuid.New())
	require.NoError(t, err)
	assert.False(t, exists)
}

func insertChunk(t *testing.T, pool *pgxpool.Pool, tenant, memory uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO memory_chunks (chunk_id, document_id, tenant_id, memory_id, ordinal, start_line, end_line, text, content_sha256)
		VALUES ($1, 'doc', $2, $3, 1, 1, 1, 'old text', 'sha')`, uuid.NewString(), tenant, memory)
	require.NoError(t, err)
}

func chunkCount(t *testing.T, pool *pgxpool.Pool, memory uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM memory_chunks WHERE memory_id = $1`, memory).Scan(&n))
	return n
}

func testUpdateContent(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	m := create(t, r, CreateParams{Tenant: tenant, Content: "old", Embedding: vec(t, "old")})
	insertChunk(t, pool, tenant, m.ID)

	contribution := UserContribution(OperationUpdate, SourceManual, nil)
	found, err := r.UpdateContent(ctx, tenant, m.ID, "new", nil, &contribution)
	require.NoError(t, err)
	assert.True(t, found)

	got, err := r.Find(ctx, tenant, m.ID)
	require.NoError(t, err)
	assert.Equal(t, "new", got.Content)
	assert.NotNil(t, got.UpdatedAt, "the local-sync cursor reads it")
	_, exists, err := r.StoredEmbedding(ctx, tenant, m.ID)
	require.NoError(t, err)
	assert.True(t, exists)
	stored, _, err := r.StoredEmbedding(ctx, tenant, m.ID)
	require.NoError(t, err)
	assert.Nil(t, stored, "no stale vector of the old text")
	assert.Equal(t, 0, chunkCount(t, pool, m.ID))
	var updates int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM memory_contributions WHERE memory_id = $1 AND operation = 'update'`, m.ID).Scan(&updates))
	assert.Equal(t, 1, updates)

	v := vec(t, "newer")
	found, err = r.UpdateContent(ctx, tenant, m.ID, "newer", v, nil)
	require.NoError(t, err)
	assert.True(t, found)
	stored, _, err = r.StoredEmbedding(ctx, tenant, m.ID)
	require.NoError(t, err)
	assert.Equal(t, v, stored)

	found, err = r.UpdateContent(ctx, newTenant(t, pool), m.ID, "stolen", nil, &contribution)
	require.NoError(t, err)
	assert.False(t, found)
}

func testUpdateTags(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	m := create(t, r, CreateParams{Tenant: tenant, Content: "x", Tags: []string{"a"}})
	assert.Nil(t, m.UpdatedAt)

	found, err := r.UpdateTags(ctx, tenant, m.ID, []string{"b", "c"}, nil)
	require.NoError(t, err)
	assert.True(t, found)
	got, err := r.Find(ctx, tenant, m.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"b", "c"}, got.Tags)
	assert.NotNil(t, got.UpdatedAt, "a Fluent save stamps updated_at")

	for _, clear := range [][]string{nil, {}} {
		found, err = r.UpdateTags(ctx, tenant, m.ID, clear, nil)
		require.NoError(t, err)
		assert.True(t, found)
		got, err = r.Find(ctx, tenant, m.ID)
		require.NoError(t, err)
		assert.Nil(t, got.Tags)
	}

	contribution := UserContribution(OperationUpdate, SourceManual, nil)
	found, err = r.UpdateTags(ctx, newTenant(t, pool), m.ID, []string{"z"}, &contribution)
	require.NoError(t, err)
	assert.False(t, found)
	var updates int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM memory_contributions WHERE memory_id = $1 AND operation = 'update'`, m.ID).Scan(&updates))
	assert.Equal(t, 0, updates)
}

func testReviewState(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	m := create(t, r, CreateParams{Tenant: tenant, Content: "x"})
	require.NoError(t, r.UpdateReviewState(ctx, tenant, m.ID, ReviewApproved))
	got, err := r.Find(ctx, tenant, m.ID)
	require.NoError(t, err)
	assert.Equal(t, ReviewApproved, got.ReviewState)
	assert.Nil(t, got.UpdatedAt, "raw SQL with no hand bump")
	assert.NoError(t, r.UpdateReviewState(ctx, tenant, uuid.New(), ReviewApproved), "a missing row is not an error")
}

func testSetEmbedding(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant, other := newTenant(t, pool), newTenant(t, pool)
	r := New(pool, nil)
	m := create(t, r, CreateParams{Tenant: tenant, Content: "x"})
	v := vec(t, "image")

	found, err := r.SetEmbedding(ctx, other, m.ID, v)
	require.NoError(t, err)
	assert.False(t, found, "another tenant's row is not found")
	stored, exists, err := r.StoredEmbedding(ctx, tenant, m.ID)
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Nil(t, stored)

	found, err = r.SetEmbedding(ctx, tenant, m.ID, v)
	require.NoError(t, err)
	assert.True(t, found)
	stored, _, err = r.StoredEmbedding(ctx, tenant, m.ID)
	require.NoError(t, err)
	assert.InDeltaSlice(t, v, stored, 1e-5)
	got, err := r.Find(ctx, tenant, m.ID)
	require.NoError(t, err)
	assert.Nil(t, got.UpdatedAt, "raw SQL with no hand bump")
	assert.Equal(t, "x", got.Content)
}

func testSetSpace(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	space := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at) VALUES ($1, $2, 'S', 's', 0, NOW(), NOW())`, space, tenant)
	require.NoError(t, err)
	m := create(t, r, CreateParams{Tenant: tenant, Content: "x"})
	insertChunk(t, pool, tenant, m.ID)

	moved, err := r.SetSpace(ctx, tenant, m.ID, &space)
	require.NoError(t, err)
	assert.True(t, moved)
	got, err := r.Find(ctx, tenant, m.ID)
	require.NoError(t, err)
	assert.Equal(t, &space, got.SpaceID)
	assert.NotNil(t, got.UpdatedAt)
	var chunkSpace *uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `SELECT space_id FROM memory_chunks WHERE memory_id = $1`, m.ID).Scan(&chunkSpace))
	assert.Equal(t, &space, chunkSpace)

	moved, err = r.SetSpace(ctx, tenant, m.ID, nil)
	require.NoError(t, err)
	assert.True(t, moved)
	require.NoError(t, pool.QueryRow(ctx, `SELECT space_id FROM memory_chunks WHERE memory_id = $1`, m.ID).Scan(&chunkSpace))
	assert.Nil(t, chunkSpace)

	moved, err = r.SetSpace(ctx, tenant, uuid.New(), nil)
	require.NoError(t, err)
	assert.False(t, moved)
}

func testForget(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	contribution := ModelContribution(OperationCreate, SourceChat, nil, nil, strPtr("message-1"))
	m := create(t, r, CreateParams{Tenant: tenant, Content: "x", Contribution: &contribution})
	insertChunk(t, pool, tenant, m.ID)
	job := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO memory_index_jobs (id, tenant_id, source_kind, source_id, content) VALUES ($1, $2, 'chat', 'message-1', 'x')`, job, tenant)
	require.NoError(t, err)

	forgotten, err := r.Forget(ctx, tenant, m.ID)
	require.NoError(t, err)
	assert.True(t, forgotten)

	var status string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM memory_index_jobs WHERE id = $1`, job).Scan(&status))
	assert.Equal(t, "suppressed", status, "not re-learned")
	got, err := r.Find(ctx, tenant, m.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Equal(t, 0, chunkCount(t, pool, m.ID))
	var tombstones int
	var deletedAt *time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*), max(deleted_at) FROM memory_sync_tombstones WHERE tenant_id = $1 AND memory_id = $2`, tenant, m.ID).Scan(&tombstones, &deletedAt))
	assert.Equal(t, 1, tombstones)
	assert.NotNil(t, deletedAt, "local-sync reads it")

	forgotten, err = r.Forget(ctx, tenant, m.ID)
	require.NoError(t, err)
	assert.False(t, forgotten)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM memory_sync_tombstones WHERE tenant_id = $1 AND memory_id = $2`, tenant, m.ID).Scan(&tombstones))
	assert.Equal(t, 1, tombstones)

	// A memory with no origin source suppresses nothing, and another tenant's forget
	// touches nothing.
	plain := create(t, r, CreateParams{Tenant: tenant, Content: "plain"})
	forgotten, err = r.Forget(ctx, newTenant(t, pool), plain.ID)
	require.NoError(t, err)
	assert.False(t, forgotten)
	got, err = r.Find(ctx, tenant, plain.ID)
	require.NoError(t, err)
	assert.NotNil(t, got)
}

func insertVaultFile(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, path string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at) VALUES ($1, $2, $3, 'text/markdown', 0, $4, NOW(), NOW())`,
		id, tenant, path, strings.Repeat("0", 64))
	require.NoError(t, err)
	return id
}

func testSourceVaultFile(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	file := insertVaultFile(t, pool, tenant, "n.md")
	m := create(t, r, CreateParams{Tenant: tenant, Content: "x", SourceVaultFileID: &file})

	id, err := r.IDBySourceVaultFileID(ctx, tenant, file)
	require.NoError(t, err)
	require.NotNil(t, id)
	assert.Equal(t, m.ID, *id)
	id, err = r.IDBySourceVaultFileID(ctx, tenant, uuid.New())
	require.NoError(t, err)
	assert.Nil(t, id)

	deleted, err := r.DeleteBySourceVaultFileID(ctx, newTenant(t, pool), file)
	require.NoError(t, err)
	assert.False(t, deleted)
	deleted, err = r.DeleteBySourceVaultFileID(ctx, tenant, file)
	require.NoError(t, err)
	assert.True(t, deleted)
}

func testLineage(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	file := insertVaultFile(t, pool, tenant, "notes/source.md")
	linked := create(t, r, CreateParams{Tenant: tenant, Content: "linked", SourceVaultFileID: &file})
	loose := create(t, r, CreateParams{Tenant: tenant, Content: "loose"})

	l, err := r.FindLineage(ctx, tenant, linked.ID)
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.Equal(t, "linked", l.MemoryContent)
	assert.Equal(t, &file, l.SourceVaultFileID)
	assert.Equal(t, "notes/source.md", *l.SourcePath)
	assert.NotNil(t, l.SourceCreatedAt)

	l, err = r.FindLineage(ctx, tenant, loose.ID)
	require.NoError(t, err)
	require.NotNil(t, l, "still returned with no source linked")
	assert.Nil(t, l.SourceVaultFileID)
	assert.Nil(t, l.SourcePath)

	l, err = r.FindLineage(ctx, newTenant(t, pool), linked.ID)
	require.NoError(t, err)
	assert.Nil(t, l)
}

func testSpaceTopology(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	r := New(pool, nil)
	for slug, notes := range map[string]int{"small": 1, "big": 9, "mid": 5} {
		_, err := pool.Exec(ctx, `INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at) VALUES ($1, $2, $3, $3, $4, NOW(), NOW())`, uuid.New(), tenant, slug, notes)
		require.NoError(t, err)
	}
	rows, err := r.SpaceTopology(ctx, tenant)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	assert.Equal(t, []string{"big", "mid", "small"}, []string{rows[0].Slug, rows[1].Slug, rows[2].Slug})
	assert.Equal(t, 9, rows[0].NoteCount)
}
