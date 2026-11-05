package vault

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"lumina/internal/achievementsink"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/diskguard"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/achievements"
	"lumina/internal/modules/spaces"
	"lumina/internal/modules/vault/db"
	"lumina/internal/onboardlatch"
	"lumina/internal/posthog"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

var testKeys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	e      *echo.Echo
	module *Module
	root   string
	hermes string
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	return pool
}

func setup(t *testing.T, change ...func(*Options)) env {
	t.Helper()
	pool := testPool(t)
	root, hermes := t.TempDir(), t.TempDir()
	identify := auth.Identify(testKeys, auth.PGUsers(pool))
	paths := vaultpaths.Paths{Root: root}
	opts := Options{
		Paths: paths, HermesDataRoot: hermes, Identify: identify, Limits: ratelimit.NewStore(),
		Spaces: spaces.New(pool, paths, identify), Embeddings: stubEmbedder{},
		Latches: onboardlatch.New(pool),
	}
	for _, f := range change {
		f(&opts)
	}
	module := New(pool, opts)
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(testKeys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{module},
	})
	return env{t: t, pool: pool, e: e, module: module, root: root, hermes: hermes}
}

type stubEmbedder struct{}

func (stubEmbedder) Embed(context.Context, string, uuid.UUID) ([]float32, error) {
	return make([]float32, 1536), nil
}

// user registers a new account and its personal vault.
func (v env) user(tier string) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	ctx := context.Background()
	_, err := v.pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
		VALUES ($1, $2, 'x', true, $3, $4, now(), now())`, id, id.String()+"@test", "u"+strings.ReplaceAll(id.String(), "-", "")[:16], tier)
	require.NoError(v.t, err)
	_, err = v.pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
		VALUES ($1, $1, 'v', now(), now())`, id)
	require.NoError(v.t, err)
	return id
}

type response struct {
	code    int
	headers http.Header
	body    []byte
}

// do sends a request with the raw target as sent, so escapes reach the router.
func (v env) do(user uuid.UUID, method, target string, headers map[string]string, body []byte) response {
	v.t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.RequestURI = target
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	if user != uuid.Nil {
		token, err := scenario.Mint(testKeys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return response{rec.Code, rec.Header(), rec.Body.Bytes()}
}

func (v env) upload(user uuid.UUID, path, body string) string {
	v.t.Helper()
	r := v.do(user, http.MethodPost, "/v1/vault/files?path="+url.QueryEscape(path), map[string]string{"Content-Type": "text/markdown"}, []byte(body))
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var out VaultUploadResponse
	require.NoError(v.t, json.Unmarshal(r.body, &out))
	return out.Path
}

func (v env) list(user uuid.UUID, query string) fileListResponse {
	v.t.Helper()
	r := v.do(user, http.MethodGet, "/v1/vault/files"+query, nil, nil)
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var out fileListResponse
	require.NoError(v.t, json.Unmarshal(r.body, &out))
	return out
}

func (v env) space(tenant uuid.UUID, slug string) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at)
		VALUES ($1, $2, $3, $3, 0, now(), now())`, id, tenant, slug)
	require.NoError(v.t, err)
	return id
}

func (v env) move(user uuid.UUID, from, to string) response {
	v.t.Helper()
	body, err := json.Marshal(moveRequest{Path: from, NewPath: to})
	require.NoError(v.t, err)
	return v.do(user, http.MethodPost, "/v1/vault/files/move", map[string]string{"Content-Type": "application/json"}, body)
}

func findFile(files []fileDTO, path string) *fileDTO {
	for i := range files {
		if files[i].Path == path {
			return &files[i]
		}
	}
	return nil
}

func TestVaultRoutes(t *testing.T) {
	v := setup(t)

	// VaultCRUDTests
	t.Run("upload then list", func(t *testing.T) {
		u := v.user("trial")
		a, b := v.upload(u, "a.md", "# a"), v.upload(u, "b.md", "# b")
		require.Equal(t, "inbox/a.md", a)
		require.Equal(t, "inbox/b.md", b)
		list := v.list(u, "")
		require.NotNil(t, findFile(list.Files, a))
		require.NotNil(t, findFile(list.Files, b))
		require.Len(t, list.Files, 2)
	})

	t.Run("list respects limit and next-before", func(t *testing.T) {
		u := v.user("trial")
		for i := range 3 {
			v.upload(u, fmt.Sprintf("n%d.md", i), "x")
		}
		list := v.list(u, "?limit=2")
		require.Len(t, list.Files, 2)
		require.Equal(t, 2, list.Limit)
		require.NotNil(t, list.NextBefore)
	})

	t.Run("delete, only your own and only once", func(t *testing.T) {
		u, other := v.user("trial"), v.user("trial")
		path := v.upload(u, "gone.md", "x")
		target := "/v1/vault/files/" + path
		require.Equal(t, http.StatusNotFound, v.do(other, http.MethodDelete, target, nil, nil).code)
		require.Equal(t, http.StatusNoContent, v.do(u, http.MethodDelete, target, nil, nil).code)
		require.Equal(t, http.StatusNotFound, v.do(u, http.MethodDelete, target, nil, nil).code)
		// The file is kept, renamed.
		entries, err := os.ReadDir(filepath.Join(v.module.paths.Raw(u), "inbox"))
		require.NoError(t, err)
		require.Empty(t, entries)
		entries, err = os.ReadDir(v.module.paths.Raw(u))
		require.NoError(t, err)
		deleted := 0
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "_deleted_") {
				deleted++
			}
		}
		require.Equal(t, 1, deleted)
	})

	t.Run("move renames", func(t *testing.T) {
		u := v.user("trial")
		old := v.upload(u, "old.md", "x")
		r := v.move(u, old, "new.md")
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var dto fileDTO
		require.NoError(t, json.Unmarshal(r.body, &dto))
		require.Equal(t, "new.md", dto.Path)
		list := v.list(u, "")
		require.NotNil(t, findFile(list.Files, "new.md"))
		require.Nil(t, findFile(list.Files, old))
	})

	t.Run("move onto an existing path conflicts", func(t *testing.T) {
		u := v.user("trial")
		a, b := v.upload(u, "a.md", "a"), v.upload(u, "b.md", "b")
		require.Equal(t, http.StatusConflict, v.move(u, a, b).code)
	})

	t.Run("move to the same path is refused", func(t *testing.T) {
		u := v.user("trial")
		a := v.upload(u, "a.md", "a")
		require.Equal(t, http.StatusBadRequest, v.move(u, a, a).code)
	})

	t.Run("move refuses traversal", func(t *testing.T) {
		u := v.user("trial")
		a := v.upload(u, "a.md", "a")
		require.Equal(t, http.StatusBadRequest, v.move(u, a, "../../escape.md").code)
	})

	t.Run("another tenant lists nothing of yours", func(t *testing.T) {
		alice, bob := v.user("trial"), v.user("trial")
		v.upload(alice, "alice-only.md", "x")
		require.Nil(t, findFile(v.list(bob, "").Files, "inbox/alice-only.md"))
	})

	t.Run("unknown space filter is 404", func(t *testing.T) {
		u := v.user("trial")
		require.Equal(t, http.StatusNotFound, v.do(u, http.MethodGet, "/v1/vault/files?space=does-not-exist", nil, nil).code)
	})

	t.Run("no token is 401", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, "/v1/vault/files", nil, nil).code)
		require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodDelete, "/v1/vault/files/foo.md", nil, nil).code)
		require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodPost, "/v1/vault/files/move",
			map[string]string{"Content-Type": "application/json"}, []byte(`{"path":"a","newPath":"b"}`)).code)
	})

	heic := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c'}
	t.Run("heic uploads as image/heic", func(t *testing.T) {
		u := v.user("trial")
		r := v.do(u, http.MethodPost, "/v1/vault/files?path=captures/sample.heic", map[string]string{"Content-Type": "image/heic"}, heic)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
	})
	t.Run("heic uploads as image/heif", func(t *testing.T) {
		u := v.user("trial")
		r := v.do(u, http.MethodPost, "/v1/vault/files?path=captures/alias.heic", map[string]string{"Content-Type": "image/heif"}, heic)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
	})
	t.Run("heic with a mismatched content type is 415", func(t *testing.T) {
		u := v.user("trial")
		r := v.do(u, http.MethodPost, "/v1/vault/files?path=captures/mismatch.heic", map[string]string{"Content-Type": "image/png"}, heic)
		require.Equal(t, http.StatusUnsupportedMediaType, r.code, string(r.body))
	})

	// VaultUploadSpaceTests
	t.Run("a space id files under its slug", func(t *testing.T) {
		u := v.user("trial")
		space := v.space(u, "papers")
		r := v.do(u, http.MethodPost, "/v1/vault/files?path=capture/note.md&space_id="+space.String(), map[string]string{"Content-Type": "text/markdown"}, []byte("x"))
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		row := findFile(v.list(u, "").Files, "papers/note.md")
		require.NotNil(t, row)
		require.Equal(t, space, row.SpaceID.UUID)
	})
	t.Run("no space id is inbox, whatever the prefix", func(t *testing.T) {
		u := v.user("trial")
		v.upload(u, "unfiled/note.md", "x")
		files := v.list(u, "").Files
		row := findFile(files, "inbox/note.md")
		require.NotNil(t, row)
		require.Nil(t, row.SpaceID)
		require.Nil(t, findFile(files, "unfiled/note.md"))
	})
	t.Run("malformed space id is 400", func(t *testing.T) {
		u := v.user("trial")
		r := v.do(u, http.MethodPost, "/v1/vault/files?path=a.md&space_id=nope", map[string]string{"Content-Type": "text/markdown"}, []byte("x"))
		require.Equal(t, http.StatusBadRequest, r.code)
	})
	t.Run("a foreign space id is 400", func(t *testing.T) {
		u, other := v.user("trial"), v.user("trial")
		foreign := v.space(other, "theirs")
		r := v.do(u, http.MethodPost, "/v1/vault/files?path=a.md&space_id="+foreign.String(), map[string]string{"Content-Type": "text/markdown"}, []byte("x"))
		require.Equal(t, http.StatusBadRequest, r.code)
	})
	t.Run("one name in two spaces is two rows", func(t *testing.T) {
		u := v.user("trial")
		a, b := v.space(u, "aa"), v.space(u, "bb")
		for _, s := range []uuid.UUID{a, b} {
			r := v.do(u, http.MethodPost, "/v1/vault/files?path=shared/path.md&space_id="+s.String(), map[string]string{"Content-Type": "text/markdown"}, []byte("x"))
			require.Equal(t, http.StatusOK, r.code)
		}
		files := v.list(u, "").Files
		require.Equal(t, a, findFile(files, "aa/path.md").SpaceID.UUID)
		require.Equal(t, b, findFile(files, "bb/path.md").SpaceID.UUID)
		require.Nil(t, findFile(files, "shared/path.md"))
	})
	t.Run("one name filed and unfiled is two rows", func(t *testing.T) {
		u := v.user("trial")
		space := v.space(u, "filed")
		r := v.do(u, http.MethodPost, "/v1/vault/files?path=path.md&space_id="+space.String(), map[string]string{"Content-Type": "text/markdown"}, []byte("x"))
		require.Equal(t, http.StatusOK, r.code)
		v.upload(u, "path.md", "y")
		files := v.list(u, "").Files
		require.Equal(t, space, findFile(files, "filed/path.md").SpaceID.UUID)
		require.Nil(t, findFile(files, "inbox/path.md").SpaceID)
	})

	// VaultLargePayloadTests
	t.Run("a multi-megabyte upload reads back whole", func(t *testing.T) {
		u := v.user("trial")
		payload := bytes.Repeat([]byte("0123456789abcdef"), 512*1024) // 8 MiB
		r := v.do(u, http.MethodPost, "/v1/vault/files?path=big.md", map[string]string{"Content-Type": "text/markdown"}, payload)
		require.Equal(t, http.StatusOK, r.code)
		var out VaultUploadResponse
		require.NoError(t, json.Unmarshal(r.body, &out))
		require.Equal(t, len(payload), out.Size)
		read := v.do(u, http.MethodGet, "/v1/vault/files/"+out.Path, nil, nil)
		require.Equal(t, http.StatusOK, read.code)
		require.Equal(t, fmt.Sprint(len(payload)), read.headers.Get("Content-Length"))
		require.True(t, bytes.Equal(payload, read.body))
	})
	t.Run("a note upload creates its memory", func(t *testing.T) {
		u := v.user("trial")
		content := "note memory " + uuid.NewString()
		r := v.do(u, http.MethodPost, "/v1/vault/files?path=notes/queued.md&note=true", map[string]string{"Content-Type": "text/markdown"}, []byte(content))
		require.Equal(t, http.StatusOK, r.code)
		var out VaultUploadResponse
		require.NoError(t, json.Unmarshal(r.body, &out))
		require.NotNil(t, out.MemoryId)
		var stored string
		require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT content FROM memories WHERE tenant_id = $1`, u).Scan(&stored))
		require.Equal(t, content, stored)
	})

	t.Run("exactly the limit is stored and one byte more is a bare 413", func(t *testing.T) {
		small := setupOn(t, v, func(o *Options) { o.MaxFileSize = 8 })
		u := v.user("trial")
		ok := small.do(u, http.MethodPost, "/v1/vault/files?path=at.md", map[string]string{"Content-Type": "text/markdown"}, []byte("12345678"))
		require.Equal(t, http.StatusOK, ok.code)
		over := small.do(u, http.MethodPost, "/v1/vault/files?path=over.md", map[string]string{"Content-Type": "text/markdown"}, []byte("123456789"))
		require.Equal(t, http.StatusRequestEntityTooLarge, over.code)
		require.Empty(t, over.body)
		require.Empty(t, over.headers.Get("Content-Type"))
	})

	// Hummingbird's router never decodes (brief open item): the raw catch-all, not Echo's.
	t.Run("a catch-all path keeps its percent escapes", func(t *testing.T) {
		u := v.user("trial")
		path := v.upload(u, "café.md", "cafe")
		require.Equal(t, "inbox/café.md", path)
		escaped := v.do(u, http.MethodGet, "/v1/vault/files/inbox/caf%C3%A9.md", nil, nil)
		require.Equal(t, http.StatusBadRequest, escaped.code, string(escaped.body))
		require.Contains(t, string(escaped.body), "illegal characters")
		require.Equal(t, "inbox/a%20b.md", catchAll(contextFor("/v1/vault/files/inbox//a%20b.md/?x=1")))
	})
	t.Run("a path called move is 404 for GET and DELETE, with no auth", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound, v.do(uuid.Nil, http.MethodGet, "/v1/vault/files/move", nil, nil).code)
		require.Equal(t, http.StatusNotFound, v.do(uuid.Nil, http.MethodDelete, "/v1/vault/files/move", nil, nil).code)
	})
	t.Run("a path id is not decoded", func(t *testing.T) {
		u := v.user("trial")
		r := v.do(u, http.MethodGet, "/v1/vault/links/"+strings.Replace(uuid.NewString(), "-", "%2D", 1), nil, nil)
		require.Equal(t, http.StatusBadRequest, r.code)
	})

	// The brief's open item: over 32 MiB with a key the middleware passes on the
	// consumed stream, so the handler has no body.
	t.Run("over 32 MiB with an idempotency key is an empty body", func(t *testing.T) {
		u := v.user("trial")
		payload := make([]byte, 32*1024*1024+1024)
		r := v.do(u, http.MethodPost, "/v1/vault/files?path=huge.md", map[string]string{"Content-Type": "text/markdown", "Idempotency-Key": uuid.NewString()}, payload)
		require.Equal(t, http.StatusBadRequest, r.code)
		require.Contains(t, string(r.body), "empty body")
	})

	// GuidedStartTests' first-capture, at this call site.
	t.Run("an upload latches first capture", func(t *testing.T) {
		u := v.user("trial")
		_, err := v.pool.Exec(context.Background(), `INSERT INTO onboarding_state (id, tenant_id, created_at, updated_at) VALUES ($1, $1, now(), now())`, u)
		require.NoError(t, err)
		v.upload(u, "first.md", "x")
		var done bool
		require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT first_capture_completed FROM onboarding_state WHERE tenant_id = $1`, u).Scan(&done))
		require.True(t, done)
	})

	t.Run("a team viewer can read and not write", func(t *testing.T) {
		editor, viewer, outsider := v.user("trial"), v.user("trial"), v.user("trial")
		team := v.teamVault(editor, viewer)
		h := map[string]string{"Content-Type": "text/markdown", "X-Vault-ID": team.String()}
		up := v.do(editor, http.MethodPost, "/v1/vault/files?path=t.md", h, []byte("team"))
		require.Equal(t, http.StatusOK, up.code, string(up.body))
		require.FileExists(t, filepath.Join(v.module.paths.Raw(team), "inbox/t.md"))

		// The vault owns the row and the editor is its actor.
		var tenant, actor uuid.UUID
		require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT tenant_id, created_by_user_id FROM vault_files WHERE tenant_id = $1`, team).Scan(&tenant, &actor))
		require.Equal(t, team, tenant)
		require.Equal(t, editor, actor)

		read := map[string]string{"X-Vault-ID": team.String()}
		require.Equal(t, http.StatusOK, v.do(viewer, http.MethodGet, "/v1/vault/files/inbox/t.md", read, nil).code)
		require.Equal(t, http.StatusForbidden, v.do(viewer, http.MethodDelete, "/v1/vault/files/inbox/t.md", read, nil).code)
		require.Equal(t, http.StatusForbidden, v.do(viewer, http.MethodPost, "/v1/vault/files?path=v.md", h, []byte("x")).code)
		require.NotEqual(t, http.StatusOK, v.do(outsider, http.MethodGet, "/v1/vault/files", read, nil).code)
	})
}

func (v env) teamVault(editor, viewer uuid.UUID) uuid.UUID {
	v.t.Helper()
	ctx := context.Background()
	team, vault := uuid.New(), uuid.New()
	_, err := v.pool.Exec(ctx, `INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at) VALUES ($1, 't', $2, $2, now(), now())`, team, editor)
	require.NoError(v.t, err)
	_, err = v.pool.Exec(ctx, `INSERT INTO vaults (id, team_id, name, created_at, updated_at) VALUES ($1, $2, 't', now(), now())`, vault, team)
	require.NoError(v.t, err)
	for user, role := range map[uuid.UUID]string{editor: "editor", viewer: "viewer"} {
		_, err = v.pool.Exec(ctx, `INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4, false, $3, now(), now())`, uuid.New(), vault, user, role)
		require.NoError(v.t, err)
	}
	return vault
}

// setupOn builds another module on v's database, for options that change routing.
func setupOn(t *testing.T, v env, change func(*Options)) env {
	t.Helper()
	identify := auth.Identify(testKeys, auth.PGUsers(v.pool))
	paths := vaultpaths.Paths{Root: t.TempDir()}
	opts := Options{Paths: paths, HermesDataRoot: t.TempDir(), Identify: identify, Limits: ratelimit.NewStore(), Spaces: spaces.New(v.pool, paths, identify)}
	change(&opts)
	module := New(v.pool, opts)
	e := httpserver.New(config.Config{}, httpserver.Deps{Modules: []httpserver.Module{module}})
	return env{t: t, pool: v.pool, e: e, module: module, root: paths.Root, hermes: opts.HermesDataRoot}
}

func contextFor(target string) *echo.Context {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RequestURI = target
	return echo.New().NewContext(req, httptest.NewRecorder())
}

// VaultInitTests
func TestVaultInit(t *testing.T) {
	v := setup(t)
	status := func(u uuid.UUID, method, path string) VaultStatusResponse {
		r := v.do(u, method, path, nil, nil)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var out VaultStatusResponse
		require.NoError(t, json.Unmarshal(r.body, &out))
		return out
	}

	t.Run("status is false after register", func(t *testing.T) {
		out := status(v.user("trial"), http.MethodGet, "/v1/vault/status")
		require.False(t, out.Initialized)
		require.Empty(t, out.DefaultSpaceSlugs)
		require.Nil(t, out.CreatedAt)
	})

	t.Run("create seeds the default spaces, and again changes nothing", func(t *testing.T) {
		u := v.user("trial")
		first := status(u, http.MethodPost, "/v1/vault/create")
		require.True(t, first.Initialized)
		require.Equal(t, spaces.DefaultSlugs(), first.DefaultSpaceSlugs)
		require.NotNil(t, first.CreatedAt)

		var slugs []string
		rows, err := v.pool.Query(context.Background(), `SELECT slug FROM spaces WHERE tenant_id = $1`, u)
		require.NoError(t, err)
		for rows.Next() {
			var s string
			require.NoError(t, rows.Scan(&s))
			slugs = append(slugs, s)
		}
		require.ElementsMatch(t, spaces.DefaultSlugs(), slugs)

		second := status(u, http.MethodPost, "/v1/vault/create")
		require.True(t, second.Initialized)
		require.Equal(t, first.CreatedAt.UTC(), second.CreatedAt.UTC())
	})

	t.Run("status is true after create", func(t *testing.T) {
		u := v.user("trial")
		status(u, http.MethodPost, "/v1/vault/create")
		out := status(u, http.MethodGet, "/v1/vault/status")
		require.True(t, out.Initialized)
		require.Len(t, out.DefaultSpaceSlugs, len(spaces.DefaultSlugs()))
	})

	t.Run("create writes SOUL.md to the vault and the Hermes profile", func(t *testing.T) {
		u := v.user("trial")
		status(u, http.MethodPost, "/v1/vault/create")
		var username string
		require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT username FROM users WHERE id = $1`, u).Scan(&username))
		inVault, err := os.ReadFile(filepath.Join(v.module.paths.Raw(u), "SOUL.md"))
		require.NoError(t, err)
		inHermes, err := os.ReadFile(filepath.Join(v.hermes, "profiles", username, "SOUL.md"))
		require.NoError(t, err)
		require.Equal(t, string(inVault), string(inHermes))
		require.Contains(t, string(inVault), "username: "+username)
		require.Contains(t, string(inVault), "<!-- lv:core:v1:start -->")
		require.True(t, strings.HasSuffix(string(inVault), "free-form notes the user wrote about themselves. -->"))

		entries, err := os.ReadDir(filepath.Join(v.hermes, "profiles"))
		require.NoError(t, err)
		for _, e := range entries {
			require.False(t, strings.HasPrefix(e.Name(), ".write-probe-"))
		}
	})

	t.Run("an existing SOUL.md is kept", func(t *testing.T) {
		u := v.user("trial")
		raw := v.module.paths.Raw(u)
		require.NoError(t, os.MkdirAll(raw, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(raw, "SOUL.md"), []byte("mine"), 0o600))
		status(u, http.MethodPost, "/v1/vault/create")
		got, err := os.ReadFile(filepath.Join(raw, "SOUL.md"))
		require.NoError(t, err)
		require.Equal(t, "mine", string(got))
	})

	t.Run("an unwritable Hermes root is 503 and create can run again", func(t *testing.T) {
		blocked := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(blocked, nil, 0o600))
		broken := setupOn(t, v, func(o *Options) { o.HermesDataRoot = blocked })
		u := v.user("trial")
		r := broken.do(u, http.MethodPost, "/v1/vault/create", nil, nil)
		require.Equal(t, http.StatusServiceUnavailable, r.code)
		require.Contains(t, string(r.body), "could not initialize SOUL.md")
		var done bool
		require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT vault_initialized FROM users WHERE id = $1`, u).Scan(&done))
		require.False(t, done)
	})
}

// TenantVectorIndexServiceTests. Swift's fourth, dropIndex, has no caller in Swift
// and isn't ported.
func TestTenantVectorIndex(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	exists := func(name string) bool {
		var n int
		require.NoError(t, v.pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = $1`, name).Scan(&n))
		return n == 1
	}
	t.Run("ensureIndex creates a partial HNSW index", func(t *testing.T) {
		tenant := uuid.New()
		require.NoError(t, v.module.ensureVectorIndex(ctx, tenant))
		require.True(t, exists(indexName(tenant)))
		var def string
		require.NoError(t, v.pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = $1`, indexName(tenant)).Scan(&def))
		require.Contains(t, def, "hnsw")
		require.Contains(t, def, "m='16'")
		require.Contains(t, def, "ef_construction='64'")
		require.Contains(t, def, tenant.String())
	})
	t.Run("ensureIndex is idempotent", func(t *testing.T) {
		tenant := uuid.New()
		require.NoError(t, v.module.ensureVectorIndex(ctx, tenant))
		require.NoError(t, v.module.ensureVectorIndex(ctx, tenant))
		require.True(t, exists(indexName(tenant)))
	})
	t.Run("indexName is stable and within the identifier limit", func(t *testing.T) {
		name := indexName(uuid.MustParse("11111111-2222-3333-4444-555566667777"))
		require.Equal(t, "idx_memories_emb_t_11111111222233334444555566667777", name)
		require.LessOrEqual(t, len(name), 63)
	})
	t.Run("create makes the index", func(t *testing.T) {
		u := v.user("trial")
		require.Equal(t, http.StatusOK, v.do(u, http.MethodPost, "/v1/vault/create", nil, nil).code)
		require.True(t, exists(indexName(u)))
	})
}

func zipNames(t *testing.T, body []byte) map[string]string {
	t.Helper()
	require.GreaterOrEqual(t, len(body), 30)
	require.Equal(t, []byte{0x50, 0x4B, 0x03, 0x04}, body[:4])
	r, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)
	out := map[string]string{}
	for _, f := range r.File {
		rc, err := f.Open()
		require.NoError(t, err, f.Name)
		content, err := io.ReadAll(rc)
		require.NoError(t, err, f.Name) // checks the CRC
		require.NoError(t, rc.Close())
		out[f.Name] = string(content)
	}
	return out
}

// VaultExportTests
func TestVaultExport(t *testing.T) {
	v := setup(t)
	exportOf := func(u uuid.UUID, query string) response {
		r := v.do(u, http.MethodGet, "/v1/vault/export"+query, nil, nil)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		require.Equal(t, "application/zip", r.headers.Get("Content-Type"))
		return r
	}

	t.Run("an empty vault still has SOUL.md and memories.json", func(t *testing.T) {
		u := v.user("trial")
		r := exportOf(u, "")
		require.Equal(t, `attachment; filename="vault-`+strings.ToUpper(u.String())+`.zip"`, r.headers.Get("Content-Disposition"))
		names := zipNames(t, r.body)
		require.Contains(t, names["SOUL.md"], "# SOUL\n")
		require.Equal(t, "[\n]\n", names["memories.json"])
	})

	t.Run("it has uploaded files under raw/ and not deleted ones", func(t *testing.T) {
		u := v.user("trial")
		a, b := v.upload(u, "notes/a.md", "# a"), v.upload(u, "notes/b.md", "# b")
		gone := v.upload(u, "gone.md", "gone")
		require.Equal(t, http.StatusNoContent, v.do(u, http.MethodDelete, "/v1/vault/files/"+gone, nil, nil).code)
		names := zipNames(t, exportOf(u, "").body)
		require.Equal(t, "# a", names["raw/"+a])
		require.Equal(t, "# b", names["raw/"+b])
		for name := range names {
			require.NotContains(t, name, "_deleted_")
		}
	})

	t.Run("since excludes older files", func(t *testing.T) {
		u := v.user("trial")
		path := v.upload(u, "notes/old.md", "# old")
		future := url.QueryEscape(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		names := zipNames(t, exportOf(u, "?since="+future).body)
		require.NotContains(t, names, "raw/"+path)
		require.Contains(t, names, "SOUL.md")
	})

	t.Run("the soul on disk is at the root only", func(t *testing.T) {
		u := v.user("pro")
		require.Equal(t, http.StatusOK, v.do(u, http.MethodPost, "/v1/vault/create", nil, nil).code)
		names := zipNames(t, exportOf(u, "").body)
		require.Contains(t, names["SOUL.md"], "# SOUL.md")
		require.NotContains(t, names, "raw/SOUL.md")
	})

	t.Run("memories page past one page and keep their order", func(t *testing.T) {
		u := v.user("pro")
		ctx := context.Background()
		for i := range exportMemoryPageSize + 1 {
			_, err := v.pool.Exec(ctx, `INSERT INTO memories (id, tenant_id, content, created_at) VALUES ($1, $2, $3, $4)`,
				uuid.New(), u, fmt.Sprintf("m%d", i), time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC))
			require.NoError(t, err)
		}
		var rows []map[string]any
		require.NoError(t, json.Unmarshal([]byte(zipNames(t, exportOf(u, "").body)["memories.json"]), &rows))
		require.Len(t, rows, exportMemoryPageSize+1)
	})

	t.Run("free gets one export and trial three", func(t *testing.T) {
		free, trial := v.user("free"), v.user("trial")
		exportOf(free, "")
		require.Equal(t, http.StatusTooManyRequests, v.do(free, http.MethodGet, "/v1/vault/export", nil, nil).code)
		for range 3 {
			exportOf(trial, "")
		}
		require.Equal(t, http.StatusTooManyRequests, v.do(trial, http.MethodGet, "/v1/vault/export", nil, nil).code)
	})

	t.Run("no token is 401, not an archive", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, "/v1/vault/export", nil, nil).code)
	})
}

func TestEncodeExportMemory(t *testing.T) {
	id := uuid.MustParse("a11ce000-0000-4000-8000-000000000001")
	at := time.Date(2026, 1, 2, 3, 4, 5, 750_000_000, time.UTC)
	require.Equal(t,
		"{\"content\":\"a\\\"b\\\\c/d\\n\\t\\r\\b\\f\\u0001  café <&>\",\"createdAt\":\"2026-01-02T03:04:05Z\",\"id\":\"A11CE000-0000-4000-8000-000000000001\",\"tags\":[\"x\",\"y/z\"]}",
		encodeExportMemory(db.ExportMemoriesRow{ID: id, Content: "a\"b\\c/d\n\t\r\b\f\x01  café <&>", CreatedAt: &at, Tags: []string{"x", "y/z"}}))
	require.Equal(t, `{"content":"","id":"A11CE000-0000-4000-8000-000000000001"}`, encodeExportMemory(db.ExportMemoriesRow{ID: id}))
	require.Equal(t, `{"content":"","id":"A11CE000-0000-4000-8000-000000000001","tags":[]}`, encodeExportMemory(db.ExportMemoriesRow{ID: id, Tags: []string{}}))
}

func TestZipFormat(t *testing.T) {
	t.Run("an empty stored entry claims a descriptor only in its local header", func(t *testing.T) {
		var b bytes.Buffer
		z := &zipStream{w: &b}
		require.NoError(t, z.storedEntry("e", nil, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
		require.Equal(t, uint16(zipFlagDescriptor), binary.LittleEndian.Uint16(b.Bytes()[6:]))
		require.Zero(t, binary.LittleEndian.Uint16(z.entries[0].centralHeader()[8:]))
	})
	t.Run("a stored entry with content has no descriptor", func(t *testing.T) {
		var b bytes.Buffer
		z := &zipStream{w: &b}
		require.NoError(t, z.storedEntry("e", []byte("x"), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
		require.Zero(t, binary.LittleEndian.Uint16(b.Bytes()[6:]))
	})
	t.Run("DOS time is clamped to the format's years", func(t *testing.T) {
		_, date := dosDateTime(time.Date(1970, 6, 15, 12, 0, 0, 0, time.Local))
		require.Equal(t, 0, int(date>>9))
		_, date = dosDateTime(time.Date(2200, 6, 15, 12, 0, 0, 0, time.Local))
		require.Equal(t, 127, int(date>>9))
	})
	t.Run("too many entries fail the export instead of trapping", func(t *testing.T) {
		z := &zipStream{w: io.Discard, entries: make([]zipEntry, maxZipEntries+1)}
		require.Error(t, z.finish())
	})
	t.Run("hidden files, symlinks, deleted files and SOUL.md stay out", func(t *testing.T) {
		root := t.TempDir()
		for _, f := range []string{"a.md", "sub/b.md", ".hidden", ".dir/c.md", "_deleted_1_d.md", "SOUL.md", "sub/SOUL.md"} {
			require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(root, f), []byte("x"), 0o600))
		}
		require.NoError(t, os.Symlink(filepath.Join(root, "a.md"), filepath.Join(root, "link.md")))
		files, err := enumerateRawFiles(root, nil)
		require.NoError(t, err)
		names := []string{}
		for _, f := range files {
			names = append(names, f.relativePath)
		}
		require.Equal(t, []string{"a.md", "sub/SOUL.md", "sub/b.md"}, names)
	})
}

// DiskSpaceGuardTests, vault's half of its route case: 4B and 4E keep theirs.
func TestFullDiskRefusesUpload(t *testing.T) {
	full := diskguard.New("/", 1<<60, 0, diskguard.Options{Provider: func(string) (diskguard.Space, error) {
		return diskguard.Space{FreeBytes: 1, TotalBytes: 1 << 40}, nil
	}})
	v := setup(t, func(o *Options) { o.Guard = full })
	u := v.user("trial")
	r := v.do(u, http.MethodPost, "/v1/vault/files?path=inbox/full-disk.md", map[string]string{"Content-Type": "text/markdown"}, []byte("# would not fit"))
	require.Equal(t, http.StatusInsufficientStorage, r.code)
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(r.body, &body))
	require.Equal(t, "insufficient_storage", body.Error.Code)
	var rows int
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM vault_files WHERE tenant_id = $1`, u).Scan(&rows))
	require.Zero(t, rows)
	require.NoFileExists(t, filepath.Join(v.module.paths.Raw(u), "inbox/full-disk.md"))
}

func TestAnalyticsAndAchievements(t *testing.T) {
	var mu sync.Mutex
	events := []map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Batch []map[string]any `json:"batch"`
		}
		if b, err := io.ReadAll(r.Body); err == nil && json.Unmarshal(b, &body) == nil {
			mu.Lock()
			events = append(events, body.Batch...)
			mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	client := posthog.New("phc_stub", srv.URL, posthog.Options{})

	var v env
	v = setup(t, func(o *Options) {
		o.Analytics = client
	})
	sink := achievementsink.New(achievements.NewRecorder(v.pool), nil, achievementsink.Options{})
	v = setupOn(t, v, func(o *Options) {
		o.Analytics = client
		o.Sink = sink
		o.Embeddings = stubEmbedder{}
		o.Latches = onboardlatch.New(v.pool)
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sink.Run(ctx) }()

	// setupOn serves without Auth, which the module doesn't use.
	u := v.user("trial")
	space := v.space(u, "papers")
	r := v.do(u, http.MethodPost, "/v1/vault/files?path=n.md&note=true&space_id="+space.String(), map[string]string{"Content-Type": "text/markdown"}, []byte("x"))
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	require.Equal(t, http.StatusNoContent, v.do(u, http.MethodDelete, "/v1/vault/files/papers/n.md", nil, nil).code)

	client.Flush(context.Background())
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 2)
	upper := strings.ToUpper(u.String())
	require.Equal(t, "vault_file_uploaded", events[0]["event"])
	require.Equal(t, upper, events[0]["distinct_id"])
	props := events[0]["properties"].(map[string]any)
	require.Equal(t, "text/markdown", props["content_type"])
	require.Equal(t, true, props["is_note"])
	require.Equal(t, true, props["has_space"])
	require.Equal(t, "vault_file_deleted", events[1]["event"])
	require.Equal(t, upper, events[1]["distinct_id"])

	var progress int64
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM achievement_progress WHERE tenant_id = $1 AND progress_count > 0`, u).Scan(&progress))
	require.Positive(t, progress)
}

// indexFixtures is one document graph: hub links to leaf (resolved), nowhere
// (twice) and hub (ambiguous), and leaf links back.
func (v env) indexFixtures(u uuid.UUID) (hub, leaf uuid.UUID) {
	v.t.Helper()
	ctx := context.Background()
	hub, leaf = uuid.New(), uuid.New()
	mem := uuid.New()
	exec := func(sql string, args ...any) {
		_, err := v.pool.Exec(ctx, sql, args...)
		require.NoError(v.t, err)
	}
	space := v.space(u, "wiki")
	file := `INSERT INTO vault_files (id, tenant_id, space_id, path, content_type, size_bytes, sha256, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'text/markdown', 1, 's', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`
	exec(file, hub, u, space, "wiki/hub.md")
	exec(file, leaf, u, space, "wiki/leaf.md")
	exec(`INSERT INTO memories (id, tenant_id, content, created_at, source_vault_file_id) VALUES ($1, $2, 'hub', now(), $3)`, mem, u, hub)
	exec(`INSERT INTO memory_chunks (chunk_id, document_id, tenant_id, memory_id, vault_file_id, source_path, ordinal, heading_path, start_line, end_line, text, content_sha256, created_at)
		VALUES ($1, 'd', $2, $3, $4, 'wiki/hub.md', 0, '["Hub","Sub"]', 3, 4, 't', 'c', '2026-02-01T00:00:00Z')`, uuid.NewString(), u, mem, hub)
	link := `INSERT INTO vault_links (tenant_id, source_vault_file_id, source_path, source_line, raw_target, target_slug, target_vault_file_id, resolved, resolution_state)
		VALUES ($1, $2, $3, $4, $5, $5, $6, $7, $8)`
	exec(link, u, hub, "wiki/hub.md", 2, "leaf", leaf, true, "resolved")
	exec(link, u, hub, "wiki/hub.md", 3, "nowhere", nil, false, "unresolved")
	exec(link, u, hub, "wiki/hub.md", 4, "nowhere", nil, false, "unresolved")
	exec(link, u, hub, "wiki/hub.md", 5, "hub", nil, false, "ambiguous")
	exec(link, u, leaf, "wiki/leaf.md", 1, "hub", hub, true, "resolved")
	return hub, leaf
}

func TestIndexRoutes(t *testing.T) {
	v := setup(t)

	t.Run("nothing to report", func(t *testing.T) {
		u := v.user("trial")
		var status VaultIndexStatus
		r := v.do(u, http.MethodGet, "/v1/vault/index", nil, nil)
		require.Equal(t, http.StatusOK, r.code)
		require.NoError(t, json.Unmarshal(r.body, &status))
		require.False(t, status.Stale)
		require.Empty(t, status.StaleReasons)
		require.Nil(t, status.LastIndexedAt)
		require.Contains(t, string(r.body), `"unindexedSample":[]`)
	})

	t.Run("index counts", func(t *testing.T) {
		u := v.user("trial")
		v.indexFixtures(u)
		var status VaultIndexStatus
		require.NoError(t, json.Unmarshal(v.do(u, http.MethodGet, "/v1/vault/index", nil, nil).body, &status))
		require.Equal(t, 2, status.DocumentCount)
		require.Equal(t, 1, status.ChunkCount)
		require.Equal(t, 2, status.ResolvedLinkCount)
		require.Equal(t, 2, status.UnresolvedLinkCount)
		require.Equal(t, 1, status.AmbiguousLinkCount)
		require.Equal(t, 1, status.UnindexedDocumentCount)
		require.Equal(t, []string{"wiki/leaf.md"}, status.UnindexedSample)
		require.True(t, status.Stale)
		require.Equal(t, []string{
			"1 document(s) have no chunks — run the chunk backfill",
			"1 wiki link(s) match more than one document",
		}, status.StaleReasons)
	})

	t.Run("context", func(t *testing.T) {
		u := v.user("trial")
		hub, leaf := v.indexFixtures(u)
		r := v.do(u, http.MethodGet, "/v1/vault/context/"+hub.String(), nil, nil)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var p VaultContextPacket
		require.NoError(t, json.Unmarshal(r.body, &p))
		require.Equal(t, "wiki/hub.md", p.Path)
		require.Equal(t, 1, p.ChunkCount)
		require.Equal(t, []string{"Hub", "Sub"}, p.Headings[0].Path)
		require.Equal(t, 3, p.Headings[0].StartLine)
		require.Equal(t, leaf, p.Siblings[0].VaultFileID.UUID)
		require.Equal(t, leaf, p.LinksTo[0].VaultFileID.UUID)
		require.Equal(t, leaf, p.LinkedFrom[0].VaultFileID.UUID)
		require.Equal(t, []string{"hub", "nowhere"}, p.DanglingLinks)
	})

	t.Run("links", func(t *testing.T) {
		u := v.user("trial")
		hub, _ := v.indexFixtures(u)
		r := v.do(u, http.MethodGet, "/v1/vault/links/"+hub.String(), nil, nil)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var out linksResponse
		require.NoError(t, json.Unmarshal(r.body, &out))
		require.Equal(t, "wiki/hub.md", out.Path)
		require.Len(t, out.Outgoing, 4)
		require.Len(t, out.Incoming, 1)
	})

	t.Run("bad and foreign ids", func(t *testing.T) {
		u, other := v.user("trial"), v.user("trial")
		hub, _ := v.indexFixtures(u)
		for _, route := range []string{"/v1/vault/context/", "/v1/vault/links/"} {
			bad := v.do(u, http.MethodGet, route+"nope", nil, nil)
			require.Equal(t, http.StatusBadRequest, bad.code)
			require.Contains(t, string(bad.body), "invalid vaultFileID")
			foreign := v.do(other, http.MethodGet, route+hub.String(), nil, nil)
			require.Equal(t, http.StatusNotFound, foreign.code)
			require.Contains(t, string(foreign.body), "no vault file with that id")
			require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, route+hub.String(), nil, nil).code)
		}
	})

	t.Run("lint", func(t *testing.T) {
		u := v.user("trial")
		ctx := context.Background()
		v.indexFixtures(u)
		_, err := v.pool.Exec(ctx, `INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at) VALUES
			($1, $2, '../escape.md', 'text/markdown', 1, 'e', now(), now()),
			($3, $2, 'inbox/big.md', 'text/markdown', 6291456, 'b', now(), now())`, uuid.New(), u, uuid.New())
		require.NoError(t, err)
		v.upload(u, "blank.md", "  \n")

		r := v.do(u, http.MethodGet, "/v1/vault/lint", nil, nil)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var report VaultLintReport
		require.NoError(t, json.Unmarshal(r.body, &report))
		require.True(t, report.HasFindings)
		byName := map[VaultLintCheckName]VaultLintCheck{}
		names := []VaultLintCheckName{}
		for _, check := range report.Checks {
			byName[check.Name] = check
			names = append(names, check.Name)
		}
		require.Equal(t, []VaultLintCheckName{BrokenLinks, AmbiguousLinks, DuplicateSlugs, Orphans, StaleIndex, OversizedFiles, EmptyDocuments, UnreadableFiles}, names)
		require.Equal(t, Warning, byName[BrokenLinks].Severity)
		require.Equal(t, "2 link(s) point at a document that does not exist", byName[BrokenLinks].Message)
		require.Equal(t, "unresolved link [[nowhere]]", byName[BrokenLinks].Findings[0].Message)
		require.Equal(t, Warning, byName[AmbiguousLinks].Severity)
		require.Equal(t, Error, byName[StaleIndex].Severity)
		require.Equal(t, Warning, byName[OversizedFiles].Severity)
		require.Equal(t, "6 MiB — chunking and embedding this is slow and expensive", byName[OversizedFiles].Findings[0].Message)
		require.Equal(t, Warning, byName[EmptyDocuments].Severity)
		require.Equal(t, Error, byName[UnreadableFiles].Severity)
		messages := map[string]bool{}
		for _, finding := range byName[UnreadableFiles].Findings {
			messages[finding.Message] = true
		}
		require.True(t, messages["path escapes the vault boundary"])
		require.True(t, messages["missing, unreadable, or not valid UTF-8"])
	})

	t.Run("lint of an empty vault passes every check", func(t *testing.T) {
		u := v.user("trial")
		var report VaultLintReport
		require.NoError(t, json.Unmarshal(v.do(u, http.MethodGet, "/v1/vault/lint", nil, nil).body, &report))
		require.False(t, report.HasFindings)
		require.Equal(t, 8, report.Passed)
	})
}

func TestDecodeUTF8(t *testing.T) {
	got, ok := decodeUTF8([]byte("\xef\xbb\xbf\xef\xbb\xbfx"))
	require.True(t, ok)
	require.Equal(t, "\ufeffx", got)
	_, ok = decodeUTF8([]byte{0xff, 0xff})
	require.False(t, ok)
}

func TestRawPathHelpers(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x/y", nil)
	req.RequestURI = "/v1/vault/files//a%20b.md/?q=1"
	require.Equal(t, "/v1/vault/files//a%20b.md/", wire.RawPath(req))
	require.Equal(t, []string{"v1", "vault", "files", "a%20b.md"}, wire.PathSegments(req))
}

// Bodies the spec describes validate against it (decisions.md D2). VaultFileDTO
// and VaultMoveRequest have drifted, so list and move aren't checked here.
func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	u := v.user("trial")
	hub, _ := v.indexFixtures(u)
	for _, c := range []struct {
		method, path string
		headers      map[string]string
		body         string
	}{
		{http.MethodGet, "/v1/vault/status", nil, ""},
		{http.MethodPost, "/v1/vault/create", nil, ""},
		{http.MethodPost, "/v1/vault/files?path=spec.md", map[string]string{"Content-Type": "text/markdown"}, "# spec"},
		{http.MethodGet, "/v1/vault/index", nil, ""},
		{http.MethodGet, "/v1/vault/lint", nil, ""},
		{http.MethodGet, "/v1/vault/context/" + hub.String(), nil, ""},
		{http.MethodGet, "/v1/vault/links/" + hub.String(), nil, ""},
	} {
		r := v.do(u, c.method, c.path, c.headers, []byte(c.body))
		require.Equal(t, http.StatusOK, r.code, "%s %s: %s", c.method, c.path, r.body)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		for k, val := range c.headers {
			req.Header.Set(k, val)
		}
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, ExcludeRequestBody: true},
		}
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: r.code,
			Header: http.Header{"Content-Type": {r.headers.Get("Content-Type")}},
		}
		out.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, r.body)
	}
}
