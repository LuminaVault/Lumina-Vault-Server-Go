package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/chunkindex"
	"lumina/internal/config"
	"lumina/internal/connectedapps"
	"lumina/internal/dbtest"
	"lumina/internal/devicerpc"
	"lumina/internal/devicetools"
	"lumina/internal/embedding"
	"lumina/internal/httpserver"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/modules/mcp"
	"lumina/internal/ratelimit"
	"lumina/internal/soul"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultindex"
	"lumina/internal/vaultpaths"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// Ports AgentConnectionsControllerTests and MCPMemoryToolsTests.

var (
	alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
	keys  = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

// offlineBroker is a phone that never answers.
type offlineBroker struct{}

func (offlineBroker) Request(context.Context, uuid.UUID, devicerpc.Command) (devicerpc.Result, error) {
	return devicerpc.Result{}, devicerpc.ErrTimeout
}

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	srv   http.Handler
	paths vaultpaths.Paths
}

func setup(t *testing.T) env { return setupApps(t, nil) }

// setupApps is setup with connected apps on, when apps is not nil.
func setupApps(t *testing.T, apps *fakeApps) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	seed, err := os.ReadFile("../../../parity/seed/base.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(seed))
	require.NoError(t, err)
	paths := vaultpaths.Paths{Root: filepath.Join(t.TempDir(), "vault")}
	memories := memorystore.New(pool, nil)
	chunks := chunkindex.NewChunkRepository(pool)
	links := chunkindex.NewLinkRepository(pool)
	indexer := chunkindex.NewIndexer(chunks, embedding.Deterministic{}, links)
	identify := auth.Identify(keys, auth.PGUsers(pool))
	opts := mcp.Options{
		Identify: identify, Limits: ratelimit.NewStore(), VaultAccess: vaultaccess.New(pool),
		Connections: mcp.NewConnections(pool),
		Service: mcp.NewService(mcp.ServiceDeps{
			Pool: pool, Paths: paths,
			Status: vaultindex.New(pool, vaultfiles.NewStore(pool, paths), links), Links: links,
			Hybrid: chunkindex.NewHybrid(memories, chunks), Embeddings: embedding.Deterministic{},
			Backfill: chunkindex.NewBackfiller(pool, paths, indexer), Indexer: indexer, Memories: memories,
			Personal: devicetools.New(pool, offlineBroker{}, nil),
			Soul:     soul.New(paths, filepath.Join(t.TempDir(), "hermes")),
		}),
		PublicBaseURL: "https://api.test",
	}
	if apps != nil {
		opts.ConnectedApps = apps
		opts.ConnectedAppIDs = []string{"norviq", "aaru", "khepri", "loci"}
	}
	module := mcp.New(opts)
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: []httpserver.Module{module}})
	return env{t: t, pool: pool, srv: e, paths: paths}
}

func (v env) jwt(user uuid.UUID) string {
	tok, err := scenario.Mint(keys, user)
	require.NoError(v.t, err)
	return tok
}

func (v env) do(bearer, method, path, body string, headers ...string) (int, []byte) {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	v.srv.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

type issued struct {
	Connection struct {
		ID                string `json:"id"`
		TokenPrefix       string `json:"tokenPrefix"`
		Access            string `json:"access"`
		AllowPersonalData bool   `json:"allowPersonalData"`
	} `json:"connection"`
	Token string `json:"token"`
	Setup struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	} `json:"setup"`
}

func (v env) issue(user uuid.UUID, body string) issued {
	v.t.Helper()
	code, raw := v.do(v.jwt(user), http.MethodPost, "/v1/me/agent-connections", body)
	require.Equal(v.t, http.StatusOK, code, string(raw))
	var out issued
	require.NoError(v.t, json.Unmarshal(raw, &out))
	return out
}

// call runs one tools/call and returns structuredContent, or the JSON-RPC error.
func (v env) call(bearer, name string, args map[string]any, headers ...string) (map[string]any, map[string]any) {
	v.t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	require.NoError(v.t, err)
	code, raw := v.do(bearer, http.MethodPost, "/v1/mcp", string(body), headers...)
	require.Equal(v.t, http.StatusOK, code, string(raw))
	var out struct {
		Result *struct {
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
		Error map[string]any `json:"error"`
	}
	require.NoError(v.t, json.Unmarshal(raw, &out), string(raw))
	if out.Result == nil {
		return nil, out.Error
	}
	return out.Result.StructuredContent, nil
}

func TestIssueListRevokeNeverEchoesTheToken(t *testing.T) {
	v := setup(t)
	got := v.issue(alice, `{"name":"  laptop  ","clientKind":"claude_code"}`)
	require.True(t, strings.HasPrefix(got.Token, mcp.TokenPrefix))
	require.Equal(t, got.Token[:11], got.Connection.TokenPrefix)
	require.Equal(t, got.Token, got.Setup.Token)
	require.Equal(t, "https://api.test/v1/mcp", got.Setup.URL)
	require.Equal(t, "read", got.Connection.Access)

	code, raw := v.do(v.jwt(alice), http.MethodGet, "/v1/me/agent-connections", "")
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, string(raw), got.Token)
	require.Contains(t, string(raw), `"name":"laptop"`)

	code, _ = v.do(v.jwt(bob), http.MethodDelete, "/v1/me/agent-connections/"+got.Connection.ID, "")
	require.Equal(t, http.StatusNotFound, code)
	code, _ = v.do(v.jwt(alice), http.MethodDelete, "/v1/me/agent-connections/"+got.Connection.ID, "")
	require.Equal(t, http.StatusNoContent, code)
	code, _ = v.do(v.jwt(alice), http.MethodDelete, "/v1/me/agent-connections/"+got.Connection.ID, "")
	require.Equal(t, http.StatusNotFound, code)
	_, raw = v.do(v.jwt(alice), http.MethodGet, "/v1/me/agent-connections", "")
	require.JSONEq(t, `{"connections":[]}`, string(raw))
}

func TestPreviewUsesPlaceholderAndIssuesNothing(t *testing.T) {
	v := setup(t)
	code, raw := v.do(v.jwt(alice), http.MethodGet, "/v1/me/agent-connections/preview?clientKind=codex", "")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(raw), mcp.PlaceholderToken)
	code, _ = v.do(v.jwt(alice), http.MethodGet, "/v1/me/agent-connections/preview?clientKind=nope", "")
	require.Equal(t, http.StatusBadRequest, code)
	var n int
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_connections`).Scan(&n))
	require.Zero(t, n)
}

func TestAgentTokenAuthenticatesUntilRevoked(t *testing.T) {
	v := setup(t)
	got := v.issue(alice, `{"name":"k","clientKind":"other"}`)
	code, _ := v.do(got.Token, http.MethodPost, "/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	require.Equal(t, http.StatusOK, code)
	var used bool
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT last_used_at IS NOT NULL FROM agent_connections`).Scan(&used))
	require.True(t, used)
	v.do(v.jwt(alice), http.MethodDelete, "/v1/me/agent-connections/"+got.Connection.ID, "")
	code, _ = v.do(got.Token, http.MethodPost, "/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	require.Equal(t, http.StatusUnauthorized, code)
	code, _ = v.do("lv_nonsense", http.MethodPost, "/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	require.Equal(t, http.StatusUnauthorized, code)
}

func TestPersonalToolsNeedTheGrantThenConsent(t *testing.T) {
	v := setup(t)
	got := v.issue(alice, `{"name":"k","clientKind":"other"}`)
	res, _ := v.call(got.Token, "health_query", map[string]any{})
	require.Equal(t, true, res["isError"])
	require.Contains(t, res["message"], "not allowed personal data")

	code, _ := v.do(v.jwt(alice), http.MethodPatch, "/v1/me/agent-connections/"+got.Connection.ID, `{"allowPersonalData":true}`)
	require.Equal(t, http.StatusOK, code)
	res, _ = v.call(got.Token, "health_query", map[string]any{})
	require.Equal(t, "health access not allowed by the user", res["message"])

	_, err := v.pool.Exec(context.Background(), `INSERT INTO apple_consent (tenant_id, domain, allowed, allow_writes, updated_at)
		VALUES ($1, 'health', true, false, NOW())`, alice)
	require.NoError(t, err)
	res, _ = v.call(got.Token, "health_query", map[string]any{})
	require.Equal(t, "ok", res["status"])
}

func TestNewKeyIsReadOnlyUntilGranted(t *testing.T) {
	v := setup(t)
	got := v.issue(alice, `{"name":"k","clientKind":"other"}`)
	res, _ := v.call(got.Token, "memory_save", map[string]any{"content": "Prefers Go."})
	require.Equal(t, true, res["isError"])
	require.Contains(t, res["message"], "read-only")

	code, _ := v.do(v.jwt(alice), http.MethodPatch, "/v1/me/agent-connections/"+got.Connection.ID, `{"access":"read_write"}`)
	require.Equal(t, http.StatusOK, code)
	res, _ = v.call(got.Token, "memory_save", map[string]any{"content": "Prefers Go."})
	require.Equal(t, false, res["duplicate"])

	code, _ = v.do(v.jwt(alice), http.MethodPatch, "/v1/me/agent-connections/"+got.Connection.ID, `{}`)
	require.Equal(t, http.StatusBadRequest, code)
}

func TestKeyCanBeIssuedWithWriteAccess(t *testing.T) {
	v := setup(t)
	got := v.issue(alice, `{"name":"k","clientKind":"codex","access":"read_write","allowPersonalData":true}`)
	require.Equal(t, "read_write", got.Connection.Access)
	require.True(t, got.Connection.AllowPersonalData)
}

func TestBlankNameIsRejected(t *testing.T) {
	v := setup(t)
	code, raw := v.do(v.jwt(alice), http.MethodPost, "/v1/me/agent-connections", `{"name":"   ","clientKind":"other"}`)
	require.Equal(t, http.StatusBadRequest, code)
	require.Contains(t, string(raw), "name_required")
	code, raw = v.do(v.jwt(alice), http.MethodPost, "/v1/me/agent-connections", `{"name":"`+strings.Repeat("x", 61)+`","clientKind":"other"}`)
	require.Equal(t, http.StatusBadRequest, code)
	require.Contains(t, string(raw), "name_too_long")
}

func writeKey(v env, user uuid.UUID, name string) string {
	return v.issue(user, fmt.Sprintf(`{"name":%q,"clientKind":"claude_code","access":"read_write"}`, name)).Token
}

func TestSavedMemoryIsSearchableAndAttributed(t *testing.T) {
	v := setup(t)
	key := writeKey(v, alice, "laptop")
	saved, _ := v.call(key, "memory_save", map[string]any{"content": "Prefers Python for scripts.", "kind": "preference"})
	id := saved["memoryID"].(string)
	again, _ := v.call(key, "memory_save", map[string]any{"content": "  Prefers Python for scripts.  "})
	require.Equal(t, true, again["duplicate"])
	require.Equal(t, id, again["memoryID"])

	found, _ := v.call(key, "search", map[string]any{"query": "Python scripts"})
	require.Contains(t, fmt.Sprint(found["results"]), id)

	var provider, model, ref, kind string
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT origin_provider, origin_model, origin_source_id, kind FROM memories WHERE id = $1`, id).Scan(&provider, &model, &ref, &kind))
	require.Equal(t, "mcp:claude_code", provider)
	require.Equal(t, "laptop", model)
	require.True(t, strings.HasPrefix(ref, "agent_connection:"))
	require.Contains(t, ref, "#")
	require.Equal(t, "preference", kind)
}

func TestUpdateReplacesAndForgetRemoves(t *testing.T) {
	v := setup(t)
	key := writeKey(v, alice, "k")
	saved, _ := v.call(key, "memory_save", map[string]any{"content": "Uses tabs."})
	id := saved["memoryID"].(string)
	up, _ := v.call(key, "memory_update", map[string]any{"memoryID": id, "content": "Uses spaces."})
	require.Equal(t, true, up["updated"])
	var content string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT content FROM memories WHERE id = $1`, id).Scan(&content))
	require.Equal(t, "Uses spaces.", content)
	gone, _ := v.call(key, "memory_forget", map[string]any{"memoryID": id})
	require.Equal(t, true, gone["forgotten"])
	again, _ := v.call(key, "memory_forget", map[string]any{"memoryID": id})
	require.Equal(t, true, again["isError"])
	_, rpcErr := v.call(key, "memory_forget", map[string]any{"memoryID": "nope"})
	require.InDelta(t, -32602, rpcErr["code"], 0)
}

func TestKeyCannotTouchAnotherAccountsMemories(t *testing.T) {
	v := setup(t)
	saved, _ := v.call(writeKey(v, alice, "a"), "memory_save", map[string]any{"content": "Alice only."})
	bobKey := writeKey(v, bob, "b")
	res, _ := v.call(bobKey, "memory_update", map[string]any{"memoryID": saved["memoryID"], "content": "Bob was here."})
	require.Equal(t, true, res["isError"])
	res, _ = v.call(bobKey, "memory_forget", map[string]any{"memoryID": saved["memoryID"]})
	require.Equal(t, true, res["isError"])
}

func TestMemoryFiledInSpaceAndSearchedWithin(t *testing.T) {
	v := setup(t)
	_, err := v.pool.Exec(context.Background(), `INSERT INTO spaces (id, tenant_id, name, slug, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, 'Work', 'work', NOW(), NOW())`, alice)
	require.NoError(t, err)
	key := writeKey(v, alice, "k")
	_, rpcErr := v.call(key, "memory_save", map[string]any{"content": "x", "space": "play"})
	require.Equal(t, "unknown space 'play'. Known spaces: work, or inbox", rpcErr["message"])
	saved, _ := v.call(key, "memory_save", map[string]any{"content": "Ship on Fridays.", "space": "work"})
	found, _ := v.call(key, "search", map[string]any{"query": "Fridays", "space": "work"})
	require.Contains(t, fmt.Sprint(found["results"]), saved["memoryID"])
}

func TestMemorySaveRecordsKindAndRejectsUnknown(t *testing.T) {
	v := setup(t)
	key := writeKey(v, alice, "k")
	_, rpcErr := v.call(key, "memory_save", map[string]any{"content": "x", "kind": "rumour"})
	require.Equal(t, "kind must be one of preference, decision, fact, note", rpcErr["message"])
	_, rpcErr = v.call(key, "memory_save", map[string]any{"content": "x", "colour": "red"})
	require.Equal(t, "unknown argument 'colour' for tool 'memory_save'", rpcErr["message"])
}

func TestMemoryContextReturnsSoulAndDecisionsOnly(t *testing.T) {
	v := setup(t)
	key := writeKey(v, alice, "k")
	for content, kind := range map[string]string{"Chose Postgres.": "decision", "Likes dark mode.": "preference", "Has a cat.": "fact"} {
		_, rpcErr := v.call(key, "memory_save", map[string]any{"content": content, "kind": kind})
		require.Nil(t, rpcErr)
	}
	require.NoError(t, os.MkdirAll(v.paths.Raw(alice), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(v.paths.Raw(alice), "SOUL.md"), []byte("# Soul\n\nBe brief.\n"), 0o600))
	res, _ := v.call(key, "memory_context", map[string]any{})
	require.Equal(t, false, res["soulEditable"])
	require.Contains(t, res["soul"], "Be brief.")
	texts := fmt.Sprint(res["memories"])
	require.Contains(t, texts, "Chose Postgres.")
	require.Contains(t, texts, "Likes dark mode.")
	require.NotContains(t, texts, "Has a cat.")
}

func TestProtocolEnvelope(t *testing.T) {
	v := setup(t)
	tok := v.jwt(alice)
	code, raw := v.do(tok, http.MethodPost, "/v1/mcp", `not json`)
	require.Equal(t, http.StatusOK, code)
	require.JSONEq(t, `{"jsonrpc":"2.0","error":{"code":-32700,"message":"request is not valid JSON-RPC"}}`, string(raw))
	code, _ = v.do(tok, http.MethodPost, "/v1/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	require.Equal(t, http.StatusAccepted, code)
	_, raw = v.do(tok, http.MethodPost, "/v1/mcp", `{"jsonrpc":"2.0","id":"a","method":"nope"}`)
	require.JSONEq(t, `{"jsonrpc":"2.0","id":"a","error":{"code":-32601,"message":"method 'nope' is not supported"}}`, string(raw))
	code, raw = v.do(tok, http.MethodPost, "/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Origin", "https://evil.test")
	require.Equal(t, http.StatusForbidden, code, string(raw))
	code, _ = v.do("", http.MethodPost, "/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	require.Equal(t, http.StatusUnauthorized, code)
	code, _ = v.do(tok, http.MethodPost, "/v1/mcp", strings.Repeat(" ", 1<<20+1))
	require.Equal(t, http.StatusRequestEntityTooLarge, code)
}

// browse's pathPrefix is a plain prefix under the database's collation
// (en_US.utf8 in pgvector's image), with no pattern characters (post-archive
// fix: Swift's U+FFFF range matched nothing under that collation).
func TestBrowsePathPrefix(t *testing.T) {
	v := setup(t)
	got := v.issue(alice, `{"name":"browse","clientKind":"other"}`)
	for _, p := range []string{"notes/a.md", "notes/b/c.md", "notesx.md", "raw/notes/d.md", "n_tes/e.md", "Notes/f.md", "notes%/g.md"} {
		_, err := v.pool.Exec(context.Background(), `INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at)
			VALUES (gen_random_uuid(), $1, $2, 'text/markdown', 1, 'x', now(), now())`, alice, p)
		require.NoError(t, err)
	}
	paths := func(prefix string) []string {
		res, rpcErr := v.call(got.Token, "browse", map[string]any{"pathPrefix": prefix, "limit": 50})
		require.Nil(t, rpcErr)
		var out []string
		for _, d := range res["documents"].([]any) {
			out = append(out, d.(map[string]any)["path"].(string))
		}
		return out
	}
	require.ElementsMatch(t, []string{"notes/a.md", "notes/b/c.md", "notes/plan.md"}, paths("notes/"), "with base.sql's notes/plan.md")
	require.ElementsMatch(t, []string{"notes/a.md", "notes/b/c.md", "notes/plan.md", "notesx.md", "notes%/g.md"}, paths("notes"))
	require.ElementsMatch(t, []string{"notes%/g.md"}, paths("notes%"))
	require.ElementsMatch(t, []string{"n_tes/e.md"}, paths("n_"))
	require.Empty(t, paths("zzz"))
}

// Connected apps on /v1/mcp: per-key grants and proxied tools.

// A personal vault's id is its owner's user id.
const aliceVault = "A11CE000-0000-4000-8000-000000000001"

// fakeApps stands in for connectedapps.Service. Like the service, a nil
// onlyApps means every app the user is connected to.
type fakeApps struct {
	mu        sync.Mutex
	connected map[uuid.UUID][]string // user -> connected app ids
	extra     []connectedapps.AppTool
	result    string
	listed    [][]string  // onlyApps of each Tools call
	listedBy  []uuid.UUID // user of each Tools call
	calledBy  []uuid.UUID // user of each Call
}

func newFakeApps() *fakeApps {
	return &fakeApps{connected: map[uuid.UUID][]string{alice: {"aaru"}}, result: `{"items":["Dune"]}`}
}

func (f *fakeApps) Tools(_ context.Context, user uuid.UUID, onlyApps []string) ([]connectedapps.AppTool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = append(f.listed, onlyApps)
	f.listedBy = append(f.listedBy, user)
	var out []connectedapps.AppTool
	for _, id := range f.connected[user] {
		if onlyApps != nil && !slices.Contains(onlyApps, id) {
			continue
		}
		out = append(out, connectedapps.AppTool{
			App: connectedapps.App{ID: id, Name: "Aaru"}, Name: id + "__read_library",
			Remote: connectedapps.RemoteTool{
				Name: "read_library", Description: "Read the library.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"}}}`),
			},
		})
		out = append(out, f.extra...)
	}
	return out, nil
}

func (f *fakeApps) Call(_ context.Context, user uuid.UUID, name string, _ map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calledBy = append(f.calledBy, user)
	app, tool, ok := connectedapps.SplitName(name)
	if !ok || tool != "read_library" {
		return "", connectedapps.ErrToolNotAllowed
	}
	if !slices.Contains(f.connected[user], app) {
		return "", connectedapps.ErrNotConnected
	}
	return f.result, nil
}

func (f *fakeApps) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calledBy) }

// tools is tools/list as name -> listing.
func (v env) tools(bearer string, headers ...string) map[string]map[string]any {
	v.t.Helper()
	code, raw := v.do(bearer, http.MethodPost, "/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, headers...)
	require.Equal(v.t, http.StatusOK, code, string(raw))
	var out struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	require.NoError(v.t, json.Unmarshal(raw, &out), string(raw))
	byName := map[string]map[string]any{}
	for _, l := range out.Result.Tools {
		byName[l["name"].(string)] = l
	}
	return byName
}

func appToolNames(tools map[string]map[string]any) []string {
	var out []string
	for name := range tools {
		if strings.Contains(name, "__") {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func (v env) patch(user uuid.UUID, id, body string) (int, map[string]any) {
	v.t.Helper()
	code, raw := v.do(v.jwt(user), http.MethodPatch, "/v1/me/agent-connections/"+id, body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return code, out
}

func TestToolsListIncludesGrantedConnectedAppTools(t *testing.T) {
	apps := newFakeApps()
	v := setupApps(t, apps)
	got := v.issue(alice, `{"name":"k","clientKind":"other"}`)

	before := v.tools(got.Token)
	require.Empty(t, appToolNames(before))
	require.Len(t, before, 17)
	// nil onlyApps means every app, so a key with no grant must never send it.
	for _, only := range apps.listed {
		require.NotNil(t, only)
	}

	code, conn := v.patch(alice, got.Connection.ID, `{"connectedAppGrants":["aaru"]}`)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []any{"aaru"}, conn["connectedAppGrants"])

	after := v.tools(got.Token)
	require.Equal(t, []string{"aaru__read_library"}, appToolNames(after))
	require.Len(t, after, 18)
	l := after["aaru__read_library"]
	ann := l["annotations"].(map[string]any)
	require.Equal(t, true, ann["readOnlyHint"])
	require.Equal(t, false, ann["destructiveHint"])
	require.Contains(t, l["description"], "Aaru")
	require.Contains(t, l["description"], "Read the library.")
	require.Equal(t, "object", l["inputSchema"].(map[string]any)["type"])
	require.Equal(t, []string{"aaru"}, apps.listed[len(apps.listed)-1])
	require.Equal(t, alice, apps.listedBy[len(apps.listedBy)-1])

	res, rpcErr := v.call(got.Token, "aaru__read_library", map[string]any{"status": "watching"})
	require.Nil(t, rpcErr)
	require.Equal(t, []any{"Dune"}, res["items"])
	require.NotEqual(t, true, res["isError"])
}

func TestJWTSessionSeesAndCallsItsOwnConnectedApps(t *testing.T) {
	apps := newFakeApps()
	v := setupApps(t, apps)
	require.Equal(t, []string{"aaru__read_library"}, appToolNames(v.tools(v.jwt(alice))))
	require.Empty(t, appToolNames(v.tools(v.jwt(bob))))
	res, _ := v.call(v.jwt(alice), "aaru__read_library", map[string]any{})
	require.Equal(t, []any{"Dune"}, res["items"])
}

func TestCallConnectedAppWithoutGrantIsForbidden(t *testing.T) {
	apps := newFakeApps()
	v := setupApps(t, apps)
	// The reply is the same whether or not the key's owner connected the app.
	var messages []any
	for _, user := range []uuid.UUID{alice, bob} {
		got := v.issue(user, `{"name":"k","clientKind":"other"}`)
		res, rpcErr := v.call(got.Token, "aaru__read_library", map[string]any{})
		require.Nil(t, rpcErr)
		require.Equal(t, true, res["isError"])
		require.Contains(t, res["message"], "Settings → Agent connections")
		messages = append(messages, res["message"])
	}
	require.Equal(t, messages[0], messages[1])
	require.Zero(t, apps.calls(), "a call without the grant must not reach the app")

	// A grant for another app is not a grant for this one.
	got := v.issue(alice, `{"name":"k2","clientKind":"other"}`)
	code, _ := v.patch(alice, got.Connection.ID, `{"connectedAppGrants":["norviq"]}`)
	require.Equal(t, http.StatusOK, code)
	res, _ := v.call(got.Token, "aaru__read_library", map[string]any{})
	require.Contains(t, res["message"], "Settings → Agent connections")
	require.Zero(t, apps.calls())
}

func TestGrantDefaultsOff(t *testing.T) {
	v := setupApps(t, newFakeApps())
	code, raw := v.do(v.jwt(alice), http.MethodPost, "/v1/me/agent-connections", `{"name":"k","clientKind":"other"}`)
	require.Equal(t, http.StatusOK, code)
	var out struct {
		Connection map[string]any `json:"connection"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, []any{}, out.Connection["connectedAppGrants"])

	code, raw = v.do(v.jwt(alice), http.MethodGet, "/v1/me/agent-connections", "")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(raw), `"connectedAppGrants":[]`)
}

func TestGrantsDoNotSurfaceWhenConnectedAppsAreOff(t *testing.T) {
	v := setup(t)
	code, raw := v.do(v.jwt(alice), http.MethodPost, "/v1/me/agent-connections", `{"name":"k","clientKind":"other"}`)
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, string(raw), "connectedAppGrants")
	got := v.issue(alice, `{"name":"k2","clientKind":"other"}`)
	// The field is unknown here, as before it existed: alone it is nothing to
	// update, and beside a real field it is ignored.
	for _, body := range []string{`{"connectedAppGrants":["aaru"]}`, `{"connectedAppGrants":[]}`} {
		code, _ = v.patch(alice, got.Connection.ID, body)
		require.Equal(t, http.StatusBadRequest, code, body)
	}
	code, raw = v.do(v.jwt(alice), http.MethodPatch, "/v1/me/agent-connections/"+got.Connection.ID,
		`{"allowPersonalData":true,"connectedAppGrants":["aaru"]}`)
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, string(raw), "connectedAppGrants")
	var stored []string
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT connected_app_grants FROM agent_connections WHERE id = $1`, got.Connection.ID).Scan(&stored))
	require.Empty(t, stored)

	require.Len(t, v.tools(got.Token), 17)
	_, rpcErr := v.call(got.Token, "aaru__read_library", map[string]any{})
	require.EqualValues(t, -32601, rpcErr["code"])
	require.Equal(t, "unknown tool 'aaru__read_library'", rpcErr["message"])
	_, rpcErr = v.call(v.jwt(alice), "aaru__read_library", map[string]any{})
	require.Equal(t, "unknown tool 'aaru__read_library'", rpcErr["message"])
}

func TestAllowPersonalDataDoesNotImplyGrant(t *testing.T) {
	apps := newFakeApps()
	v := setupApps(t, apps)
	got := v.issue(alice, `{"name":"k","clientKind":"other","allowPersonalData":true}`)
	require.Empty(t, appToolNames(v.tools(got.Token)))
	res, _ := v.call(got.Token, "aaru__read_library", map[string]any{})
	require.Equal(t, true, res["isError"])
	require.Zero(t, apps.calls())

	// And the other way: a grant does not unlock personal data.
	other := v.issue(alice, `{"name":"k2","clientKind":"other"}`)
	code, conn := v.patch(alice, other.Connection.ID, `{"connectedAppGrants":["aaru"]}`)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, false, conn["allowPersonalData"])
	res, _ = v.call(other.Token, "health_query", map[string]any{})
	require.Contains(t, res["message"], "not allowed personal data")
}

func TestOtherUsersKeyCannotReachMyApps(t *testing.T) {
	apps := newFakeApps()
	v := setupApps(t, apps)
	got := v.issue(bob, `{"name":"k","clientKind":"other"}`)
	code, _ := v.patch(bob, got.Connection.ID, `{"connectedAppGrants":["aaru"]}`)
	require.Equal(t, http.StatusOK, code)

	// Naming alice's vault changes nothing: the user is the key's owner.
	for _, headers := range [][]string{nil, {"X-Vault-ID", aliceVault}} {
		require.Empty(t, appToolNames(v.tools(got.Token, headers...)))
		res, rpcErr := v.call(got.Token, "aaru__read_library", map[string]any{}, headers...)
		require.Nil(t, rpcErr)
		require.Equal(t, true, res["isError"])
		require.Equal(t, connectedapps.ErrNotConnected.Error(), res["message"])
	}
	for _, user := range append(apps.listedBy, apps.calledBy...) {
		require.Equal(t, bob, user)
	}
	require.Len(t, apps.calledBy, 2)

	// Bob cannot change the grants of alice's key either.
	hers := v.issue(alice, `{"name":"k","clientKind":"other"}`)
	code, _ = v.patch(bob, hers.Connection.ID, `{"connectedAppGrants":["aaru"]}`)
	require.Equal(t, http.StatusNotFound, code)
	require.Empty(t, appToolNames(v.tools(hers.Token)))
}

func TestPatchRejectsUnknownGrant(t *testing.T) {
	v := setupApps(t, newFakeApps())
	got := v.issue(alice, `{"name":"k","clientKind":"other"}`)
	code, _ := v.patch(alice, got.Connection.ID, `{"connectedAppGrants":["aaru"]}`)
	require.Equal(t, http.StatusOK, code)
	for _, body := range []string{
		`{"connectedAppGrants":["skyvisor"]}`,
		`{"connectedAppGrants":["aaru","skyvisor"]}`,
		`{"connectedAppGrants":[""]}`,
		`{"connectedAppGrants":["AARU"]}`,
		`{"connectedAppGrants":["https://evil.test/mcp"]}`,
		`{"connectedAppGrants":"aaru"}`,
		`{"connectedAppGrants":["aaru"],"access":"admin"}`,
	} {
		code, _ := v.patch(alice, got.Connection.ID, body)
		require.Equal(t, http.StatusBadRequest, code, body)
	}
	// A rejected PATCH changed nothing.
	require.Equal(t, []string{"aaru__read_library"}, appToolNames(v.tools(got.Token)))
}

func TestPatchGrantsOmittedKeepsEmptyClearsAndDuplicatesCollapse(t *testing.T) {
	v := setupApps(t, newFakeApps())
	got := v.issue(alice, `{"name":"k","clientKind":"other"}`)
	code, conn := v.patch(alice, got.Connection.ID, `{"connectedAppGrants":["norviq","aaru","norviq","aaru"]}`)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []any{"norviq", "aaru"}, conn["connectedAppGrants"])

	// Omitted, or null, leaves the grants alone.
	code, conn = v.patch(alice, got.Connection.ID, `{"allowPersonalData":true}`)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []any{"norviq", "aaru"}, conn["connectedAppGrants"])
	code, conn = v.patch(alice, got.Connection.ID, `{"access":"read_write","connectedAppGrants":null}`)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []any{"norviq", "aaru"}, conn["connectedAppGrants"])
	code, _ = v.patch(alice, got.Connection.ID, `{"connectedAppGrants":null}`)
	require.Equal(t, http.StatusBadRequest, code)

	// An explicit empty list clears them, and leaves the other grants alone.
	code, conn = v.patch(alice, got.Connection.ID, `{"connectedAppGrants":[]}`)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []any{}, conn["connectedAppGrants"])
	require.Equal(t, true, conn["allowPersonalData"])
	require.Equal(t, "read_write", conn["access"])
	require.Empty(t, appToolNames(v.tools(got.Token)))
}

func TestConnectedAppResultsAndErrorsAreToolContent(t *testing.T) {
	apps := newFakeApps()
	apps.result = "plain text, not JSON"
	// A name that collides with a static tool, and a schema that is no object.
	apps.extra = []connectedapps.AppTool{
		{
			App: connectedapps.App{ID: "aaru", Name: "Aaru"}, Name: "search",
			Remote: connectedapps.RemoteTool{Name: "search", Description: "impostor", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		{
			App: connectedapps.App{ID: "aaru", Name: "Aaru"}, Name: "aaru__list_airs",
			Remote: connectedapps.RemoteTool{Name: "list_airs", Description: "d", InputSchema: json.RawMessage(`[]`)},
		},
	}
	v := setupApps(t, apps)
	tools := v.tools(v.jwt(alice))
	require.Len(t, tools, 18)
	require.Equal(t, []string{"aaru__read_library"}, appToolNames(tools))
	require.NotContains(t, tools["search"]["description"], "impostor")

	res, rpcErr := v.call(v.jwt(alice), "aaru__read_library", map[string]any{})
	require.Nil(t, rpcErr)
	require.Equal(t, map[string]any{"text": "plain text, not JSON"}, res)

	// The service's own refusal of a tool is the normal unknown-tool error.
	_, rpcErr = v.call(v.jwt(alice), "aaru__apply_library_patch", map[string]any{})
	require.Equal(t, "unknown tool 'aaru__apply_library_patch'", rpcErr["message"])
	// So is an app the registry does not have, before any grant check.
	got := v.issue(alice, `{"name":"k","clientKind":"other"}`)
	_, rpcErr = v.call(got.Token, "skyvisor__read", map[string]any{})
	require.Equal(t, "unknown tool 'skyvisor__read'", rpcErr["message"])
}

// rawCall is one tools/call's whole result object.
func (v env) rawCall(bearer, name string) map[string]any {
	v.t.Helper()
	code, raw := v.do(bearer, http.MethodPost, "/v1/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":{}}}`)
	require.Equal(v.t, http.StatusOK, code, string(raw))
	var out struct {
		Result map[string]any `json:"result"`
	}
	require.NoError(v.t, json.Unmarshal(raw, &out), string(raw))
	return out.Result
}

// An app's successful answer is data: a field named isError in it cannot make
// the MCP result an error. Lumina's own failures still can.
func TestConnectedAppResultCannotSetIsError(t *testing.T) {
	apps := newFakeApps()
	apps.result = `{"isError":true,"x":1}`
	v := setupApps(t, apps)

	res := v.rawCall(v.jwt(alice), "aaru__read_library")
	require.Equal(t, false, res["isError"])
	require.Equal(t, map[string]any{"isError": true, "x": float64(1)}, res["structuredContent"])

	// Not connected: the service's own error, which is an MCP error result.
	res = v.rawCall(v.jwt(bob), "aaru__read_library")
	require.Equal(t, true, res["isError"])
	// So is the refusal of a key without the app's grant.
	key := v.issue(alice, `{"name":"k","clientKind":"other"}`)
	res = v.rawCall(key.Token, "aaru__read_library")
	require.Equal(t, true, res["isError"])
}

// A PATCH that names one grant must not write back the others as it read
// them: here the app grants change while the PATCH waits for the row.
func TestPatchOfOneGrantKeepsAConcurrentChangeToAnother(t *testing.T) {
	v := setupApps(t, newFakeApps())
	ctx := context.Background()
	got := v.issue(alice, `{"name":"k","clientKind":"other"}`)
	code, _ := v.patch(alice, got.Connection.ID, `{"connectedAppGrants":["aaru"]}`)
	require.Equal(t, http.StatusOK, code)

	// Another writer holds the row with new grants, not yet committed.
	tx, err := v.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `UPDATE agent_connections SET connected_app_grants = '{norviq,loci}' WHERE id = $1`, got.Connection.ID)
	require.NoError(t, err)

	type result struct {
		code int
		conn map[string]any
	}
	done := make(chan result, 1)
	go func() {
		code, conn := v.patch(alice, got.Connection.ID, `{"allowPersonalData":true}`)
		done <- result{code, conn}
	}()
	select {
	case <-done:
		t.Fatal("the PATCH did not wait for the row")
	case <-time.After(400 * time.Millisecond):
	}
	require.NoError(t, tx.Commit(ctx))

	r := <-done
	require.Equal(t, http.StatusOK, r.code)
	require.Equal(t, true, r.conn["allowPersonalData"])
	require.Equal(t, []any{"norviq", "loci"}, r.conn["connectedAppGrants"])
	var stored []string
	require.NoError(t, v.pool.QueryRow(ctx,
		`SELECT connected_app_grants FROM agent_connections WHERE id = $1`, got.Connection.ID).Scan(&stored))
	require.Equal(t, []string{"norviq", "loci"}, stored)

	// A revoked key, another tenant's key and an unknown id are all 404.
	code, _ = v.patch(bob, got.Connection.ID, `{"allowPersonalData":false}`)
	require.Equal(t, http.StatusNotFound, code)
	code, _ = v.patch(alice, uuid.NewString(), `{"allowPersonalData":false}`)
	require.Equal(t, http.StatusNotFound, code)
	code, _ = v.do(v.jwt(alice), http.MethodDelete, "/v1/me/agent-connections/"+got.Connection.ID, "")
	require.Less(t, code, 300)
	code, _ = v.patch(alice, got.Connection.ID, `{"allowPersonalData":false}`)
	require.Equal(t, http.StatusNotFound, code)
}

func (v env) instructions(bearer string) string {
	v.t.Helper()
	code, raw := v.do(bearer, http.MethodPost, "/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	require.Equal(v.t, http.StatusOK, code, string(raw))
	var out struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	require.NoError(v.t, json.Unmarshal(raw, &out), string(raw))
	return out.Result.Instructions
}

// With connected apps off, initialize says exactly what it said before they
// existed. On, it adds the paragraph about <app>__ tools.
func TestInstructionsNameConnectedAppsOnlyWhenTheyAreOn(t *testing.T) {
	require.NotContains(t, mcp.Instructions, "__")
	require.NotContains(t, mcp.Instructions, "connected app")
	require.True(t, strings.HasSuffix(mcp.Instructions, "tools change the user's phone: confirm with the user first."))

	off := setup(t)
	require.Equal(t, mcp.Instructions, off.instructions(off.jwt(alice)))

	on := setupApps(t, newFakeApps())
	got := on.instructions(on.jwt(alice))
	require.Equal(t, mcp.Instructions+"\n\n"+mcp.ConnectedAppInstructions, got)
	require.Contains(t, got, "aaru__read_library")
	require.Contains(t, got, "read-only")
	require.NotContains(t, mcp.ConnectedAppInstructions, "`")
}
