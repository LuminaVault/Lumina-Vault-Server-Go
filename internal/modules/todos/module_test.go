package todos_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/diskguard"
	"lumina/internal/embedding"
	"lumina/internal/httpserver"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/modules/todos"
	"lumina/internal/vaultpaths"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var (
	tia     = uuid.MustParse("70D00000-0000-4000-8000-0000000000A1")
	tom     = uuid.MustParse("70D00000-0000-4000-8000-0000000000B2")
	project = "70D00000-0000-4000-8000-0000000001A1"
	archive = "70D00000-0000-4000-8000-0000000001A2"
	tomProj = "70D00000-0000-4000-8000-0000000001B2"
	later   = "70D00000-0000-4000-8000-000000000F01"
	sooner  = "70D00000-0000-4000-8000-000000000F02"
	note    = "70D00000-0000-4000-8000-000000000F07"
	nullMD  = "70D00000-0000-4000-8000-000000000F08"
	removed = "70D00000-0000-4000-8000-000000000F0A"
	tomTodo = "70D00000-0000-4000-8000-000000000F11"
	keys    = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

type env struct {
	t     *testing.T
	e     *echo.Echo
	pool  *pgxpool.Pool
	paths vaultpaths.Paths
}

type diskOption func(string) *diskguard.Guard

func setup(t *testing.T, disk diskOption) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, name := range []string{"base.sql", "todos.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + name)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, name)
	}
	root := t.TempDir()
	paths := vaultpaths.Paths{Root: root}
	opt := todos.Options{Memories: memorystore.New(pool, nil), Embeddings: embedding.New(pool, embedding.Config{}, nil)}
	if disk != nil {
		opt.Disk = disk(root)
	}
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{todos.New(pool, paths, opt)},
	})
	return env{t: t, e: e, pool: pool, paths: paths}
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
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()}
}

type dto struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Done      bool    `json:"done"`
	DueAt     *string `json:"dueAt"`
	ProjectID *string `json:"projectID"`
	CreatedAt string  `json:"createdAt"`
}

func decode[T any](t *testing.T, r resp) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(r.body, &out), string(r.body))
	return out
}

func (v env) list(user uuid.UUID, query string) []dto {
	v.t.Helper()
	r := v.do(user, http.MethodGet, "/v1/todos"+query, "")
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	return decode[struct {
		Todos []dto `json:"todos"`
	}](v.t, r).Todos
}

func titles(todos []dto) []string {
	out := []string{}
	for _, d := range todos {
		out = append(out, d.Title)
	}
	return out
}

func (v env) metadata(id string) map[string]any {
	v.t.Helper()
	var m map[string]any
	require.NoError(v.t, v.pool.QueryRow(context.Background(), `SELECT metadata FROM vault_files WHERE id = $1`, uuid.MustParse(id)).Scan(&m))
	return m
}

func (v env) count(query string, args ...any) int {
	v.t.Helper()
	var n int
	require.NoError(v.t, v.pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func TestCreateWritesFileRowAndMemory(t *testing.T) {
	v := setup(t, nil)
	r := v.do(tia, http.MethodPost, "/v1/todos", `{"title":" \n Call bank\t ","dueAt":"2026-12-01T10:00:00Z","projectID":"`+strings.ToLower(project)+`"}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	d := decode[dto](t, r)
	require.Equal(t, "Call bank", d.Title)
	require.False(t, d.Done)
	require.Equal(t, "2026-12-01T10:00:00Z", *d.DueAt)
	require.Equal(t, project, *d.ProjectID)
	require.Equal(t, strings.ToUpper(d.ID), d.ID)

	var path, contentType, sha string
	var size int64
	var processed *time.Time
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT path, content_type, size_bytes, sha256, processed_at FROM vault_files WHERE id = $1`, uuid.MustParse(d.ID)).
		Scan(&path, &contentType, &size, &sha, &processed))
	// QUIRK: the file name has its own UUID, not the row's.
	require.Regexp(t, regexp.MustCompile(`^inbox/[0-9A-F-]{36}\.md$`), path)
	require.NotContains(t, path, d.ID)
	require.Equal(t, "text/markdown", contentType)
	require.EqualValues(t, len("Call bank"), size)
	require.Len(t, sha, 64)
	require.NotNil(t, processed)
	data, err := os.ReadFile(filepath.Join(v.paths.Raw(tia), path))
	require.NoError(t, err)
	require.Equal(t, "Call bank", string(data))

	md := v.metadata(d.ID)
	require.Equal(t, true, md["isTodo"])
	require.Equal(t, false, md["done"])
	require.Equal(t, "Call bank", md["title"])
	require.Equal(t, project, md["projectID"])

	require.Equal(t, 1, v.count(`SELECT count(*) FROM memories WHERE tenant_id = $1 AND source_vault_file_id = $2 AND content = 'Call bank'`, tia, uuid.MustParse(d.ID)))
}

func TestCreateRefusals(t *testing.T) {
	v := setup(t, nil)
	for _, c := range []struct {
		name, body, message string
		code                int
	}{
		{"blank", `{"title":"  \n\t "}`, "todo title required", http.StatusBadRequest},
		{"empty", `{"title":""}`, "todo title required", http.StatusBadRequest},
		{"blank before project", `{"title":" ","projectID":"` + tomProj + `"}`, "todo title required", http.StatusBadRequest},
		{"unknown project", `{"title":"x","projectID":"70D00000-0000-4000-8000-00000000FFFF"}`, "unknown project", http.StatusBadRequest},
		{"another user's project", `{"title":"x","projectID":"` + tomProj + `"}`, "unknown project", http.StatusBadRequest},
	} {
		r := v.do(tia, http.MethodPost, "/v1/todos", c.body)
		require.Equal(t, c.code, r.code, c.name)
		require.Contains(t, string(r.body), c.message, c.name)
	}
	before := v.count(`SELECT count(*) FROM vault_files WHERE tenant_id = $1`, tia)
	for _, body := range []string{`{"title":`, ``, `[]`, `{}`, `{"title":null}`, `{"title":42}`, `{"title":"x","dueAt":"tomorrow"}`, `{"title":"x","projectID":"nope"}`} {
		r := v.do(tia, http.MethodPost, "/v1/todos", body)
		require.Equal(t, http.StatusBadRequest, r.code, body)
	}
	require.Equal(t, before, v.count(`SELECT count(*) FROM vault_files WHERE tenant_id = $1`, tia))
}

func TestArchivedProjectIsStillAProject(t *testing.T) {
	v := setup(t, nil)
	r := v.do(tia, http.MethodPost, "/v1/todos", `{"title":"x","projectID":"`+archive+`"}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
}

func TestAuthRequired(t *testing.T) {
	v := setup(t, nil)
	for _, c := range [][2]string{{http.MethodGet, "/v1/todos"}, {http.MethodPost, "/v1/todos"}, {http.MethodPatch, "/v1/todos/" + later}, {http.MethodDelete, "/v1/todos/" + later}} {
		require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, c[0], c[1], `{}`).code, c[1])
	}
}

func TestListOrderAndFilters(t *testing.T) {
	v := setup(t, nil)
	all := v.list(tia, "")
	require.NotEmpty(t, all)
	// Open before done, then by due date with no date last.
	seenDone := false
	for _, d := range all {
		if d.Done {
			seenDone = true
		}
		require.False(t, seenDone && !d.Done, "an open todo after a done one: %v", titles(all))
	}
	require.Equal(t, "Sooner", all[0].Title)
	require.Equal(t, "Later", all[1].Title)

	for _, d := range v.list(tia, "?done=true") {
		require.True(t, d.Done)
	}
	for _, d := range v.list(tia, "?done=false") {
		require.False(t, d.Done)
	}
	// Only the exact strings filter.
	for _, q := range []string{"?done=yes", "?done=TRUE", "?done=1", "?done=", "?projectID=nope", "?projectID="} {
		require.Len(t, v.list(tia, q), len(all), q)
	}
	for _, d := range v.list(tia, "?projectID="+strings.ToLower(project)) {
		require.Equal(t, project, *d.ProjectID)
	}
	require.Empty(t, v.list(tia, "?projectID="+tomProj))
	require.Empty(t, v.list(tia, "?projectID="+archive))
}

func TestTenantIsolation(t *testing.T) {
	v := setup(t, nil)
	require.Equal(t, http.StatusNotFound, v.do(tom, http.MethodPatch, "/v1/todos/"+sooner, `{"title":"hijack"}`).code)
	require.Equal(t, http.StatusNotFound, v.do(tom, http.MethodDelete, "/v1/todos/"+sooner, "").code)
	require.Equal(t, http.StatusBadRequest, v.do(tom, http.MethodPatch, "/v1/todos/"+tomTodo, `{"projectID":"`+project+`"}`).code)
	require.Equal(t, "Sooner", v.metadata(sooner)["title"])
	for _, d := range v.list(tom, "") {
		require.Equal(t, tomTodo, d.ID)
	}
}

func TestPatchSemantics(t *testing.T) {
	v := setup(t, nil)
	patch := func(id, body string) dto {
		t.Helper()
		r := v.do(tia, http.MethodPatch, "/v1/todos/"+id, body)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		return decode[dto](t, r)
	}
	require.True(t, patch(sooner, `{"done":true}`).Done)
	require.False(t, patch(sooner, `{"done":false}`).Done)
	// Not trimmed on patch, unlike create.
	require.Equal(t, "  Oat ", patch(sooner, `{"title":"  Oat "}`).Title)
	require.Equal(t, "2027-01-02T03:04:05Z", *patch(sooner, `{"dueAt":"2027-01-02T03:04:05Z"}`).DueAt)
	require.Equal(t, archive, *patch(sooner, `{"projectID":"`+archive+`"}`).ProjectID)
	// A due date or project can't be cleared: null means "unchanged".
	cleared := patch(sooner, `{"title":null,"done":null,"dueAt":null,"projectID":null}`)
	require.Equal(t, "  Oat ", cleared.Title)
	require.Equal(t, "2027-01-02T03:04:05Z", *cleared.DueAt)
	require.Equal(t, archive, *cleared.ProjectID)
	// An empty title reads back as the file's name.
	empty := patch(sooner, `{"title":""}`)
	require.NotEmpty(t, empty.Title)
	require.Equal(t, "", v.metadata(sooner)["title"])
}

func TestPatchPromotesAnyVaultFile(t *testing.T) {
	v := setup(t, nil)
	before := len(v.list(tia, ""))
	r := v.do(tia, http.MethodPatch, "/v1/todos/"+note, `{"done":true}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	md := v.metadata(note)
	require.Equal(t, true, md["isTodo"])
	require.Equal(t, true, md["done"])
	require.NotContains(t, md, "unknownKey", "a key VaultFileMetadata doesn't know is dropped on save")

	r = v.do(tia, http.MethodPatch, "/v1/todos/"+nullMD, `{}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	require.Equal(t, true, v.metadata(nullMD)["isTodo"])
	require.Len(t, v.list(tia, ""), before+2)
}

func TestPatchRefusalOrder(t *testing.T) {
	v := setup(t, nil)
	missing := "70D00000-0000-4000-8000-00000000FFFF"
	for _, c := range []struct {
		id, body, message string
		code              int
	}{
		{"nope", `{`, "invalid todo id", http.StatusBadRequest},
		{"70D00000000040008000000000000F01", `{}`, "invalid todo id", http.StatusBadRequest},
		{missing, `{"done":true}`, "todo not found", http.StatusNotFound},
		{missing, `{"projectID":"` + missing + `"}`, "todo not found", http.StatusNotFound},
		{sooner, `{"projectID":"` + missing + `"}`, "unknown project", http.StatusBadRequest},
	} {
		r := v.do(tia, http.MethodPatch, "/v1/todos/"+c.id, c.body)
		require.Equal(t, c.code, r.code, c.id+" "+c.body)
		require.Contains(t, string(r.body), c.message)
	}
	for _, body := range []string{`{`, ``, `[]`, `{"title":1}`, `{"done":"yes"}`, `{"dueAt":false}`, `{"dueAt":"soon"}`, `{"projectID":[]}`} {
		require.Equal(t, http.StatusBadRequest, v.do(tia, http.MethodPatch, "/v1/todos/"+sooner, body).code, body)
	}
}

func TestDeleteSoftDeletesFileAndCascadesMemory(t *testing.T) {
	v := setup(t, nil)
	created := decode[dto](t, v.do(tia, http.MethodPost, "/v1/todos", `{"title":"Gone"}`))
	var path string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT path FROM vault_files WHERE id = $1`, uuid.MustParse(created.ID)).Scan(&path))
	raw := v.paths.Raw(tia)

	r := v.do(tia, http.MethodDelete, "/v1/todos/"+created.ID, "")
	require.Equal(t, http.StatusNoContent, r.code)
	require.Empty(t, r.body)
	require.NoFileExists(t, filepath.Join(raw, path))
	trashed := []string{}
	require.NoError(t, filepath.WalkDir(raw, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(p, "_deleted_") {
			trashed = append(trashed, p)
		}
		return err
	}))
	require.Len(t, trashed, 1, "the file is renamed, not removed")
	require.Equal(t, 0, v.count(`SELECT count(*) FROM vault_files WHERE id = $1`, uuid.MustParse(created.ID)))
	require.Equal(t, 0, v.count(`SELECT count(*) FROM memories WHERE source_vault_file_id = $1`, uuid.MustParse(created.ID)))

	require.Equal(t, http.StatusNotFound, v.do(tia, http.MethodDelete, "/v1/todos/"+created.ID, "").code)
	require.Equal(t, http.StatusBadRequest, v.do(tia, http.MethodDelete, "/v1/todos/nope", "").code)
}

// Delete takes any vault file, and one whose file is missing still goes.
func TestDeleteRowWithoutFile(t *testing.T) {
	v := setup(t, nil)
	require.Equal(t, 1, v.count(`SELECT count(*) FROM memories WHERE source_vault_file_id = $1`, uuid.MustParse(removed)))
	require.Equal(t, http.StatusNoContent, v.do(tia, http.MethodDelete, "/v1/todos/"+removed, "").code)
	require.Equal(t, 0, v.count(`SELECT count(*) FROM vault_files WHERE id = $1`, uuid.MustParse(removed)))
	require.Equal(t, 0, v.count(`SELECT count(*) FROM memories WHERE source_vault_file_id = $1`, uuid.MustParse(removed)))
}

// DiskSpaceGuardHTTPTests, the todo half: 507 with the coded envelope, nothing written.
func TestFullDiskRefusesCreate(t *testing.T) {
	v := setup(t, func(root string) *diskguard.Guard {
		return diskguard.New(root, 1<<62, 0, diskguard.Options{})
	})
	before := v.count(`SELECT count(*) FROM vault_files WHERE tenant_id = $1`, tia)
	r := v.do(tia, http.MethodPost, "/v1/todos", `{"title":"buy disks"}`)
	require.Equal(t, http.StatusInsufficientStorage, r.code, string(r.body))
	body := decode[struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}](t, r)
	require.Equal(t, "insufficient_storage", body.Error.Code)
	require.Equal(t, before, v.count(`SELECT count(*) FROM vault_files WHERE tenant_id = $1`, tia))
	require.NoDirExists(t, filepath.Join(v.paths.Raw(tia), "inbox"))

	// Validation comes first, and what writes no file isn't refused.
	require.Equal(t, http.StatusBadRequest, v.do(tia, http.MethodPost, "/v1/todos", `{"title":" "}`).code)
	require.Equal(t, http.StatusOK, v.do(tia, http.MethodPatch, "/v1/todos/"+sooner, `{"done":true}`).code)
}

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t, nil)
	created := decode[dto](t, v.do(tia, http.MethodPost, "/v1/todos", `{"title":"Spec"}`))
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/todos", ""},
		{http.MethodGet, "/v1/todos?done=true&projectID=" + project, ""},
		{http.MethodPost, "/v1/todos", `{"title":"Spec2","dueAt":"2026-12-01T10:00:00Z","projectID":"` + project + `"}`},
		{http.MethodPatch, "/v1/todos/" + created.ID, `{"done":true}`},
		{http.MethodDelete, "/v1/todos/" + created.ID, ""},
	} {
		r := v.do(tia, c.method, c.path, c.body)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: r.code, Header: http.Header{"Content-Type": {r.ct}},
		}
		out.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, r.body)
	}
}
