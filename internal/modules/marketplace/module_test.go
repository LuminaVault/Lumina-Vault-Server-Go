package marketplace_test

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/marketplace"
	"lumina/internal/modules/marketplace/db"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultpaths"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

const (
	masterKey  = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	signingKey = "test-artifact-signing-key-with-at-least-32-bytes"
	wasmB64    = "AGFzbQEAAAA="

	alphaV1 = "AA7A0000-0000-4000-8000-000000000001"
	alphaV2 = "AA7A0000-0000-4000-8000-000000000002"
	alphaV3 = "AA7A0000-0000-4000-8000-000000000003"
)

var (
	admin = user("A1")
	pub   = user("A2")
	inst  = user("A7")
	rd    = user("AD")
	none  = user("AE")
	wasm  = user("B1")
	runr  = user("A9")
	capu  = user("AA")
	keys  = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

func user(suffix string) uuid.UUID {
	return uuid.MustParse("AA770000-0000-4000-8000-0000000000" + suffix)
}

type env struct {
	t     *testing.T
	e     *echo.Echo
	pool  *pgxpool.Pool
	m     *marketplace.Module
	paths vaultpaths.Paths
}

func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "marketplace.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	paths := vaultpaths.Paths{Root: t.TempDir()}
	m := marketplace.New(pool, masterKey, marketplace.Options{
		ArtifactRoot: t.TempDir(), ArtifactSigningKey: signingKey,
	}, auth.Identify(keys, auth.PGUsers(pool)), ratelimit.NewStore(), paths)
	require.NotNil(t, m)
	m.SetVersionInserts(true)
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{m},
	})
	return env{t: t, e: e, pool: pool, m: m, paths: paths}
}

type resp struct {
	code int
	ct   string
	body []byte
}

func (r resp) obj(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(r.body, &m), string(r.body))
	return m
}

func (v env) do(u uuid.UUID, method, path, body string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if u != uuid.Nil {
		tok, err := scenario.Mint(keys, u)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()}
}

func (v env) ok(u uuid.UUID, method, path, body string) map[string]any {
	v.t.Helper()
	r := v.do(u, method, path, body)
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	return r.obj(v.t)
}

func (v env) errIs(r resp, status int, msg string) {
	v.t.Helper()
	require.Equal(v.t, status, r.code, string(r.body))
	m, err := json.Marshal(msg)
	require.NoError(v.t, err)
	require.JSONEq(v.t, `{"error":{"message":`+string(m)+`}}`, string(r.body))
}

// MARK: - MarketplaceSafetyTests

func wasmVersion(manifest string) db.MarketplaceVersion {
	return db.MarketplaceVersion{
		ID: uuid.New(), ListingID: uuid.New(), Version: "1.0.0", Status: "draft", RuntimeKind: "wasm",
		Permissions: []string{}, NetworkHosts: []string{},
		ArtifactKey: new("publisher/plugin/module.wasm"), ArtifactSha256: new(strings.Repeat("a", 64)),
		ArtifactSignature: new(strings.Repeat("b", 64)), ManifestJson: []byte(manifest),
	}
}

func TestWasmVersionsRequireValidUniqueToolManifest(t *testing.T) {
	version := wasmVersion(`{"schemaVersion":1,"tools":[{"name":"run","description":"Run safely"}]}`)
	require.Empty(t, marketplace.ValidateVersion(version))
	manifest, err := marketplace.ParseManifest(version)
	require.NoError(t, err)
	require.Len(t, manifest.Tools, 1)
	require.Equal(t, "run", manifest.Tools[0].Name)
}

func TestDuplicateAndMalformedToolNamesAreRejected(t *testing.T) {
	errs := marketplace.ValidateVersion(wasmVersion(`{"schemaVersion":1,"tools":[{"name":"Bad Tool","description":"Unsafe"},{"name":"Bad Tool","description":"Duplicate"}]}`))
	require.Contains(t, errs, "duplicate_plugin_tools")
	require.Contains(t, errs, "invalid_plugin_tool_name")
}

func TestManifestValidation(t *testing.T) {
	require.Equal(t, []string{"plugin_tools_required"}, marketplace.ValidateVersion(wasmVersion(`{"schemaVersion":2,"tools":[{"name":"run","description":"d"}]}`)))
	require.Equal(t, []string{"plugin_tools_required"}, marketplace.ValidateVersion(wasmVersion(`{"schemaVersion":1,"tools":[]}`)))
	require.Equal(t, []string{"invalid_plugin_manifest"}, marketplace.ValidateVersion(wasmVersion(`{"tools":[]}`)))
	require.Equal(t, []string{"invalid_plugin_manifest"}, marketplace.ValidateVersion(wasmVersion(`{"schemaVersion":1,"tools":[{"name":"run"}]}`)))
	nilManifest := wasmVersion("")
	nilManifest.ManifestJson = nil
	require.Equal(t, []string{"invalid_plugin_manifest"}, marketplace.ValidateVersion(nilManifest))
}

func TestArtifactSignaturesBindBothStorageKeyAndDigest(t *testing.T) {
	key, digest := "publisher/plugin/module.wasm", strings.Repeat("a", 64)
	signature := marketplace.SignArtifact(key, digest, signingKey)
	require.True(t, marketplace.VerifyArtifactSignature(signature, key, digest, signingKey))
	require.False(t, marketplace.VerifyArtifactSignature(signature, "publisher/plugin/other.wasm", digest, signingKey))
	require.False(t, marketplace.VerifyArtifactSignature(signature, key, strings.Repeat("c", 64), signingKey))
}

func TestArtifactSignatureVerificationFailsClosedForWeakKeys(t *testing.T) {
	require.False(t, marketplace.VerifyArtifactSignature(strings.Repeat("a", 64), "publisher/plugin/module.wasm", strings.Repeat("b", 64), "short"))
}

func noNetBroker() *marketplace.CapabilityBroker {
	return marketplace.NewBroker(nil, vaultpaths.Paths{Root: "/tmp/marketplace-capability-test"})
}

func TestCapabilityBrokerRejectsOperationsWithoutAnInstallGrant(t *testing.T) {
	results := noNetBroker().Execute(context.Background(), []marketplace.CapabilityRequest{
		{ID: "read-1", Operation: "memory.read", Arguments: map[string]string{"id": strings.ToUpper(uuid.NewString())}},
	}, uuid.New(), "test-plugin", nil, nil)
	require.Len(t, results, 1)
	require.Equal(t, "read-1", results[0].ID)
	require.False(t, results[0].OK)
	require.Empty(t, results[0].Values)
	require.Equal(t, "permission_denied", *results[0].Error)
}

func TestCapabilityBrokerCapsASingleExecutionBatch(t *testing.T) {
	requests := make([]marketplace.CapabilityRequest, 17)
	for i := range requests {
		requests[i] = marketplace.CapabilityRequest{ID: fmt.Sprint(i), Operation: "memory.read", Arguments: map[string]string{}}
	}
	results := noNetBroker().Execute(context.Background(), requests, uuid.New(), "test-plugin", nil, nil)
	require.Len(t, results, len(requests))
	for _, r := range results {
		require.Equal(t, "too_many_capability_requests", *r.Error)
	}
}

type scriptedOutput struct {
	output map[string]string
	fuel   int
}

// scriptedRunner is ScriptedMarketplaceRunner.
type scriptedRunner struct {
	mu      sync.Mutex
	outputs []scriptedOutput
	inputs  []map[string]string
}

func (r *scriptedRunner) Execute(_ context.Context, _ []byte, _ string, _ []marketplace.PluginPermission, input map[string]string) (map[string]string, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inputs = append(r.inputs, input)
	next := r.outputs[0]
	if len(r.outputs) > 1 {
		r.outputs = r.outputs[1:]
	}
	return next.output, next.fuel, nil
}

// installWasm takes a WASM plugin through the API, as a publisher and an
// admin, then installs it for u.
func (v env) installWasm(u uuid.UUID, slug string, permissions, hosts []string, tools ...string) string {
	v.t.Helper()
	root := "/v1/marketplace/publisher/plugins/" + slug
	v.ok(wasm, http.MethodPost, "/v1/marketplace/publisher/plugins", fmt.Sprintf(`{"slug":%q,"name":%q,"summary":"S","description":"D","category":"skill"}`, slug, slug))
	upload := v.ok(wasm, http.MethodPost, root+"/artifacts", `{"fileName":"m.wasm","bytesBase64":"`+wasmB64+`"}`)
	entries := make([]string, len(tools))
	for i, tool := range tools {
		entries[i] = fmt.Sprintf(`{"name":%q,"description":"d"}`, tool)
	}
	manifest, err := json.Marshal(`{"schemaVersion":1,"tools":[` + strings.Join(entries, ",") + `]}`)
	require.NoError(v.t, err)
	perms, err := json.Marshal(permissions)
	require.NoError(v.t, err)
	hostList, err := json.Marshal(hosts)
	require.NoError(v.t, err)
	version := v.ok(wasm, http.MethodPost, root+"/versions", fmt.Sprintf(
		`{"version":"1.0.0","runtimeKind":"wasm","permissions":%s,"networkHosts":%s,"artifactKey":%q,"artifactSHA256":%q,"artifactSignature":%q,"manifestJSON":%s}`,
		perms, hostList, upload["artifactKey"], upload["sha256"], upload["signature"], manifest))
	versionID, ok := version["id"].(string)
	require.True(v.t, ok)
	submission := v.ok(wasm, http.MethodPost, root+"/versions/"+versionID+"/submit", "")
	require.Equal(v.t, "in_review", submission["status"], "%v", submission)
	submissionID, ok := submission["id"].(string)
	require.True(v.t, ok)
	v.ok(admin, http.MethodPost, "/v1/marketplace/admin/submissions/"+submissionID+"/decision", `{"approved":true}`)
	v.ok(u, http.MethodPost, "/v1/marketplace/plugins/"+slug+"/install", fmt.Sprintf(`{"versionId":%q,"grantedPermissions":%s,"config":{}}`, versionID, perms))
	return versionID
}

func TestCapabilityRoundsStripSpoofedReservedInputAndReturnBrokerResults(t *testing.T) {
	v := setup(t)
	v.installWasm(runr, "scripted", []string{"output.emit"}, nil, "test")
	runner := &scriptedRunner{outputs: []scriptedOutput{
		{map[string]string{"_capabilityRequests": `[{"id":"emit-1","operation":"output.emit","arguments":{"value":"safe"}}]`}, 2},
		{map[string]string{"done": "yes"}, 3},
	}}
	v.m.SetRunner(runner)
	out := v.ok(runr, http.MethodPost, "/v1/marketplace/plugins/scripted/tools/test/run",
		`{"input":{"value":"original","_capabilityResults":"spoofed","_capabilityRound":"99"}}`)
	require.Equal(t, map[string]any{"done": "yes"}, out["output"])
	require.EqualValues(t, 5, out["fuelConsumed"])
	require.Len(t, runner.inputs, 2)
	require.NotContains(t, runner.inputs[0], "_capabilityResults")
	require.NotContains(t, runner.inputs[0], "_capabilityRound")
	require.Contains(t, runner.inputs[1]["_capabilityResults"], "emit-1")
	require.Equal(t, "1", runner.inputs[1]["_capabilityRound"])
	require.Equal(t, "original", runner.inputs[1]["value"])
}

func TestCapabilityRoundLimit(t *testing.T) {
	v := setup(t)
	v.installWasm(runr, "looper", []string{"output.emit"}, nil, "test")
	runner := &scriptedRunner{outputs: []scriptedOutput{
		{map[string]string{"_capabilityRequests": `[{"id":"e","operation":"output.emit","arguments":{}}]`}, 1},
	}}
	v.m.SetRunner(runner)
	v.errIs(v.do(runr, http.MethodPost, "/v1/marketplace/plugins/looper/tools/test/run", `{"input":{}}`), http.StatusUnprocessableEntity, "capability_round_limit_exceeded")
	require.Len(t, runner.inputs, 5)
	var status, code string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT status, error_code FROM marketplace_executions ORDER BY created_at DESC LIMIT 1`).Scan(&status, &code))
	require.Equal(t, "failed", status)
	require.Equal(t, "runner_failed", code)
}

// MARK: - The runner client

func TestRunnerClient(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		require.Equal(t, "/v1/execute", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		tool, _ := got["toolName"].(string)
		switch tool {
		case "reject":
			w.WriteHeader(http.StatusBadGateway)
		case "garbage":
			_, _ = w.Write([]byte("nope"))
		default:
			_, _ = w.Write([]byte(`{"output":{"a":"b"},"fuelConsumed":9}`))
		}
	}))
	defer srv.Close()

	runner := marketplace.NewRunner(srv.URL+"/", "tok")
	output, fuel, err := runner.Execute(context.Background(), []byte{1, 2}, "t", []marketplace.PluginPermission{"memory.read"}, map[string]string{"k": "v"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"a": "b"}, output)
	require.Equal(t, 9, fuel)
	require.Equal(t, "Bearer tok", auth)
	require.Equal(t, map[string]any{
		"moduleBase64": "AQI=", "toolName": "t", "permissions": []any{"memory.read"},
		"input": map[string]any{"k": "v"}, "fuel": float64(10_000_000),
	}, got)

	_, _, err = runner.Execute(context.Background(), nil, "reject", nil, nil)
	require.ErrorContains(t, err, "plugin_runner_rejected")
	_, _, err = runner.Execute(context.Background(), nil, "garbage", nil, nil)
	require.Error(t, err)

	_, _, err = marketplace.NewRunner(srv.URL, "").Execute(context.Background(), nil, "t", nil, nil)
	require.ErrorContains(t, err, "plugin_runner_disabled")
}

// MARK: - The broker, with the database and the network

func capability(op string, args map[string]string) []marketplace.CapabilityRequest {
	return []marketplace.CapabilityRequest{{ID: "r", Operation: op, Arguments: args}}
}

func TestBrokerMemoryAndVault(t *testing.T) {
	v := setup(t)
	b := marketplace.NewBroker(v.pool, v.paths)
	ctx := context.Background()
	all := []marketplace.PluginPermission{"memory.read", "memory.write", "vault.read", "vault.write", "output.emit"}

	r := b.Execute(ctx, capability("memory.write", map[string]string{"content": " Hi ", "tags": "a, b ,,c"}), capu, "slug", all, nil)[0]
	require.True(t, r.OK, "%v", r)
	var content, kind, source string
	var tags []string
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT content, tags, origin_kind, origin_source_id FROM memories WHERE id = $1`, r.Values["id"]).Scan(&content, &tags, &kind, &source))
	require.Equal(t, "Hi", content)
	require.Equal(t, []string{"a", "b", "c"}, tags)
	require.Equal(t, "import", kind)
	require.Equal(t, "marketplace:slug", source)

	read := b.Execute(ctx, capability("memory.read", map[string]string{"id": "AA7B3000-0000-4000-8000-000000000001"}), capu, "slug", all, nil)[0]
	require.Equal(t, map[string]string{"id": "AA7B3000-0000-4000-8000-000000000001", "content": "A remembered thing", "tags": "one,two"}, read.Values)
	other := b.Execute(ctx, capability("memory.read", map[string]string{"id": "AA7B3000-0000-4000-8000-000000000002"}), capu, "slug", all, nil)[0]
	require.Equal(t, "resource_not_found", *other.Error)

	w := b.Execute(ctx, capability("vault.write", map[string]string{"path": "a/b.txt", "text": "hello", "contentType": "text/plain"}), capu, "slug", all, nil)[0]
	require.True(t, w.OK, "%v", w)
	require.Equal(t, "5", w.Values["sizeBytes"])
	got := b.Execute(ctx, capability("vault.read", map[string]string{"path": "a/b.txt"}), capu, "slug", all, nil)[0]
	require.Equal(t, "aGVsbG8=", got.Values["bytesBase64"])
	require.Equal(t, "text/plain", got.Values["contentType"])

	for _, path := range []string{"../x", "a//b", "./a", "a/..", "", "a\x00b"} {
		bad := b.Execute(ctx, capability("vault.read", map[string]string{"path": path}), capu, "slug", all, nil)[0]
		require.Equal(t, "invalid_arguments", *bad.Error, path)
	}

	// A symlink out of the tenant's raw directory is refused.
	raw := v.paths.Raw(capu)
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(raw, "out")))
	esc := b.Execute(ctx, capability("vault.write", map[string]string{"path": "out/x.txt", "text": "x"}), capu, "slug", all, nil)[0]
	require.Equal(t, "invalid_arguments", *esc.Error)

	emit := b.Execute(ctx, capability("output.emit", map[string]string{"a": "b"}), capu, "slug", all, nil)[0]
	require.Equal(t, map[string]string{"a": "b"}, emit.Values)
	unknown := b.Execute(ctx, capability("root.shell", nil), capu, "slug", all, nil)[0]
	require.Equal(t, "operation_unsupported", *unknown.Error)
}

func TestBrokerNetworkFetch(t *testing.T) {
	var redirected bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			require.Equal(t, "application/json, text/plain;q=0.9, */*;q=0.1", r.Header.Get("Accept"))
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("hi"))
		case "/redirect":
			http.Redirect(w, r, "/followed", http.StatusFound)
		case "/followed":
			redirected = true
		case "/big":
			_, _ = w.Write(make([]byte, 1_048_577))
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()
	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// allowed.test pins to a public address, then dials the test server.
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	addr := srv.Listener.Addr().String()
	transport.DialContext = dialTo(addr)
	transport.TLSClientConfig.ServerName = "example.com" // the test certificate's name

	b := marketplace.NewBroker(nil, vaultpaths.Paths{Root: t.TempDir()})
	b.SetNetwork(func(_ context.Context, host string) ([]string, error) {
		switch host {
		case "allowed.test", "other.test":
			return []string{"93.184.216.34"}, nil
		case "rebind.test":
			return []string{"10.0.0.5"}, nil
		}
		return []string{host}, nil
	}, client)
	ctx := context.Background()
	fetch := func(url string) marketplace.CapabilityResult {
		return b.Execute(ctx, capability("network.fetch", map[string]string{"url": url}), uuid.New(), "slug",
			[]marketplace.PluginPermission{"network.fetch"}, []string{"allowed.test", "rebind.test"})[0]
	}

	ok1 := fetch("https://allowed.test/ok")
	require.True(t, ok1.OK, "%v", ok1)
	require.Equal(t, map[string]string{"status": "200", "contentType": "text/plain", "bytesBase64": "aGk="}, ok1.Values)
	require.Equal(t, "upstream_failure", *fetch("https://allowed.test/teapot").Error)
	require.Equal(t, "upstream_failure", *fetch("https://allowed.test/redirect").Error)
	require.False(t, redirected)
	require.Equal(t, "capability_failed", *fetch("https://allowed.test/big").Error)
	require.Equal(t, "host_not_allowed", *fetch("https://other.test/ok").Error)
	require.Equal(t, "invalid_arguments", *fetch("https://allowed.test:8443/ok").Error)
	require.Equal(t, "invalid_arguments", *fetch("https://u:p@allowed.test/ok").Error)
	for _, blocked := range []string{
		"http://allowed.test/ok", "ftp://allowed.test/ok", "https://localhost/x", "https://127.0.0.1/x",
		"https://10.0.0.1/x", "https://172.16.0.1/x", "https://192.168.1.1/x", "https://169.254.169.254/x",
		"https://[::1]/x", "https://[fe80::1]/x", "https://[fc00::1]/x", "https://[::ffff:10.0.0.1]/x", "https://rebind.test/ok",
	} {
		require.Equal(t, "capability_failed", *fetch(blocked).Error, blocked)
	}
}

// MARK: - HTTP surface

func TestInstallUpgradeSealConfigForTheTenant(t *testing.T) {
	v := setup(t)
	alpha := "/v1/marketplace/plugins/alpha-notes"
	out := v.ok(inst, http.MethodPost, alpha+"/install", fmt.Sprintf(`{"versionId":%q,"grantedPermissions":["memory.read"],"config":{"api_key":"k","region":"eu"}}`, alphaV1))
	require.Equal(t, true, out["hasConfig"])
	require.Equal(t, alphaV1, out["marketplaceVersionId"])

	v.errIs(v.do(inst, http.MethodPost, alpha+"/upgrade", fmt.Sprintf(`{"fromVersionId":%q,"toVersionId":%q,"grantedPermissions":["memory.read"],"config":{}}`, alphaV2, alphaV3)),
		http.StatusConflict, "marketplace_install_version_changed")
	v.errIs(v.do(inst, http.MethodPost, alpha+"/upgrade", fmt.Sprintf(`{"fromVersionId":%q,"toVersionId":%q,"grantedPermissions":["memory.read"],"config":{}}`, alphaV1, alphaV1)),
		http.StatusConflict, "marketplace_version_already_installed")
	// Keys the new version does not declare are unknown even though they came from the old config.
	v.errIs(v.do(inst, http.MethodPost, alpha+"/upgrade", fmt.Sprintf(`{"fromVersionId":%q,"toVersionId":%q,"grantedPermissions":[],"config":{"new_required":"x"}}`, alphaV1, alphaV3)),
		http.StatusBadRequest, "unknown_field")

	up := v.ok(inst, http.MethodPost, alpha+"/upgrade", fmt.Sprintf(`{"fromVersionId":%q,"toVersionId":%q,"grantedPermissions":["vault.read","memory.read"],"config":{"region":"us"}}`, alphaV1, alphaV2))
	require.Equal(t, alphaV2, up["marketplaceVersionId"])
	require.Equal(t, []any{"memory.read", "vault.read"}, up["grantedPermissions"])
}

func TestCatalogSeedsOnFirstReadAndIsIdempotent(t *testing.T) {
	v := setup(t)
	_, err := v.pool.Exec(context.Background(), `DELETE FROM marketplace_publishers WHERE handle = 'luminavault'`)
	require.NoError(t, err)
	out := v.ok(rd, http.MethodGet, "/v1/marketplace/plugins?limit=50&featured=true", "")
	items, ok := out["items"].([]any)
	require.True(t, ok)
	slugs := []string{}
	for _, it := range items {
		m, ok := it.(map[string]any)
		require.True(t, ok)
		slug, ok := m["slug"].(string)
		require.True(t, ok)
		slugs = append(slugs, slug)
	}
	// PluginMarketplaceTests' curation assertions.
	require.Contains(t, slugs, "readwise")
	require.Contains(t, slugs, "news-ticker")
	v.ok(rd, http.MethodGet, "/v1/marketplace/plugins/readwise", "")
	var count int
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM marketplace_listings l JOIN marketplace_publishers p ON p.id = l.publisher_id WHERE p.handle = 'luminavault'`).Scan(&count))
	require.Equal(t, 6, count)
}

// Swift at fdbed9b0 cannot insert a marketplace_versions row (errVersionInsert).
func TestVersionInsertsFailAsInSwift(t *testing.T) {
	v := setup(t)
	v.m.SetVersionInserts(false)
	ctx := context.Background()

	v.errIs(v.do(pub, http.MethodPost, "/v1/marketplace/publisher/plugins/alpha-notes/versions", `{"version":"9.9.9","runtimeKind":"declarative","permissions":[]}`),
		http.StatusConflict, "version_already_exists")
	v.errIs(v.do(pub, http.MethodPost, "/v1/marketplace/publisher/plugins/alpha-notes/versions", `{"version":"bad","runtimeKind":"declarative","permissions":[]}`),
		http.StatusUnprocessableEntity, "invalid_semver")

	_, err := v.pool.Exec(ctx, `DELETE FROM marketplace_publishers WHERE handle = 'luminavault'`)
	require.NoError(t, err)
	require.Equal(t, http.StatusInternalServerError, v.do(rd, http.MethodGet, "/v1/marketplace/plugins", "").code)
	var orphans int
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT count(*) FROM marketplace_listings l JOIN marketplace_publishers p ON p.id = l.publisher_id WHERE p.handle = 'luminavault' AND l.slug = 'byok-embeddings'`).Scan(&orphans))
	require.Equal(t, 1, orphans)
	// The orphan listing, published with no version, fails every list that reaches it.
	v.errIs(v.do(rd, http.MethodGet, "/v1/marketplace/plugins", ""), http.StatusNotFound, "marketplace_version_not_found")
	v.errIs(v.do(rd, http.MethodGet, "/v1/marketplace/plugins/byok-embeddings", ""), http.StatusNotFound, "marketplace_version_not_found")
	v.errIs(v.do(rd, http.MethodGet, "/v1/marketplace/plugins/readwise", ""), http.StatusNotFound, "marketplace_plugin_not_found")
	v.ok(rd, http.MethodGet, "/v1/marketplace/plugins/alpha-notes", "")
}

func TestNewIsNilWithoutAMasterKey(t *testing.T) {
	require.Nil(t, marketplace.New(nil, "", marketplace.Options{}, nil, nil, vaultpaths.Paths{}))
}

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	alpha := "/v1/marketplace/plugins/alpha-notes"
	check := func(u uuid.UUID, method, path, body string) resp {
		t.Helper()
		r := v.do(u, method, path, body)
		require.Less(t, r.code, 300, "%s %s: %s", method, path, r.body)
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		outp := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: r.code,
			Header: http.Header{"Content-Type": {r.ct}},
		}
		outp.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, outp), "%s %s: %s", method, path, r.body)
		return r
	}
	check(rd, http.MethodGet, "/v1/marketplace/plugins", "")
	check(rd, http.MethodGet, alpha, "")
	check(rd, http.MethodGet, alpha+"/reviews", "")
	check(inst, http.MethodPost, alpha+"/install", fmt.Sprintf(`{"versionId":%q,"grantedPermissions":["memory.read"],"config":{"api_key":"k"}}`, alphaV1))
	check(inst, http.MethodPost, alpha+"/upgrade", fmt.Sprintf(`{"fromVersionId":%q,"toVersionId":%q,"grantedPermissions":["memory.read","vault.read"],"config":{}}`, alphaV1, alphaV2))
	check(inst, http.MethodPut, alpha+"/rating", `{"rating":4,"body":"Nice"}`)
	check(none, http.MethodPost, "/v1/marketplace/publisher/apply", `{"handle":"spec-handle","displayName":"Spec","bio":"b","websiteURL":"https://spec.parity.test"}`)
	check(none, http.MethodGet, "/v1/marketplace/publisher", "")
	check(wasm, http.MethodPost, "/v1/marketplace/publisher/plugins", `{"slug":"spec-plugin","name":"Spec","summary":"S","description":"D","category":"skill"}`)
	root := "/v1/marketplace/publisher/plugins/spec-plugin"
	upload := check(wasm, http.MethodPost, root+"/artifacts", `{"fileName":"m.wasm","bytesBase64":"`+wasmB64+`"}`).obj(t)
	manifest, err := json.Marshal(`{"schemaVersion":1,"tools":[{"name":"echo","description":"d"}]}`)
	require.NoError(t, err)
	version := check(wasm, http.MethodPost, root+"/versions", fmt.Sprintf(
		`{"version":"1.0.0","runtimeKind":"wasm","permissions":[],"artifactKey":%q,"artifactSHA256":%q,"artifactSignature":%q,"manifestJSON":%s}`,
		upload["artifactKey"], upload["sha256"], upload["signature"], manifest)).obj(t)
	versionID, ok := version["id"].(string)
	require.True(t, ok)
	submission := check(wasm, http.MethodPost, root+"/versions/"+versionID+"/submit", "").obj(t)
	submissionID, ok := submission["id"].(string)
	require.True(t, ok)
	check(wasm, http.MethodGet, "/v1/marketplace/publisher/submissions", "")
	check(admin, http.MethodGet, "/v1/marketplace/admin/submissions", "")
	check(admin, http.MethodGet, "/v1/marketplace/admin/publishers", "")
	check(admin, http.MethodPost, "/v1/marketplace/admin/submissions/"+submissionID+"/decision", `{"approved":true,"note":"ok"}`)
	check(admin, http.MethodPost, "/v1/marketplace/admin/publishers/AA780000-0000-4000-8000-000000000003/decision", `{"approved":true}`)
	check(admin, http.MethodPost, "/v1/marketplace/admin/versions/AA7A0000-0000-4000-8000-000000000009/revoke", `{"reason":"Security issue"}`)
	v.m.SetRunner(&scriptedRunner{outputs: []scriptedOutput{{map[string]string{"a": "b"}, 1}}})
	check(runr, http.MethodPost, "/v1/marketplace/plugins/spec-plugin/install", fmt.Sprintf(`{"versionId":%q,"grantedPermissions":[],"config":{}}`, versionID))
	check(runr, http.MethodPost, "/v1/marketplace/plugins/spec-plugin/tools/echo/run", `{"input":{"x":"y"}}`)
	v.errIs(v.do(pub, http.MethodPost, "/v1/marketplace/publisher/plugins/nope/versions", `{"version":"1.0.0","runtimeKind":"declarative","permissions":[]}`), http.StatusNotFound, "marketplace_plugin_not_found")
}
