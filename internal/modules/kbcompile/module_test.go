package kbcompile_test

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/embedding"
	"lumina/internal/httpserver"
	"lumina/internal/idempotency"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/memorycompile"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/modules/kbcompile"
	"lumina/internal/onboardlatch"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultpaths"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

var keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

// stubTransport replies with one assistant message.
type stubTransport struct{ content string }

func (s stubTransport) ChatCompletions(context.Context, []byte, string, string) (llm.Metadata, error) {
	data, err := json.Marshal(map[string]any{
		"id": "test-1", "model": "test-model",
		"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": s.content}}},
	})
	return llm.Metadata{Data: data}, err
}

func (stubTransport) ChatStream(context.Context, []byte, string, string) llm.Stream {
	return func(yield func(llm.Chunk, error) bool) { yield(llm.Chunk{}, errors.New("no stream")) }
}

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
	root string
}

func setup(t *testing.T, content string) env {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	root := t.TempDir()
	users := auth.PGUsers(pool)
	service := memorycompile.New(pool, memorycompile.Options{
		Paths: vaultpaths.Paths{Root: root}, Transport: stubTransport{content: content},
		Memories: memorystore.New(pool, nil), Embeddings: embedding.Deterministic{}, DefaultModel: "test-model",
	})
	module := kbcompile.New(pool, kbcompile.Options{
		Identify: auth.Identify(keys, users), Idempotency: idempotency.New(pool, idempotency.Options{}),
		Limits: ratelimit.NewStore(), Service: service, Latches: onboardlatch.New(pool),
	})
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, users), Modules: []httpserver.Module{module}})
	return env{t: t, e: e, pool: pool, root: root}
}

func (v env) user() uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	slug := "kbc" + id.String()[:8]
	ctx := context.Background()
	_, err := v.pool.Exec(ctx, `INSERT INTO users (id, email, username, password_hash, is_verified, tier, created_at, updated_at)
		VALUES ($1, $2, $3, 'stub', true, 'trial', now(), now())`, id, slug+"@test.luminavault", slug)
	require.NoError(v.t, err)
	_, err = v.pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'test', now(), now())`, id)
	require.NoError(v.t, err)
	return id
}

func (v env) file(tenant uuid.UUID, path, content string) uuid.UUID {
	v.t.Helper()
	target := filepath.Join(v.root, "tenants", ids.Upper(tenant), "raw", path)
	require.NoError(v.t, os.MkdirAll(filepath.Dir(target), 0o750))
	require.NoError(v.t, os.WriteFile(target, []byte(content), 0o600))
	return v.row(tenant, path, len(content))
}

func (v env) row(tenant uuid.UUID, path string, size int) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at)
		VALUES ($1, $2, $3, 'text/markdown', $4, $5, now(), now())`, id, tenant, path, size, strings.Repeat("b", 64))
	require.NoError(v.t, err)
	return id
}

func (v env) processed(file uuid.UUID) bool {
	v.t.Helper()
	var done bool
	require.NoError(v.t, v.pool.QueryRow(context.Background(), `SELECT processed_at IS NOT NULL FROM vault_files WHERE id = $1`, file).Scan(&done))
	return done
}

type response struct {
	MemoriesIngested int      `json:"memoriesIngested"`
	MemoriesUpdated  *int     `json:"memoriesUpdated"`
	DurationMs       *int     `json:"durationMs"`
	RunID            string   `json:"runId"`
	PendingMemoryIDs []string `json:"pendingMemoryIds"`
}

func (v env) post(user uuid.UUID, body string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/memory-compile", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != uuid.Nil {
		token, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec
}

func (v env) compile(user uuid.UUID, body string) response {
	v.t.Helper()
	rec := v.post(user, body)
	require.Equal(v.t, http.StatusOK, rec.Code, rec.Body.String())
	var out response
	require.NoError(v.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func (v env) latched(tenant uuid.UUID) bool {
	v.t.Helper()
	var done bool
	err := v.pool.QueryRow(context.Background(), `SELECT first_memory_compile_completed FROM onboarding_state WHERE tenant_id = $1`, tenant).Scan(&done)
	if err != nil {
		return false
	}
	return done
}

func TestUnauthenticatedIs401(t *testing.T) {
	v := setup(t, "")
	assert.Equal(t, http.StatusUnauthorized, v.post(uuid.Nil, `{"forceFullRecompile":false}`).Code)
}

func TestNoPendingFilesReturnsZero(t *testing.T) {
	v := setup(t, "")
	body := v.compile(v.user(), `{"forceFullRecompile":false}`)
	assert.Zero(t, body.MemoriesIngested)
	require.NotNil(t, body.MemoriesUpdated)
	assert.Zero(t, *body.MemoriesUpdated)
	require.NotNil(t, body.DurationMs)
	assert.Zero(t, *body.DurationMs)
	assert.Len(t, body.RunID, 36)
}

func TestEachCompileResponseCarriesAUniqueRunID(t *testing.T) {
	v := setup(t, "")
	user := v.user()
	assert.NotEqual(t, v.compile(user, `{"forceFullRecompile":false}`).RunID, v.compile(user, `{"forceFullRecompile":false}`).RunID)
}

func TestForceFullRecompileWithNoRowsReturnsZero(t *testing.T) {
	v := setup(t, "")
	assert.Zero(t, v.compile(v.user(), `{"forceFullRecompile":true}`).MemoriesIngested)
}

func TestVaultFileIDsFromAnotherTenantAreFilteredOut(t *testing.T) {
	v := setup(t, "")
	a, b := v.user(), v.user()
	seeded := v.row(a, "notes/leak.md", 12)
	body := v.compile(b, `{"forceFullRecompile":false,"vaultFileIds":["`+ids.Upper(seeded)+`"]}`)
	assert.Zero(t, body.MemoriesIngested)
	assert.False(t, v.processed(seeded), "tenant A's row is not touched")
}

func TestLatchStaysFalseAfterANoOpCompile(t *testing.T) {
	v := setup(t, "")
	user := v.user()
	v.compile(user, `{"forceFullRecompile":false}`)
	assert.False(t, v.latched(user))
}

func TestMissingRequiredFlagIs400(t *testing.T) {
	v := setup(t, "")
	assert.Equal(t, http.StatusBadRequest, v.post(v.user(), `{}`).Code)
}

func TestHappyPathFlipsProcessedAndLatchAndIsIdempotentOnSecondTap(t *testing.T) {
	v := setup(t, "Done — no memories worth storing in this stub.")
	user := v.user()
	row := v.file(user, "notes/hello.md", "# Hello\n\nThis is a kb-compile test note.")
	first := v.compile(user, `{"forceFullRecompile":false}`)
	assert.Zero(t, first.MemoriesIngested)
	require.NotNil(t, first.DurationMs)
	assert.Positive(t, *first.DurationMs)
	assert.True(t, v.processed(row))
	assert.True(t, v.latched(user))
	second := v.compile(user, `{"forceFullRecompile":false}`)
	assert.Zero(t, second.MemoriesIngested)
	require.NotNil(t, second.DurationMs)
	assert.Zero(t, *second.DurationMs)
	assert.True(t, v.latched(user))
}

func TestExtractedMemoriesPersistAndRollUpIntoTheResponse(t *testing.T) {
	v := setup(t, `{"memories":["User prefers dark mode.","User journals every Sunday evening."]}`)
	user := v.user()
	row := v.file(user, "notes/preferences.md", "# Preferences\n\nI prefer dark mode in every app I use.\nI journal every Sunday evening before bed.")
	body := v.compile(user, `{"forceFullRecompile":false}`)
	assert.Equal(t, 2, body.MemoriesIngested)
	assert.Len(t, body.PendingMemoryIDs, 2)
	require.NotNil(t, body.DurationMs)
	assert.Positive(t, *body.DurationMs)
	assert.True(t, v.processed(row))
	saved, err := memorystore.New(v.pool, nil).List(context.Background(), user)
	require.NoError(t, err)
	contents := make([]string, len(saved))
	for i, m := range saved {
		contents[i] = m.Content
	}
	assert.ElementsMatch(t, []string{"User prefers dark mode.", "User journals every Sunday evening."}, contents)
}

// Every success body must validate against api/openapi.yaml (decisions.md D2).
func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t, `{"memories":["one"]}`)
	user := v.user()
	v.file(user, "notes/a.md", "note")
	for range 2 { // with a file, then empty
		body := `{"forceFullRecompile":false}`
		rec := v.post(user, body)
		req := httptest.NewRequest(http.MethodPost, "/v1/memory-compile", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
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
}
