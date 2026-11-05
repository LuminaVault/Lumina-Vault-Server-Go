package capture_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"lumina/internal/achievementsink"
	"lumina/internal/auth"
	linkcapture "lumina/internal/capture"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/diskguard"
	"lumina/internal/embedding"
	"lumina/internal/entitlement"
	"lumina/internal/httpserver"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/modules/achievements"
	"lumina/internal/modules/capture"
	"lumina/internal/onboardlatch"
	"lumina/internal/posthog"
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
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var (
	amy      = uuid.MustParse("CA97E000-0000-4000-8000-0000000000A1")
	ben      = uuid.MustParse("CA97E000-0000-4000-8000-0000000000B2")
	hal      = uuid.MustParse("CA97E000-0000-4000-8000-0000000000D4")
	amySpace = "CA97E000-0000-4000-8000-0000000005A1"
	keys     = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

// stubEnricher stands in for the network: URLEnricherGuard refuses the loopback
// address a test server would have.
type stubEnricher struct {
	metadata linkcapture.EnrichedMetadata
	err      error
}

func (stubEnricher) CanHandle(*linkcapture.URL) bool { return true }
func (s stubEnricher) Enrich(_ context.Context, u *linkcapture.URL) (linkcapture.EnrichedMetadata, error) {
	m := s.metadata
	m.URL = u.Absolute
	return m, s.err
}

type stubRecorder struct {
	mu     sync.Mutex
	events []achievements.Event
}

func (r *stubRecorder) Record(_ context.Context, _ uuid.UUID, e achievements.Event) ([]achievements.Sub, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil, nil
}

type options struct {
	enricher  stubEnricher
	jina      *linkcapture.JinaEnricher
	disk      func(root string) *diskguard.Guard
	gate      func(*pgxpool.Pool) echo.MiddlewareFunc
	analytics *posthog.Client
	sink      *achievementsink.Sink
}

type env struct {
	t     *testing.T
	e     *echo.Echo
	pool  *pgxpool.Pool
	paths vaultpaths.Paths
	// pending holds spawned enrichment until a test runs it.
	pending []func()
}

func setup(t *testing.T, o options) *env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, name := range []string{"base.sql", "capture.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + name)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, name)
	}
	root := t.TempDir()
	paths := vaultpaths.Paths{Root: root}
	store := vaultfiles.NewStore(pool, paths)
	v := &env{t: t, pool: pool, paths: paths}
	co := linkcapture.Options{
		Paths: paths, Store: store, Achievements: o.sink, Latches: onboardlatch.New(pool),
		Spawn: func(f func()) { v.pending = append(v.pending, f) },
		Enrichment: linkcapture.NewEnrichmentService(linkcapture.EnrichmentOptions{
			Paths: paths, Store: store, Jina: o.jina,
			Enrichers:  []linkcapture.Enricher{o.enricher},
			Embeddings: embedding.New(pool, embedding.Config{}, nil), Memories: memorystore.New(pool, nil),
			Hooks: &linkcapture.HookDispatcher{
				Registry:  linkcapture.NewHookRegistry(linkcapture.ReadingTimeHook{}),
				Installed: linkcapture.InstalledSlugsFrom(pool),
			},
		}),
	}
	if o.disk != nil {
		co.Disk = o.disk(root)
	}
	gate := func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	if o.gate != nil {
		gate = o.gate(pool)
	}
	v.e = httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{capture.New(pool, linkcapture.New(pool, co), gate, ratelimit.NewStore(), o.analytics)},
	})
	return v
}

type resp struct {
	code int
	ct   string
	body []byte
}

func (v *env) post(user uuid.UUID, body string) resp {
	v.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/capture/safari", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()}
}

type accepted struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Status string `json:"status"`
}

func (v *env) capture(user uuid.UUID, body string) accepted {
	v.t.Helper()
	r := v.post(user, body)
	require.Equal(v.t, http.StatusAccepted, r.code, string(r.body))
	var a accepted
	require.NoError(v.t, json.Unmarshal(r.body, &a))
	return a
}

func (v *env) runEnrichment() {
	for _, f := range v.pending {
		f()
	}
	v.pending = nil
}

func (v *env) enrichmentStatus(id string) string {
	v.t.Helper()
	var s string
	require.NoError(v.t, v.pool.QueryRow(context.Background(), `SELECT metadata->>'enrichmentStatus' FROM vault_files WHERE id = $1`, uuid.MustParse(id)).Scan(&s))
	return s
}

func (v *env) count(query string, args ...any) int {
	v.t.Helper()
	var n int
	require.NoError(v.t, v.pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func (v *env) file(user uuid.UUID, relative string) string {
	v.t.Helper()
	data, err := os.ReadFile(filepath.Join(v.paths.Raw(user), relative))
	require.NoError(v.t, err)
	return string(data)
}

func article() stubEnricher {
	title, description := "Parity", "An article."
	return stubEnricher{metadata: linkcapture.EnrichedMetadata{Title: &title, Description: &description}}
}

func TestCapturePlaceholderThenEnrichment(t *testing.T) {
	v := setup(t, options{enricher: article()})
	a := v.capture(amy, `{"url":"https://example.com/post?x=1","notes":"my note"}`)
	require.Equal(t, "accepted", a.Status)
	require.Equal(t, strings.ToUpper(a.ID), a.ID)
	require.Regexp(t, `^inbox/\d{4}-\d{2}-\d{2}-\d{6}-example-com-[0-9a-f]{8}\.md$`, a.Path)

	// The answer comes before enrichment, with the placeholder on disk.
	require.Equal(t, "pending", v.enrichmentStatus(a.ID))
	require.Equal(t, "---\nsource: \"https://example.com/post?x=1\"\n---\n\n# [Pending Enrichment]\nURL: https://example.com/post?x=1\n\n## Note\nmy note\n", v.file(amy, a.Path))

	v.runEnrichment()
	require.Equal(t, "done", v.enrichmentStatus(a.ID))
	got := v.file(amy, a.Path)
	require.Equal(t, "---\ntitle: \"Parity\"\nsource: \"https://example.com/post?x=1\"\n---\n\n# Parity\n\n## Description\nAn article.\n\n", got)
	require.NotContains(t, got, "my note", "QUIRK: enrichment drops the note")

	var size int64
	var content string
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT f.size_bytes, m.content FROM vault_files f JOIN memories m ON m.source_vault_file_id = f.id WHERE f.id = $1`,
		uuid.MustParse(a.ID)).Scan(&size, &content))
	require.EqualValues(t, len(got), size)
	require.Equal(t, "Parity\n\nAn article.", content)
}

func TestCaptureFiledIntoSpace(t *testing.T) {
	v := setup(t, options{enricher: article()})
	a := v.capture(amy, `{"url":"https://example.com/","space_id":"`+strings.ToLower(amySpace)+`"}`)
	require.True(t, strings.HasPrefix(a.Path, "reading/"), a.Path)
	require.Equal(t, 1, v.count(`SELECT count(*) FROM vault_files WHERE id = $1 AND space_id = $2`, uuid.MustParse(a.ID), uuid.MustParse(amySpace)))
	v.runEnrichment()
	require.Equal(t, 1, v.count(`SELECT count(*) FROM memories WHERE source_vault_file_id = $1 AND space_id = $2`, uuid.MustParse(a.ID), uuid.MustParse(amySpace)))

	// The key is snake_case: spaceId decodes as nothing, and the link lands unfiled.
	b := v.capture(amy, `{"url":"https://example.com/","spaceId":"`+amySpace+`"}`)
	require.True(t, strings.HasPrefix(b.Path, "inbox/"), b.Path)
}

func TestRefusals(t *testing.T) {
	v := setup(t, options{enricher: article()})
	unknown := "CA97E000-0000-4000-8000-00000000FFFF"
	for _, c := range []struct{ body, message string }{
		{`{"url":""}`, "Invalid URL"},
		{`{"url":"https://example.com "}`, "Invalid URL"},
		{`{"url":"http://[::1"}`, "Invalid URL"},
		{`{"url":"not a url"}`, "URL host is not enrichable"},
		{`{"url":"example.com/path"}`, "URL host is not enrichable"},
		{`{"url":"ftp://example.com/"}`, "URL host is not enrichable"},
		{`{"url":"http://localhost/"}`, "URL host is not enrichable"},
		{`{"url":"http://10.0.0.1/"}`, "URL host is not enrichable"},
		{`{"url":"https://fdroid.org/"}`, "URL host is not enrichable"},
		{`{"url":"http://localhost/","space_id":"` + unknown + `"}`, "URL host is not enrichable"},
		{`{"url":"https://example.com/","space_id":"` + unknown + `"}`, "does not belong to the caller"},
	} {
		r := v.post(amy, c.body)
		require.Equal(t, http.StatusBadRequest, r.code, c.body)
		require.Contains(t, string(r.body), c.message, c.body)
	}
	r := v.post(ben, `{"url":"https://example.com/","space_id":"`+amySpace+`"}`)
	require.Equal(t, http.StatusBadRequest, r.code)
	require.Contains(t, string(r.body), "does not belong to the caller")
	for _, body := range []string{`{"url":`, ``, `[]`, `{}`, `{"url":null}`, `{"url":42}`, `{"url":"https://example.com/","notes":7}`, `{"url":"https://example.com/","space_id":"nope"}`} {
		require.Equal(t, http.StatusBadRequest, v.post(amy, body).code, body)
	}
	require.Equal(t, http.StatusUnauthorized, v.post(uuid.Nil, `{"url":"https://example.com/"}`).code)
	require.Equal(t, 0, v.count(`SELECT count(*) FROM vault_files WHERE tenant_id = ANY($1)`, []uuid.UUID{amy, ben}))
	require.Empty(t, v.pending)
}

// A failing enricher answers nothing different: the row ends "failed" with the
// placeholder still on disk.
func TestEnrichmentFailureMarksRow(t *testing.T) {
	v := setup(t, options{enricher: stubEnricher{err: errors.New("boom")}})
	a := v.capture(amy, `{"url":"https://example.com/","notes":"kept"}`)
	before := v.file(amy, a.Path)
	v.runEnrichment()
	require.Equal(t, "failed", v.enrichmentStatus(a.ID))
	require.Equal(t, before, v.file(amy, a.Path))
	require.Equal(t, 0, v.count(`SELECT count(*) FROM memories WHERE source_vault_file_id = $1`, uuid.MustParse(a.ID)))
}

func TestEmptyMetadataMakesNoMemory(t *testing.T) {
	v := setup(t, options{enricher: stubEnricher{}})
	a := v.capture(amy, `{"url":"https://example.com/"}`)
	v.runEnrichment()
	require.Equal(t, "done", v.enrichmentStatus(a.ID))
	require.Equal(t, "---\nsource: \"https://example.com/\"\n---\n\n", v.file(amy, a.Path))
	require.Equal(t, 0, v.count(`SELECT count(*) FROM memories WHERE source_vault_file_id = $1`, uuid.MustParse(a.ID)))
}

// A second enrichment of the same file updates its memory.
func TestEnrichmentUpdatesExistingMemory(t *testing.T) {
	ctx := context.Background()
	v := setup(t, options{enricher: article()})
	a := v.capture(amy, `{"url":"https://example.com/"}`)
	v.runEnrichment()
	id := uuid.MustParse(a.ID)
	_, err := v.pool.Exec(ctx, `UPDATE memories SET content = 'old' WHERE source_vault_file_id = $1`, id)
	require.NoError(t, err)

	store := vaultfiles.NewStore(v.pool, v.paths)
	title := "Second"
	s := linkcapture.NewEnrichmentService(linkcapture.EnrichmentOptions{
		Paths: v.paths, Store: store, Embeddings: embedding.New(v.pool, embedding.Config{}, nil), Memories: memorystore.New(v.pool, nil),
		Enrichers: []linkcapture.Enricher{stubEnricher{metadata: linkcapture.EnrichedMetadata{Title: &title}}},
	})
	s.EnrichAndRewrite(ctx, id, "https://example.com/", amy)
	require.Equal(t, 1, v.count(`SELECT count(*) FROM memories WHERE source_vault_file_id = $1 AND content = 'Second'`, id))
	require.Equal(t, 1, v.count(`SELECT count(*) FROM memories WHERE source_vault_file_id = $1`, id))
}

// hal has reading-time installed. ben has it disabled, and amy has no install.
func TestReadingTimeHookFollowsInstalls(t *testing.T) {
	body := strings.TrimSpace(strings.Repeat("lorem ", 400))
	v := setup(t, options{enricher: stubEnricher{metadata: linkcapture.EnrichedMetadata{Description: &body}}})
	hooked := v.capture(hal, `{"url":"https://example.com/"}`)
	disabled := v.capture(ben, `{"url":"https://example.com/"}`)
	none := v.capture(amy, `{"url":"https://example.com/"}`)
	v.runEnrichment()
	require.Contains(t, v.file(hal, hooked.Path), "\nreading_time: 2\n")
	require.NotContains(t, v.file(ben, disabled.Path), "reading_time")
	require.NotContains(t, v.file(amy, none.Path), "reading_time")
}

func TestJinaRunsOnlyWhenShallow(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte("Full body"))
	}))
	defer srv.Close()
	jina := &linkcapture.JinaEnricher{BaseURL: srv.URL + "/"}

	v := setup(t, options{enricher: article(), jina: jina})
	a := v.capture(amy, `{"url":"https://example.com/"}`)
	v.runEnrichment()
	require.Equal(t, 1, calls)
	require.Contains(t, v.file(amy, a.Path), "## Content\nFull body\n")
	var content string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT content FROM memories WHERE source_vault_file_id = $1`, uuid.MustParse(a.ID)).Scan(&content))
	require.Equal(t, "Parity\n\nAn article.\n\nFull body", content)

	long := strings.Repeat("x", 500)
	deep := setup(t, options{enricher: stubEnricher{metadata: linkcapture.EnrichedMetadata{Description: &long}}, jina: jina})
	deep.capture(amy, `{"url":"https://example.com/"}`)
	deep.runEnrichment()
	require.Equal(t, 1, calls, "500 characters is deep enough")
}

func TestJinaFailureKeepsPrimary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer srv.Close()
	v := setup(t, options{enricher: article(), jina: &linkcapture.JinaEnricher{BaseURL: srv.URL + "/"}})
	a := v.capture(amy, `{"url":"https://example.com/"}`)
	v.runEnrichment()
	require.Equal(t, "done", v.enrichmentStatus(a.ID))
	require.NotContains(t, v.file(amy, a.Path), "## Content")
}

// The decoded host goes in the file name: a %2F makes folders, and dots become
// dashes, so nothing climbs out. No SanitizePath, as in Swift.
func TestEncodedSlashInHostMakesFolders(t *testing.T) {
	v := setup(t, options{enricher: article()})
	a := v.capture(amy, `{"url":"http://a%2Fb.example.com/x"}`)
	require.Regexp(t, `^inbox/\d{4}-\d{2}-\d{2}-\d{6}-a/b-example-com-[0-9a-f]{8}\.md$`, a.Path)
	b := v.capture(amy, `{"url":"http://..%2F..%2Fup.example.com/x"}`)
	require.NotContains(t, b.Path, "..")
	require.NotEmpty(t, v.file(amy, b.Path))
}

// Swift CaptureController.swift: name, uppercase distinct ID, has_space, and only
// after a capture landed.
func TestLinkCapturedEvent(t *testing.T) {
	var mu sync.Mutex
	events := []map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Batch []map[string]any `json:"batch"`
		}
		if json.Unmarshal(b, &body) == nil {
			mu.Lock()
			events = append(events, body.Batch...)
			mu.Unlock()
		}
	}))
	defer srv.Close()
	client := posthog.New("phc_stub", srv.URL, posthog.Options{})
	v := setup(t, options{enricher: article(), analytics: client})

	require.Equal(t, http.StatusBadRequest, v.post(amy, `{"url":"http://localhost/"}`).code)
	v.capture(amy, `{"url":"https://example.com/"}`)
	v.capture(amy, `{"url":"https://example.com/","space_id":"`+amySpace+`"}`)
	client.Flush(context.Background())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 2)
	require.Equal(t, "link_captured", events[0]["event"])
	require.Equal(t, "CA97E000-0000-4000-8000-0000000000A1", events[0]["distinct_id"])
	require.Equal(t, map[string]any{"has_space": false, "$lib": "luminavault-server"}, events[0]["properties"])
	require.Equal(t, map[string]any{"has_space": true, "$lib": "luminavault-server"}, events[1]["properties"])
}

func TestLatchAndAchievementOnCapture(t *testing.T) {
	rec := &stubRecorder{}
	sink := achievementsink.New(rec, nil, achievementsink.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sink.Run(ctx)
	}()
	v := setup(t, options{enricher: article(), sink: sink})

	require.Equal(t, http.StatusBadRequest, v.post(amy, `{"url":"http://localhost/"}`).code)
	require.Equal(t, 0, v.count(`SELECT count(*) FROM onboarding_state WHERE tenant_id = $1 AND first_capture_completed`, amy))
	v.capture(amy, `{"url":"https://example.com/"}`)
	require.Equal(t, 1, v.count(`SELECT count(*) FROM onboarding_state WHERE tenant_id = $1 AND first_capture_completed`, amy))

	cancel()
	<-done
	require.Equal(t, []achievements.Event{achievements.EventVaultUploaded}, rec.events)
}

// DiskSpaceGuardHTTPTests, the capture half: 507 with the coded envelope, nothing
// written, and no enrichment started.
func TestFullDiskRefusesCapture(t *testing.T) {
	v := setup(t, options{enricher: article(), disk: func(root string) *diskguard.Guard {
		return diskguard.New(root, 1<<62, 0, diskguard.Options{})
	}})
	r := v.post(amy, `{"url":"https://example.com/article"}`)
	require.Equal(t, http.StatusInsufficientStorage, r.code, string(r.body))
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(r.body, &body))
	require.Equal(t, "insufficient_storage", body.Error.Code)
	require.Equal(t, 0, v.count(`SELECT count(*) FROM vault_files WHERE tenant_id = $1`, amy))
	require.NoDirExists(t, filepath.Join(v.paths.Raw(amy), "inbox"))
	require.Empty(t, v.pending)
	require.Equal(t, 0, v.count(`SELECT count(*) FROM onboarding_state WHERE tenant_id = $1 AND first_capture_completed`, amy))

	// The URL's checks come first.
	require.Equal(t, http.StatusBadRequest, v.post(amy, `{"url":"http://localhost/"}`).code)
}

// jwt, then entitlement, then the rate limit: a refused caller spends no budget.
func TestGateAndRateLimitOrder(t *testing.T) {
	v := setup(t, options{enricher: article(), gate: func(pool *pgxpool.Pool) echo.MiddlewareFunc {
		return entitlement.Middleware(pool, entitlement.Capture, true, nil)
	}})
	_, err := v.pool.Exec(context.Background(), `UPDATE users SET tier = 'archived' WHERE id = $1`, ben)
	require.NoError(t, err)
	for range 70 {
		r := v.post(ben, `{"url":"http://localhost/"}`)
		require.Equal(t, http.StatusPaymentRequired, r.code, string(r.body))
	}
	_, err = v.pool.Exec(context.Background(), `UPDATE users SET tier = 'trial' WHERE id = $1`, ben)
	require.NoError(t, err)
	for i := range 60 {
		require.Equal(t, http.StatusBadRequest, v.post(ben, `{"url":"http://localhost/"}`).code, i)
	}
	require.Equal(t, http.StatusTooManyRequests, v.post(ben, `{"url":"http://localhost/"}`).code)
}

// api/openapi.yaml says 200 where Swift answers 202 and Swift wins, so the body is
// validated against the spec's 200 schema.
func TestResponseConformsToSpec(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t, options{enricher: article()})
	body := `{"url":"https://example.com/","notes":"n","space_id":"` + amySpace + `"}`
	r := v.post(amy, body)
	require.Equal(t, http.StatusAccepted, r.code)
	req := httptest.NewRequest(http.MethodPost, "/v1/capture/safari", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	route, params, err := router.FindRoute(req)
	require.NoError(t, err)
	in := &openapi3filter.RequestValidationInput{
		Request: req, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}
	out := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: in, Status: http.StatusOK, Header: http.Header{"Content-Type": {r.ct}},
	}
	out.SetBodyBytes(r.body)
	require.NoError(t, openapi3filter.ValidateResponse(ctx, out), string(r.body))
}
