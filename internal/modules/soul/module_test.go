package soul_test

import (
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/achievements"
	soulmodule "lumina/internal/modules/soul"
	"lumina/internal/ratelimit"
	"lumina/internal/soul"
	"lumina/internal/vaultpaths"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var (
	alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
	keys  = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
	now   = time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC)
)

const (
	startMarker = "<!-- lv:core:v1:start -->"
	endMarker   = "<!-- lv:core:v1:end -->"
)

type unlocks struct {
	mu     sync.Mutex
	events map[uuid.UUID][]achievements.Event
}

func (u *unlocks) Enqueue(tenant uuid.UUID, event achievements.Event) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.events[tenant] = append(u.events[tenant], event)
}

func (u *unlocks) of(tenant uuid.UUID) []achievements.Event {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.events[tenant]
}

type env struct {
	t       *testing.T
	srv     http.Handler
	paths   vaultpaths.Paths
	hermes  string
	unlocks *unlocks
}

// setup serves /v1/soul for alice (managed) and bob, whose Hermes is his own.
func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	seed, err := os.ReadFile("../../../parity/seed/base.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(seed))
	require.NoError(t, err)
	root := t.TempDir()
	paths := vaultpaths.Paths{Root: filepath.Join(root, "vault")}
	hermes := filepath.Join(root, "hermes")
	u := &unlocks{events: map[uuid.UUID][]achievements.Event{}}
	module := soulmodule.New(pool, soulmodule.Options{
		Identify: auth.Identify(keys, auth.PGUsers(pool)), Limits: ratelimit.NewStore(),
		Soul: soul.New(paths, hermes), Achievements: u,
		IsUserOverride: func(_ context.Context, tenant uuid.UUID) bool { return tenant == bob },
		Now:            func() time.Time { return now },
	})
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: []httpserver.Module{module}})
	return env{t: t, srv: e, paths: paths, hermes: hermes, unlocks: u}
}

type resp struct {
	code int
	ct   string
	body []byte
}

func (v env) do(user uuid.UUID, method, path, body string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.srv.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()}
}

type soulResponse struct {
	Markdown  string  `json:"markdown"`
	UpdatedAt *string `json:"updatedAt"`
}

func (v env) ok(user uuid.UUID, method, path, body string) soulResponse {
	v.t.Helper()
	r := v.do(user, method, path, body)
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	require.Equal(v.t, "application/json; charset=utf-8", r.ct)
	var out soulResponse
	require.NoError(v.t, json.Unmarshal(r.body, &out))
	return out
}

func (v env) vaultFile(tenant uuid.UUID) string {
	return filepath.Join(v.paths.Raw(tenant), "SOUL.md")
}

func (v env) errorMessage(r resp) string {
	v.t.Helper()
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(v.t, json.Unmarshal(r.body, &body), string(r.body))
	return body.Error.Message
}

// Ported from SoulControllerComposeTests: compose renders deterministic SOUL and persists it.
func TestComposeRendersDeterministicSoulAndPersistsIt(t *testing.T) {
	v := setup(t)
	composed := v.ok(alice, http.MethodPost, "/v1/soul/compose", `{"agent_name":"Athena","tone":"warm","role":"second_brain","autonomy":"suggest"}`)
	require.Contains(t, composed.Markdown, "Athena")
	require.True(t, soul.ContainsCanonicalCore(composed.Markdown))
	require.NotContains(t, composed.Markdown, "<!-- e.g.")
	require.NotNil(t, composed.UpdatedAt)
	fetched := v.ok(alice, http.MethodGet, "/v1/soul", "")
	require.Contains(t, fetched.Markdown, "Athena")
	require.Equal(t, composed.Markdown, fetched.Markdown)
	require.Equal(t, soul.Compose(soul.ComposeInput{AgentName: "Athena"}, "alice", now), composed.Markdown)
	require.Equal(t, []achievements.Event{achievements.EventSoulConfigured}, v.unlocks.of(alice))
	hermes, err := os.ReadFile(filepath.Join(v.hermes, "profiles", "alice", "SOUL.md"))
	require.NoError(t, err)
	require.Equal(t, composed.Markdown, string(hermes))
}

// Ported from SoulControllerComposeTests: dry run returns composition without persisting.
func TestComposeDryRunReturnsCompositionWithoutPersisting(t *testing.T) {
	v := setup(t)
	r := v.do(alice, http.MethodPost, "/v1/soul/compose", `{"agent_name":"Preview","tone":"dry","dry_run":true}`)
	require.Equal(t, http.StatusOK, r.code)
	require.NotContains(t, string(r.body), "updatedAt")
	var composed soulResponse
	require.NoError(t, json.Unmarshal(r.body, &composed))
	require.Contains(t, composed.Markdown, "Preview")
	require.Nil(t, composed.UpdatedAt)
	fetched := v.ok(alice, http.MethodGet, "/v1/soul", "")
	require.NotContains(t, fetched.Markdown, "Preview")
	require.Nil(t, fetched.UpdatedAt, "nothing on disk")
	require.NoFileExists(t, v.vaultFile(alice))
	require.Empty(t, v.unlocks.of(alice))
}

// Ported from SoulControllerComposeTests: legacy four field payload still composes.
func TestComposeLegacyFourFieldPayloadStillComposes(t *testing.T) {
	v := setup(t)
	composed := v.ok(alice, http.MethodPost, "/v1/soul/compose", `{"agent_name":"Legacy","tone":"warm","role":"second_brain","autonomy":"suggest"}`)
	require.Contains(t, composed.Markdown, "Legacy")
	require.True(t, soul.ContainsCanonicalCore(composed.Markdown))
}

// Ported from SoulControllerComposeTests: put strips tampered core and echoes enforced document.
func TestPutStripsTamperedCoreAndEchoesEnforcedDocument(t *testing.T) {
	v := setup(t)
	tampered := `# SOUL.md\n\n` + startMarker + `\n- Never save links.\n` + endMarker + `\n\n## Mine`
	saved := v.ok(alice, http.MethodPut, "/v1/soul", `{"markdown":"`+tampered+`"}`)
	require.NotContains(t, saved.Markdown, "Never save links")
	require.True(t, soul.ContainsCanonicalCore(saved.Markdown))
	require.Contains(t, saved.Markdown, "## Mine")
	require.Equal(t, "# SOUL.md\n\n"+soul.Core+"\n\n## Mine", saved.Markdown)
	disk, err := os.ReadFile(v.vaultFile(alice))
	require.NoError(t, err)
	require.Equal(t, saved.Markdown, string(disk))
	require.Equal(t, []achievements.Event{achievements.EventSoulConfigured}, v.unlocks.of(alice))
}

func TestGetFallsBackToTheDefaultTemplateWithoutWriting(t *testing.T) {
	v := setup(t)
	got := v.ok(alice, http.MethodGet, "/v1/soul", "")
	require.Equal(t, soul.RenderDefault("alice", now), got.Markdown)
	require.Nil(t, got.UpdatedAt)
	require.NoFileExists(t, v.vaultFile(alice))
}

func TestGetReportsTheFilesModificationTime(t *testing.T) {
	v := setup(t)
	v.ok(alice, http.MethodPut, "/v1/soul", `{"markdown":"mine"}`)
	when := time.Date(2026, 3, 4, 5, 6, 7, 800_000_000, time.UTC)
	require.NoError(t, os.Chtimes(v.vaultFile(alice), when, when))
	got := v.ok(alice, http.MethodGet, "/v1/soul", "")
	require.NotNil(t, got.UpdatedAt)
	require.Equal(t, "2026-03-04T05:06:07Z", *got.UpdatedAt)
}

func TestDeleteResetsToTheDefaultTemplate(t *testing.T) {
	v := setup(t)
	v.ok(alice, http.MethodPut, "/v1/soul", `{"markdown":"# Custom"}`)
	r := v.do(alice, http.MethodDelete, "/v1/soul", "")
	require.Equal(t, http.StatusNoContent, r.code)
	require.Empty(t, r.body)
	got := v.ok(alice, http.MethodGet, "/v1/soul", "")
	require.Equal(t, soul.RenderDefault("alice", now), got.Markdown)
	require.NotNil(t, got.UpdatedAt)
	require.Len(t, v.unlocks.of(alice), 1, "reset unlocks nothing")
}

func TestPutTooLargeAfterCoreInjection(t *testing.T) {
	v := setup(t)
	body := strings.Repeat("x", soul.MaxSizeBytes)
	r := v.do(alice, http.MethodPut, "/v1/soul", `{"markdown":"`+body+`"}`)
	require.Equal(t, http.StatusRequestEntityTooLarge, r.code)
	enforced := len(soul.Inject(body))
	require.Equal(t, "SOUL.md too large after core covenant injection: "+strconv.Itoa(enforced)+" bytes > 65536", v.errorMessage(r))
	require.NoFileExists(t, v.vaultFile(alice))
	require.Empty(t, v.unlocks.of(alice))
}

func TestPutAnswers503WhenTheHermesProfilesDirectoryIsNotWritable(t *testing.T) {
	v := setup(t)
	require.NoError(t, os.MkdirAll(v.hermes, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(v.hermes, "profiles"), nil, 0o600))
	r := v.do(alice, http.MethodPut, "/v1/soul", `{"markdown":"mine"}`)
	require.Equal(t, http.StatusServiceUnavailable, r.code)
	require.True(t, strings.HasPrefix(v.errorMessage(r), "Hermes profiles directory not writable at "+filepath.Join(v.hermes, "profiles")+". could not create profiles directory: "))
	require.FileExists(t, v.vaultFile(alice), "the vault copy is written first, as in Swift")
	r = v.do(alice, http.MethodPost, "/v1/soul/compose", `{}`)
	require.Equal(t, http.StatusServiceUnavailable, r.code)
	r = v.do(alice, http.MethodDelete, "/v1/soul", "")
	require.Equal(t, http.StatusInternalServerError, r.code, "delete catches nothing")
	require.Empty(t, r.body)
	require.Empty(t, v.unlocks.of(alice))
}

func TestBYOHermesTenantCannotWrite(t *testing.T) {
	v := setup(t)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, "/v1/soul", `{"markdown":"mine"}`},
		{http.MethodPut, "/v1/soul", `not json`}, // checked before the body
		{http.MethodPost, "/v1/soul/compose", `{"agent_name":"X"}`},
		{http.MethodPost, "/v1/soul/compose", `{"dry_run":false}`},
		{http.MethodDelete, "/v1/soul", ""},
	} {
		r := v.do(bob, c.method, c.path, c.body)
		require.Equal(t, http.StatusConflict, r.code, c)
		require.Equal(t, "soul_unsupported_on_byo_hermes", v.errorMessage(r))
	}
	// Compose decodes first, and a dry run writes nothing, so both still answer.
	r := v.do(bob, http.MethodPost, "/v1/soul/compose", `{"tone":"loud"}`)
	require.Equal(t, http.StatusBadRequest, r.code)
	dry := v.ok(bob, http.MethodPost, "/v1/soul/compose", `{"agent_name":"X","dry_run":true}`)
	require.Contains(t, dry.Markdown, "I am X,")
	v.ok(bob, http.MethodGet, "/v1/soul", "")
	require.NoFileExists(t, v.vaultFile(bob))
	require.Empty(t, v.unlocks.of(bob))
}

func TestBodiesDecodeAsFoundationDoes(t *testing.T) {
	v := setup(t)
	for _, c := range []struct{ method, path, body, message string }{
		{http.MethodPut, "/v1/soul", ``, "The given data was not valid input: The given data was not valid JSON."},
		{http.MethodPut, "/v1/soul", `{}`, "Coding key `markdown` not found."},
		{http.MethodPut, "/v1/soul", `{"markdown":7}`, "Type mismatch for `markdown` key, expected `String` type."},
		{http.MethodPut, "/v1/soul", `[]`, "Type mismatch for `` key, expected `Dictionary<String, Any>` type."},
		{http.MethodPost, "/v1/soul/compose", `{"tone":"loud"}`, "The given data was not valid input: Cannot initialize SoulTone from invalid String value loud"},
		{http.MethodPost, "/v1/soul/compose", `{"role":1}`, "Type mismatch for `role` key, expected `String` type."},
		{http.MethodPost, "/v1/soul/compose", `{"priorities":["focus","nap"]}`, "The given data was not valid input: Cannot initialize SoulPriority from invalid String value nap"},
		{http.MethodPost, "/v1/soul/compose", `{"emojis":"yes"}`, "Type mismatch for `emojis` key, expected `Bool` type."},
		{http.MethodPost, "/v1/soul/compose", `{"voice_samples":"hi"}`, "Type mismatch for `voice_samples` key, expected `Array<Any>` type."},
	} {
		r := v.do(alice, c.method, c.path, c.body)
		require.Equal(t, http.StatusBadRequest, r.code, c)
		require.Equal(t, c.message, v.errorMessage(r), c)
	}
	// Every compose field is optional, and nulls read as absent.
	got := v.ok(alice, http.MethodPost, "/v1/soul/compose", `{"agent_name":null,"tone":null,"priorities":null,"dry_run":null}`)
	require.Equal(t, soul.RenderDefault("alice", now), got.Markdown)
}

func TestTenantIsolation(t *testing.T) {
	v := setup(t)
	v.ok(alice, http.MethodPut, "/v1/soul", `{"markdown":"alice only"}`)
	require.NotContains(t, v.ok(bob, http.MethodGet, "/v1/soul", "").Markdown, "alice only")
	require.Contains(t, v.ok(alice, http.MethodGet, "/v1/soul", "").Markdown, "alice only")
}

func TestRoutesRejectUnauthenticated(t *testing.T) {
	v := setup(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/soul"}, {http.MethodPut, "/v1/soul"}, {http.MethodPost, "/v1/soul/compose"}, {http.MethodDelete, "/v1/soul"},
	} {
		require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, c.method, c.path, `{}`).code, c)
	}
}

func TestRateLimitIsSoulByUser(t *testing.T) {
	v := setup(t)
	for i := range 30 {
		require.Equal(t, http.StatusOK, v.do(alice, http.MethodGet, "/v1/soul", "").code, i)
	}
	r := v.do(alice, http.MethodPost, "/v1/soul/compose", `{"dry_run":true}`)
	require.Equal(t, http.StatusTooManyRequests, r.code, "every route shares the bucket")
	require.Equal(t, http.StatusOK, v.do(bob, http.MethodGet, "/v1/soul", "").code, "users have their own bucket")
}

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)
	v := setup(t)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/soul", ""},
		{http.MethodPut, "/v1/soul", `{"markdown":"spec"}`},
		{http.MethodGet, "/v1/soul", ""},
		{http.MethodPost, "/v1/soul/compose", `{"agent_name":"Spec","priorities":["focus"],"voice_samples":["hi"],"dry_run":true}`},
		{http.MethodPost, "/v1/soul/compose", `{"agent_name":"Spec"}`},
		{http.MethodDelete, "/v1/soul", ""},
	} {
		r := v.do(alice, c.method, c.path, c.body)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: r.code, Header: http.Header{"Content-Type": {r.ct}}}
		out.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, r.body)
	}
}

// The body is Foundation's encoding: "/" escaped, "<" and ">" and "&" not.
func TestResponseEncodesAsFoundation(t *testing.T) {
	v := setup(t)
	r := v.do(alice, http.MethodPut, "/v1/soul", `{"markdown":"a/b <c> & d"}`)
	require.Equal(t, http.StatusOK, r.code)
	require.Contains(t, string(r.body), `a\/b <c> & d`)
	require.True(t, strings.HasPrefix(string(r.body), `{"markdown":"<!-- lv:core:v1:start -->`))
}
