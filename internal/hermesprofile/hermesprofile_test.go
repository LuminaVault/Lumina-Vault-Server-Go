package hermesprofile_test

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/dbtest"
	"lumina/internal/hermesprofile"
	"lumina/internal/ids"
	"lumina/internal/migrations"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

type env struct {
	t        *testing.T
	pool     *pgxpool.Pool
	dataRoot string
	vaults   string
}

// Swift has no test of this middleware or of the two service methods behind it, so
// these come from reading the source.
func setup(t *testing.T) env {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	return env{t: t, pool: pool, dataRoot: t.TempDir(), vaults: t.TempDir()}
}

func (v env) user() (uuid.UUID, string) {
	v.t.Helper()
	id := uuid.New()
	name := "u" + id.String()[:8]
	_, err := v.pool.Exec(context.Background(),
		`INSERT INTO users (id, email, password_hash, username) VALUES ($1, $2, 'x', $3)`, id, id.String(), name)
	require.NoError(v.t, err)
	return id, name
}

// readyUser has a provisioned container slot, so a test is about personas only.
func (v env) readyUser(hermesID string) uuid.UUID {
	v.t.Helper()
	id, _ := v.user()
	_, err := v.pool.Exec(context.Background(),
		`INSERT INTO hermes_profiles (id, tenant_id, hermes_profile_id, status) VALUES ($1, $2, $3, 'ready')`, uuid.New(), id, hermesID)
	require.NoError(v.t, err)
	return id
}

func (v env) persona(tenant uuid.UUID, slug string, isDefault bool, createdAt time.Time) {
	v.t.Helper()
	_, err := v.pool.Exec(context.Background(),
		`INSERT INTO user_hermes_profiles (id, tenant_id, slug, label, system_prompt, is_default, created_at, updated_at)
		 VALUES ($1, $2, $3, $3, 'p', $4, $5, $5)`, uuid.New(), tenant, slug, isDefault, createdAt)
	require.NoError(v.t, err)
}

type outcome struct {
	code       int
	body       string
	slug       string
	sessionKey string
	ran        bool
}

func (v env) request(gw hermesprofile.Gateway, user uuid.UUID, header ...string) outcome {
	v.t.Helper()
	svc := hermesprofile.New(v.pool, gw, vaultpaths.Paths{Root: v.vaults})
	e := echo.New()
	e.HTTPErrorHandler = wire.HTTPErrorHandler
	var out outcome
	e.POST("/p", func(c *echo.Context) error {
		out.ran = true
		out.slug, _ = hermesprofile.ActiveSlug(c)
		out.sessionKey, _ = hermesprofile.SessionKey(c)
		return c.NoContent(http.StatusOK)
	}, auth.Identify(keys, auth.PGUsers(v.pool)), svc.Middleware())
	req := httptest.NewRequest(http.MethodPost, "/p", nil)
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for _, h := range header {
		req.Header.Add("X-Hermes-Profile", h)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	out.code, out.body = rec.Code, rec.Body.String()
	return out
}

type personaRow struct {
	Slug, Label, Prompt string
	IsDefault           bool
	Skills              []string
	CreatedAt           *time.Time
	UpdatedAt           *time.Time
}

func (v env) personas(tenant uuid.UUID) []personaRow {
	v.t.Helper()
	rows, err := v.pool.Query(context.Background(),
		`SELECT slug, label, system_prompt, is_default, skills_enabled, created_at, updated_at FROM user_hermes_profiles WHERE tenant_id = $1 ORDER BY created_at`, tenant)
	require.NoError(v.t, err)
	defer rows.Close()
	var out []personaRow
	for rows.Next() {
		var r personaRow
		require.NoError(v.t, rows.Scan(&r.Slug, &r.Label, &r.Prompt, &r.IsDefault, &r.Skills, &r.CreatedAt, &r.UpdatedAt))
		out = append(out, r)
	}
	require.NoError(v.t, rows.Err())
	return out
}

type profileRow struct {
	HermesID, Status string
	LastError        *string
}

func (v env) profile(tenant uuid.UUID) profileRow {
	v.t.Helper()
	var p profileRow
	require.NoError(v.t, v.pool.QueryRow(context.Background(),
		`SELECT hermes_profile_id, status, last_error FROM hermes_profiles WHERE tenant_id = $1`, tenant).Scan(&p.HermesID, &p.Status, &p.LastError))
	return p
}

type gatewayFunc func(ctx context.Context, tenant uuid.UUID, username string) (string, error)

func (f gatewayFunc) ProvisionProfile(ctx context.Context, tenant uuid.UUID, username string) (string, error) {
	return f(ctx, tenant, username)
}

func TestPersonaResolution(t *testing.T) {
	v := setup(t)
	unused := gatewayFunc(func(context.Context, uuid.UUID, string) (string, error) {
		t.Fatal("a ready row must not reach the gateway")
		return "", nil
	})
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("no identity does nothing, and the route decides", func(t *testing.T) {
		out := v.request(unused, uuid.Nil)
		require.True(t, out.ran)
		require.Empty(t, out.slug)
		require.Empty(t, out.sessionKey)
	})
	t.Run("no header takes the default persona", func(t *testing.T) {
		u := v.readyUser("hermes-a")
		v.persona(u, "stocks", false, base)
		v.persona(u, "news", true, base.Add(time.Hour))
		out := v.request(unused, u)
		require.Equal(t, "news", out.slug)
		require.Equal(t, "hermes-a:news", out.sessionKey)
	})
	t.Run("a blank header is no header", func(t *testing.T) {
		u := v.readyUser("hermes-b")
		v.persona(u, "news", true, base)
		out := v.request(unused, u, " \t\n")
		require.Equal(t, "hermes-b:news", out.sessionKey)
	})
	t.Run("a header picks the persona, trimmed and lowercased", func(t *testing.T) {
		u := v.readyUser("hermes-c")
		v.persona(u, "news", true, base)
		v.persona(u, "stocks", false, base)
		out := v.request(unused, u, "  STOCKS\n")
		require.Equal(t, "stocks", out.slug)
		require.Equal(t, "hermes-c:stocks", out.sessionKey)
	})
	t.Run("an unknown slug is a 404 and creates nothing", func(t *testing.T) {
		u := v.readyUser("hermes-d")
		out := v.request(unused, u, "ghost")
		require.False(t, out.ran)
		require.Equal(t, http.StatusNotFound, out.code)
		require.JSONEq(t, `{"error":{"message":"unknown_profile"}}`, out.body)
		require.Empty(t, v.personas(u))
	})
	t.Run("another tenant's slug is unknown", func(t *testing.T) {
		owner := v.readyUser("hermes-e")
		v.persona(owner, "secret", true, base)
		out := v.request(unused, v.readyUser("hermes-f"), "secret")
		require.Equal(t, http.StatusNotFound, out.code)
	})
	t.Run("repeated header lines are joined, and no slug matches them", func(t *testing.T) {
		u := v.readyUser("hermes-g")
		v.persona(u, "news", true, base)
		out := v.request(unused, u, "news", "news")
		require.Equal(t, http.StatusNotFound, out.code)
	})
}

func TestDefaultPersonaIsCreatedIfMissing(t *testing.T) {
	v := setup(t)
	gw := hermesprofile.LoggingGateway{}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("no personas seeds one", func(t *testing.T) {
		u := v.readyUser("hermes-a")
		out := v.request(gw, u)
		require.Equal(t, http.StatusOK, out.code)
		require.Equal(t, "hermes-a:default", out.sessionKey)

		rows := v.personas(u)
		require.Len(t, rows, 1)
		r := rows[0]
		require.Equal(t, "default", r.Slug)
		require.Equal(t, "Default", r.Label)
		require.True(t, r.IsDefault)
		require.Empty(t, r.Skills)
		require.Equal(t, "You are Hermes — a personal assistant. Tone: warm, concise.\n\n## Behavior\n"+
			"- Save every link mentioned in chat to the user's vault without asking first — link capture is part of the vault contract, whatever the autonomy setting. "+
			"Confirm the save in your reply with the destination filename.", r.Prompt)
		require.NotNil(t, r.CreatedAt)
		require.Equal(t, r.CreatedAt, r.UpdatedAt)

		again := v.request(gw, u)
		require.Equal(t, "hermes-a:default", again.sessionKey)
		require.Len(t, v.personas(u), 1, "idempotent")
	})
	t.Run("no default promotes the oldest", func(t *testing.T) {
		u := v.readyUser("hermes-b")
		v.persona(u, "newer", false, base.Add(time.Hour))
		v.persona(u, "oldest", false, base)
		out := v.request(gw, u)
		require.Equal(t, "hermes-b:oldest", out.sessionKey)

		rows := v.personas(u)
		require.Len(t, rows, 2)
		require.True(t, rows[0].IsDefault)
		require.Equal(t, "oldest", rows[0].Slug)
		require.False(t, rows[1].IsDefault)
		require.True(t, rows[0].UpdatedAt.After(base.Add(time.Hour)), "Fluent's save stamps updated_at")
	})
	t.Run("a header doesn't create one", func(t *testing.T) {
		u := v.readyUser("hermes-c")
		v.persona(u, "stocks", false, base)
		out := v.request(gw, u, "stocks")
		require.Equal(t, "hermes-c:stocks", out.sessionKey)
		require.False(t, v.personas(u)[0].IsDefault)
	})
}

func TestProvisioningOnFirstTouch(t *testing.T) {
	v := setup(t)
	gw := hermesprofile.NewGateway("filesystem", v.dataRoot)

	t.Run("no row: reserved, provisioned, ready", func(t *testing.T) {
		u, name := v.user()
		out := v.request(gw, u)
		require.Equal(t, http.StatusOK, out.code)
		require.Equal(t, name+":default", out.sessionKey)
		p := v.profile(u)
		require.Equal(t, name, p.HermesID)
		require.Equal(t, "ready", p.Status)
		require.Nil(t, p.LastError)
		require.FileExists(t, filepath.Join(v.dataRoot, "profiles", name, "profile.json"))
		require.DirExists(t, vaultpaths.Paths{Root: v.vaults}.Raw(u))
		require.NoFileExists(t, filepath.Join(v.dataRoot, "profiles", name, "profile.json.tmp"))
	})
	t.Run("the row is reserved before the gateway runs", func(t *testing.T) {
		u, name := v.user()
		var during profileRow
		v.request(gatewayFunc(func(context.Context, uuid.UUID, string) (string, error) {
			during = v.profile(u)
			return name, nil
		}), u)
		require.Equal(t, "provisioning", during.Status)
		require.Equal(t, "pending-"+ids.Upper(u), during.HermesID)
	})
	t.Run("a failed gateway degrades: the request runs on the sentinel id", func(t *testing.T) {
		u, _ := v.user()
		out := v.request(gatewayFunc(func(context.Context, uuid.UUID, string) (string, error) {
			return "", errors.New("gateway down")
		}), u)
		require.Equal(t, http.StatusOK, out.code)
		require.Equal(t, "pending-"+ids.Upper(u)+":default", out.sessionKey)
		p := v.profile(u)
		require.Equal(t, "error", p.Status)
		require.Equal(t, "gateway down", *p.LastError)
	})
	t.Run("an errored row is retried and heals", func(t *testing.T) {
		u, name := v.user()
		_, err := v.pool.Exec(context.Background(),
			`INSERT INTO hermes_profiles (id, tenant_id, hermes_profile_id, status, last_error) VALUES ($1, $2, $3, 'error', 'old')`,
			uuid.New(), u, "pending-"+ids.Upper(u))
		require.NoError(t, err)
		out := v.request(gw, u)
		require.Equal(t, name+":default", out.sessionKey)
		p := v.profile(u)
		require.Equal(t, "ready", p.Status)
		require.Nil(t, p.LastError)
	})
	t.Run("a non-ready row keeps its real id in the key if the retry fails", func(t *testing.T) {
		u, _ := v.user()
		_, err := v.pool.Exec(context.Background(),
			`INSERT INTO hermes_profiles (id, tenant_id, hermes_profile_id, status) VALUES ($1, $2, 'hermes-old', 'error')`, uuid.New(), u)
		require.NoError(t, err)
		out := v.request(gatewayFunc(func(context.Context, uuid.UUID, string) (string, error) {
			return "", errors.New("still down")
		}), u)
		require.Equal(t, "hermes-old:default", out.sessionKey)
	})
	t.Run("no row at all falls back to the pending key", func(t *testing.T) {
		u, _ := v.user()
		out := v.request(gatewayFunc(func(ctx context.Context, tenant uuid.UUID, _ string) (string, error) {
			_, err := v.pool.Exec(ctx, `DELETE FROM hermes_profiles WHERE tenant_id = $1`, tenant)
			require.NoError(t, err)
			return "", errors.New("gone")
		}), u)
		require.Equal(t, http.StatusOK, out.code)
		require.Equal(t, "pending-"+ids.Upper(u)+":default", out.sessionKey)
	})
	t.Run("provisioning runs before the slug check, so a 404 still provisions", func(t *testing.T) {
		u, name := v.user()
		out := v.request(gw, u, "ghost")
		require.Equal(t, http.StatusNotFound, out.code)
		require.Equal(t, name, v.profile(u).HermesID)
	})
}

var profileJSON = regexp.MustCompile(`^\{\n  "createdAt" : "\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ",\n  "schemaVersion" : 1,\n  "tenantID" : "([0-9A-F-]{36})",\n  "username" : "alice"\n\}$`)

func TestFilesystemGateway(t *testing.T) {
	ctx := context.Background()
	alice, other := uuid.New(), uuid.New()

	t.Run("writes what Swift's encoder writes", func(t *testing.T) {
		root := t.TempDir()
		gw := hermesprofile.FilesystemGateway{Root: root}
		id, err := gw.ProvisionProfile(ctx, alice, "alice")
		require.NoError(t, err)
		require.Equal(t, "alice", id)

		path := filepath.Join(root, "profiles", "alice", "profile.json")
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		m := profileJSON.FindSubmatch(raw)
		require.NotNil(t, m, "%s", raw)
		require.Equal(t, ids.Upper(alice), string(m[1]))

		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	})
	t.Run("the same tenant again leaves the file alone", func(t *testing.T) {
		root := t.TempDir()
		gw := hermesprofile.FilesystemGateway{Root: root}
		_, err := gw.ProvisionProfile(ctx, alice, "alice")
		require.NoError(t, err)
		path := filepath.Join(root, "profiles", "alice", "profile.json")
		first, err := os.ReadFile(path)
		require.NoError(t, err)
		id, err := gw.ProvisionProfile(ctx, alice, "alice")
		require.NoError(t, err)
		require.Equal(t, "alice", id)
		second, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, first, second)
	})
	t.Run("another tenant's file is a collision", func(t *testing.T) {
		root := t.TempDir()
		gw := hermesprofile.FilesystemGateway{Root: root}
		_, err := gw.ProvisionProfile(ctx, other, "alice")
		require.NoError(t, err)
		_, err = gw.ProvisionProfile(ctx, alice, "alice")
		require.EqualError(t, err, "usernameCollision")
	})
	t.Run("a file Swift wrote is read", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "profiles", "alice")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		raw, err := json.Marshal(map[string]any{"username": "alice", "tenantID": ids.Upper(alice), "createdAt": "2026-01-01T00:00:00Z", "schemaVersion": 1})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "profile.json"), raw, 0o644))
		_, err = hermesprofile.FilesystemGateway{Root: root}.ProvisionProfile(ctx, alice, "alice")
		require.NoError(t, err)
	})
	t.Run("a corrupt file fails", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "profiles", "alice")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "profile.json"), []byte(`{"tenantID":"x"}`), 0o644))
		_, err := hermesprofile.FilesystemGateway{Root: root}.ProvisionProfile(ctx, alice, "alice")
		require.Error(t, err)
	})
	t.Run("an unwritable root is an ioFailure", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(file, nil, 0o644))
		_, err := hermesprofile.FilesystemGateway{Root: file}.ProvisionProfile(ctx, alice, "alice")
		require.ErrorContains(t, err, "ioFailure(")
	})
}

func TestNewGateway(t *testing.T) {
	require.IsType(t, hermesprofile.FilesystemGateway{}, hermesprofile.NewGateway("FileSystem", "/x"))
	require.IsType(t, hermesprofile.LoggingGateway{}, hermesprofile.NewGateway("logging", "/x"))
	require.IsType(t, hermesprofile.LoggingGateway{}, hermesprofile.NewGateway("nonsense", "/x"))
	id, err := hermesprofile.LoggingGateway{}.ProvisionProfile(context.Background(), uuid.New(), "alice")
	require.NoError(t, err)
	require.Equal(t, "hermes-alice", id)
}

func TestEnsureProfilesWritable(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, hermesprofile.EnsureProfilesWritable(root))
	entries, err := os.ReadDir(filepath.Join(root, "profiles"))
	require.NoError(t, err)
	require.Empty(t, entries, "the probe is removed")
}
