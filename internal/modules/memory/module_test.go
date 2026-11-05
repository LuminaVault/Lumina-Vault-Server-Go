package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/chunkindex"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/entitlement"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/modules/achievements"
	"lumina/internal/modules/memory"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture users, parity/seed/memory.sql.
var (
	ann    = uuid.MustParse("3E300000-0000-4000-8000-0000000000A1")
	bob    = uuid.MustParse("3E300000-0000-4000-8000-0000000000A2")
	cat    = uuid.MustParse("3E300000-0000-4000-8000-0000000000A3")
	gra    = uuid.MustParse("3E300000-0000-4000-8000-0000000000A5")
	cur    = uuid.MustParse("3E300000-0000-4000-8000-0000000000A6")
	newbie = uuid.MustParse("3E300000-0000-4000-8000-0000000000A8")
	wri    = uuid.MustParse("3E300000-0000-4000-8000-0000000000A9")
	vie    = uuid.MustParse("3E300000-0000-4000-8000-0000000000AA")
	lim    = uuid.MustParse("3E300000-0000-4000-8000-0000000000AC")
	sug    = uuid.MustParse("3E300000-0000-4000-8000-0000000000AD")
	keys   = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

const teamVault = "3E3C0000-0000-4000-8000-0000000000C1"

// switchableEmbedding is EmbeddingDegradationTests' SwitchableEmbedding: axis 0 of a
// unit vector while working, an outage on demand.
type switchableEmbedding struct {
	failing atomic.Bool
	calls   atomic.Int64
}

func (s *switchableEmbedding) Embed(context.Context, string, uuid.UUID) ([]float32, error) {
	s.calls.Add(1)
	if s.failing.Load() {
		return nil, errors.New("provider out of credit")
	}
	v := make([]float32, 1536)
	v[0] = 1
	return v, nil
}

type recordedEvents struct {
	mu     sync.Mutex
	events []uuid.UUID
}

func (r *recordedEvents) Enqueue(tenant uuid.UUID, _ achievements.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, tenant)
}

type env struct {
	t          *testing.T
	e          *echo.Echo
	pool       *pgxpool.Pool
	repo       *memorystore.Repository
	embeddings *switchableEmbedding
	chunks     *chunkindex.ChunkRepository
	sink       *recordedEvents
	upserted   *atomic.Int64
	paths      vaultpaths.Paths
}

func setupOpts(t *testing.T, mutate func(*memory.Options)) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "memory.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	paths := vaultpaths.Paths{Root: t.TempDir()}
	embeddings := &switchableEmbedding{}
	chunks := chunkindex.NewChunkRepository(pool)
	sink := &recordedEvents{}
	upserted := &atomic.Int64{}
	repo := memorystore.New(pool, nil)
	opts := memory.Options{
		Identify:               auth.Identify(keys, auth.PGUsers(pool)),
		Limits:                 ratelimit.NewStore(),
		Paths:                  paths,
		Embeddings:             embeddings,
		Indexer:                chunkindex.NewIndexer(chunks, embeddings, chunkindex.NewLinkRepository(pool)),
		Preferences:            llm.NewPreferenceRepository(pool),
		Achievements:           sink,
		OnUpserted:             func(uuid.UUID) { upserted.Add(1) },
		HybridExecutionEnabled: true,
		RelatedMaxDistance:     memory.DefaultRelatedMaxDistance,
	}
	if mutate != nil {
		mutate(&opts)
	}
	m := memory.New(pool, repo, opts)
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{m},
	})
	return env{t: t, e: e, pool: pool, repo: repo, embeddings: embeddings, chunks: chunks, sink: sink, upserted: upserted, paths: paths}
}

func setup(t *testing.T) env {
	t.Helper()
	return setupOpts(t, nil)
}

type resp struct {
	code int
	ct   string
	body []byte
}

func (v env) do(user uuid.UUID, method, path, body string, hdr ...string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()}
}

func decode[T any](t *testing.T, r resp) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(r.body, &out), string(r.body))
	return out
}

type memoryOut struct {
	ID              string   `json:"id"`
	Content         string   `json:"content"`
	Tags            []string `json:"tags"`
	CreatedAt       *string  `json:"createdAt"`
	Lat             *float64 `json:"lat"`
	Lng             *float64 `json:"lng"`
	AccuracyM       *float64 `json:"accuracyM"`
	PlaceName       *string  `json:"placeName"`
	ReviewState     string   `json:"reviewState"`
	Kind            *string  `json:"kind"`
	SpaceID         *string  `json:"spaceId"`
	UpdatedByUserID *string  `json:"updatedByUserId"`
	CreatedByUserID *string  `json:"createdByUserId"`
	Provenance      *struct {
		CreatedBy *struct {
			Model *struct {
				Provider string `json:"provider"`
				Model    string `json:"model"`
			} `json:"model"`
		} `json:"createdBy"`
		Contributors []struct {
			Provider string `json:"provider"`
		} `json:"contributors"`
	} `json:"provenance"`
}

type listOut struct {
	Memories []memoryOut `json:"memories"`
	Limit    int         `json:"limit"`
	Offset   int         `json:"offset"`
}

func (v env) upsert(user uuid.UUID, body string) string {
	v.t.Helper()
	r := v.do(user, http.MethodPost, "/v1/memory/upsert", body)
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	return decode[struct {
		MemoryID string `json:"memoryId"`
	}](v.t, r).MemoryID
}

func (v env) content(user uuid.UUID, text string) string {
	v.t.Helper()
	raw, err := json.Marshal(map[string]string{"content": text})
	require.NoError(v.t, err)
	return v.upsert(user, string(raw))
}

// MemoryCRUDTests.

func TestListReturnsAllMemoriesForTenant(t *testing.T) {
	v := setup(t)
	v.content(newbie, "first memory")
	v.content(newbie, "second memory")
	r := v.do(newbie, http.MethodGet, "/v1/memory", "")
	require.Equal(t, http.StatusOK, r.code)
	list := decode[listOut](t, r)
	assert.GreaterOrEqual(t, len(list.Memories), 2)
	assert.Equal(t, 20, list.Limit)
	assert.Equal(t, 0, list.Offset)
}

func TestListSupportsLimitAndOffset(t *testing.T) {
	v := setup(t)
	for i := range 3 {
		v.content(newbie, fmt.Sprintf("memo %d", i))
	}
	r := v.do(newbie, http.MethodGet, "/v1/memory?limit=2&offset=0", "")
	require.Equal(t, http.StatusOK, r.code)
	list := decode[listOut](t, r)
	assert.Len(t, list.Memories, 2)
	assert.Equal(t, 2, list.Limit)
	assert.Equal(t, 0, list.Offset)
}

func TestListFilterByAnUnknownSpaceIsA404(t *testing.T) {
	v := setup(t)
	assert.Equal(t, http.StatusNotFound, v.do(newbie, http.MethodGet, "/v1/memory?space=stocks", "").code)
}

func TestPatchUpdatesContentAndReembeds(t *testing.T) {
	v := setup(t)
	id := v.content(newbie, "before edit")
	before := v.embeddings.calls.Load()
	r := v.do(newbie, http.MethodPatch, "/v1/memory/"+id, `{"content":"after edit"}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	dto := decode[memoryOut](t, r)
	assert.Equal(t, id, dto.ID)
	assert.Equal(t, "after edit", dto.Content)
	assert.Greater(t, v.embeddings.calls.Load(), before)
}

func TestPatchTagsRoundTrips(t *testing.T) {
	v := setup(t)
	id := v.content(newbie, "taggable")
	r := v.do(newbie, http.MethodPatch, "/v1/memory/"+id, `{"tags":["work","ai"]}`)
	require.Equal(t, http.StatusOK, r.code)
	assert.ElementsMatch(t, []string{"work", "ai"}, decode[memoryOut](t, r).Tags)
	r = v.do(newbie, http.MethodGet, "/v1/memory?tag=ai", "")
	require.Equal(t, http.StatusOK, r.code)
	list := decode[listOut](t, r)
	require.Len(t, list.Memories, 1)
	assert.Equal(t, id, list.Memories[0].ID)
}

func TestPatchEmptyBodyReturns400(t *testing.T) {
	v := setup(t)
	id := v.content(newbie, "no-op patch")
	assert.Equal(t, http.StatusBadRequest, v.do(newbie, http.MethodPatch, "/v1/memory/"+id, "{}").code)
}

func TestDeleteRemovesMemoryAndIsIdempotent(t *testing.T) {
	v := setup(t)
	id := v.content(newbie, "to delete")
	assert.Equal(t, http.StatusNoContent, v.do(newbie, http.MethodDelete, "/v1/memory/"+id, "").code)
	assert.Equal(t, http.StatusNotFound, v.do(newbie, http.MethodDelete, "/v1/memory/"+id, "").code)
}

func TestTenantIsolationDeleteIsScoped(t *testing.T) {
	v := setup(t)
	id := v.content(newbie, "alice's secret")
	assert.Equal(t, http.StatusNotFound, v.do(bob, http.MethodDelete, "/v1/memory/"+id, "").code)
	assert.Equal(t, http.StatusOK, v.do(newbie, http.MethodGet, "/v1/memory/"+id, "").code)
}

func TestUnauthenticatedReturns401(t *testing.T) {
	v := setup(t)
	assert.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, "/v1/memory", "").code)
}

// "Memory health queues validate their filter".

func TestHealthFilterIsValidated(t *testing.T) {
	v := setup(t)
	r := v.do(newbie, http.MethodGet, "/v1/memory?healthFilter=not-real", "")
	assert.Equal(t, http.StatusBadRequest, r.code)
	assert.Contains(t, string(r.body), "healthFilter must be one of review-overdue, pending, unorganized, unused")
}

// MemoryGeoTests.

func TestUpsertWithFullGeoPersistsAllFourColumns(t *testing.T) {
	v := setup(t)
	id := v.upsert(newbie, `{"content":"had coffee at A Brasileira","lat":38.7106,"lng":-9.1418,"accuracyM":12.5,"placeName":"Café A Brasileira, Lisbon"}`)
	dto := decode[memoryOut](t, v.do(newbie, http.MethodGet, "/v1/memory/"+id, ""))
	assert.InDelta(t, 38.7106, *dto.Lat, 0)
	assert.InDelta(t, -9.1418, *dto.Lng, 0)
	assert.InDelta(t, 12.5, *dto.AccuracyM, 0)
	assert.Equal(t, "Café A Brasileira, Lisbon", *dto.PlaceName)
}

func TestUpsertWithoutGeoLeavesColumnsNull(t *testing.T) {
	v := setup(t)
	id := v.content(newbie, "plain memory, no location")
	dto := decode[memoryOut](t, v.do(newbie, http.MethodGet, "/v1/memory/"+id, ""))
	assert.Nil(t, dto.Lat)
	assert.Nil(t, dto.Lng)
	assert.Nil(t, dto.AccuracyM)
	assert.Nil(t, dto.PlaceName)
}

func TestPartialGeoPersistsSuppliedFieldsOnly(t *testing.T) {
	v := setup(t)
	id := v.upsert(newbie, `{"content":"queued before geocode finished","lat":38.7,"lng":-9.14,"accuracyM":25}`)
	dto := decode[memoryOut](t, v.do(newbie, http.MethodGet, "/v1/memory/"+id, ""))
	assert.InDelta(t, 38.7, *dto.Lat, 0)
	assert.InDelta(t, -9.14, *dto.Lng, 0)
	assert.InDelta(t, 25, *dto.AccuracyM, 0)
	assert.Nil(t, dto.PlaceName)
}

// Upsert's side effects.

func TestUpsertStampsTheActorAndFiresItsSideEffects(t *testing.T) {
	v := setup(t)
	id := v.upsert(newbie, `{"content":"  stamped  ","kind":"fact"}`)
	dto := decode[memoryOut](t, v.do(newbie, http.MethodGet, "/v1/memory/"+id, ""))
	assert.Equal(t, "stamped", dto.Content)
	assert.Equal(t, "fact", *dto.Kind)
	assert.True(t, strings.EqualFold(newbie.String(), *dto.CreatedByUserID))
	assert.True(t, strings.EqualFold(newbie.String(), *dto.UpdatedByUserID))
	assert.Equal(t, int64(1), v.upserted.Load())
	assert.Equal(t, []uuid.UUID{newbie}, v.sink.events)

	var events int
	var key string
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT count(*), max(idempotency_key) FROM analytics_events WHERE vault_id = $1 AND event_name = 'memory_created'`, newbie).Scan(&events, &key))
	assert.Equal(t, 1, events)
	assert.Equal(t, "memory-created:"+strings.ToUpper(id), key)
}

func TestSpaceIDQueryOnUpsert(t *testing.T) {
	v := setup(t)
	for _, c := range []struct {
		q    string
		code int
		msg  string
	}{
		{"nope", http.StatusBadRequest, "`space_id` is not a valid UUID"},
		{"3E355000-0000-4000-8000-0000000000A1", http.StatusBadRequest, "`space_id` does not belong to the caller"},
		{"3E355000-0000-4000-8000-0000000000A9", http.StatusOK, ""},
		{"", http.StatusOK, ""},
	} {
		r := v.do(wri, http.MethodPost, "/v1/memory/upsert?space_id="+c.q, `{"content":"x"}`)
		assert.Equal(t, c.code, r.code, c.q)
		assert.Contains(t, string(r.body), c.msg)
	}
}

// EmbeddingDegradationTests, the two HTTP tests.

func embeddingMissing(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var missing bool
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT embedding IS NULL FROM memories WHERE id = $1`, id).Scan(&missing))
	return missing
}

func TestUpsertSavesTheMemoryWithoutAnEmbeddingWhenTheProviderIsOutOfCredit(t *testing.T) {
	v := setup(t)
	v.embeddings.failing.Store(true)
	id := v.content(newbie, "Call the plumber on Thursday")
	assert.GreaterOrEqual(t, v.embeddings.calls.Load(), int64(1))
	assert.True(t, embeddingMissing(t, v.pool, id))
}

func TestEditingAMemoryDuringAnOutageSavesTheNewTextWithoutAStaleVector(t *testing.T) {
	v := setup(t)
	id := v.content(newbie, "Dentist at 9")
	v.embeddings.failing.Store(true)
	assert.Equal(t, http.StatusOK, v.do(newbie, http.MethodPatch, "/v1/memory/"+id, `{"content":"Dentist moved to 10"}`).code)
	assert.True(t, embeddingMissing(t, v.pool, id))
	var content string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT content FROM memories WHERE id = $1`, id).Scan(&content))
	assert.Equal(t, "Dentist moved to 10", content)
}

// MemoryLineageTests.

type lineageOut struct {
	MemoryID string `json:"memoryId"`
	Source   *struct {
		VaultFileID string `json:"vaultFileId"`
		Path        string `json:"path"`
	} `json:"source"`
	Trace string `json:"trace"`
}

func TestLineageReturnsSourceWhenLinked(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	file := uuid.New()
	_, err := v.pool.Exec(ctx, `INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at) VALUES ($1, $2, 'notes/2026-05-11-standup.md', 'text/markdown', 0, $3, '2026-05-11T00:00:00Z', '2026-05-11T00:00:00Z')`, file, newbie, strings.Repeat("0", 64))
	require.NoError(t, err)
	vec := make([]float32, 1536)
	m, err := v.repo.Create(ctx, memorystore.CreateParams{Tenant: newbie, Content: "Met with infra team about migrations.", Embedding: vec, SourceVaultFileID: &file})
	require.NoError(t, err)
	r := v.do(newbie, http.MethodGet, "/v1/memory/"+m.ID.String()+"/lineage", "")
	require.Equal(t, http.StatusOK, r.code)
	body := decode[lineageOut](t, r)
	assert.True(t, strings.EqualFold(m.ID.String(), body.MemoryID))
	require.NotNil(t, body.Source)
	assert.True(t, strings.EqualFold(file.String(), body.Source.VaultFileID))
	assert.Equal(t, "notes/2026-05-11-standup.md", body.Source.Path)
	assert.Contains(t, body.Trace, "notes/2026-05-11-standup.md")
	assert.Contains(t, body.Trace, "Hermes learned")
	assert.Contains(t, body.Trace, "2026-05-11")
}

func TestLineageReturnsNullSourceForUnlinkedMemory(t *testing.T) {
	v := setup(t)
	m, err := v.repo.Create(context.Background(), memorystore.CreateParams{Tenant: newbie, Content: "Random thought, no source.", Embedding: make([]float32, 1536)})
	require.NoError(t, err)
	r := v.do(newbie, http.MethodGet, "/v1/memory/"+m.ID.String()+"/lineage", "")
	require.Equal(t, http.StatusOK, r.code)
	body := decode[lineageOut](t, r)
	assert.True(t, strings.EqualFold(m.ID.String(), body.MemoryID))
	assert.Nil(t, body.Source)
	assert.Contains(t, body.Trace, "no source file")
}

func TestLineage404s(t *testing.T) {
	v := setup(t)
	assert.Equal(t, http.StatusNotFound, v.do(newbie, http.MethodGet, "/v1/memory/"+uuid.NewString()+"/lineage", "").code)
	m, err := v.repo.Create(context.Background(), memorystore.CreateParams{Tenant: bob, Content: "Tenant A secret.", Embedding: make([]float32, 1536)})
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, v.do(newbie, http.MethodGet, "/v1/memory/"+m.ID.String()+"/lineage", "").code, "cross tenant")
}

// MemoryGraphControllerTests and MemoryGraphServiceTests.

type graphOut struct {
	Nodes []struct {
		ID       string   `json:"id"`
		Title    string   `json:"title"`
		Kind     string   `json:"kind"`
		Score    float64  `json:"score"`
		Activity *float64 `json:"activity"`
		Position *struct {
			X float64 `json:"x"`
		} `json:"position"`
	} `json:"nodes"`
	Edges []struct {
		From       string   `json:"from"`
		To         string   `json:"to"`
		Kind       string   `json:"kind"`
		Tag        *string  `json:"tag"`
		Similarity *float64 `json:"similarity"`
		Weight     float64  `json:"weight"`
	} `json:"edges"`
}

func basis(axis int) []float32 {
	v := make([]float32, 1536)
	v[axis] = 1
	return v
}

func midpoint(a, b int) []float32 {
	v := make([]float32, 1536)
	const component = float32(0.70710678)
	v[a], v[b] = component, component
	return v
}

func (v env) seed(tenant uuid.UUID, content string, vec []float32, tags ...string) string {
	v.t.Helper()
	m, err := v.repo.Create(context.Background(), memorystore.CreateParams{Tenant: tenant, Content: content, Embedding: vec, Tags: tags})
	require.NoError(v.t, err)
	return strings.ToUpper(m.ID.String())
}

func TestGraphReturnsNodesAndEdgesForAuthedTenant(t *testing.T) {
	v := setup(t)
	v.seed(newbie, "Note 1", basis(0), "theme-a")
	v.seed(newbie, "Note 2", basis(0), "theme-a")
	r := v.do(newbie, http.MethodGet, "/v1/memory/graph", "")
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	g := decode[graphOut](t, r)
	assert.Len(t, g.Nodes, 2)
	assert.Len(t, g.Edges, 1, "tag wins the dedupe over semantic")
}

func TestGraphReturns401WithoutAuthHeader(t *testing.T) {
	assert.Equal(t, http.StatusUnauthorized, setup(t).do(uuid.Nil, http.MethodGet, "/v1/memory/graph", "").code)
}

func TestGraphClampsOutOfRangeQueryParams(t *testing.T) {
	v := setup(t)
	r := v.do(newbie, http.MethodGet, "/v1/memory/graph?limit=9999&similarityThreshold=2.0&maxEdgesPerNode=0", "")
	require.Equal(t, http.StatusOK, r.code)
	assert.Empty(t, decode[graphOut](t, r).Nodes)
}

func TestEmptyGraphReturnedForTenantWithNoMemories(t *testing.T) {
	v := setup(t)
	g := decode[graphOut](t, v.do(newbie, http.MethodGet, "/v1/memory/graph?limit=100&similarityThreshold=0.5&maxEdgesPerNode=5", ""))
	assert.Empty(t, g.Nodes)
	assert.Empty(t, g.Edges)
}

func TestSharedTagProducesATagEdge(t *testing.T) {
	v := setup(t)
	a := v.seed(newbie, "A", basis(0), "swift", "ios")
	b := v.seed(newbie, "B", basis(100), "swift", "server")
	v.seed(newbie, "C", basis(200), "python")
	g := decode[graphOut](t, v.do(newbie, http.MethodGet, "/v1/memory/graph?limit=100&similarityThreshold=0.99&maxEdgesPerNode=5", ""))
	assert.Len(t, g.Nodes, 3)
	var tags int
	for _, e := range g.Edges {
		if e.Kind != "tag" {
			continue
		}
		tags++
		assert.ElementsMatch(t, []string{a, b}, []string{e.From, e.To})
		assert.Equal(t, "swift", *e.Tag)
		assert.InDelta(t, 1.0, e.Weight, 0)
	}
	assert.Equal(t, 1, tags)
}

func TestSemanticSimilarityEdgeRespectsThreshold(t *testing.T) {
	v := setup(t)
	a := v.seed(newbie, "A", basis(0))
	b := v.seed(newbie, "B", basis(0))
	v.seed(newbie, "C", midpoint(0, 1))
	g := decode[graphOut](t, v.do(newbie, http.MethodGet, "/v1/memory/graph?limit=100&similarityThreshold=0.8&maxEdgesPerNode=5&kinds=semantic", ""))
	require.Len(t, g.Edges, 1)
	assert.ElementsMatch(t, []string{a, b}, []string{g.Edges[0].From, g.Edges[0].To})
	assert.Greater(t, *g.Edges[0].Similarity, 0.99)
}

func TestGraphTenantIsolationExcludesOtherTenantsMemories(t *testing.T) {
	v := setup(t)
	v.seed(newbie, "A1", basis(0), "shared")
	v.seed(newbie, "A2", basis(0), "shared")
	v.seed(bob, "B1", basis(0), "shared")
	g := decode[graphOut](t, v.do(newbie, http.MethodGet, "/v1/memory/graph?limit=100&similarityThreshold=0.5&maxEdgesPerNode=5", ""))
	assert.Len(t, g.Nodes, 2)
	ids := map[string]bool{}
	for _, n := range g.Nodes {
		ids[n.ID] = true
	}
	for _, e := range g.Edges {
		assert.True(t, ids[e.From])
		assert.True(t, ids[e.To])
	}
}

func TestGraphSeededShape(t *testing.T) {
	v := setup(t)
	g := decode[graphOut](t, v.do(gra, http.MethodGet, "/v1/memory/graph?similarityThreshold=0.5", ""))
	kinds := map[string]int{}
	for _, n := range g.Nodes {
		kinds[n.Kind]++
	}
	assert.Equal(t, map[string]int{"memory": 6, "wikiPage": 4, "space": 1}, kinds)
	var edgeKinds []string
	for _, e := range g.Edges {
		edgeKinds = append(edgeKinds, e.Kind)
	}
	assert.Subset(t, edgeKinds, []string{"wikilink", "tag", "space", "temporal"})
	semantic := decode[graphOut](t, v.do(gra, http.MethodGet, "/v1/memory/graph?similarityThreshold=0.5&kinds=semantic", ""))
	require.NotEmpty(t, semantic.Edges)
	assert.Equal(t, "semantic", semantic.Edges[0].Kind)
	assert.Equal(t, http.StatusBadRequest, v.do(gra, http.MethodGet, "/v1/memory/graph?sources=nope", "").code)
	assert.Equal(t, http.StatusBadRequest, v.do(gra, http.MethodGet, "/v1/memory/graph?createdAfter=yesterday", "").code)
	bare := decode[graphOut](t, v.do(gra, http.MethodGet, "/v1/memory/graph?includeWikiPages=false&kinds=tag", ""))
	for _, n := range bare.Nodes {
		assert.Equal(t, "memory", n.Kind)
	}
}

func TestGraphSortsSeveralSpaceHubs(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	first, second := uuid.MustParse("3E355100-0000-4000-8000-000000000002"), uuid.MustParse("3E355100-0000-4000-8000-000000000001")
	for _, s := range []uuid.UUID{first, second} {
		_, err := v.pool.Exec(ctx, `INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at) VALUES ($1, $2, $3, $3, 0, now(), now())`, s, newbie, "s"+s.String()[35:])
		require.NoError(t, err)
		m, err := v.repo.Create(ctx, memorystore.CreateParams{Tenant: newbie, Content: "in " + s.String(), SpaceID: &s})
		require.NoError(t, err)
		require.NotNil(t, m)
	}
	g := decode[graphOut](t, v.do(newbie, http.MethodGet, "/v1/memory/graph", ""))
	var hubs []string
	for _, n := range g.Nodes {
		if n.Kind == "space" {
			hubs = append(hubs, n.ID)
		}
	}
	assert.Equal(t, []string{strings.ToUpper(second.String()), strings.ToUpper(first.String())}, hubs)
}

// MemoryRelatedTests. "a note upload returns its memory id" is on the vault route.

type relatedOut struct {
	MemoryID string `json:"memoryId"`
	Items    []struct {
		ID         string  `json:"id"`
		Snippet    string  `json:"snippet"`
		Kind       *string `json:"kind"`
		Similarity float64 `json:"similarity"`
	} `json:"items"`
}

// unit is a unit vector at cosine similarity to axis 0, leaning on axis.
func unit(similarity float32, axis int) []float32 {
	v := make([]float32, 1536)
	v[0] = similarity
	rest := float64(1 - similarity*similarity)
	root := float32(0)
	if rest > 0 {
		x := rest
		for range 40 {
			x = (x + rest/x) / 2
		}
		root = float32(x)
	}
	v[axis] = root
	return v
}

func TestRelatedReturnsCloseMemoriesOnly(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	stranger := bob
	create := func(tenant uuid.UUID, content string, vec []float32, review string, kind *string) string {
		m, err := v.repo.Create(ctx, memorystore.CreateParams{Tenant: tenant, Content: content, Embedding: vec, ReviewState: review, Kind: kind})
		require.NoError(t, err)
		return strings.ToUpper(m.ID.String())
	}
	decision := "decision"
	base := create(newbie, "Ship on Tuesdays.", unit(1, 1), memorystore.ReviewAuto, nil)
	long := "We release every Tuesday after QA signs off on Monday. " + strings.Repeat("More detail. ", 30)
	near := create(newbie, long, unit(0.95, 1), memorystore.ReviewAuto, &decision)
	closer := create(newbie, "Tuesday is ship day.", unit(0.99, 2), memorystore.ReviewAuto, nil)
	create(newbie, "Loosely related.", unit(0.5, 1), memorystore.ReviewAuto, nil)
	create(newbie, "Unrelated.", unit(0, 3), memorystore.ReviewAuto, nil)
	create(newbie, "Rejected twin.", unit(0.98, 1), memorystore.ReviewRejected, nil)
	create(stranger, "Ship on Tuesdays.", unit(1, 1), memorystore.ReviewAuto, nil)

	r := v.do(newbie, http.MethodGet, "/v1/memory/"+base+"/related", "")
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	body := decode[relatedOut](t, r)
	assert.Equal(t, base, body.MemoryID)
	require.Len(t, body.Items, 2)
	assert.Equal(t, closer, body.Items[0].ID)
	assert.Equal(t, near, body.Items[1].ID)
	assert.InDelta(t, 0.99, body.Items[0].Similarity, 0.001)
	assert.Equal(t, "decision", *body.Items[1].Kind)
	assert.Len(t, []rune(body.Items[1].Snippet), 160)
	assert.True(t, strings.HasSuffix(body.Items[1].Snippet, "…"))
	assert.NotContains(t, body.Items[1].Snippet, "\n")

	limited := decode[relatedOut](t, v.do(newbie, http.MethodGet, "/v1/memory/"+base+"/related?limit=1", ""))
	require.Len(t, limited.Items, 1)
	assert.Equal(t, closer, limited.Items[0].ID)

	// Not recall: nothing shown here gets a query hit.
	for _, id := range []string{near, closer} {
		var hits int64
		require.NoError(t, v.pool.QueryRow(ctx, `SELECT query_hit_count FROM memories WHERE id = $1`, id).Scan(&hits))
		assert.Zero(t, hits)
	}
	assert.Equal(t, http.StatusNotFound, v.do(stranger, http.MethodGet, "/v1/memory/"+base+"/related", "").code)
	assert.Equal(t, http.StatusNotFound, v.do(newbie, http.MethodGet, "/v1/memory/"+uuid.NewString()+"/related", "").code)
}

func TestRelatedWithNoStoredVectorIsEmpty(t *testing.T) {
	v := setup(t)
	id := v.seed(newbie, "no vector", nil)
	r := v.do(newbie, http.MethodGet, "/v1/memory/"+id+"/related", "")
	require.Equal(t, http.StatusOK, r.code)
	assert.Empty(t, decode[relatedOut](t, r).Items)
}

// MemorySpaceMoveTests.

func (v env) space(tenant uuid.UUID, slug string) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at) VALUES ($1, $2, $3, $3, 0, now(), now())`, id, tenant, slug)
	require.NoError(v.t, err)
	return id
}

func (v env) listIDs(user uuid.UUID, space string) []string {
	v.t.Helper()
	r := v.do(user, http.MethodGet, "/v1/memory?space="+space, "")
	require.Equal(v.t, http.StatusOK, r.code)
	out := []string{}
	for _, m := range decode[listOut](v.t, r).Memories {
		out = append(out, m.ID)
	}
	return out
}

type provenanceOut struct {
	Contributions []struct {
		Operation string `json:"operation"`
		Actor     string `json:"actor"`
		Model     *struct {
			Provider string `json:"provider"`
		} `json:"model"`
	} `json:"contributions"`
}

func TestAMemoryMovesIntoASpaceShowsUpThereAndCanBeUnfiled(t *testing.T) {
	v := setup(t)
	space := v.space(newbie, "work")
	id := v.content(newbie, "Standup is at nine.")
	r := v.do(newbie, http.MethodPatch, "/v1/memory/"+id, fmt.Sprintf(`{"spaceId":"%s"}`, space))
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	moved := decode[memoryOut](t, r)
	assert.True(t, strings.EqualFold(space.String(), *moved.SpaceID))
	assert.Equal(t, "Standup is at nine.", moved.Content)
	assert.Contains(t, v.listIDs(newbie, "work"), id)
	assert.NotContains(t, v.listIDs(newbie, "inbox"), id)
	unfiled := decode[memoryOut](t, v.do(newbie, http.MethodPatch, "/v1/memory/"+id, `{"spaceId":null}`))
	assert.Nil(t, unfiled.SpaceID)
	assert.Contains(t, v.listIDs(newbie, "inbox"), id)
	p := decode[provenanceOut](t, v.do(newbie, http.MethodGet, "/v1/memory/"+id+"/provenance", ""))
	var updates int
	for _, c := range p.Contributions {
		if c.Operation == "update" && c.Actor == "user" {
			updates++
		}
	}
	assert.Equal(t, 2, updates)
}

func TestMovingANoteBackedMemoryMovesItsNote(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	space := v.space(newbie, "work")
	file := uuid.New()
	_, err := v.pool.Exec(ctx, `INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at) VALUES ($1, $2, 'inbox/plan.md', 'text/markdown', 0, $3, now(), now())`, file, newbie, strings.Repeat("0", 64))
	require.NoError(t, err)
	// With bytes on disk too, unlike the parity fixture.
	root := v.paths.Raw(newbie)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "inbox"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "inbox", "plan.md"), []byte("Plan the launch."), 0o600))
	m, err := v.repo.Create(ctx, memorystore.CreateParams{Tenant: newbie, Content: "Plan the launch.", Embedding: basis(0), SourceVaultFileID: &file})
	require.NoError(t, err)
	r := v.do(newbie, http.MethodPatch, "/v1/memory/"+m.ID.String(), fmt.Sprintf(`{"spaceId":"%s"}`, space))
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	assert.True(t, strings.EqualFold(space.String(), *decode[memoryOut](t, r).SpaceID))
	note, err := vaultfiles.NewStore(v.pool, v.paths).FindByID(ctx, newbie, file)
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.Equal(t, "work/plan.md", note.Path)
	assert.Equal(t, space, *note.SpaceID)
	moved, err := os.ReadFile(filepath.Join(root, "work", "plan.md"))
	require.NoError(t, err)
	assert.Equal(t, "Plan the launch.", string(moved))
}

func TestAnotherAccountsSpaceAndAnUnknownSlugAreRefused(t *testing.T) {
	v := setup(t)
	foreign := v.space(bob, "theirs")
	id := v.content(newbie, "Mine.")
	assert.Equal(t, http.StatusBadRequest, v.do(newbie, http.MethodPatch, "/v1/memory/"+id, fmt.Sprintf(`{"spaceId":"%s"}`, foreign)).code)
	assert.Equal(t, http.StatusNotFound, v.do(newbie, http.MethodGet, "/v1/memory?space=nope", "").code)
}

// MemoryCorrectionTests. "moving a note into a Space folder" drives /v1/vault/files/move,
// the vault group's route.

const (
	original  = "# Routing\nWhen the primary provider errors we retry once. The codeword is zarquonfallback."
	corrected = "# Routing\nWe no longer retry; one provider only. The codeword is zarquonsingle."
)

func (v env) seedNote(tenant uuid.UUID, path string) uuid.UUID {
	v.t.Helper()
	ctx := context.Background()
	file := uuid.New()
	_, err := v.pool.Exec(ctx, `INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at) VALUES ($1, $2, $3, 'text/markdown', 0, $4, now(), now())`, file, tenant, path, strings.Repeat("0", 64))
	require.NoError(v.t, err)
	m, err := v.repo.Create(ctx, memorystore.CreateParams{Tenant: tenant, Content: original, Embedding: basis(0), SourceVaultFileID: &file})
	require.NoError(v.t, err)
	written, err := chunkindex.NewIndexer(v.chunks, v.embeddings, chunkindex.NewLinkRepository(v.pool)).Index(ctx, chunkindex.IndexParams{
		Tenant: tenant, MemoryID: m.ID, VaultFileID: &file, SourcePath: &path, Content: original,
	})
	require.NoError(v.t, err)
	require.Positive(v.t, written, "seeding must produce chunks")
	return m.ID
}

func (v env) chunkHits(tenant uuid.UUID, query string) []memorystore.SearchResult {
	v.t.Helper()
	hits, err := v.chunks.HybridSearch(context.Background(), tenant, query, basis(0), 10, nil)
	require.NoError(v.t, err)
	return hits
}

func carries(hits []memorystore.SearchResult, id uuid.UUID, token string) bool {
	for i := range hits {
		if hits[i].ID != id {
			continue
		}
		if strings.Contains(hits[i].Content, token) || (hits[i].Snippet != nil && strings.Contains(*hits[i].Snippet, token)) {
			return true
		}
	}
	return false
}

func TestACorrectedMemoryStopsMatchingItsOldTextAndMatchesTheNewText(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	id := v.seedNote(newbie, "inbox/routing.md")
	before, err := v.repo.Find(ctx, newbie, id)
	require.NoError(t, err)
	assert.True(t, carries(v.chunkHits(newbie, "zarquonfallback"), id, "zarquonfallback"), "the seeded text must be findable first")

	raw, err := json.Marshal(map[string]string{"content": corrected})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, v.do(newbie, http.MethodPatch, "/v1/memory/"+id.String(), string(raw)).code)

	assert.False(t, carries(v.chunkHits(newbie, "zarquonfallback"), id, "zarquonfallback"), "old wording must not survive a correction")
	fresh := v.chunkHits(newbie, "zarquonsingle")
	assert.True(t, carries(fresh, id, "zarquonsingle"), "corrected text must be chunked straight away")
	var hit *memorystore.SearchResult
	for i := range fresh {
		if fresh[i].ID == id {
			hit = &fresh[i]
			break
		}
	}
	require.NotNil(t, hit)
	require.NotNil(t, hit.Citation, "the corrected memory must come back from the chunk arm")
	assert.Nil(t, hit.Citation.Path, "the file still holds the old text, so it is not cited")

	after, err := v.repo.Find(ctx, newbie, id)
	require.NoError(t, err)
	assert.Equal(t, corrected, after.Content)
	require.NotNil(t, after.UpdatedAt, "an edit must stamp updated_at")
	if before.UpdatedAt != nil {
		assert.True(t, after.UpdatedAt.After(*before.UpdatedAt))
	}
	assert.Equal(t, newbie, *after.UpdatedByUserID)
}

func TestAContentUpdateFromAnotherPathAlsoDropsStaleChunks(t *testing.T) {
	v := setup(t)
	id := v.seedNote(newbie, "inbox/repo.md")
	updated, err := v.repo.UpdateContent(context.Background(), newbie, id, corrected, basis(0), nil)
	require.NoError(t, err)
	assert.True(t, updated)
	assert.False(t, carries(v.chunkHits(newbie, "zarquonfallback"), id, "zarquonfallback"))
}

func TestReadingAMemoryDoesNotModifyIt(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	id := v.seedNote(newbie, "inbox/read.md")
	before, err := v.repo.Find(ctx, newbie, id)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, v.do(newbie, http.MethodGet, "/v1/memory/"+id.String(), "").code)
	after, err := v.repo.Find(ctx, newbie, id)
	require.NoError(t, err)
	assert.Equal(t, before.UpdatedAt, after.UpdatedAt)
	assert.Equal(t, before.UpdatedByUserID, after.UpdatedByUserID)
}

// MemoryKindTests.

func TestASavedKindComesBackAndFiltersTheList(t *testing.T) {
	v := setup(t)
	preference := v.upsert(newbie, `{"content":"Prefers tea.","kind":"preference"}`)
	plain := v.content(newbie, "Bought milk.")
	all := map[string]memoryOut{}
	for _, m := range decode[listOut](t, v.do(newbie, http.MethodGet, "/v1/memory?limit=50", "")).Memories {
		all[m.ID] = m
	}
	assert.Equal(t, "preference", *all[preference].Kind)
	assert.Nil(t, all[plain].Kind)
	only := decode[listOut](t, v.do(newbie, http.MethodGet, "/v1/memory?kind=preference", "")).Memories
	require.Len(t, only, 1)
	assert.Equal(t, preference, only[0].ID)
	assert.Equal(t, http.StatusBadRequest, v.do(newbie, http.MethodGet, "/v1/memory?kind=hunch", "").code)
}

func TestTheKindCanBeSetOnItsOwnAndTheChangeIsAttributedToTheUser(t *testing.T) {
	v := setup(t)
	id := v.content(newbie, "We ship on Tuesdays.")
	r := v.do(newbie, http.MethodPatch, "/v1/memory/"+id, `{"kind":"decision"}`)
	require.Equal(t, http.StatusOK, r.code)
	patched := decode[memoryOut](t, r)
	assert.Equal(t, "decision", *patched.Kind)
	assert.Equal(t, "We ship on Tuesdays.", patched.Content)
	p := decode[provenanceOut](t, v.do(newbie, http.MethodGet, "/v1/memory/"+id+"/provenance", ""))
	require.Len(t, p.Contributions, 2)
	assert.Equal(t, "create", p.Contributions[0].Operation)
	assert.Equal(t, "update", p.Contributions[1].Operation)
	assert.Equal(t, "user", p.Contributions[1].Actor)
}

// Disclosure, review, delete, local sync: behaviour with no Swift test of its own.

func TestModelsAreHiddenFromManagedReaders(t *testing.T) {
	v := setup(t)
	const id = "3E3A0000-0000-4000-8000-000000000001"
	p := decode[provenanceOut](t, v.do(ann, http.MethodGet, "/v1/memory/"+id+"/provenance", ""))
	var kept []string
	for _, c := range p.Contributions {
		if c.Model != nil {
			kept = append(kept, c.Model.Provider)
		}
	}
	assert.Equal(t, []string{"mcp:codex"}, kept, "only the user's own agent connection is named")

	const hers = "3E3A0000-0000-4000-8000-0000000000C1"
	dto := decode[memoryOut](t, v.do(cat, http.MethodGet, "/v1/memory/"+hers, ""))
	require.NotNil(t, dto.Provenance.CreatedBy.Model)
	assert.Equal(t, "openrouter", dto.Provenance.CreatedBy.Model.Provider)
	assert.Len(t, dto.Provenance.Contributors, 1)

	facets := v.do(ann, http.MethodGet, "/v1/memory/facets", "")
	require.Equal(t, http.StatusOK, facets.code)
	assert.Contains(t, string(facets.body), `"providers":[]`)
	assert.Contains(t, string(v.do(cat, http.MethodGet, "/v1/memory/facets", "").body), "openrouter")

	team := decode[memoryOut](t, v.do(cat, http.MethodGet, "/v1/memory/3E3A0000-0000-4000-8000-0000000000D1", "", "X-Vault-ID", teamVault))
	require.NotNil(t, team.Provenance.CreatedBy.Model, "the reader's preference counts, not the vault's")
}

func TestViewerCannotWriteAndBadVaultHeaderComesFirst(t *testing.T) {
	v := setup(t)
	const id = "/v1/memory/3E3A0000-0000-4000-8000-0000000000D1"
	assert.Equal(t, http.StatusOK, v.do(vie, http.MethodGet, id, "", "X-Vault-ID", teamVault).code)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPatch, id, `{"kind":"note"}`},
		{http.MethodDelete, id, ""},
		{http.MethodPost, id + "/review", ""},
		{http.MethodPost, "/v1/memory/upsert", `{"content":"x"}`},
	} {
		assert.Equal(t, http.StatusForbidden, v.do(vie, c.method, c.path, c.body, "X-Vault-ID", teamVault).code, c.method+c.path)
	}
	assert.Equal(t, http.StatusBadRequest, v.do(uuid.Nil, http.MethodGet, "/v1/memory", "", "X-Vault-ID", "nope").code)
}

func TestReviewRoute(t *testing.T) {
	v := setup(t)
	id := "3E3A1000-0000-4000-8000-00000000000E"
	path := "/v1/memory/" + id + "/review"
	type out struct {
		MemoryID       string  `json:"memoryId"`
		ReviewCount    int     `json:"reviewCount"`
		NextReviewAt   *string `json:"nextReviewAt"`
		ResurfaceMuted *bool   `json:"resurfaceMuted"`
	}
	kept := decode[out](t, v.do(wri, http.MethodPost, path, ""))
	assert.Equal(t, 1, kept.ReviewCount)
	assert.Nil(t, kept.NextReviewAt)
	assert.False(t, *kept.ResurfaceMuted)
	soon := decode[out](t, v.do(wri, http.MethodPost, path, `{"action":"soon"}`))
	assert.Equal(t, 2, soon.ReviewCount)
	assert.NotNil(t, soon.NextReviewAt)
	never := decode[out](t, v.do(wri, http.MethodPost, path, `{"action":"never"}`))
	assert.Nil(t, never.NextReviewAt)
	assert.True(t, *never.ResurfaceMuted)
	assert.Equal(t, http.StatusBadRequest, v.do(wri, http.MethodPost, path, `{"action":"nope"}`).code)
	assert.Equal(t, http.StatusBadRequest, v.do(wri, http.MethodPost, path, `[]`).code)
	assert.Equal(t, http.StatusRequestEntityTooLarge, v.do(wri, http.MethodPost, path, strings.Repeat(" ", 4097)).code)
	assert.Equal(t, http.StatusNotFound, v.do(wri, http.MethodPost, "/v1/memory/"+uuid.NewString()+"/review", "").code)
	var events int
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM analytics_events WHERE vault_id = $1 AND event_name = 'memory_reviewed'`, wri).Scan(&events))
	assert.Equal(t, 3, events)
}

func TestReviewStateTransitions(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	patch := func(id, target string) int {
		return v.do(wri, http.MethodPatch, "/v1/memory/3E3A1000-0000-4000-8000-00000000000"+id, `{"reviewState":"`+target+`"}`).code
	}
	assert.Equal(t, http.StatusOK, patch("3", "approved"))
	assert.Equal(t, http.StatusUnprocessableEntity, patch("3", "approved"))
	assert.Equal(t, http.StatusOK, patch("4", "rejected"))
	assert.Equal(t, http.StatusUnprocessableEntity, patch("5", "approved"))
	assert.Equal(t, http.StatusUnprocessableEntity, patch("6", "rejected"))
	var hash string
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT content_hash FROM kb_compile_reject_list WHERE tenant_id = $1`, wri).Scan(&hash))
	assert.Equal(t, memorystore.ContentHash("Pending to reject"), hash)
}

func TestDeleteSuppressesTheJobAndLeavesATombstone(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	const id = "3E3A1000-0000-4000-8000-00000000000C"
	require.Equal(t, http.StatusNoContent, v.do(wri, http.MethodDelete, "/v1/memory/"+id, "").code)
	var status string
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT status FROM memory_index_jobs WHERE tenant_id = $1 AND source_id = 'wri-msg'`, wri).Scan(&status))
	assert.Equal(t, "suppressed", status)
	var tombstones int
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT count(*) FROM memory_sync_tombstones WHERE tenant_id = $1 AND memory_id = $2`, wri, id).Scan(&tombstones))
	assert.Equal(t, 1, tombstones)
}

func TestLocalSyncPagesThroughMemoriesAndTombstones(t *testing.T) {
	v := setup(t)
	type page struct {
		Memories []struct {
			ID     string `json:"id"`
			Source string `json:"source"`
		} `json:"memories"`
		DeletedIDs []string `json:"deletedIDs"`
		NextCursor *string  `json:"nextCursor"`
	}
	full := decode[page](t, v.do(cur, http.MethodGet, "/v1/memory/local-sync", ""))
	assert.Len(t, full.Memories, 5, "a row with no updated_at is dropped")
	assert.Len(t, full.DeletedIDs, 2, "a tombstone with no deleted_at is dropped, and another tenant's is not seen")
	assert.Equal(t, "legacy", full.Memories[2].Source)

	seen := map[string]bool{}
	path := "/v1/memory/local-sync?limit=2"
	for range 20 {
		p := decode[page](t, v.do(cur, http.MethodGet, path, ""))
		for _, m := range p.Memories {
			seen[m.ID] = true
		}
		for _, id := range p.DeletedIDs {
			seen[id] = true
		}
		if p.NextCursor == nil {
			break
		}
		path = "/v1/memory/local-sync?limit=2&cursor=" + *p.NextCursor
	}
	// A cursor can land a microsecond before its row, as in Swift, so a row may repeat
	// across pages; none is skipped.
	assert.Len(t, seen, 7)
	r := v.do(cur, http.MethodGet, "/v1/memory/local-sync?cursor=garbage", "")
	assert.Equal(t, http.StatusBadRequest, r.code)
	assert.Contains(t, string(r.body), "invalid local memory sync cursor")
}

func TestLocalSyncOffFallsToTheIDRoute(t *testing.T) {
	v := setupOpts(t, func(o *memory.Options) { o.HybridExecutionEnabled = false })
	r := v.do(cur, http.MethodGet, "/v1/memory/local-sync", "")
	assert.Equal(t, http.StatusBadRequest, r.code)
	assert.Contains(t, string(r.body), "invalid memory id")
}

func TestSuggestionsNeedNoTokenAndShareTheMeBucket(t *testing.T) {
	v := setup(t)
	assert.Equal(t, http.StatusOK, v.do(uuid.Nil, http.MethodGet, "/v1/me/suggestions", "", "X-Forwarded-For", "10.77.1.1").code)
	for i := range 60 {
		require.Equal(t, http.StatusOK, v.do(sug, http.MethodGet, "/v1/me/suggestions", "").code, i)
	}
	assert.Equal(t, http.StatusTooManyRequests, v.do(sug, http.MethodGet, "/v1/me/suggestions", "").code)
}

func TestCaptureGate(t *testing.T) {
	const archived = "3E300000-0000-4000-8000-0000000000AE"
	var pool *pgxpool.Pool
	gate := func(c entitlement.Capability, own func(*echo.Context) bool) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(ctx *echo.Context) error {
				return entitlement.MiddlewareWithHermes(pool, c, true, nil, own)(next)(ctx)
			}
		}
	}
	yes := func(*echo.Context) bool { return true }

	v := setupOpts(t, func(o *memory.Options) { o.CaptureGate = gate(entitlement.Capture, yes) })
	pool = v.pool
	r := v.do(uuid.MustParse(archived), http.MethodPost, "/v1/memory/upsert", `{"content":"x"}`)
	assert.Equal(t, http.StatusPaymentRequired, r.code, "an archived account has no exemption, own Hermes or not")
	assert.JSONEq(t, `{"paywall":true,"paywallId":"default","requiredTier":"pro"}`, string(r.body))
	assert.Equal(t, http.StatusOK, v.do(newbie, http.MethodPost, "/v1/memory/upsert", `{"content":"x"}`).code)
	assert.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodPost, "/v1/memory/upsert", `{"content":"x"}`, "X-Forwarded-For", "10.77.1.2").code)

	// The own-Hermes exemption, on a capability a lapsed account is not entitled to.
	_, err := v.pool.Exec(context.Background(), `UPDATE users SET tier = 'lapsed' WHERE id = $1`, newbie)
	require.NoError(t, err)
	v2 := setupOpts(t, func(o *memory.Options) { o.CaptureGate = gate(entitlement.HealthIngest, yes) })
	pool = v2.pool
	_, err = v2.pool.Exec(context.Background(), `UPDATE users SET tier = 'lapsed' WHERE id = $1`, newbie)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, v2.do(newbie, http.MethodPost, "/v1/memory/upsert", `{"content":"x"}`).code)
	v3 := setupOpts(t, func(o *memory.Options) { o.CaptureGate = gate(entitlement.HealthIngest, nil) })
	pool = v3.pool
	_, err = v3.pool.Exec(context.Background(), `UPDATE users SET tier = 'lapsed' WHERE id = $1`, newbie)
	require.NoError(t, err)
	assert.Equal(t, http.StatusPaymentRequired, v3.do(newbie, http.MethodPost, "/v1/memory/upsert", `{"content":"x"}`).code)
}

func TestUpsertIsLimitedEvenWhenRefused(t *testing.T) {
	v := setup(t)
	for i := range 60 {
		require.Equal(t, http.StatusBadRequest, v.do(lim, http.MethodPost, "/v1/memory/upsert", "{").code, i)
	}
	assert.Equal(t, http.StatusTooManyRequests, v.do(lim, http.MethodPost, "/v1/memory/upsert", "{").code)
	assert.Equal(t, http.StatusOK, v.do(lim, http.MethodGet, "/v1/memory", "").code)
}

// The spec documents these responses. It is wrong on upsert's (it names MemoryDTO) and on
// MemoryLineageResponse (another shape), and thin on MemoryDTO: see the PR body.
func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	const id = "3E3A0000-0000-4000-8000-000000000001"
	for _, c := range []struct {
		user               uuid.UUID
		method, path, body string
	}{
		{ann, http.MethodGet, "/v1/me/suggestions", ""},
		{ann, http.MethodGet, "/v1/memory/facets", ""},
		{cat, http.MethodGet, "/v1/memory/facets", ""},
		{gra, http.MethodGet, "/v1/memory/graph", ""},
		{cur, http.MethodGet, "/v1/memory/local-sync", ""},
		{ann, http.MethodGet, "/v1/memory/" + id, ""},
		{ann, http.MethodGet, "/v1/memory", ""},
		{ann, http.MethodGet, "/v1/memory/" + id + "/provenance", ""},
		{ann, http.MethodGet, "/v1/memory/" + id + "/related", ""},
		{wri, http.MethodPost, "/v1/memory/3E3A1000-0000-4000-8000-00000000000E/review", `{"action":"soon"}`},
		{wri, http.MethodPatch, "/v1/memory/3E3A1000-0000-4000-8000-000000000007", `{"kind":"fact"}`},
	} {
		r := v.do(c.user, c.method, c.path, c.body)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: r.code,
			Header: http.Header{"Content-Type": {r.ct}},
		}
		out.SetBodyBytes(r.body)
		assert.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, r.body)
	}
}
