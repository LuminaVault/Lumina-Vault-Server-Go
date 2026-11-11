package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/chunkindex"
	"lumina/internal/config"
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
	"strings"
	"testing"

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

func setup(t *testing.T) env {
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
	module := mcp.New(mcp.Options{
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
	})
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
