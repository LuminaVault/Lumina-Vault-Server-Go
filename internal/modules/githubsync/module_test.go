package githubsync_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/githubapp"
	"lumina/internal/githubapp/githubapptest"
	"lumina/internal/githubsync"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultpaths"
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

	module "lumina/internal/modules/githubsync"
)

var testKeys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

const webOrigin = "https://app.lumina.test"

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	gh     *githubapptest.Server
	module *module.Module
	e      *echo.Echo
	root   string
	logs   *syncBuffer
	bodies *syncBuffer
	clock  *clock
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

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	gh := githubapptest.New(t)
	logs := &syncBuffer{}
	root := t.TempDir()
	clk := &clock{t: time.Now()}
	m := module.New(module.Options{
		Client: gh.Client(t), Store: githubsync.NewStore(pool), Access: vaultaccess.New(pool),
		Paths: vaultpaths.Paths{Root: root}, CallbackURL: "https://api.lumina.test/v1/github-sync/callback",
		Slug: "luminavault", MaxRepoBytes: 200 << 20, MaxFiles: 5000,
		WebReturnOrigins: []string{webOrigin},
		Log:              slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	m.SetClock(clk.now)
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(testKeys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{m},
	})
	return &env{t: t, pool: pool, gh: gh, module: m, e: e, root: root, logs: logs, bodies: &syncBuffer{}, clock: clk}
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

	// A forged callback: someone else's installation id, no valid state.
	for _, q := range []string{"installation_id=99&setup_action=install", "state=forged&code=x&installation_id=99"} {
		r := v.do(uuid.Nil, nil, http.MethodGet, "/v1/github-sync/callback?"+q, nil)
		require.Equal(t, http.StatusFound, r.code)
		require.Equal(t, "luminavault://github-sync?status=error", r.header.Get("Location"))
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
func TestNoSecretsInResponsesOrLogs(t *testing.T) {
	v := newEnv(t)
	alice := v.user()
	v.seedGitHub("ghu_alice_secret_token", "octocat")
	session := v.signIn(alice, nil, "code-secret-value", "ghu_alice_secret_token")
	r := v.put(alice, nil, map[string]any{"session": session, "installationId": 7, "repositoryId": 456})
	require.Equal(t, http.StatusAccepted, r.code, r.String())
	r = v.do(alice, nil, http.MethodGet, "/v1/vault/github-sync", nil)
	require.Equal(t, http.StatusOK, r.code)
	require.Equal(t, http.StatusNoContent, v.do(alice, nil, http.MethodDelete, "/v1/vault/github-sync/connection", nil).code)

	secrets := []string{"ghu_alice_secret_token", "code-secret-value", githubapptest.ClientSecret}
	for _, m := range v.gh.Mints() {
		secrets = append(secrets, m.Token)
	}
	for _, s := range secrets {
		require.NotContains(t, v.bodies.String(), s)
		require.NotContains(t, v.logs.String(), s)
	}
	require.NotContains(t, v.logs.String(), session)
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
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, ExcludeRequestBody: true},
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
	call(alice, http.MethodDelete, "/v1/vault/github-sync/connection", nil)
	cb := v.do(uuid.Nil, nil, http.MethodGet, "/v1/github-sync/callback?state=x", nil)
	check(http.MethodGet, "/v1/github-sync/callback", cb)
}
