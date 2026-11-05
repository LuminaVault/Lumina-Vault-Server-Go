package imports_test

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/capture"
	"lumina/internal/chunkindex"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/diskguard"
	"lumina/internal/embedding"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/memorycompile"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/modules/imports"
	"lumina/internal/modules/spaces"
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

// replyTransport answers each completion with what reply builds from the request.
type replyTransport struct {
	reply func(payload []byte) string
	calls int
}

func (s *replyTransport) ChatCompletions(_ context.Context, payload []byte, _, _ string) (llm.Metadata, error) {
	s.calls++
	content := ""
	if s.reply != nil {
		content = s.reply(payload)
	}
	data, err := json.Marshal(map[string]any{
		"id": "test", "model": "test",
		"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": content}}},
	})
	return llm.Metadata{Data: data}, err
}

func (*replyTransport) ChatStream(context.Context, []byte, string, string) llm.Stream {
	return func(yield func(llm.Chunk, error) bool) { yield(llm.Chunk{}, errors.New("no stream")) }
}

// noLinks refuses: these tests stage files, not links.
type noLinks struct{}

func (noLinks) CaptureLink(context.Context, uuid.UUID, string, *string, *uuid.UUID) (capture.CapturedLink, error) {
	return capture.CapturedLink{}, errors.New("no links in this test")
}

type env struct {
	t         *testing.T
	e         *echo.Echo
	pool      *pgxpool.Pool
	root      string
	transport *replyTransport
}

func setup(t *testing.T) env {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	root := t.TempDir()
	paths := vaultpaths.Paths{Root: root}
	users := auth.PGUsers(pool)
	identify := auth.Identify(keys, users)
	transport := &replyTransport{}
	repo := memorystore.New(pool, nil)
	compile := memorycompile.New(pool, memorycompile.Options{
		Paths: paths, Transport: transport, Memories: repo, Embeddings: embedding.Deterministic{}, DefaultModel: "test",
	})
	service := imports.NewService(pool, noLinks{}, spaces.New(pool, paths, identify), paths, compile, nil)
	indexer := chunkindex.NewIndexer(chunkindex.NewChunkRepository(pool), embedding.Deterministic{}, chunkindex.NewLinkRepository(pool))
	ingest := imports.NewIngest(pool, service, repo, embedding.Deterministic{}, indexer, diskguard.New(root, 0, 0, diskguard.Options{}))
	module := imports.New(service, &imports.Categorizer{Service: service, Transport: transport, DefaultModel: "test"}, ingest, identify)
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, users), Modules: []httpserver.Module{module}})
	return env{t: t, e: e, pool: pool, root: root, transport: transport}
}

func (v env) user() uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	slug := "imp" + id.String()[:8]
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
	id := uuid.New()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at)
		VALUES ($1, $2, $3, 'text/markdown', $4, $5, now(), now())`, id, tenant, path, len(content), strings.Repeat("c", 64))
	require.NoError(v.t, err)
	return id
}

func (v env) do(user uuid.UUID, method, path, body string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		token, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec
}

func (v env) stage(user uuid.UUID, files ...uuid.UUID) string {
	v.t.Helper()
	list := make([]string, len(files))
	for i, f := range files {
		list[i] = `"` + ids.Upper(f) + `"`
	}
	rec := v.do(user, http.MethodPost, "/v1/import/files", `{"sourceType":"documents","vaultFileIds":[`+strings.Join(list, ",")+`]}`)
	require.Equal(v.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		SessionID string `json:"sessionId"`
	}
	require.NoError(v.t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotEmpty(v.t, out.SessionID, rec.Body.String())
	return out.SessionID
}

type item struct {
	ID            string  `json:"id"`
	ProposedSpace *string `json:"proposedSpace"`
	Status        string  `json:"status"`
}

func (v env) status(user uuid.UUID, session string) (string, []item) {
	v.t.Helper()
	rec := v.do(user, http.MethodGet, "/v1/import/"+session, "")
	require.Equal(v.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Status string `json:"status"`
		Items  []item `json:"items"`
	}
	require.NoError(v.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.Status, out.Items
}

// itemIDs reads the [id=...] markers out of the categorize prompt, as the model would.
func itemIDs(payload []byte) []string {
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(payload, &body) != nil || len(body.Messages) < 2 {
		return nil
	}
	out := []string{}
	for _, part := range strings.Split(body.Messages[1].Content, "[id=")[1:] {
		out = append(out, part[:strings.Index(part, "]")])
	}
	return out
}

// The model's mapping can't be pinned in parity because item ids are random; here the
// stub reads them from the prompt.
func TestCategorizeAppliesTheMappingAndCapsNewSpaces(t *testing.T) {
	v := setup(t)
	user := v.user()
	files := make([]uuid.UUID, 12)
	for i := range files {
		files[i] = v.file(user, "inbox/n"+string(rune('a'+i))+".md", "note")
	}
	session := v.stage(user, files...)
	v.transport.reply = func(payload []byte) string {
		list := itemIDs(payload)
		mappings := make([]map[string]string, 0, len(list))
		for i, id := range list {
			mappings = append(mappings, map[string]string{"id": id, "space": "new:Topic " + string(rune('A'+i))})
		}
		data, err := json.Marshal(map[string]any{"mappings": mappings})
		if err != nil {
			return ""
		}
		return string(data)
	}
	rec := v.do(user, http.MethodPost, "/v1/import/"+session+"/categorize", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	status, items := v.status(user, session)
	assert.Equal(t, imports.StatusReview, status)
	require.Len(t, items, 12)
	created, inbox := 0, 0
	for _, it := range items {
		require.NotNil(t, it.ProposedSpace)
		if strings.HasPrefix(*it.ProposedSpace, "new:") {
			created++
		} else {
			assert.Equal(t, imports.ImportedSlug, *it.ProposedSpace)
			inbox++
		}
	}
	assert.Equal(t, 6, created, "the new-Space cap")
	assert.Equal(t, 6, inbox, "beyond the cap items fall back to the inbox")
}

func TestCategorizeMissingMappingFallsBackToInbox(t *testing.T) {
	v := setup(t)
	user := v.user()
	session := v.stage(user, v.file(user, "inbox/x.md", "note"))
	v.transport.reply = func([]byte) string { return "no mapping at all" }
	require.Equal(t, http.StatusOK, v.do(user, http.MethodPost, "/v1/import/"+session+"/categorize", "").Code)
	_, items := v.status(user, session)
	require.Len(t, items, 1)
	require.NotNil(t, items[0].ProposedSpace)
	assert.Equal(t, imports.ImportedSlug, *items[0].ProposedSpace)
}

func TestApproveFilesCompilesAndIsTenantScoped(t *testing.T) {
	v := setup(t)
	user, other := v.user(), v.user()
	file := v.file(user, "inbox/x.md", "A note about tea.")
	session := v.stage(user, file)
	v.transport.reply = func([]byte) string { return `{"memories":["Likes tea."]}` }
	assert.Equal(t, http.StatusNotFound, v.do(other, http.MethodPost, "/v1/import/"+session+"/approve", "").Code)

	rec := v.do(user, http.MethodPost, "/v1/import/"+session+"/approve", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusNotFound, v.do(other, http.MethodGet, "/v1/import/"+session, "").Code)

	status, _ := v.status(user, session)
	assert.Equal(t, imports.StatusDone, status)
	var path string
	var done bool
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT path, processed_at IS NOT NULL FROM vault_files WHERE id = $1`, file).Scan(&path, &done))
	assert.True(t, done)
	assert.True(t, strings.HasPrefix(path, imports.ImportedSlug+"/"), path)
	_, err := os.Stat(filepath.Join(v.root, "tenants", ids.Upper(user), "raw", path))
	require.NoError(t, err)
	var memories int
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM memories WHERE tenant_id = $1 AND content = 'Likes tea.'`, user).Scan(&memories))
	assert.Equal(t, 1, memories)
}

func TestVaultBulkImportsAndSkipsUnchanged(t *testing.T) {
	v := setup(t)
	user := v.user()
	body := `{"space":"Notes","files":[{"path":"a/one.md","content":"# One\n\nbody"},{"path":"two.md","content":"two"}]}`
	type result struct {
		Imported int `json:"imported"`
		Skipped  int `json:"skipped"`
		Failed   int `json:"failed"`
	}
	decode := func(rec *httptest.ResponseRecorder) result {
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out result
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}
	assert.Equal(t, result{Imported: 2}, decode(v.do(user, http.MethodPost, "/v1/import/vault-bulk", body)))
	assert.Equal(t, result{Skipped: 2}, decode(v.do(user, http.MethodPost, "/v1/import/vault-bulk", body)))
}

func TestRouteQuirks(t *testing.T) {
	v := setup(t)
	user := v.user()
	assert.Equal(t, http.StatusNotFound, v.do(user, http.MethodGet, "/v1/import/files", "").Code, "the literal shadows :id")
	assert.Equal(t, http.StatusBadRequest, v.do(user, http.MethodGet, "/v1/import/nope", "").Code)
	assert.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, "/v1/import/"+ids.Upper(uuid.New()), "").Code)
}

func TestTitleAndSafePath(t *testing.T) {
	assert.Equal(t, "One", imports.Title("# One\n\nbody", "fallback"))
	assert.Equal(t, "fallback", imports.Title("no heading here", "fallback"))
	assert.NotContains(t, imports.SafePath("notes", "../../etc/passwd"), "..")
	assert.True(t, strings.HasPrefix(imports.SafePath("notes", "a/b.md"), "notes/"))
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

	v := setup(t)
	user := v.user()
	file := v.file(user, "inbox/x.md", "note")
	session := v.stage(user, file)
	v.transport.reply = func([]byte) string { return `{"memories":[]}` }
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/import/files", `{"sourceType":"documents","vaultFileIds":[]}`},
		{http.MethodPost, "/v1/import/vault-bulk", `{"space":"Notes","files":[{"path":"a.md","content":"a"}]}`},
		{http.MethodGet, "/v1/import/" + session, ""},
		{http.MethodPost, "/v1/import/" + session + "/categorize", ""},
		{http.MethodPost, "/v1/import/" + session + "/approve", `{}`},
	} {
		rec := v.do(user, c.method, c.path, c.body)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		if c.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: rec.Code,
			Header: http.Header{"Content-Type": {rec.Header().Get("Content-Type")}},
		}
		out.SetBodyBytes(rec.Body.Bytes())
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, rec.Body.String())
	}
}
