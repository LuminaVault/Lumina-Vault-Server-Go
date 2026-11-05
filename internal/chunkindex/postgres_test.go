package chunkindex

import (
	"context"
	"errors"
	"lumina/internal/dbtest"
	"lumina/internal/embedding"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"strings"
	"testing"

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
		// MemoryChunkSearchTests. "backfill is idempotent" needs ChunkBackfillService,
		// which has no caller before the MCP and admin routes.
		"CitationPointsAtSourceLinesThatContainTheMatch":  testCitationPointsAtSource,
		"DifferentSectionsCiteDifferentLineRanges":        testSectionsCiteDifferentRanges,
		"ExactRareTokenFoundWithNoEmbeddingSignal":        testRareToken,
		"OperatorLookingQueriesAreText":                   testOperatorQueries,
		"ChunkSearchNeverCrossesTenants":                  testTenancy,
		"ReindexingAShortenedDocumentLeavesNoStaleChunks": testReindexShortened,
		// EmbeddingDegradationTests that need no route.
		"SearchWithoutAVectorFindsByWordsIncludingUnembedded": testSearchWithoutVector,
		"ChunkIndexingDuringAnOutageStoresNoVectors":          testIndexDuringOutage,
		// Both servers write these rows: the IDs Go stores are the ones Swift computed.
		"StoredChunkIDsMatchSwift":          testStoredIDsMatchSwift,
		"NoSourcePathUsesUppercaseMemoryID": testMemoryWithoutPath,
		"HeadingPathIsStoredAsJSON":         testHeadingPathStored,
		"SharedChunkIDIsSkippedNotAnError":  testSharedChunkID,
		"EmptiedDocumentClearsItsChunks":    testEmptiedDocument,
		"RejectedMemoriesLeaveTheChunkArm":  testRejectedExcluded,
		"SpaceScopesTheChunkArm":            testSpaceScope,
		"HybridSearchMergesArmsAndDedupes":  testHybridIntegration,
		"BrokenChunkArmDegradesToDocuments": testBrokenChunkArm,
		"ReplaceChunksCountMismatch":        testCountMismatch,
		"IndexerWritesTheLinkGraph":         testIndexerLinks,
		"ReresolveUpgradesAndBreaksLinks":   testReresolve,
		"LinkReadsAreTenantScoped":          testLinkTenancy,
		"OrphansAndCounts":                  testOrphansAndCounts,
	} {
		t.Run(name, func(t *testing.T) { fn(t, pool) })
	}
}

const searchDocument = "# Hermes routing\n\nThe router picks a provider per request.\n\n## Fallbacks\n\nWhen the primary provider errors we retry once, then fall through to the\nsecondary. The escalation codeword is zarquonfallback.\n\n## Timeouts\n\nThirty seconds, then abort. The abort marker is zarquontimeout."

func newTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	return newTenantWithID(t, pool, uuid.New())
}

func newTenantWithID(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) uuid.UUID {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at) VALUES ($1, $2, 'x', true, $3, NOW(), NOW()) ON CONFLICT DO NOTHING`,
		id, id.String()+"@test.luminavault", "u"+id.String()[:8])
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(),
		`INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'v', NOW(), NOW()) ON CONFLICT DO NOTHING`, id)
	require.NoError(t, err)
	return id
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

func embedText(t *testing.T, text string, tenant uuid.UUID) []float32 {
	t.Helper()
	vec, err := embedding.Deterministic{}.Embed(context.Background(), text, tenant)
	require.NoError(t, err)
	return vec
}

// seed is the Swift suite's seed: one chunk-indexed document.
func seed(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, path, content string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	vaultFile := insertVaultFile(t, pool, tenant, path)
	m, err := memorystore.New(pool, nil).Create(ctx, memorystore.CreateParams{
		Tenant: tenant, Content: content, Embedding: embedText(t, content, tenant), SourceVaultFileID: &vaultFile,
	})
	require.NoError(t, err)
	written, err := NewIndexer(NewChunkRepository(pool), embedding.Deterministic{}, nil).Index(ctx, IndexParams{
		Tenant: tenant, MemoryID: m.ID, VaultFileID: &vaultFile, SourcePath: &path, Content: content,
	})
	require.NoError(t, err)
	require.Positive(t, written, "seeding must produce chunks")
	return m.ID
}

func search(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, query string) []memorystore.SearchResult {
	t.Helper()
	hits, err := NewChunkRepository(pool).HybridSearch(context.Background(), tenant, query, embedText(t, query, tenant), 5, nil)
	require.NoError(t, err)
	return hits
}

func testCitationPointsAtSource(t *testing.T, pool *pgxpool.Pool) {
	tenant := newTenant(t, pool)
	seed(t, pool, tenant, "projects/hermes.md", searchDocument)

	hits := search(t, pool, tenant, "zarquonfallback")
	require.NotEmpty(t, hits, "the lexical arm must find a rare exact token")
	citation := hits[0].Citation
	require.NotNil(t, citation, "a chunk hit must carry a citation")
	assert.Equal(t, "projects/hermes.md", *citation.Path)
	assert.Equal(t, []string{"Hermes routing", "Fallbacks"}, citation.HeadingPath)

	// Follow the citation the way a user would: read exactly the lines it names.
	lines := strings.Split(searchDocument, "\n")
	require.GreaterOrEqual(t, citation.StartLine, 1)
	require.LessOrEqual(t, citation.EndLine, len(lines))
	cited := strings.Join(lines[citation.StartLine-1:citation.EndLine], "\n")
	assert.Contains(t, cited, "zarquonfallback", "cited lines must contain the term that matched")
	assert.Contains(t, *hits[0].Snippet, "[zarquonfallback]")
	assert.Positive(t, *hits[0].LexicalScore)
}

func testSectionsCiteDifferentRanges(t *testing.T, pool *pgxpool.Pool) {
	tenant := newTenant(t, pool)
	seed(t, pool, tenant, "projects/hermes.md", searchDocument)
	fallback := search(t, pool, tenant, "zarquonfallback")[0].Citation
	timeout := search(t, pool, tenant, "zarquontimeout")[0].Citation
	assert.NotEqual(t, fallback.StartLine, timeout.StartLine)
	assert.Equal(t, []string{"Hermes routing", "Fallbacks"}, fallback.HeadingPath)
	assert.Equal(t, []string{"Hermes routing", "Timeouts"}, timeout.HeadingPath)
}

func testRareToken(t *testing.T, pool *pgxpool.Pool) {
	tenant := newTenant(t, pool)
	seed(t, pool, tenant, "notes/a.md", searchDocument)
	hits := search(t, pool, tenant, "zarquontimeout")
	require.NotEmpty(t, hits)
	found := false
	for _, hit := range hits {
		found = found || strings.Contains(hit.Content, "zarquontimeout")
	}
	assert.True(t, found)
}

func testOperatorQueries(t *testing.T, pool *pgxpool.Pool) {
	tenant := newTenant(t, pool)
	seed(t, pool, tenant, "notes/a.md", searchDocument)
	for _, query := range []string{"fallbacks AND timeouts", `what's the "codeword"?`, "a & b | c", "!!!", "NEAR(x, y)"} {
		search(t, pool, tenant, query) // must not raise
	}
}

func testTenancy(t *testing.T, pool *pgxpool.Pool) {
	a, b := newTenant(t, pool), newTenant(t, pool)
	seed(t, pool, a, "a/secret.md", searchDocument)
	assert.Empty(t, search(t, pool, b, "zarquonfallback"), "tenant B must not see tenant A's chunks")
	assert.NotEmpty(t, search(t, pool, a, "zarquonfallback"), "tenant A must still see its own")
}

func testReindexShortened(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	memory := seed(t, pool, tenant, "notes/a.md", searchDocument)
	assert.NotEmpty(t, search(t, pool, tenant, "zarquontimeout"))

	path := "notes/a.md"
	_, err := NewIndexer(NewChunkRepository(pool), embedding.Deterministic{}, nil).Index(ctx, IndexParams{
		Tenant: tenant, MemoryID: memory, SourcePath: &path, Content: "# Hermes routing\n\nOnly this remains.\n",
	})
	require.NoError(t, err)

	// The dense arm has no distance threshold, so the query still returns the closest
	// chunks. The invariant is that the removed text is gone.
	for _, hit := range search(t, pool, tenant, "zarquontimeout") {
		assert.NotContains(t, hit.Content, "zarquontimeout", "the removed section must no longer be retrievable")
		assert.Contains(t, hit.Content, "Only this remains", "only the rewritten chunks may survive")
	}
}

var errProvider = errors.New("stub http 503")

// outage fails like a provider whose fallback chain is exhausted.
type outage struct{}

func (outage) Embed(context.Context, string, uuid.UUID) ([]float32, error) {
	return nil, errProvider
}

func realVector() []float32 {
	v := make([]float32, embedding.Dimensions)
	v[1] = 1
	return v
}

func testSearchWithoutVector(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	memories := memorystore.New(pool, nil)
	unembedded, err := memories.Create(ctx, memorystore.CreateParams{Tenant: tenant, Content: "Quarterly zebra migration report is due"})
	require.NoError(t, err)
	embedded, err := memories.Create(ctx, memorystore.CreateParams{Tenant: tenant, Content: "Zebra crossings near the school", Embedding: realVector()})
	require.NoError(t, err)

	hits, err := NewHybrid(memories, NewChunkRepository(pool)).Search(ctx, tenant, "zebra migration", nil, 5, memorystore.SearchOptions{})
	require.NoError(t, err)
	var hit *memorystore.SearchResult
	for i := range hits {
		if hits[i].ID == unembedded.ID {
			hit = &hits[i]
		}
	}
	require.NotNil(t, hit)
	require.NotNil(t, hit.LexicalScore)
	assert.Positive(t, *hit.LexicalScore)
	// Swift also asserts ChatGrounding.relevant keeps this hit. That is chat (wave 5).

	// With a vector, the NULL row is excluded from the dense arm instead of breaking it.
	dense, err := memories.SemanticSearch(ctx, tenant, realVector(), 10, memorystore.SearchOptions{})
	require.NoError(t, err)
	ids := make([]uuid.UUID, len(dense))
	for i, d := range dense {
		ids[i] = d.ID
	}
	assert.Contains(t, ids, embedded.ID)
	assert.NotContains(t, ids, unembedded.ID)
}

func testIndexDuringOutage(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	content := "# Trip\n\nThe ferry to Ilha Deserta leaves at noon.\n"
	m, err := memorystore.New(pool, nil).Create(ctx, memorystore.CreateParams{Tenant: tenant, Content: content})
	require.NoError(t, err)

	chunks := NewChunkRepository(pool)
	path := "trips/ferry.md"
	count, err := NewIndexer(chunks, outage{}, nil).Index(ctx, IndexParams{Tenant: tenant, MemoryID: m.ID, SourcePath: &path, Content: content})
	require.NoError(t, err)
	assert.Positive(t, count)

	var total, missing int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE embedding IS NULL) FROM memory_chunks WHERE tenant_id = $1 AND memory_id = $2`, tenant, m.ID).Scan(&total, &missing))
	assert.Equal(t, count, total)
	assert.Equal(t, total, missing)

	hits, err := chunks.HybridSearch(ctx, tenant, "ferry Ilha Deserta", nil, 5, nil)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	assert.Equal(t, m.ID, hits[0].ID)
	assert.Equal(t, "trips/ferry.md", *hits[0].Citation.Path)
	assert.InDelta(t, 1.0, hits[0].Distance, 0, "a lexical-only hit reports maximally far")
}

// testStoredIDsMatchSwift runs the indexer into Postgres and compares the rows with
// what Swift's sources computed for the same tenant and path.
func testStoredIDsMatchSwift(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenantWithID(t, pool, uuid.MustParse(goldenTenant))
	checked := 0
	for _, g := range loadGolden(t) {
		if g.Case.Op != "chunk" || g.Case.Max != nil || len(g.Want.Chunks) == 0 || checked >= 4 {
			continue
		}
		checked++
		content := *g.Case.S
		m, err := memorystore.New(pool, nil).Create(ctx, memorystore.CreateParams{Tenant: tenant, Content: content})
		require.NoError(t, err)
		path := "golden.md"
		count, err := NewIndexer(NewChunkRepository(pool), embedding.Deterministic{}, nil).Index(ctx, IndexParams{Tenant: tenant, MemoryID: m.ID, SourcePath: &path, Content: content})
		require.NoError(t, err)
		require.Equal(t, len(g.Want.Chunks), count)

		rows, err := pool.Query(ctx, `SELECT chunk_id, content_sha256, ordinal, start_line, end_line, text, heading_path::text FROM memory_chunks WHERE memory_id = $1 ORDER BY ordinal`, m.ID)
		require.NoError(t, err)
		i := 0
		for rows.Next() {
			var (
				id, sha, text, headings string
				ordinal, start, end     int
			)
			require.NoError(t, rows.Scan(&id, &sha, &ordinal, &start, &end, &text, &headings))
			want := g.Want.Chunks[i]
			assert.Equal(t, want.ChunkID, id)
			assert.Equal(t, want.SHA, sha)
			assert.Equal(t, want.Ordinal, ordinal)
			assert.Equal(t, want.StartLine, start)
			assert.Equal(t, want.EndLine, end)
			assert.Equal(t, want.Text, text)
			assert.Equal(t, want.HeadingPath, DecodeHeadingPath(&headings))
			i++
		}
		require.NoError(t, rows.Err())
		rows.Close()
		// Clear them: the next document shares this path, and its ordinal 1 would
		// otherwise land on the same document ID.
		require.NoError(t, NewChunkRepository(pool).DeleteChunks(ctx, tenant, m.ID))
	}
	assert.Equal(t, 4, checked)
}

func testMemoryWithoutPath(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	var want string
	for _, g := range loadGolden(t) {
		if g.Case.Op == "docid-memory" {
			want = *g.Want.Str
			tenant := uuid.MustParse(g.Case.T)
			memory := uuid.MustParse(*g.Case.S)
			newTenantWithID(t, pool, tenant)
			_, err := pool.Exec(ctx, `INSERT INTO memories (id, tenant_id, content, created_at) VALUES ($1, $2, 'x', NOW())`, memory, tenant)
			require.NoError(t, err)
			_, err = NewIndexer(NewChunkRepository(pool), embedding.Deterministic{}, nil).Index(ctx, IndexParams{Tenant: tenant, MemoryID: memory, Content: "# T\n\nbody"})
			require.NoError(t, err)
			var got string
			var path *string
			require.NoError(t, pool.QueryRow(ctx, `SELECT document_id, source_path FROM memory_chunks WHERE memory_id = $1 LIMIT 1`, memory).Scan(&got, &path))
			assert.Equal(t, want, got)
			assert.Nil(t, path)
		}
	}
	require.NotEmpty(t, want)
}

func testHeadingPathStored(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	content := "# a/b <c> & d \"e\" é 日本 👍\n\nbody"
	m, err := memorystore.New(pool, nil).Create(ctx, memorystore.CreateParams{Tenant: tenant, Content: content})
	require.NoError(t, err)
	_, err = NewIndexer(NewChunkRepository(pool), embedding.Deterministic{}, nil).Index(ctx, IndexParams{Tenant: tenant, MemoryID: m.ID, Content: content})
	require.NoError(t, err)
	var stored string
	// jsonb stores the value, not the spelling: Swift's "\/" and Go's "<" both
	// land as this text.
	require.NoError(t, pool.QueryRow(ctx, `SELECT heading_path::text FROM memory_chunks WHERE memory_id = $1`, m.ID).Scan(&stored))
	assert.Equal(t, `["a/b <c> & d \"e\" é 日本 👍"]`, stored)

	content = "preamble only"
	_, err = NewIndexer(NewChunkRepository(pool), embedding.Deterministic{}, nil).Index(ctx, IndexParams{Tenant: tenant, MemoryID: m.ID, Content: content})
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `SELECT heading_path::text FROM memory_chunks WHERE memory_id = $1`, m.ID).Scan(&stored))
	assert.Equal(t, `[]`, stored)
}

// Two memories at one path share a document ID, so identical chunks share a chunk_id.
func testSharedChunkID(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	memories := memorystore.New(pool, nil)
	first, err := memories.Create(ctx, memorystore.CreateParams{Tenant: tenant, Content: "x"})
	require.NoError(t, err)
	second, err := memories.Create(ctx, memorystore.CreateParams{Tenant: tenant, Content: "x"})
	require.NoError(t, err)
	path := "same.md"
	indexer := NewIndexer(NewChunkRepository(pool), embedding.Deterministic{}, nil)
	_, err = indexer.Index(ctx, IndexParams{Tenant: tenant, MemoryID: first.ID, SourcePath: &path, Content: "same body"})
	require.NoError(t, err)
	_, err = indexer.Index(ctx, IndexParams{Tenant: tenant, MemoryID: second.ID, SourcePath: &path, Content: "same body"})
	require.NoError(t, err)
	var firstCount, secondCount int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE memory_id = $1), count(*) FILTER (WHERE memory_id = $2) FROM memory_chunks WHERE tenant_id = $3`, first.ID, second.ID, tenant).Scan(&firstCount, &secondCount))
	assert.Equal(t, 1, firstCount)
	assert.Equal(t, 0, secondCount)
}

func testEmptiedDocument(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	memory := seed(t, pool, tenant, "notes/e.md", searchDocument)
	chunks := NewChunkRepository(pool)
	has, err := chunks.HasChunks(ctx, tenant)
	require.NoError(t, err)
	assert.True(t, has)
	count, err := NewIndexer(chunks, embedding.Deterministic{}, nil).Index(ctx, IndexParams{Tenant: tenant, MemoryID: memory, Content: " \n\t\n"})
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	has, err = chunks.HasChunks(ctx, tenant)
	require.NoError(t, err)
	assert.False(t, has)
}

func testRejectedExcluded(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	memory := seed(t, pool, tenant, "notes/r.md", searchDocument)
	require.NoError(t, memorystore.New(pool, nil).UpdateReviewState(ctx, tenant, memory, memorystore.ReviewRejected))
	assert.Empty(t, search(t, pool, tenant, "zarquonfallback"))
}

func testSpaceScope(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	var space uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at) VALUES ($1, $2, 'S', 's', 0, NOW(), NOW()) RETURNING id`, uuid.New(), tenant).Scan(&space))
	memory := seed(t, pool, tenant, "notes/s.md", searchDocument)
	chunks := NewChunkRepository(pool)

	hits, err := chunks.HybridSearch(ctx, tenant, "zarquonfallback", nil, 5, &space)
	require.NoError(t, err)
	assert.Empty(t, hits, "the chunk is unfiled")

	// setSpace moves the memory and its chunks together.
	moved, err := memorystore.New(pool, nil).SetSpace(ctx, tenant, memory, &space)
	require.NoError(t, err)
	assert.True(t, moved)
	hits, err = chunks.HybridSearch(ctx, tenant, "zarquonfallback", nil, 5, &space)
	require.NoError(t, err)
	assert.NotEmpty(t, hits)
}

func testHybridIntegration(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	memory := seed(t, pool, tenant, "notes/h.md", searchDocument)
	loose, err := memorystore.New(pool, nil).Create(ctx, memorystore.CreateParams{Tenant: tenant, Content: "zarquonfallback with no chunks at all"})
	require.NoError(t, err)

	hits, err := NewHybrid(memorystore.New(pool, nil), NewChunkRepository(pool)).Search(ctx, tenant, "zarquonfallback", nil, 5, memorystore.SearchOptions{})
	require.NoError(t, err)
	require.Len(t, hits, 2, "the chunked memory appears once")
	assert.Equal(t, memory, hits[0].ID)
	assert.NotNil(t, hits[0].Citation)
	assert.Equal(t, loose.ID, hits[1].ID)
	assert.Nil(t, hits[1].Citation)

	// Only the document arm counts hits. The chunked memory is found by it too.
	var bumped int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT query_hit_count FROM memories WHERE id = $1`, loose.ID).Scan(&bumped))
	assert.Equal(t, int64(1), bumped)
	_, err = NewHybrid(memorystore.New(pool, nil), NewChunkRepository(pool)).Search(ctx, tenant, "zarquonfallback", nil, 5, memorystore.SearchOptions{SkipHitCount: true})
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `SELECT query_hit_count FROM memories WHERE id = $1`, loose.ID).Scan(&bumped))
	assert.Equal(t, int64(1), bumped)
}

func testBrokenChunkArm(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	seed(t, pool, tenant, "notes/b.md", searchDocument)
	closed, err := pgxpool.NewWithConfig(ctx, pool.Config())
	require.NoError(t, err)
	closed.Close()

	hits, err := NewHybrid(memorystore.New(pool, nil), NewChunkRepository(closed)).Search(ctx, tenant, "zarquonfallback", nil, 5, memorystore.SearchOptions{})
	require.NoError(t, err, "a broken chunk query must not take down search")
	require.Len(t, hits, 1)
	assert.Nil(t, hits[0].Citation)

	_, err = NewHybrid(memorystore.New(closed, nil), NewChunkRepository(pool)).Search(ctx, tenant, "zarquonfallback", nil, 5, memorystore.SearchOptions{})
	assert.Error(t, err, "a broken document arm is returned")
}

func testCountMismatch(t *testing.T, pool *pgxpool.Pool) {
	err := NewChunkRepository(pool).ReplaceChunks(context.Background(), ReplaceChunksParams{Chunks: Chunk("x")})
	assert.ErrorContains(t, err, "count mismatch")
}

func testIndexerLinks(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	target := insertVaultFile(t, pool, tenant, "projects/hermes.md")
	source := insertVaultFile(t, pool, tenant, "notes/source.md")
	sourcePath := "notes/source.md"
	content := "see [[hermes#Routing|router]]\n\n```\n[[not-a-link]]\n```\n[[missing]]"
	m, err := memorystore.New(pool, nil).Create(ctx, memorystore.CreateParams{Tenant: tenant, Content: content})
	require.NoError(t, err)
	links := NewLinkRepository(pool)
	indexer := NewIndexer(NewChunkRepository(pool), embedding.Deterministic{}, links)
	_, err = indexer.Index(ctx, IndexParams{Tenant: tenant, MemoryID: m.ID, VaultFileID: &source, SourcePath: &sourcePath, Content: content})
	require.NoError(t, err)

	out, err := links.Outgoing(ctx, tenant, source)
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, 1, out[0].SourceLine)
	assert.Equal(t, "hermes#Routing|router", out[0].RawTarget)
	assert.Equal(t, "hermes", out[0].TargetSlug)
	assert.Equal(t, "Routing", *out[0].TargetHeading)
	assert.Equal(t, "router", *out[0].Label)
	assert.Equal(t, string(Resolved), out[0].ResolutionState)
	assert.Equal(t, "projects/hermes.md", *out[0].TargetPath)
	assert.Equal(t, string(Unresolved), out[1].ResolutionState)
	assert.Nil(t, out[1].TargetPath)

	in, err := links.Incoming(ctx, tenant, target)
	require.NoError(t, err)
	require.Len(t, in, 1)
	assert.Equal(t, "notes/source.md", *in[0].SourcePath)
	assert.Equal(t, 1, in[0].SourceLine)

	// A document that lost a link keeps no stale edge.
	_, err = indexer.Index(ctx, IndexParams{Tenant: tenant, MemoryID: m.ID, VaultFileID: &source, SourcePath: &sourcePath, Content: "no links now"})
	require.NoError(t, err)
	out, err = links.Outgoing(ctx, tenant, source)
	require.NoError(t, err)
	assert.Empty(t, out)

	var resolved bool
	_, err = indexer.Index(ctx, IndexParams{Tenant: tenant, MemoryID: m.ID, VaultFileID: &source, SourcePath: &sourcePath, Content: "[[hermes]]"})
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `SELECT resolved FROM vault_links WHERE tenant_id = $1`, tenant).Scan(&resolved))
	assert.True(t, resolved, "the resolved column agrees with resolution_state")
}

func testReresolve(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	source := insertVaultFile(t, pool, tenant, "s.md")
	links := NewLinkRepository(pool)
	require.NoError(t, links.ReplaceLinks(ctx, tenant, source, nil, ExtractWikilinks("[[later]] and [[index]]")))
	counts, err := links.Counts(ctx, tenant)
	require.NoError(t, err)
	assert.Equal(t, LinkCounts{Unresolved: 2}, counts)

	changed, err := links.Reresolve(ctx, tenant)
	require.NoError(t, err)
	assert.Equal(t, 0, changed, "nothing changed in the vault")

	later := insertVaultFile(t, pool, tenant, "notes/later.md")
	insertVaultFile(t, pool, tenant, "a/index.md")
	insertVaultFile(t, pool, tenant, "b/index.md")
	changed, err = links.Reresolve(ctx, tenant)
	require.NoError(t, err)
	assert.Equal(t, 2, changed)
	counts, err = links.Counts(ctx, tenant)
	require.NoError(t, err)
	assert.Equal(t, LinkCounts{Resolved: 1, Ambiguous: 1}, counts)
	in, err := links.Incoming(ctx, tenant, later)
	require.NoError(t, err)
	assert.Len(t, in, 1)

	_, err = pool.Exec(ctx, `DELETE FROM vault_files WHERE id = $1`, later)
	require.NoError(t, err)
	changed, err = links.Reresolve(ctx, tenant)
	require.NoError(t, err)
	assert.Equal(t, 1, changed, "deleting a target breaks its links")
}

func testLinkTenancy(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	a, b := newTenant(t, pool), newTenant(t, pool)
	insertVaultFile(t, pool, b, "theirs.md")
	source := insertVaultFile(t, pool, a, "s.md")
	links := NewLinkRepository(pool)
	require.NoError(t, links.ReplaceLinks(ctx, a, source, nil, ExtractWikilinks("[[theirs]]")))
	out, err := links.Outgoing(ctx, a, source)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, string(Unresolved), out[0].ResolutionState, "another tenant's file is no target")
	out, err = links.Outgoing(ctx, b, source)
	require.NoError(t, err)
	assert.Empty(t, out)
	counts, err := links.Counts(ctx, b)
	require.NoError(t, err)
	assert.Equal(t, LinkCounts{}, counts)
}

func testOrphansAndCounts(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	source := insertVaultFile(t, pool, tenant, "b-source.md")
	insertVaultFile(t, pool, tenant, "a-linked.md")
	insertVaultFile(t, pool, tenant, "c-orphan.md")
	links := NewLinkRepository(pool)
	require.NoError(t, links.ReplaceLinks(ctx, tenant, source, nil, ExtractWikilinks("[[a-linked]]")))
	paths, err := links.OrphanPaths(ctx, tenant, DefaultOrphanLimit)
	require.NoError(t, err)
	assert.Equal(t, []string{"b-source.md", "c-orphan.md"}, paths)
	paths, err = links.OrphanPaths(ctx, tenant, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"b-source.md"}, paths, "the limit floors at 1")
}
