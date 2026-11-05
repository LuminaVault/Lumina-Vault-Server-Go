package memoryagent

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/embedding"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testKeys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

// stubStream yields scripted tokens, or fails.
type stubStream struct {
	tokens []string
	fail   error
}

func (s stubStream) ChatStream(context.Context, string, string, llm.ChatRequest) llm.Stream {
	return func(yield func(llm.Chunk, error) bool) {
		if s.fail != nil {
			yield(llm.Chunk{}, s.fail)
			return
		}
		for _, token := range s.tokens {
			if !yield(llm.Chunk{Delta: token}, nil) {
				return
			}
		}
	}
}

type moduleEnv struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
	hits *memorystore.HitCountQueue
}

func newModuleEnv(t *testing.T, transport *scriptedTransport, stream llm.StreamService) moduleEnv {
	t.Helper()
	pool := migratedPool(t)
	users := auth.PGUsers(pool)
	hits := memorystore.NewHitCountQueue(pool)
	repo := memorystore.New(pool, hits)
	paths := vaultpaths.Paths{Root: t.TempDir()}
	module := New(pool, Options{
		Identify: auth.Identify(testKeys, users), Limits: ratelimit.NewStore(), MaxStreams: 3,
		Service: NewService(ServiceOptions{Transport: transport, Memories: repo, Embeddings: embedding.Deterministic{}, DefaultModel: "test"}),
		Memo: NewMemoService(MemoOptions{
			Transport: transport, Memories: repo, Embeddings: embedding.Deterministic{},
			Paths: paths, Store: vaultfiles.NewStore(pool, paths), DefaultModel: "test",
		}),
		FollowUps: &FollowUpGenerator{Transport: &scriptedTransport{fail: errors.New("no follow-ups")}, DefaultModel: "test"},
		Stream:    stream, Memories: repo, Embeddings: embedding.Deterministic{}, DefaultModel: "test",
	})
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(testKeys, users), Modules: []httpserver.Module{module}})
	return moduleEnv{t: t, e: e, pool: pool, hits: hits}
}

func (v moduleEnv) do(user uuid.UUID, method, path, body string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		token, err := scenario.Mint(testKeys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec
}

func (v moduleEnv) memory(tenant uuid.UUID, content string) uuid.UUID {
	v.t.Helper()
	ctx := context.Background()
	vec, err := embedding.Deterministic{}.Embed(ctx, content, tenant)
	require.NoError(v.t, err)
	m, err := memorystore.New(v.pool, nil).CreateEmbedded(ctx, tenant, content, vec)
	require.NoError(v.t, err)
	return m.ID
}

// A search that finds a row bumps its hit count through the one queue cmd/App runs.
func TestSearchRouteReturnsHitsAndBumpsQueryHitCount(t *testing.T) {
	transport := &scriptedTransport{steps: []step{
		toolStep("session_search", `{"query":"tea","limit":3}`),
		plainStep("You like tea."),
	}}
	v := newModuleEnv(t, transport, stubStream{})
	user := createTenant(t, v.pool)
	id := v.memory(user, "favourite drink: jasmine tea")
	rec := v.do(user, http.MethodPost, "/v1/memory/search", `{"query":"tea"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Hits []struct {
			Content string `json:"content"`
		} `json:"hits"`
		Summary string `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "You like tea.", body.Summary)
	require.Len(t, body.Hits, 1)
	assert.Equal(t, "favourite drink: jasmine tea", body.Hits[0].Content)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v.hits.Drain(ctx)
	var count int64
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT query_hit_count FROM memories WHERE id = $1`, id).Scan(&count))
	assert.Equal(t, int64(1), count)
}

func TestSearchRouteIsTenantScoped(t *testing.T) {
	transport := &scriptedTransport{steps: []step{
		toolStep("session_search", `{"query":"tea"}`),
		plainStep("Nothing."),
	}}
	v := newModuleEnv(t, transport, stubStream{})
	alice, bob := createTenant(t, v.pool), createTenant(t, v.pool)
	v.memory(alice, "alice likes tea")
	rec := v.do(bob, http.MethodPost, "/v1/memory/search", `{"query":"tea"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "alice likes tea")
}

func TestEmptyQueriesAndAnonymousCalls(t *testing.T) {
	v := newModuleEnv(t, &scriptedTransport{}, stubStream{})
	user := createTenant(t, v.pool)
	for _, path := range []string{"/v1/memory/search", "/v1/query", "/v1/query/stream"} {
		assert.Equal(t, http.StatusBadRequest, v.do(user, http.MethodPost, path, `{"query":""}`).Code, path)
		assert.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodPost, path, `{"query":"q"}`).Code, path)
	}
	assert.Equal(t, http.StatusBadRequest, v.do(user, http.MethodPost, "/v1/memos", `{"topic":""}`).Code)
	assert.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, "/v1/memos", "").Code)
}

func TestQueryStreamFramesEndWithDone(t *testing.T) {
	v := newModuleEnv(t, &scriptedTransport{}, stubStream{tokens: []string{"Hel", "lo"}})
	user := createTenant(t, v.pool)
	rec := v.do(user, http.MethodPost, "/v1/query/stream", `{"query":"hello"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	types := []string{}
	for _, record := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n") {
		var event struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(record, "data: ")), &event), record)
		types = append(types, event.Type)
	}
	require.NotEmpty(t, types)
	assert.Contains(t, types, "token")
	assert.Equal(t, "done", types[len(types)-1])
}

func TestQueryStreamUpstreamFailureIsAnErrorFrame(t *testing.T) {
	v := newModuleEnv(t, &scriptedTransport{}, stubStream{fail: errors.New("upstream died")})
	user := createTenant(t, v.pool)
	rec := v.do(user, http.MethodPost, "/v1/query/stream", `{"query":"hello"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"type":"error"`)
}

func TestMemoRoutesRoundTrip(t *testing.T) {
	transport := &scriptedTransport{steps: []step{plainStep("## Summary\nA memo.")}}
	v := newModuleEnv(t, transport, stubStream{})
	user := createTenant(t, v.pool)
	rec := v.do(user, http.MethodPost, "/v1/memos", `{"topic":"Sleep patterns"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	list := v.do(user, http.MethodGet, "/v1/memos", "")
	require.Equal(t, http.StatusOK, list.Code)
	var body struct {
		Memos []struct {
			Title string `json:"title"`
			Path  string `json:"path"`
		} `json:"memos"`
	}
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &body))
	require.Len(t, body.Memos, 1)
	assert.Equal(t, "Sleep Patterns", body.Memos[0].Title)
	assert.True(t, strings.HasSuffix(body.Memos[0].Path, "/sleep-patterns.md"), body.Memos[0].Path)

	other := createTenant(t, v.pool)
	foreign := v.do(other, http.MethodGet, "/v1/memos", "")
	assert.NotContains(t, foreign.Body.String(), "sleep-patterns")
}

// Validates success bodies against api/openapi.yaml. /v1/memory/search, /v1/query and
// /v1/memos POST are left out: the spec drifts from what Swift sends (see PR body), and
// the parity scenarios pin those bodies to Swift's.
func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	transport := &scriptedTransport{steps: []step{plainStep("## Summary\nA memo.")}}
	v := newModuleEnv(t, transport, stubStream{})
	user := createTenant(t, v.pool)
	require.Equal(t, http.StatusOK, v.do(user, http.MethodPost, "/v1/memos", `{"topic":"Spec"}`).Code)

	rec := v.do(user, http.MethodGet, "/v1/memos", "")
	req := httptest.NewRequest(http.MethodGet, "/v1/memos", nil)
	route, params, err := router.FindRoute(req)
	require.NoError(t, err)
	in := &openapi3filter.RequestValidationInput{
		Request: req, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}
	out := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: in, Status: rec.Code,
		Header: http.Header{"Content-Type": {rec.Header().Get("Content-Type")}},
	}
	out.SetBodyBytes(rec.Body.Bytes())
	require.NoError(t, openapi3filter.ValidateResponse(ctx, out), rec.Body.String())
}
