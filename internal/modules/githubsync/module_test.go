package githubsync_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/githubapp"
	"lumina/internal/githubapp/githubapptest"
	"lumina/internal/githubsync"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultgit"
	"lumina/internal/vaultpaths"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	module "lumina/internal/modules/githubsync"
)

var testKeys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

const webOrigin = "https://app.lumina.test"

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	gh     *githubapptest.Server
	module *module.Module
	// committer and syncer are the push worker behind the routes, wired as
	// cmd/App does.
	committer *vaultgit.Committer
	syncer    *githubsync.Syncer
	paths     vaultpaths.Paths
	e         *echo.Echo
	root      string
	logs      *syncBuffer
	bodies    *syncBuffer
	clock     *clock
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newEnv(t *testing.T) *env { return newEnvWith(t, []string{webOrigin}) }

func newEnvWith(t *testing.T, origins []string) *env {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	gh := githubapptest.New(t)
	logs := &syncBuffer{}
	root := t.TempDir()
	clk := &clock{t: time.Now()}
	// The rate limits run on the test's clock, so a slow machine cannot roll
	// a window over in the middle of a test.
	limits := ratelimit.NewStore()
	limits.SetClock(clk.now)
	client, store, paths := gh.Client(t), githubsync.NewStore(pool), vaultpaths.Paths{Root: root}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var syncer *githubsync.Syncer
	committer := vaultgit.NewCommitter(vaultgit.Options{
		RawRoot: paths.Raw, Lock: vaultgit.PGLocker{Pool: pool}, Logger: log,
		OnCommit: func(tenant uuid.UUID, sha string) { syncer.OnCommit(tenant, sha) },
	})
	syncer = githubsync.NewSyncer(githubsync.SyncerOptions{
		Store: store, GitHub: client, Repos: committer, RawRoot: paths.Raw, GitBaseURL: gh.URL, Now: clk.now, Log: log,
	})
	t.Cleanup(func() {
		syncer.Stop()
		committer.Close(context.Background())
	})
	m := module.New(module.Options{
		Client: client, Store: store, Access: vaultaccess.New(pool),
		Paths: paths, CallbackURL: "https://api.lumina.test/v1/github-sync/callback",
		Slug: "luminavault", MaxRepoBytes: 200 << 20, MaxFiles: 5000,
		WebReturnOrigins: origins, Limits: limits, Sync: syncer, Log: log,
	})
	m.SetClock(clk.now)
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(testKeys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{m},
	})
	return &env{
		t: t, pool: pool, gh: gh, module: m, committer: committer, syncer: syncer, paths: paths,
		e: e, root: root, logs: logs, bodies: &syncBuffer{}, clock: clk,
	}
}

func (v *env) exec(sql string, args ...any) {
	v.t.Helper()
	_, err := v.pool.Exec(context.Background(), sql, args...)
	require.NoError(v.t, err)
}

// user registers an account and its personal vault.
func (v *env) user() uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	v.exec(`INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
		VALUES ($1, $2, 'x', true, $3, 'trial', now(), now())`, id, id.String()+"@test", "u"+strings.ReplaceAll(id.String(), "-", "")[:16])
	v.exec(`INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'v', now(), now())`, id)
	return id
}

// teamVault makes a team owned by owner with a vault; members get roles.
func (v *env) teamVault(owner uuid.UUID, members map[uuid.UUID]string) (team, vault uuid.UUID) {
	v.t.Helper()
	team, vault = uuid.New(), uuid.New()
	v.exec(`INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at) VALUES ($1, 't', $2, $2, now(), now())`, team, owner)
	v.exec(`INSERT INTO vaults (id, team_id, name, created_at, updated_at) VALUES ($1, $2, 'team', now(), now())`, vault, team)
	all := map[uuid.UUID]string{owner: "admin"}
	for u, role := range members {
		all[u] = role
	}
	for u, role := range all {
		v.exec(`INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
			VALUES (gen_random_uuid(), $1, $2, $3, false, $4, now(), now())`, vault, u, role, owner)
	}
	return team, vault
}

type response struct {
	code   int
	header http.Header
	body   []byte
}

func (r response) String() string { return string(r.body) }

func (r response) errCode() string {
	var b struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(r.body, &b)
	return b.Error.Code
}

func (v *env) do(user uuid.UUID, vault *uuid.UUID, method, target string, body any) response {
	v.t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(v.t, err)
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if user != uuid.Nil {
		token, err := scenario.Mint(testKeys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if vault != nil {
		req.Header.Set("X-Vault-ID", vault.String())
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	_, _ = v.bodies.Write(rec.Body.Bytes())
	_, _ = v.bodies.Write([]byte(rec.Header().Get("Location")))
	return response{rec.Code, rec.Header(), rec.Body.Bytes()}
}

// Repositories of the standard stub: 456 the user can push to, 457 the user
// can only read, 458 is in the installation but not in the user's list, and
// installation 99 belongs to someone else entirely.
func (v *env) seedGitHub(userToken, login string) {
	v.gh.Lock()
	defer v.gh.Unlock()
	v.gh.Users[userToken] = login
	v.gh.UserInstallations[userToken] = []githubapp.Installation{{ID: 7, Account: githubapp.Account{Login: login, Type: "User"}, RepositorySelection: "selected"}}
	push := githubapp.Repository{ID: 456, FullName: login + "/notes", Private: true, DefaultBranch: "main", Size: 0}
	push.Permissions.Push = true
	read := githubapp.Repository{ID: 457, FullName: login + "/read-only", DefaultBranch: "main", Size: 10}
	v.gh.UserRepos[userToken] = map[int64][]githubapp.Repository{7: {push, read}}
	v.gh.InstallationRepos[7] = []int64{456, 457, 458}
	v.gh.InstallationRepos[99] = []int64{999}
	v.gh.Repos[456] = push
	v.gh.Repos[457] = read
	v.gh.Repos[458] = githubapp.Repository{ID: 458, FullName: login + "/hidden", DefaultBranch: "main"}
	v.gh.Repos[999] = githubapp.Repository{ID: 999, FullName: "victim/private", DefaultBranch: "main"}
}

// startFlow posts connect and runs the callback; it returns the completion
// handle and the callback's redirect.
func (v *env) startFlow(user uuid.UUID, vault *uuid.UUID, code string, returnTo *string) (handle, location string) {
	v.t.Helper()
	body := map[string]any{}
	if returnTo != nil {
		body["returnTo"] = *returnTo
	}
	r := v.do(user, vault, http.MethodPost, "/v1/vault/github-sync/connect", body)
	require.Equal(v.t, http.StatusOK, r.code, r.String())
	var out struct {
		AuthorizeURL string `json:"authorizeUrl"`
	}
	require.NoError(v.t, json.Unmarshal(r.body, &out))
	u, err := url.Parse(out.AuthorizeURL)
	require.NoError(v.t, err)
	require.Equal(v.t, "/login/oauth/authorize", u.Path)
	require.Equal(v.t, githubapptest.ClientID, u.Query().Get("client_id"))
	require.Equal(v.t, "https://api.lumina.test/v1/github-sync/callback", u.Query().Get("redirect_uri"))
	state := u.Query().Get("state")
	require.NotEmpty(v.t, state)
	// installation_id and setup_action are ignored, whatever they say.
	cb := v.do(uuid.Nil, nil, http.MethodGet, "/v1/github-sync/callback?code="+code+"&state="+state+"&installation_id=99&setup_action=install", nil)
	require.Equal(v.t, http.StatusFound, cb.code)
	location = cb.header.Get("Location")
	loc, err := url.Parse(location)
	require.NoError(v.t, err)
	require.Equal(v.t, "authorized", loc.Query().Get("status"), location)
	return loc.Query().Get("completion"), location
}

type completeOut struct {
	Session       string `json:"session"`
	GitHubLogin   string `json:"githubLogin"`
	InstallURL    string `json:"installUrl"`
	Installations []struct {
		ID int64 `json:"id"`
	} `json:"installations"`
}

func (v *env) signIn(user uuid.UUID, vault *uuid.UUID, code, userToken string) string {
	v.t.Helper()
	v.gh.Lock()
	v.gh.Codes[code] = userToken
	v.gh.Unlock()
	handle, _ := v.startFlow(user, vault, code, nil)
	r := v.do(user, vault, http.MethodPost, "/v1/vault/github-sync/complete", map[string]string{"completion": handle})
	require.Equal(v.t, http.StatusOK, r.code, r.String())
	var out completeOut
	require.NoError(v.t, json.Unmarshal(r.body, &out))
	require.NotEmpty(v.t, out.Session)
	return out.Session
}

func (v *env) put(user uuid.UUID, vault *uuid.UUID, body map[string]any) response {
	v.t.Helper()
	return v.do(user, vault, http.MethodPut, "/v1/vault/github-sync/connection", body)
}

func (v *env) connection(vault uuid.UUID) *githubsync.Connection {
	v.t.Helper()
	c, err := githubsync.NewStore(v.pool).Get(context.Background(), vault)
	require.NoError(v.t, err)
	return c
}

// E4.7 done-when: the happy path against the stubs ends with a row in state
// active, and the picker's user token is revoked.
func TestConnectHappyPath(t *testing.T) {
	v := newEnv(t)
	alice := v.user()
	v.seedGitHub("ghu_alice", "octocat")

	ret := webOrigin + "/settings/integrations"
	v.gh.Lock()
	v.gh.Codes["code-a"] = "ghu_alice"
	v.gh.Unlock()
	handle, location := v.startFlow(alice, nil, "code-a", &ret)
	require.True(t, strings.HasPrefix(location, ret+"?status=authorized&completion="), location)
	require.Zero(t, v.gh.ExchangeCount(), "the callback exchanges nothing")

	r := v.do(alice, nil, http.MethodPost, "/v1/vault/github-sync/complete", map[string]string{"completion": handle})
	require.Equal(t, http.StatusOK, r.code, r.String())
	var done completeOut
	require.NoError(t, json.Unmarshal(r.body, &done))
	require.Equal(t, "octocat", done.GitHubLogin)
	require.Len(t, done.Installations, 1)
	require.Equal(t, v.gh.URL+"/apps/luminavault/installations/new", done.InstallURL)
	session := done.Session

	r = v.do(alice, nil, http.MethodGet, "/v1/vault/github-sync/installations?session="+session, nil)
	require.Equal(t, http.StatusOK, r.code, r.String())

	r = v.do(alice, nil, http.MethodGet, "/v1/vault/github-sync/repositories?session="+session+"&installationId=7", nil)
	require.Equal(t, http.StatusOK, r.code, r.String())
	var repos struct {
		Repositories []struct {
			ID      int64 `json:"id"`
			CanPush bool  `json:"canPush"`
			InUse   bool  `json:"inUse"`
			Empty   bool  `json:"empty"`
		} `json:"repositories"`
		NextPage *int `json:"nextPage"`
	}
	require.NoError(t, json.Unmarshal(r.body, &repos))
	require.Len(t, repos.Repositories, 1, "only repositories the user can push to")
	require.Equal(t, int64(456), repos.Repositories[0].ID)
	require.True(t, repos.Repositories[0].Empty)
	require.Nil(t, repos.NextPage)

	r = v.do(alice, nil, http.MethodGet, "/v1/vault/github-sync/branches?session="+session+"&installationId=7&repositoryId=456", nil)
	require.Equal(t, http.StatusOK, r.code, r.String())
	require.JSONEq(t, `{"branches":[],"defaultBranch":"main"}`, r.String())

	r = v.put(alice, nil, map[string]any{"session": session, "installationId": 7, "repositoryId": 456, "branch": "main"})
	require.Equal(t, http.StatusAccepted, r.code, r.String())
	var st struct {
		CanManage  bool `json:"canManage"`
		Connection struct {
			State        string `json:"state"`
			RepoFullName string `json:"repoFullName"`
			ManageURL    string `json:"manageUrl"`
		} `json:"connection"`
	}
	require.NoError(t, json.Unmarshal(r.body, &st))
	require.True(t, st.CanManage)
	require.Equal(t, githubsync.StateActive, st.Connection.State)
	require.Equal(t, "octocat/notes", st.Connection.RepoFullName)
	require.Equal(t, v.gh.URL+"/settings/installations/7", st.Connection.ManageURL)

	c := v.connection(alice)
	require.NotNil(t, c)
	require.Equal(t, int64(7), c.InstallationID)
	require.Equal(t, int64(456), c.RepoID)
	require.Equal(t, "main", c.Branch)
	require.Equal(t, "octocat", c.ConnectedGitHubLogin)
	require.Equal(t, alice, *c.ConnectedByUserID)

	require.True(t, v.gh.Revoked("ghu_alice"), "the user token is revoked at connect")
	r = v.do(alice, nil, http.MethodGet, "/v1/vault/github-sync/installations?session="+session, nil)
	require.Equal(t, http.StatusBadRequest, r.code)
	require.Equal(t, "session_expired", r.errCode(), "the picker session ended")

	mints := v.gh.Mints()
	require.NotEmpty(t, mints)
	require.Equal(t, []int64{456}, mints[0].RepositoryIDs)

	// Disconnect: row gone, cached installation token revoked, files untouched.
	r = v.do(alice, nil, http.MethodDelete, "/v1/vault/github-sync/connection", nil)
	require.Equal(t, http.StatusNoContent, r.code, r.String())
	require.Nil(t, v.connection(alice))
	require.Equal(t, []string{mints[len(mints)-1].Token}, v.gh.RevokedInstallations())
	r = v.do(alice, nil, http.MethodDelete, "/v1/vault/github-sync/connection", nil)
	require.Equal(t, http.StatusNotFound, r.code)
	require.Equal(t, "not_connected", r.errCode())
	r = v.do(alice, nil, http.MethodGet, "/v1/vault/github-sync", nil)
	require.JSONEq(t, `{"available":true,"canManage":true,"connection":null}`, r.String())
}

// D3: the server never trusts an installation id it did not get from the
// user's own GitHub token.
func TestForgedInstallChangesNothing(t *testing.T) {
	v := newEnv(t)
	alice := v.user()
	v.seedGitHub("ghu_alice", "octocat")

	// A forged callback: someone else's installation id, no valid state. With
	// no flow to say where it came from, the browser goes to the web app.
	for _, q := range []string{"installation_id=99&setup_action=install", "state=forged&code=x&installation_id=99"} {
		r := v.do(uuid.Nil, nil, http.MethodGet, "/v1/github-sync/callback?"+q, nil)
		require.Equal(t, http.StatusFound, r.code)
		require.Equal(t, webOrigin+"/settings/integrations?status=error", r.header.Get("Location"))
	}
	require.Zero(t, v.gh.ExchangeCount())

	session := v.signIn(alice, nil, "code-a", "ghu_alice")
	for _, body := range []map[string]any{
		{"session": session, "installationId": 99, "repositoryId": 999}, // the App could mint for it
		{"session": session, "installationId": 7, "repositoryId": 458},  // in the installation, not the user's list
		{"session": session, "installationId": 7, "repositoryId": 457},  // read-only for this user
	} {
		r := v.put(alice, nil, body)
		require.Equal(t, http.StatusNotFound, r.code, r.String())
		require.Equal(t, "repo_not_accessible", r.errCode())
	}
	r := v.do(alice, nil, http.MethodGet, "/v1/vault/github-sync/repositories?session="+session+"&installationId=99", nil)
	require.Equal(t, http.StatusNotFound, r.code, "an installation outside the user's list")
	r = v.do(alice, nil, http.MethodGet, "/v1/vault/github-sync/branches?session="+session+"&installationId=7&repositoryId=458", nil)
	require.Equal(t, http.StatusNotFound, r.code)
	require.Empty(t, v.gh.Mints(), "no installation token was minted")
	require.Nil(t, v.connection(alice))
}

// D17 "wrong account" (the #181 class): user A starts, user B completes. B
// gets 403, the handle is spent, and the stub sees no code exchange. Same for
// the right user with another vault.
func TestCompletionFromAnotherAccountOrVault(t *testing.T) {
	v := newEnv(t)
	alice, bob := v.user(), v.user()
	_, team := v.teamVault(alice, nil)
	v.seedGitHub("ghu_alice", "octocat")
	v.gh.Lock()
	v.gh.Codes["code-a"] = "ghu_alice"
	v.gh.Unlock()

	handle, _ := v.startFlow(alice, nil, "code-a", nil)
	r := v.do(bob, nil, http.MethodPost, "/v1/vault/github-sync/complete", map[string]string{"completion": handle})
	require.Equal(t, http.StatusForbidden, r.code, r.String())
	require.Equal(t, "wrong_account", r.errCode())
	require.Zero(t, v.gh.ExchangeCount())
	r = v.do(alice, nil, http.MethodPost, "/v1/vault/github-sync/complete", map[string]string{"completion": handle})
	require.Equal(t, http.StatusBadRequest, r.code)
	require.Equal(t, "session_expired", r.errCode(), "the handle was spent")

	v.gh.Lock()
	v.gh.Codes["code-b"] = "ghu_alice"
	v.gh.Unlock()
	handle, _ = v.startFlow(alice, nil, "code-b", nil)
	r = v.do(alice, &team, http.MethodPost, "/v1/vault/github-sync/complete", map[string]string{"completion": handle})
	require.Equal(t, http.StatusForbidden, r.code, r.String())
	require.Equal(t, "wrong_account", r.errCode())
	require.Zero(t, v.gh.ExchangeCount())

	// A picker session is bound the same way.
	session := v.signIn(alice, nil, "code-c", "ghu_alice")
	r = v.do(alice, &team, http.MethodGet, "/v1/vault/github-sync/installations?session="+session, nil)
	require.Equal(t, http.StatusBadRequest, r.code)
	require.Equal(t, "session_expired", r.errCode())
}

// States, completion handles and picker sessions work once and expire; an
// expired picker session's user token is revoked.
func TestHandlesWorkOnceAndExpire(t *testing.T) {
	v := newEnv(t)
	alice := v.user()
	v.seedGitHub("ghu_alice", "octocat")

	r := v.do(alice, nil, http.MethodPost, "/v1/vault/github-sync/connect", nil)
	require.Equal(t, http.StatusOK, r.code)
	var out struct {
		AuthorizeURL string `json:"authorizeUrl"`
	}
	require.NoError(t, json.Unmarshal(r.body, &out))
	u, _ := url.Parse(out.AuthorizeURL)
	state := u.Query().Get("state")
	first := v.do(uuid.Nil, nil, http.MethodGet, "/v1/github-sync/callback?code=x&state="+state, nil)
	require.Contains(t, first.header.Get("Location"), "status=authorized")
	again := v.do(uuid.Nil, nil, http.MethodGet, "/v1/github-sync/callback?code=x&state="+state, nil)
	require.Equal(t, "luminavault://github-sync?status=error", again.header.Get("Location"), "a state works once")

	// A declined sign-in carries no code.
	r = v.do(alice, nil, http.MethodPost, "/v1/vault/github-sync/connect", nil)
	require.NoError(t, json.Unmarshal(r.body, &out))
	u, _ = url.Parse(out.AuthorizeURL)
	declined := v.do(uuid.Nil, nil, http.MethodGet, "/v1/github-sync/callback?error=access_denied&state="+u.Query().Get("state"), nil)
	require.Equal(t, "luminavault://github-sync?status=error", declined.header.Get("Location"))

	// A state expires after 10 minutes.
	r = v.do(alice, nil, http.MethodPost, "/v1/vault/github-sync/connect", nil)
	require.NoError(t, json.Unmarshal(r.body, &out))
	u, _ = url.Parse(out.AuthorizeURL)
	v.clock.advance(11 * time.Minute)
	late := v.do(uuid.Nil, nil, http.MethodGet, "/v1/github-sync/callback?code=x&state="+u.Query().Get("state"), nil)
	require.Equal(t, "luminavault://github-sync?status=error", late.header.Get("Location"))

	// A completion handle expires after 60 seconds.
	v.gh.Lock()
	v.gh.Codes["code-a"] = "ghu_alice"
	v.gh.Unlock()
	handle, _ := v.startFlow(alice, nil, "code-a", nil)
	v.clock.advance(61 * time.Second)
	r = v.do(alice, nil, http.MethodPost, "/v1/vault/github-sync/complete", map[string]string{"completion": handle})
	require.Equal(t, http.StatusBadRequest, r.code)
	require.Equal(t, "session_expired", r.errCode())
	require.Zero(t, v.gh.ExchangeCount())

	// A picker session expires after 10 minutes and its token is revoked.
	session := v.signIn(alice, nil, "code-b", "ghu_alice")
	require.False(t, v.gh.Revoked("ghu_alice"))
	v.clock.advance(10*time.Minute + time.Second)
	r = v.do(alice, nil, http.MethodGet, "/v1/vault/github-sync/installations?session="+session, nil)
	require.Equal(t, http.StatusBadRequest, r.code)
	require.Equal(t, "session_expired", r.errCode())
	require.True(t, v.gh.Revoked("ghu_alice"), "revoked at session expiry")
}

// D14: only the vault owner manages; members read the status. Ownership is
// read on every request, so a transfer moves management at once.
func TestOnlyTheOwnerManages(t *testing.T) {
	v := newEnv(t)
	alice, bob, carol := v.user(), v.user(), v.user()
	team, vault := v.teamVault(alice, map[uuid.UUID]string{bob: "admin", carol: "viewer"})
	v.seedGitHub("ghu_alice", "octocat")
	require.NoError(t, githubsync.NewStore(v.pool).Insert(context.Background(), githubsync.Connection{
		VaultID: vault, InstallationID: 7, AccountLogin: "org", AccountType: "Organization", RepoID: 456,
		RepoFullName: "org/notes", RepoPrivate: true, Branch: "main", State: githubsync.StateActive,
		ConnectedByUserID: &alice, ConnectedGitHubLogin: "octocat",
	}))

	managing := []struct{ method, path string }{
		{http.MethodPost, "/v1/vault/github-sync/connect"},
		{http.MethodGet, "/v1/vault/github-sync/installations?session=x"},
		{http.MethodGet, "/v1/vault/github-sync/repositories?session=x&installationId=7"},
		{http.MethodGet, "/v1/vault/github-sync/branches?session=x&installationId=7&repositoryId=456"},
		{http.MethodPut, "/v1/vault/github-sync/connection"},
		{http.MethodDelete, "/v1/vault/github-sync/connection"},
	}
	for _, who := range []uuid.UUID{bob, carol} {
		for _, rt := range managing {
			r := v.do(who, &vault, rt.method, rt.path, map[string]any{"session": "x", "installationId": 7, "repositoryId": 456})
			require.Equal(t, http.StatusForbidden, r.code, "%s %s", rt.method, rt.path)
			require.Equal(t, "owner_required", r.errCode())
		}
		r := v.do(who, &vault, http.MethodGet, "/v1/vault/github-sync", nil)
		require.Equal(t, http.StatusOK, r.code, r.String())
		require.Contains(t, r.String(), `"canManage":false`)
		require.Contains(t, r.String(), `"manageUrl":"`+v.gh.URL+`/organizations/org/settings/installations/7"`)
	}
	stranger := v.user()
	r := v.do(stranger, &vault, http.MethodGet, "/v1/vault/github-sync", nil)
	require.Equal(t, http.StatusForbidden, r.code, "not a member")

	v.exec(`UPDATE teams SET owner_user_id = $2 WHERE id = $1`, team, bob)
	r = v.do(alice, &vault, http.MethodDelete, "/v1/vault/github-sync/connection", nil)
	require.Equal(t, http.StatusForbidden, r.code)
	require.Equal(t, "owner_required", r.errCode())
	r = v.do(bob, &vault, http.MethodDelete, "/v1/vault/github-sync/connection", nil)
	require.Equal(t, http.StatusNoContent, r.code, r.String())
	require.Nil(t, v.connection(vault))
}

// D4, D11: repo_in_use, remote_not_empty with its summary, repo_too_large,
// invalid_branch, already_connected.
func TestConnectChecks(t *testing.T) {
	v := newEnv(t)
	alice, other := v.user(), v.user()
	v.seedGitHub("ghu_alice", "octocat")
	session := v.signIn(alice, nil, "code-a", "ghu_alice")
	body := func(extra map[string]any) map[string]any {
		b := map[string]any{"session": session, "installationId": 7, "repositoryId": 456, "branch": "main"}
		for k, val := range extra {
			b[k] = val
		}
		return b
	}

	r := v.put(alice, nil, body(map[string]any{"branch": "a..b"}))
	require.Equal(t, http.StatusBadRequest, r.code, r.String())
	require.Equal(t, "invalid_branch", r.errCode())

	// Too large.
	v.gh.Lock()
	big := v.gh.Repos[456]
	big.Size = 300 << 10 // KiB
	v.gh.Repos[456] = big
	v.gh.Unlock()
	r = v.put(alice, nil, body(nil))
	require.Equal(t, http.StatusRequestEntityTooLarge, r.code, r.String())
	require.Equal(t, "repo_too_large", r.errCode())
	v.gh.Lock()
	big.Size = 10
	v.gh.Repos[456] = big
	v.gh.Unlock()

	// A branch with files: summary first, then the confirmed merge.
	require.NoError(t, os.MkdirAll(filepath.Join(v.root, "tenants"), 0o755))
	raw := vaultpaths.Paths{Root: v.root}.Raw(alice)
	require.NoError(t, os.MkdirAll(filepath.Join(raw, "inbox"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(raw, "inbox", "plan.md"), []byte("local"), 0o600))
	v.gh.Lock()
	b := githubapp.Branch{Name: "main"}
	b.Commit.SHA = "c0ffee"
	b.Commit.Commit.Tree.SHA = "7ree"
	v.gh.Branches["octocat/notes"] = []githubapp.Branch{b}
	v.gh.Trees["7ree"] = githubapp.Tree{Entries: []githubapp.TreeEntry{
		{Path: "inbox", Mode: "040000", Type: "tree"},
		{Path: "inbox/plan.md", Mode: "100644", Type: "blob", SHA: githubsync.BlobID([]byte("remote")), Size: 6},
		{Path: "inbox/new.md", Mode: "100644", Type: "blob", SHA: "1", Size: 4},
		{Path: ".obsidian/app.json", Mode: "100644", Type: "blob", SHA: "2", Size: 10},
	}}
	v.gh.Unlock()
	r = v.put(alice, nil, body(nil))
	require.Equal(t, http.StatusConflict, r.code, r.String())
	require.Equal(t, "remote_not_empty", r.errCode())
	var conflict struct {
		Summary githubsync.Summary `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(r.body, &conflict))
	require.Equal(t, githubsync.Summary{Files: 2, Notes: 2, Conflicts: 1, Held: 1, Bytes: 20}, conflict.Summary)
	require.Nil(t, v.connection(alice), "a summary changes nothing")

	// A truncated listing is too large to plan.
	v.gh.Lock()
	tr := v.gh.Trees["7ree"]
	tr.Truncated = true
	v.gh.Trees["7ree"] = tr
	v.gh.Unlock()
	r = v.put(alice, nil, body(map[string]any{"mergeExisting": true}))
	require.Equal(t, http.StatusRequestEntityTooLarge, r.code, r.String())
	v.gh.Lock()
	tr.Truncated = false
	v.gh.Trees["7ree"] = tr
	v.gh.Unlock()

	// In use by another vault.
	require.NoError(t, githubsync.NewStore(v.pool).Insert(context.Background(), githubsync.Connection{
		VaultID: other, InstallationID: 7, AccountLogin: "octocat", AccountType: "User", RepoID: 456,
		RepoFullName: "octocat/notes", Branch: "main", State: githubsync.StateActive, ConnectedGitHubLogin: "x",
	}))
	r = v.do(alice, nil, http.MethodGet, "/v1/vault/github-sync/repositories?session="+session+"&installationId=7", nil)
	require.Contains(t, r.String(), `"inUse":true`)
	r = v.put(alice, nil, body(map[string]any{"mergeExisting": true}))
	require.Equal(t, http.StatusConflict, r.code, r.String())
	require.Equal(t, "repo_in_use", r.errCode())
	v.exec(`DELETE FROM github_sync_connections WHERE vault_id = $1`, other)

	r = v.put(alice, nil, body(map[string]any{"mergeExisting": true}))
	require.Equal(t, http.StatusAccepted, r.code, r.String())
	require.NotNil(t, v.connection(alice))

	v.seedGitHub("ghu_alice2", "octocat")
	session = v.signIn(alice, nil, "code-b", "ghu_alice2")
	r = v.put(alice, nil, map[string]any{"session": session, "installationId": 7, "repositoryId": 456})
	require.Equal(t, http.StatusConflict, r.code, r.String())
	require.Equal(t, "already_connected", r.errCode())

	r = v.put(alice, nil, map[string]any{"session": "unknown", "installationId": 7, "repositoryId": 456})
	require.Equal(t, http.StatusBadRequest, r.code)
	require.Equal(t, "session_expired", r.errCode())
}

// A 401 on the App's own credential (a rotated key, a skewed clock) is the
// server's fault: 502 and a log line, not "sign in again".
func TestAppCredentialFailureIsUnavailable(t *testing.T) {
	v := newEnv(t)
	alice := v.user()
	v.seedGitHub("ghu_alice", "octocat")
	session := v.signIn(alice, nil, "code-a", "ghu_alice")
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	v.gh.Lock()
	v.gh.Key = other
	v.gh.Unlock()
	r := v.put(alice, nil, map[string]any{"session": session, "installationId": 7, "repositoryId": 456})
	require.Equal(t, http.StatusBadGateway, r.code, r.String())
	require.Equal(t, "github_unavailable", r.errCode())
	require.Contains(t, v.logs.String(), "op=installation_token")
	require.Nil(t, v.connection(alice))
}

func TestInvalidReturnTo(t *testing.T) {
	v := newEnv(t)
	alice := v.user()
	r := v.do(alice, nil, http.MethodPost, "/v1/vault/github-sync/connect", map[string]string{"returnTo": "https://evil.example/x"})
	require.Equal(t, http.StatusBadRequest, r.code)
	require.Equal(t, "invalid_return_to", r.errCode())
}

// D17: no token, code, client secret or key reaches a response body, a
// redirect or a log line (beyond the handle and session the client is given).
// The completion handle and the session go to their client and nowhere else:
// neither is ever logged. (The webhook secret never reaches this module; the
// cmd/App test covers it, with the key as the server is configured.)
func TestNoSecretsInResponsesOrLogs(t *testing.T) {
	v := newEnv(t)
	alice, mallory := v.user(), v.user()
	v.seedGitHub("ghu_alice_secret_token", "octocat")
	v.gh.Lock()
	v.gh.Codes["code-secret-value"] = "ghu_alice_secret_token"
	v.gh.Codes["code-refused-value"] = "ghu_alice_secret_token"
	v.gh.Unlock()

	// A completion refused for another account, which logs.
	refused, _ := v.startFlow(alice, nil, "code-refused-value", nil)
	r := v.do(mallory, nil, http.MethodPost, "/v1/vault/github-sync/complete", map[string]string{"completion": refused})
	require.Equal(t, "wrong_account", r.errCode())

	handle, _ := v.startFlow(alice, nil, "code-secret-value", nil)
	r = v.do(alice, nil, http.MethodPost, "/v1/vault/github-sync/complete", map[string]string{"completion": handle})
	require.Equal(t, http.StatusOK, r.code, r.String())
	var done completeOut
	require.NoError(t, json.Unmarshal(r.body, &done))
	session := done.Session

	// A GitHub failure, which logs too.
	v.gh.Lock()
	v.gh.Intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/user/installations" {
			return false
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"message":"upstream said ghu_alice_secret_token"}`))
		return true
	}
	v.gh.Unlock()
	r = v.do(alice, nil, http.MethodGet, "/v1/vault/github-sync/installations?session="+session, nil)
	require.Equal(t, "github_unavailable", r.errCode())
	v.gh.Lock()
	v.gh.Intercept = nil
	v.gh.Unlock()

	r = v.put(alice, nil, map[string]any{"session": session, "installationId": 7, "repositoryId": 456})
	require.Equal(t, http.StatusAccepted, r.code, r.String())
	r = v.do(alice, nil, http.MethodGet, "/v1/vault/github-sync", nil)
	require.Equal(t, http.StatusOK, r.code)
	require.Equal(t, http.StatusNoContent, v.do(alice, nil, http.MethodDelete, "/v1/vault/github-sync/connection", nil).code)

	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(v.gh.Key)}))
	secrets := []string{"ghu_alice_secret_token", "code-secret-value", "code-refused-value", githubapptest.ClientSecret}
	secrets = append(secrets, strings.Split(strings.TrimSpace(keyPEM), "\n")[1:4]...) // lines of the key's body
	for _, m := range v.gh.Mints() {
		secrets = append(secrets, m.Token)
	}
	require.NotEmpty(t, v.logs.String())
	for _, s := range secrets {
		require.NotContains(t, v.bodies.String(), s)
		require.NotContains(t, v.logs.String(), s)
	}
	for _, own := range []string{session, handle, refused} {
		require.NotContains(t, v.logs.String(), own)
	}
}

// Every success body matches api/openapi.yaml.
func TestSpecConformance(t *testing.T) {
	v := newEnv(t)
	alice, bob := v.user(), v.user()
	v.seedGitHub("ghu_alice", "octocat")
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	check := func(method, target string, r response) {
		t.Helper()
		req := httptest.NewRequest(method, strings.Split(target, "?")[0], nil)
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, target)
		in := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{
				Request: req, PathParams: params, Route: route,
				Options: &openapi3filter.Options{
					AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, ExcludeRequestBody: true,
					IncludeResponseStatus: true, // an undeclared status fails
				},
			},
			Status: r.code, Header: http.Header{"Content-Type": {r.header.Get("Content-Type")}},
		}
		in.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(context.Background(), in), "%s %s -> %d %s", method, target, r.code, r.String())
	}
	call := func(user uuid.UUID, method, target string, body any) response {
		t.Helper()
		r := v.do(user, nil, method, target, body)
		check(method, target, r)
		return r
	}

	call(alice, http.MethodGet, "/v1/vault/github-sync", nil)
	call(alice, http.MethodPost, "/v1/vault/github-sync/connect", map[string]string{"returnTo": webOrigin + "/settings"})
	v.gh.Lock()
	v.gh.Codes["code-a"] = "ghu_alice"
	v.gh.Unlock()
	handle, _ := v.startFlow(alice, nil, "code-a", nil)
	r := call(alice, http.MethodPost, "/v1/vault/github-sync/complete", map[string]string{"completion": handle})
	var done completeOut
	require.NoError(t, json.Unmarshal(r.body, &done))
	call(alice, http.MethodGet, "/v1/vault/github-sync/installations?session="+done.Session, nil)
	call(alice, http.MethodGet, "/v1/vault/github-sync/repositories?session="+done.Session+"&installationId=7", nil)
	call(alice, http.MethodGet, "/v1/vault/github-sync/branches?session="+done.Session+"&installationId=7&repositoryId=456", nil)
	call(alice, http.MethodPut, "/v1/vault/github-sync/connection", map[string]any{"session": done.Session, "installationId": 7, "repositoryId": 456})
	v.exec(`UPDATE github_sync_connections SET error_code = 'push_rejected', error_at = now(), state = 'needs_attention',
		report = '{"conflicts":[{"path":"a.md","conflictPath":"a.conflict-20261120-100340.md","at":"2026-11-20T10:03:40Z"}],"heldCount":1,"held":[{"path":"My Note.md","reason":"unsupported_name"}]}'
		WHERE vault_id = $1`, alice)
	r = call(alice, http.MethodGet, "/v1/vault/github-sync", nil)
	require.Contains(t, r.String(), `"retrying":false`)
	require.Contains(t, r.String(), `"conflictPath":"a.conflict-20261120-100340.md"`)
	call(bob, http.MethodGet, "/v1/vault/github-sync", nil)
	r = call(alice, http.MethodPost, "/v1/vault/github-sync/sync", nil)
	require.Equal(t, http.StatusAccepted, r.code, r.String())
	require.Contains(t, r.String(), `"retrying":true`, "Sync now put the stopped connection back to work")
	call(alice, http.MethodDelete, "/v1/vault/github-sync/connection", nil)
	r = call(alice, http.MethodPost, "/v1/vault/github-sync/sync", nil)
	require.Equal(t, "not_connected", r.errCode())
	cb := v.do(uuid.Nil, nil, http.MethodGet, "/v1/github-sync/callback?state=x", nil)
	check(http.MethodGet, "/v1/github-sync/callback", cb)

	// Finding 10: every error body matches GitHubSyncError, code included,
	// also the errors this module did not make.
	fail := func(user uuid.UUID, vault *uuid.UUID, method, target string, body any, status int, code string) response {
		t.Helper()
		r := v.do(user, vault, method, target, body)
		require.Equal(t, status, r.code, "%s %s: %s", method, target, r.String())
		require.Equal(t, code, r.errCode(), "%s %s: %s", method, target, r.String())
		check(method, target, r)
		return r
	}
	const base = "/v1/vault/github-sync"
	nowhere := uuid.New()
	_, team := v.teamVault(alice, nil)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, base},
		{http.MethodPost, base + "/connect"},
		{http.MethodPost, base + "/complete"},
		{http.MethodGet, base + "/installations"},
		{http.MethodGet, base + "/repositories"},
		{http.MethodGet, base + "/branches"},
		{http.MethodPut, base + "/connection"},
		{http.MethodDelete, base + "/connection"},
	} {
		fail(uuid.Nil, nil, route.method, route.path, map[string]any{}, http.StatusUnauthorized, "unauthenticated")
		fail(alice, &nowhere, route.method, route.path, map[string]any{}, http.StatusNotFound, "not_found")
		fail(bob, &team, route.method, route.path, map[string]any{}, http.StatusForbidden, "forbidden")
	}
	fail(alice, nil, http.MethodPost, base+"/connect", "not an object", http.StatusBadRequest, "invalid_request")
	fail(alice, nil, http.MethodPost, base+"/complete", []int{1}, http.StatusBadRequest, "invalid_request")
	fail(alice, nil, http.MethodPost, base+"/complete", map[string]string{"completion": "nope"}, http.StatusBadRequest, "session_expired")
	fail(bob, nil, http.MethodDelete, base+"/connection", nil, http.StatusNotFound, "not_connected")
	fail(alice, nil, http.MethodPost, base+"/connect", map[string]string{"returnTo": "https://evil.example/"}, http.StatusBadRequest, "invalid_return_to")

	v.seedGitHub("ghu_alice2", "octocat") // the connect above revoked the first token
	session := v.signIn(alice, nil, "code-e", "ghu_alice2")
	fail(alice, nil, http.MethodGet, base+"/repositories?session="+session, nil, http.StatusBadRequest, "invalid_request")
	fail(alice, nil, http.MethodPut, base+"/connection", map[string]any{"session": session}, http.StatusBadRequest, "invalid_request")
	fail(alice, nil, http.MethodGet, base+"/branches?session="+session+"&installationId=7&repositoryId=458", nil, http.StatusNotFound, "repo_not_accessible")

	// GitHub's rate limit, with the time it named.
	v.gh.Lock()
	v.gh.Intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/user/installations") {
			return false
		}
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusForbidden)
		return true
	}
	v.gh.Unlock()
	r = fail(alice, nil, http.MethodGet, base+"/installations?session="+session, nil, http.StatusServiceUnavailable, "github_rate_limited")
	require.Equal(t, "42", r.header.Get("Retry-After"))
	v.gh.Lock()
	v.gh.Intercept = nil
	archived := v.gh.Repos[456]
	archived.Archived = true
	v.gh.Repos[456] = archived
	v.gh.Unlock()
	fail(alice, nil, http.MethodPut, base+"/connection", map[string]any{"session": session, "installationId": 7, "repositoryId": 456},
		http.StatusConflict, "repo_archived")

	// Our own rate limit: the heavy routes take 10 a minute.
	var limited response
	for range 12 {
		limited = v.do(alice, nil, http.MethodGet, base+"/branches?session="+session+"&installationId=7&repositoryId=458", nil)
		if limited.code == http.StatusTooManyRequests {
			break
		}
	}
	require.Equal(t, http.StatusTooManyRequests, limited.code)
	require.Equal(t, "rate_limited", limited.errCode())
	require.NotEmpty(t, limited.header.Get("Retry-After"))
	check(http.MethodGet, base+"/branches", limited)
	for range 35 {
		limited = v.do(uuid.Nil, nil, http.MethodGet, "/v1/github-sync/callback?state=x", nil)
		if limited.code == http.StatusTooManyRequests {
			break
		}
	}
	require.Equal(t, "rate_limited", limited.errCode())
	check(http.MethodGet, "/v1/github-sync/callback", limited)
}

// Finding 1: every signed-in route is limited per user; the status read,
// which clients poll every 3 s, has room for that; one user's limit is not
// another's.
func TestRateLimits(t *testing.T) {
	v := newEnv(t)
	alice, bob := v.user(), v.user()
	const base = "/v1/vault/github-sync"
	for i := range 60 { // three minutes of polling in one
		require.Equal(t, http.StatusOK, v.do(alice, nil, http.MethodGet, base, nil).code, "poll %d", i)
	}
	codes := map[int]int{}
	for range 35 {
		codes[v.do(alice, nil, http.MethodPost, base+"/connect", nil).code]++
	}
	require.Equal(t, map[int]int{http.StatusOK: 30, http.StatusTooManyRequests: 5}, codes)
	require.Equal(t, http.StatusTooManyRequests, v.do(alice, nil, http.MethodDelete, base+"/connection", nil).code, "one policy for the managing routes")
	require.Equal(t, http.StatusOK, v.do(alice, nil, http.MethodGet, base, nil).code, "the status read is not caught by it")
	require.Equal(t, http.StatusOK, v.do(bob, nil, http.MethodPost, base+"/connect", nil).code)
	v.clock.advance(61 * time.Second)
	require.Equal(t, http.StatusOK, v.do(alice, nil, http.MethodPost, base+"/connect", nil).code, "a new window")

	// The public callback is limited per address.
	codes = map[int]int{}
	for range 35 {
		codes[v.do(uuid.Nil, nil, http.MethodGet, "/v1/github-sync/callback?state=x", nil).code]++
	}
	require.Equal(t, map[int]int{http.StatusFound: 30, http.StatusTooManyRequests: 5}, codes)
	over := v.do(uuid.Nil, nil, http.MethodGet, "/v1/github-sync/callback?state=x", nil)
	require.Equal(t, "rate_limited", over.errCode())
	require.NotEmpty(t, over.header.Get("Retry-After"))
}

// Finding 1: a client that starts sign-ins in a loop only replaces its own.
func TestStartedSignInsAreBounded(t *testing.T) {
	v := newEnv(t)
	alice, bob := v.user(), v.user()
	start := func(user uuid.UUID) string {
		t.Helper()
		r := v.do(user, nil, http.MethodPost, "/v1/vault/github-sync/connect", nil)
		require.Equal(t, http.StatusOK, r.code, r.String())
		var out struct {
			AuthorizeURL string `json:"authorizeUrl"`
		}
		require.NoError(t, json.Unmarshal(r.body, &out))
		u, err := url.Parse(out.AuthorizeURL)
		require.NoError(t, err)
		return u.Query().Get("state")
	}
	callback := func(state string) string {
		t.Helper()
		loc, err := url.Parse(v.do(uuid.Nil, nil, http.MethodGet, "/v1/github-sync/callback?code=c&state="+url.QueryEscape(state), nil).header.Get("Location"))
		require.NoError(t, err)
		return loc.Query().Get("status")
	}
	bobs := start(bob)
	var states []string
	for range 7 {
		states = append(states, start(alice))
	}
	require.Equal(t, 6, v.module.PendingSignIns(), "five of alice's and bob's one")
	require.Equal(t, "error", callback(states[0]), "the oldest was dropped")
	require.Equal(t, "error", callback(states[1]))
	require.Equal(t, "authorized", callback(states[2]))
	require.Equal(t, "authorized", callback(states[6]))
	require.Equal(t, "authorized", callback(bobs), "another user's sign-in is untouched")
}

// Finding 8: a callback whose state the server no longer knows goes back to
// where the sign-in started: the app's scheme for the app, the web app's
// integrations page for a browser.
func TestCallbackWithUnknownState(t *testing.T) {
	location := func(v *env, state string) string {
		t.Helper()
		r := v.do(uuid.Nil, nil, http.MethodGet, "/v1/github-sync/callback?code=c&state="+url.QueryEscape(state), nil)
		require.Equal(t, http.StatusFound, r.code)
		return r.header.Get("Location")
	}
	stateOf := func(v *env, user uuid.UUID, body any) string {
		t.Helper()
		r := v.do(user, nil, http.MethodPost, "/v1/vault/github-sync/connect", body)
		require.Equal(t, http.StatusOK, r.code, r.String())
		var out struct {
			AuthorizeURL string `json:"authorizeUrl"`
		}
		require.NoError(t, json.Unmarshal(r.body, &out))
		u, err := url.Parse(out.AuthorizeURL)
		require.NoError(t, err)
		return u.Query().Get("state")
	}
	const (
		app = "luminavault://github-sync?status=error"
		web = webOrigin + "/settings/integrations?status=error"
	)

	// The first origin is a development one: the https origin is preferred.
	v := newEnvWith(t, []string{"http://localhost:5173", webOrigin + "/"})
	alice := v.user()
	fromWeb := stateOf(v, alice, map[string]string{"returnTo": webOrigin + "/settings/integrations?tab=github"})
	fromApp := stateOf(v, alice, nil)
	v.clock.advance(11 * time.Minute) // both expire
	require.Equal(t, web, location(v, fromWeb))
	require.Equal(t, app, location(v, fromApp), "an app sign-in still ends in the app")
	require.Equal(t, web, location(v, "forged"))
	require.Equal(t, web, location(v, ""))

	// A live web flow that was declined keeps its own returnTo.
	declined := stateOf(v, alice, map[string]string{"returnTo": webOrigin + "/settings/integrations?tab=github"})
	r := v.do(uuid.Nil, nil, http.MethodGet, "/v1/github-sync/callback?error=access_denied&state="+url.QueryEscape(declined), nil)
	require.Equal(t, webOrigin+"/settings/integrations?tab=github&status=error", r.header.Get("Location"))

	// Only a development origin: that is the web app there.
	dev := newEnvWith(t, []string{"http://localhost:5173"})
	require.Equal(t, "http://localhost:5173/settings/integrations?status=error", location(dev, "forged"))

	// No web origin at all: the app's scheme is all there is.
	none := newEnvWith(t, nil)
	require.Equal(t, app, location(none, "forged"))
	require.Equal(t, app, location(none, "w.forged"))
}

// Findings 2, 5 and 11 at connect.
func TestConnectRefusals(t *testing.T) {
	v := newEnv(t)
	alice := v.user()
	v.seedGitHub("ghu_alice", "octocat")
	session := v.signIn(alice, nil, "code-a", "ghu_alice")
	body := map[string]any{"session": session, "installationId": 7, "repositoryId": 456, "branch": "main", "mergeExisting": true}

	// The installation no longer covers the repository: GitHub answers the
	// token request with 422.
	v.gh.Lock()
	v.gh.InstallationRepos[7] = []int64{457}
	v.gh.Unlock()
	r := v.put(alice, nil, body)
	require.Equal(t, http.StatusNotFound, r.code, r.String())
	require.Equal(t, "repo_not_accessible", r.errCode())
	v.gh.Lock()
	v.gh.InstallationRepos[7] = []int64{456, 457}
	v.gh.Unlock()

	// A tree listing larger than the client reads is a repository too large,
	// not GitHub being down.
	entries := make([]githubapp.TreeEntry, 0, 70_000)
	for i := range 70_000 {
		entries = append(entries, githubapp.TreeEntry{
			Path: "notes/" + strings.Repeat("d", 40) + "/" + strconv.Itoa(i) + ".md", Mode: "100644", Type: "blob",
			SHA: strings.Repeat("a", 40), Size: 10,
		})
	}
	b := githubapp.Branch{Name: "main"}
	b.Commit.SHA = "c0ffee"
	b.Commit.Commit.Tree.SHA = "big"
	v.gh.Lock()
	v.gh.Branches["octocat/notes"] = []githubapp.Branch{b}
	v.gh.Trees["big"] = githubapp.Tree{Entries: entries}
	v.gh.Unlock()
	r = v.put(alice, nil, body)
	require.Equal(t, http.StatusRequestEntityTooLarge, r.code, r.String())
	require.Equal(t, "repo_too_large", r.errCode())

	// A root .gitignore is merged, not written as a vault file: it is not in
	// the summary's files, held files or conflicts.
	raw := vaultpaths.Paths{Root: v.root}.Raw(alice)
	require.NoError(t, os.MkdirAll(raw, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(raw, ".gitignore"), []byte("local\n"), 0o600))
	v.gh.Lock()
	v.gh.Trees["big"] = githubapp.Tree{Entries: []githubapp.TreeEntry{
		{Path: ".gitignore", Mode: "100644", Type: "blob", SHA: "1", Size: 12},
		{Path: "inbox/a.md", Mode: "100644", Type: "blob", SHA: "2", Size: 3},
	}}
	v.gh.Unlock()
	delete(body, "mergeExisting")
	r = v.put(alice, nil, body)
	require.Equal(t, "remote_not_empty", r.errCode(), r.String())
	var conflict struct {
		Summary githubsync.Summary `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(r.body, &conflict))
	require.Equal(t, githubsync.Summary{Files: 1, Notes: 1, Bytes: 15}, conflict.Summary)

	// An archived repository cannot be pushed to.
	v.gh.Lock()
	archived := v.gh.Repos[456]
	archived.Archived = true
	v.gh.Repos[456] = archived
	list := v.gh.UserRepos["ghu_alice"][7]
	list[0].Archived = true
	v.gh.Unlock()
	body["mergeExisting"] = true
	r = v.put(alice, nil, body)
	require.Equal(t, http.StatusConflict, r.code, r.String())
	require.Equal(t, "repo_archived", r.errCode())
	require.Nil(t, v.connection(alice))
	r = v.do(alice, nil, http.MethodGet, "/v1/vault/github-sync/repositories?session="+session+"&installationId=7", nil)
	require.Contains(t, r.String(), `"archived":true`)
}

// MARK: E4.8

// connectVault runs the whole connect flow for the standard stub repository.
func (v *env) connectVault(owner uuid.UUID, vault *uuid.UUID) {
	v.t.Helper()
	v.seedGitHub("ghu_"+owner.String(), "octocat")
	session := v.signIn(owner, vault, "code-"+owner.String(), "ghu_"+owner.String())
	r := v.put(owner, vault, map[string]any{"session": session, "installationId": 7, "repositoryId": 456, "branch": "main"})
	require.Equal(v.t, http.StatusAccepted, r.code, r.String())
}

// commit writes a note into the vault and commits it, as a save does once
// its debounce has passed.
func (v *env) commit(vault uuid.UUID, path, body string) string {
	v.t.Helper()
	full := filepath.Join(v.paths.Raw(vault), path)
	require.NoError(v.t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(v.t, os.WriteFile(full, []byte(body), 0o600))
	_, err := v.committer.Flush(context.Background(), vault, vaultgit.Message{Reason: "upload"})
	require.NoError(v.t, err)
	repo, err := vaultgit.Open(v.paths.Raw(vault))
	require.NoError(v.t, err)
	head, err := repo.Head()
	require.NoError(v.t, err)
	return head
}

// settled waits until the vault's row satisfies ok and no sync is running or
// queued. It reads the store, not the status route, which is rate-limited on
// the test's frozen clock.
func (v *env) settled(vault uuid.UUID, ok func(*githubsync.Connection) bool) {
	v.t.Helper()
	require.Eventually(v.t, func() bool {
		c, err := githubsync.NewStore(v.pool).Get(context.Background(), vault)
		return err == nil && c != nil && ok(c) && !v.syncer.Syncing(vault)
	}, 20*time.Second, 10*time.Millisecond)
}

type syncStatus struct {
	CanManage  bool `json:"canManage"`
	Connection *struct {
		State       string  `json:"state"`
		LastPushAt  *string `json:"lastPushAt"`
		PendingPush bool    `json:"pendingPush"`
		Syncing     bool    `json:"syncing"`
		Error       *struct {
			Code     string `json:"code"`
			Retrying bool   `json:"retrying"`
		} `json:"error"`
	} `json:"connection"`
}

func (v *env) status(user uuid.UUID, vault *uuid.UUID) syncStatus {
	v.t.Helper()
	r := v.do(user, vault, http.MethodGet, "/v1/vault/github-sync", nil)
	require.Equal(v.t, http.StatusOK, r.code, r.String())
	var st syncStatus
	require.NoError(v.t, json.Unmarshal(r.body, &st))
	return st
}

// D14: POST …/sync. Only the vault owner queues a sync and may put a stopped
// connection back to work; members may not, whatever their role; 6 per
// minute per vault. The status tells the truth about what is pending and what failed.
func TestSyncNow(t *testing.T) {
	v := newEnv(t)
	alice, bob, carol, dave := v.user(), v.user(), v.user(), v.user()
	_, vault := v.teamVault(alice, map[uuid.UUID]string{bob: "editor", carol: "viewer"})
	const route = "/v1/vault/github-sync/sync"

	r := v.do(alice, &vault, http.MethodPost, route, nil)
	require.Equal(t, http.StatusNotFound, r.code, r.String())
	require.Equal(t, "not_connected", r.errCode())
	require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, &vault, http.MethodPost, route, nil).code)

	v.connectVault(alice, &vault)
	st := v.status(bob, &vault)
	require.False(t, st.CanManage)
	require.False(t, st.Connection.PendingPush, "no history yet")
	require.Nil(t, st.Connection.LastPushAt)

	// GitHub is down when the first commit is made.
	v.gh.Lock()
	v.gh.Git.Status[456] = http.StatusServiceUnavailable
	v.gh.Unlock()
	head := v.commit(vault, "note.md", "hello")
	v.settled(vault, func(c *githubsync.Connection) bool { return c.ErrorCode != nil })
	st = v.status(carol, &vault)
	require.Equal(t, githubsync.StateActive, st.Connection.State)
	require.Equal(t, "sync_failed", st.Connection.Error.Code)
	require.True(t, st.Connection.Error.Retrying)
	require.True(t, st.Connection.PendingPush)
	require.Nil(t, st.Connection.LastPushAt)

	// GitHub is back. Only the owner may ask: a member who may write to the
	// vault cannot, nor a viewer, and someone outside cannot see it. None of
	// them starts a sync.
	v.gh.Lock()
	delete(v.gh.Git.Status, 456)
	v.gh.Unlock()
	for _, member := range []uuid.UUID{bob, carol} {
		r = v.do(member, &vault, http.MethodPost, route, nil)
		require.Equal(t, http.StatusForbidden, r.code, r.String())
		require.Equal(t, "owner_required", r.errCode())
	}
	r = v.do(dave, &vault, http.MethodPost, route, nil)
	require.Equal(t, http.StatusForbidden, r.code, r.String())
	require.NotEmpty(t, r.errCode())
	require.Empty(t, v.gh.Git.Tip(456, "main"), "a refused request pushed")
	require.NotNil(t, v.status(bob, &vault).Connection.Error, "a refused request cleared the wait")

	// The owner syncs now, without waiting for the retry.
	r = v.do(alice, &vault, http.MethodPost, route, nil)
	require.Equal(t, http.StatusAccepted, r.code, r.String())
	require.Contains(t, r.String(), `"canManage":true`)
	v.settled(vault, func(c *githubsync.Connection) bool { return c.RemoteSHA != nil && *c.RemoteSHA == head })
	require.Equal(t, head, v.gh.Git.Tip(456, "main"))
	st = v.status(bob, &vault)
	require.False(t, st.Connection.PendingPush)
	require.False(t, st.Connection.Syncing)
	require.Nil(t, st.Connection.Error)
	require.NotNil(t, st.Connection.LastPushAt)

	// Stopped for a rejected push; the owner fixed the branch rule.
	v.exec(`UPDATE github_sync_connections SET state = 'needs_attention', error_code = 'push_rejected', error_at = now(), remote_sha = NULL WHERE vault_id = $1`, vault)
	require.False(t, v.status(alice, &vault).Connection.Error.Retrying)
	r = v.do(alice, &vault, http.MethodPost, route, nil)
	require.Equal(t, http.StatusAccepted, r.code, r.String())
	v.settled(vault, func(c *githubsync.Connection) bool {
		return c.State == githubsync.StateActive && c.ErrorCode == nil && c.RemoteSHA != nil
	})
	require.False(t, v.status(alice, &vault).Connection.PendingPush)

	// An uninstalled App is not something "Sync now" can fix.
	v.exec(`UPDATE github_sync_connections SET state = 'needs_attention', error_code = 'installation_removed', error_at = now() WHERE vault_id = $1`, vault)
	r = v.do(alice, &vault, http.MethodPost, route, nil)
	require.Equal(t, http.StatusAccepted, r.code, r.String())
	require.Equal(t, githubsync.StateNeedsAttention, v.status(alice, &vault).Connection.State)

	// 6 per minute per vault: that was 4 that reached the limiter (the
	// members and the outsider never did).
	codes := map[int]int{}
	for range 6 {
		codes[v.do(alice, &vault, http.MethodPost, route, nil).code]++
	}
	require.Equal(t, map[int]int{http.StatusAccepted: 2, http.StatusTooManyRequests: 4}, codes)
	over := v.do(alice, &vault, http.MethodPost, route, nil)
	require.Equal(t, "rate_limited", over.errCode())
	require.NotEmpty(t, over.header.Get("Retry-After"))
	// Another vault has its own budget.
	require.Equal(t, http.StatusNotFound, v.do(alice, nil, http.MethodPost, route, nil).code)
	v.clock.advance(61 * time.Second)
	require.Equal(t, http.StatusAccepted, v.do(alice, &vault, http.MethodPost, route, nil).code, "a new window")
}

// D13: disconnect removes the origin remote from the vault's repository,
// besides the row and the cached token. Nothing is pushed afterwards.
func TestDisconnectRemovesTheRemote(t *testing.T) {
	v := newEnv(t)
	alice := v.user()
	v.connectVault(alice, nil)
	head := v.commit(alice, "note.md", "hello")
	v.settled(alice, func(c *githubsync.Connection) bool { return c.RemoteSHA != nil && *c.RemoteSHA == head })
	require.Equal(t, head, v.gh.Git.Tip(456, "main"))
	require.False(t, v.status(alice, nil).Connection.PendingPush)
	remote := func() string {
		t.Helper()
		repo, err := vaultgit.Open(v.paths.Raw(alice))
		require.NoError(t, err)
		u, err := repo.RemoteURL()
		require.NoError(t, err)
		return u
	}
	require.Equal(t, v.gh.URL+"/octocat/notes.git", remote())
	mints := v.gh.Mints()

	r := v.do(alice, nil, http.MethodDelete, "/v1/vault/github-sync/connection", nil)
	require.Equal(t, http.StatusNoContent, r.code, r.String())
	require.Empty(t, remote(), "the remote is gone when the request returns")
	require.Equal(t, []string{mints[len(mints)-1].Token}, v.gh.RevokedInstallations())
	cfg, err := os.ReadFile(filepath.Join(v.paths.Raw(alice), ".git", "config"))
	require.NoError(t, err)
	require.NotContains(t, string(cfg), "octocat")
	require.NotContains(t, v.logs.String(), mints[len(mints)-1].Token)

	// The vault and its history go on; GitHub hears nothing more.
	second := v.commit(alice, "note.md", "after")
	require.NotEqual(t, head, second)
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, head, v.gh.Git.Tip(456, "main"))
	require.Len(t, v.gh.Mints(), len(mints))
	require.Nil(t, v.status(alice, nil).Connection)
}
