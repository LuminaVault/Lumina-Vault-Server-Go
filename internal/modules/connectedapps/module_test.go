package connectedapps_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"lumina/internal/auth"
	"lumina/internal/config"
	core "lumina/internal/connectedapps"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/connectedapps"
	"lumina/internal/secretbox"
	"lumina/parity/scenario"
)

const (
	listPath     = "/v1/me/connected-apps"
	callbackPath = "/v1/connected-apps/oauth/callback"
	// 32 zero bytes, base64: a test-only master key.
	testMasterKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	webOrigin     = "https://app.lumina.test"
)

var (
	alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
	keys  = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

// permissive lets the stub authorization server's loopback URL through.
type permissive struct{}

func (permissive) Validate(_ context.Context, raw string) (*url.URL, error) { return url.Parse(raw) }

// asStub is Aaru's authorization server: discovery, registration, token and
// revocation. It hands out fixed fake tokens.
type asStub struct {
	srv *httptest.Server

	mu           sync.Mutex
	revokeForms  []url.Values
	revokeStatus int // 0 means 200
	tokenStatus  int // 0 means 200
	down         bool
}

func newAS(t *testing.T) *asStub {
	s := &asStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		down := s.down
		s.mu.Unlock()
		if down {
			http.Error(w, "upstream-secret-detail", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"authorization_servers": []string{s.srv.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": s.srv.URL, "authorization_endpoint": s.srv.URL + "/authorize",
			"token_endpoint": s.srv.URL + "/token", "registration_endpoint": s.srv.URL + "/register",
			"revocation_endpoint": s.srv.URL + "/revoke",
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "cid-1"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		status := s.tokenStatus
		s.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "acc-1", "refresh_token": "ref-1", "expires_in": 3600})
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.revokeForms = append(s.revokeForms, r.PostForm)
		status := s.revokeStatus
		s.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
		}
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *asStub) revoked() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.revokeForms...)
}

// forgets records Service.Forget calls.
type forgets struct {
	mu    sync.Mutex
	calls []string
}

func (f *forgets) Forget(user uuid.UUID, appID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, user.String()+"/"+appID)
}

type env struct {
	t       *testing.T
	pool    *pgxpool.Pool
	srv     http.Handler
	store   *core.Store
	oauth   *core.OAuth
	as      *asStub
	forgets *forgets
}

// setup builds the module over a real database and a stub authorization
// server. oauthReady false is a server without a public base URL.
func setup(t *testing.T, oauthReady bool) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	seed, err := os.ReadFile("../../../parity/seed/base.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(seed))
	require.NoError(t, err)

	as := newAS(t)
	box, err := secretbox.New(testMasterKey)
	require.NoError(t, err)
	reg := core.NewRegistry(config.Config{
		ConnectedAppsNorviqMCPURL: "https://norviq.test/mcp",
		ConnectedAppsAaruMCPURL:   as.srv.URL + "/mcp",
		ConnectedAppsKhepriMCPURL: "https://khepri.test/mcp",
		ConnectedAppsLociMCPURL:   "https://loci.test/mcp",
	})
	store := core.NewStore(pool, box)
	callbackURL := ""
	if oauthReady {
		callbackURL = "https://api.lumina.test" + callbackPath
	}
	oauth := core.NewOAuth(pool, reg, store, as.srv.Client(), permissive{}, callbackURL)
	fg := &forgets{}
	module := connectedapps.New(connectedapps.Options{
		Registry: reg, Store: store, OAuth: oauth, Service: fg,
		OAuthReady: oauthReady, WebReturnOrigins: []string{webOrigin},
	})
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: []httpserver.Module{module}})
	return env{t: t, pool: pool, srv: e, store: store, oauth: oauth, as: as, forgets: fg}
}

func (v env) jwt(user uuid.UUID) string {
	tok, err := scenario.Mint(keys, user)
	require.NoError(v.t, err)
	return tok
}

func (v env) do(bearer, method, path, body string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	v.srv.ServeHTTP(rec, req)
	return rec
}

type appRow struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Blurb       string  `json:"blurb"`
	Auth        string  `json:"auth"`
	Status      string  `json:"status"`
	ConnectedAt *string `json:"connectedAt"`
}

// list returns the user's rows by app id, plus the raw body.
func (v env) list(user uuid.UUID) (map[string]appRow, []appRow, string) {
	v.t.Helper()
	rec := v.do(v.jwt(user), http.MethodGet, listPath, "")
	require.Equal(v.t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []appRow
	require.NoError(v.t, json.Unmarshal(rec.Body.Bytes(), &rows))
	byID := map[string]appRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	return byID, rows, rec.Body.String()
}

// connectAaru runs connect and returns the state the authorize URL carries.
func (v env) connectAaru(user uuid.UUID, body string) string {
	v.t.Helper()
	rec := v.do(v.jwt(user), http.MethodPost, listPath+"/aaru/connect", body)
	require.Equal(v.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		AuthorizeURL string `json:"authorizeUrl"`
	}
	require.NoError(v.t, json.Unmarshal(rec.Body.Bytes(), &out))
	u, err := url.Parse(out.AuthorizeURL)
	require.NoError(v.t, err)
	require.Equal(v.t, v.as.srv.URL+"/authorize", u.Scheme+"://"+u.Host+u.Path)
	state := u.Query().Get("state")
	require.NotEmpty(v.t, state)
	return state
}

func (v env) callback(state, code string) (int, string) {
	v.t.Helper()
	rec := v.do("", http.MethodGet, callbackPath+"?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code), "")
	return rec.Code, rec.Header().Get("Location")
}

func TestListShowsRegistryWithStatus(t *testing.T) {
	v := setup(t, true)
	require.Equal(t, http.StatusUnauthorized, v.do("", http.MethodGet, listPath, "").Code)

	ctx := context.Background()
	require.NoError(t, v.store.Save(ctx, alice, core.Account{AppID: "khepri", AccessToken: "khepri-access", RefreshToken: "khepri-refresh"}))
	require.NoError(t, v.store.MarkNeedsReauth(ctx, alice, "khepri", "invalid_grant"))

	byID, rows, raw := v.list(alice)
	require.Len(t, rows, 4)
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
		require.NotEmpty(t, r.Name)
		require.NotEmpty(t, r.Blurb)
	}
	require.Equal(t, []string{"norviq", "aaru", "khepri", "loci"}, ids)
	require.Equal(t, appRow{ID: "norviq", Name: "Norviq", Blurb: byID["norviq"].Blurb, Auth: "pat", Status: "disconnected"}, byID["norviq"])
	require.Equal(t, "oauth", byID["aaru"].Auth)
	require.Equal(t, "disconnected", byID["aaru"].Status)
	require.Equal(t, "needs_reauth", byID["khepri"].Status)
	require.Nil(t, byID["khepri"].ConnectedAt, "connectedAt only when connected")
	require.NotContains(t, raw, "khepri-access")
	require.NotContains(t, raw, "khepri-refresh")
	require.NotContains(t, raw, "connectedAt")
}

func TestConnectPATThenList(t *testing.T) {
	v := setup(t, true)
	connect := func(app, body string) int {
		return v.do(v.jwt(alice), http.MethodPost, listPath+"/"+app+"/connect", body).Code
	}
	require.Equal(t, http.StatusUnauthorized, v.do("", http.MethodPost, listPath+"/norviq/connect", `{"token":"nvq_pat_abc123"}`).Code)
	require.Equal(t, http.StatusBadRequest, connect("norviq", `{"token":"wrong_prefix"}`))
	require.Equal(t, http.StatusBadRequest, connect("norviq", `{}`), "a PAT app needs a token")
	require.Equal(t, http.StatusBadRequest, connect("norviq", ``))
	require.Equal(t, http.StatusBadRequest, connect("aaru", `{"token":"nvq_pat_abc123"}`), "a token for an OAuth app is the wrong mode")
	byID, _, _ := v.list(alice)
	require.Equal(t, "disconnected", byID["norviq"].Status)

	rec := v.do(v.jwt(alice), http.MethodPost, listPath+"/norviq/connect", `{"token":"nvq_pat_abc123"}`)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.Empty(t, rec.Body.String())

	byID, _, raw := v.list(alice)
	require.Equal(t, "connected", byID["norviq"].Status)
	require.NotNil(t, byID["norviq"].ConnectedAt)
	require.NotContains(t, raw, "nvq_pat_abc123")
	acct, err := v.store.Get(context.Background(), alice, "norviq")
	require.NoError(t, err)
	require.Equal(t, "nvq_pat_abc123", acct.AccessToken)
}

func TestUserCannotSeeOrDeleteOthersConnection(t *testing.T) {
	v := setup(t, true)
	ctx := context.Background()
	require.NoError(t, v.store.Save(ctx, alice, core.Account{AppID: "aaru", AccessToken: "alice-access"}))

	byID, _, raw := v.list(bob)
	require.Equal(t, "disconnected", byID["aaru"].Status)
	require.NotContains(t, raw, "alice-access")

	// The vault header is not an identity: bob stays bob.
	req := httptest.NewRequest(http.MethodDelete, listPath+"/aaru", nil)
	req.Header.Set("Authorization", "Bearer "+v.jwt(bob))
	req.Header.Set("X-Vault-ID", alice.String())
	rec := httptest.NewRecorder()
	v.srv.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	acct, err := v.store.Get(ctx, alice, "aaru")
	require.NoError(t, err, "alice's row survives bob's delete")
	require.Equal(t, "alice-access", acct.AccessToken)
	byID, _, _ = v.list(alice)
	require.Equal(t, "connected", byID["aaru"].Status)
	require.Equal(t, []string{bob.String() + "/aaru"}, v.forgets.calls)
}

func TestUnknownAppIs404(t *testing.T) {
	v := setup(t, true)
	tok := v.jwt(alice)
	require.Equal(t, http.StatusNotFound, v.do(tok, http.MethodPost, listPath+"/nope/connect", `{"token":"nvq_pat_abc"}`).Code)
	require.Equal(t, http.StatusNotFound, v.do(tok, http.MethodPost, listPath+"/nope/connect", `{}`).Code)
	require.Equal(t, http.StatusNotFound, v.do(tok, http.MethodDelete, listPath+"/nope", "").Code)
	require.Empty(t, v.forgets.calls)
}

func TestCallbackBadStateRedirectsError(t *testing.T) {
	v := setup(t, true)
	for _, q := range []string{"?state=nope&code=secret-code", "?state=nope", "?code=secret-code", ""} {
		rec := v.do("", http.MethodGet, callbackPath+q, "")
		require.Equal(t, http.StatusFound, rec.Code, q)
		require.Equal(t, "luminavault://connected-apps?status=error", rec.Header().Get("Location"), q)
	}
}

func TestOAuthConnectThenCallback(t *testing.T) {
	v := setup(t, true)
	// No body at all is a plain app-initiated connect.
	state := v.connectAaru(alice, "")
	code, loc := v.callback(state, "the-code")
	require.Equal(t, http.StatusFound, code)
	require.Equal(t, "luminavault://connected-apps/aaru?status=connected", loc)
	byID, _, raw := v.list(alice)
	require.Equal(t, "connected", byID["aaru"].Status)
	require.NotNil(t, byID["aaru"].ConnectedAt)
	require.NotContains(t, raw, "acc-1")
	require.NotContains(t, raw, "ref-1")
	byID, _, _ = v.list(bob)
	require.Equal(t, "disconnected", byID["aaru"].Status)

	// A state works once.
	code, loc = v.callback(state, "the-code")
	require.Equal(t, http.StatusFound, code)
	require.Equal(t, "luminavault://connected-apps?status=error", loc)
}

func TestCallbackHonoursAllowedReturnTo(t *testing.T) {
	v := setup(t, true)
	for _, bad := range []string{"https://evil.test/settings", "luminavault://x", "/relative", webOrigin + "/s#frag", "https://user@app.lumina.test/"} {
		body, _ := json.Marshal(map[string]string{"returnTo": bad})
		rec := v.do(v.jwt(alice), http.MethodPost, listPath+"/aaru/connect", string(body))
		require.Equal(t, http.StatusBadRequest, rec.Code, bad)
	}
	back := webOrigin + "/settings/apps"
	body, _ := json.Marshal(map[string]string{"returnTo": back})
	state := v.connectAaru(alice, string(body))
	code, loc := v.callback(state, "the-code")
	require.Equal(t, http.StatusFound, code)
	require.Equal(t, back+"?status=connected", loc)

	// A returnTo that already has a query keeps it.
	withQuery := webOrigin + "/settings/apps?tab=apps"
	body, _ = json.Marshal(map[string]string{"returnTo": withQuery})
	code, loc = v.callback(v.connectAaru(alice, string(body)), "the-code")
	require.Equal(t, http.StatusFound, code)
	require.Equal(t, withQuery+"&status=connected", loc)
}

// A provider decline has a state and no code. It consumes the state and sends
// the browser back with a status only.
func TestCallbackDeclineConsumesStateAndReturns(t *testing.T) {
	v := setup(t, true)
	back := webOrigin + "/settings/apps"
	body, _ := json.Marshal(map[string]string{"returnTo": back})
	state := v.connectAaru(alice, string(body))
	decline := callbackPath + "?error=access_denied&error_description=user+said+no&state=" + url.QueryEscape(state)
	rec := v.do("", http.MethodGet, decline, "")
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, back+"?status=error", rec.Header().Get("Location"))

	// The state is gone: a later code for it is rejected, and nothing connected.
	code, loc := v.callback(state, "the-code")
	require.Equal(t, http.StatusFound, code)
	require.Equal(t, "luminavault://connected-apps?status=error", loc)
	byID, _, _ := v.list(alice)
	require.Equal(t, "disconnected", byID["aaru"].Status)

	// Without a returnTo the decline lands in the iOS app, naming the app.
	state = v.connectAaru(alice, "")
	rec = v.do("", http.MethodGet, callbackPath+"?error=access_denied&state="+url.QueryEscape(state), "")
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "luminavault://connected-apps/aaru?status=error", rec.Header().Get("Location"))
}

// A local failure while starting the flow is ours (500), not the app's (502).
func TestConnectLocalFailureIs500(t *testing.T) {
	v := setup(t, true)
	_, err := v.pool.Exec(context.Background(), `DROP TABLE connected_app_clients`)
	require.NoError(t, err)
	rec := v.do(v.jwt(alice), http.MethodPost, listPath+"/aaru/connect", `{}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.JSONEq(t, `{"error":{"message":"Something went wrong. Try again later."}}`, rec.Body.String())
}

// The session's returnTo is checked again at the callback: a value that got
// past connect some other way still cannot send the browser off-site.
func TestCallbackRevalidatesReturnTo(t *testing.T) {
	v := setup(t, true)
	evil := "https://evil.test/x"
	au, err := v.oauth.Start(context.Background(), alice, "aaru", &evil)
	require.NoError(t, err)
	u, err := url.Parse(au)
	require.NoError(t, err)
	code, loc := v.callback(u.Query().Get("state"), "the-code")
	require.Equal(t, http.StatusFound, code)
	require.Equal(t, "luminavault://connected-apps/aaru?status=connected", loc)
}

func TestCallbackExchangeFailureRedirectsErrorForApp(t *testing.T) {
	v := setup(t, true)
	webState := v.connectAaru(alice, `{"returnTo":"`+webOrigin+`/settings"}`)
	appState := v.connectAaru(alice, "")
	v.as.mu.Lock()
	v.as.tokenStatus = http.StatusBadRequest
	v.as.mu.Unlock()
	code, loc := v.callback(webState, "secret-code")
	require.Equal(t, http.StatusFound, code)
	require.Equal(t, webOrigin+"/settings?status=error", loc)
	code, loc = v.callback(appState, "secret-code")
	require.Equal(t, http.StatusFound, code)
	require.Equal(t, "luminavault://connected-apps/aaru?status=error", loc)
	byID, _, _ := v.list(alice)
	require.Equal(t, "disconnected", byID["aaru"].Status)
}

func TestConnectUpstreamFailureIs502WithoutRemoteText(t *testing.T) {
	v := setup(t, true)
	v.as.mu.Lock()
	v.as.down = true
	v.as.mu.Unlock()
	rec := v.do(v.jwt(alice), http.MethodPost, listPath+"/aaru/connect", `{}`)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.JSONEq(t, `{"error":{"message":"Could not reach Aaru. Try again later."}}`, rec.Body.String())
	require.NotContains(t, rec.Body.String(), v.as.srv.URL)
}

func TestOAuthConnectWithoutPublicBaseURLIs503(t *testing.T) {
	v := setup(t, false)
	require.Equal(t, http.StatusServiceUnavailable, v.do(v.jwt(alice), http.MethodPost, listPath+"/aaru/connect", `{}`).Code)
	require.Equal(t, http.StatusNoContent, v.do(v.jwt(alice), http.MethodPost, listPath+"/norviq/connect", `{"token":"nvq_pat_abc123"}`).Code)
}

func TestDeleteRevokesDeletesAndForgets(t *testing.T) {
	v := setup(t, true)
	ctx := context.Background()
	code, _ := v.callback(v.connectAaru(alice, ""), "the-code")
	require.Equal(t, http.StatusFound, code)
	require.Equal(t, http.StatusUnauthorized, v.do("", http.MethodDelete, listPath+"/aaru", "").Code)

	rec := v.do(v.jwt(alice), http.MethodDelete, listPath+"/aaru", "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	forms := v.as.revoked()
	require.Len(t, forms, 1)
	require.Equal(t, "ref-1", forms[0].Get("token"))
	require.Equal(t, "refresh_token", forms[0].Get("token_type_hint"))
	require.Equal(t, "cid-1", forms[0].Get("client_id"))
	_, err := v.store.Get(ctx, alice, "aaru")
	require.ErrorIs(t, err, core.ErrNotConnected)
	require.Equal(t, []string{alice.String() + "/aaru"}, v.forgets.calls)

	// Deleting again is still 204 and revokes nothing.
	require.Equal(t, http.StatusNoContent, v.do(v.jwt(alice), http.MethodDelete, listPath+"/aaru", "").Code)
	require.Len(t, v.as.revoked(), 1)

	// A failing revocation endpoint does not block the disconnect.
	code, _ = v.callback(v.connectAaru(alice, ""), "the-code")
	require.Equal(t, http.StatusFound, code)
	v.as.mu.Lock()
	v.as.revokeStatus = http.StatusInternalServerError
	v.as.mu.Unlock()
	require.Equal(t, http.StatusNoContent, v.do(v.jwt(alice), http.MethodDelete, listPath+"/aaru", "").Code)
	_, err = v.store.Get(ctx, alice, "aaru")
	require.ErrorIs(t, err, core.ErrNotConnected)

	// A PAT app has nothing to revoke.
	require.Equal(t, http.StatusNoContent, v.do(v.jwt(alice), http.MethodPost, listPath+"/norviq/connect", `{"token":"nvq_pat_abc123"}`).Code)
	require.Equal(t, http.StatusNoContent, v.do(v.jwt(alice), http.MethodDelete, listPath+"/norviq", "").Code)
	_, err = v.store.Get(ctx, alice, "norviq")
	require.ErrorIs(t, err, core.ErrNotConnected)
	require.Len(t, v.as.revoked(), 2)
}
