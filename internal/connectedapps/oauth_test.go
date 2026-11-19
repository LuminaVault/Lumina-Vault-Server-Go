package connectedapps_test

import (
	"context"
	"encoding/json"
	"lumina/internal/config"
	"lumina/internal/connectedapps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// permissive allows every URL; client_test.go's allowAll is in the internal
// test package and cannot be imported here.
type permissive struct{}

func (permissive) Validate(_ context.Context, raw string) (*url.URL, error) { return url.Parse(raw) }

type rejecting struct{}

func (rejecting) Validate(context.Context, string) (*url.URL, error) { return nil, context.Canceled }

type asStub struct {
	srv       *httptest.Server
	regs      atomic.Int32
	refreshes atomic.Int32
	mu        sync.Mutex
	lastForm  url.Values
	issuer    string // override; empty means srv.URL
	refreshFn func(w http.ResponseWriter)

	revokes atomic.Int32
	// guarded by mu
	revokeForm   url.Values
	revokeStatus int  // 0 means 200
	noRevocation bool // leave revocation_endpoint out of the metadata
}

func newAS(t *testing.T) *asStub {
	s := &asStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"authorization_servers": []string{s.srv.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		iss := s.srv.URL
		if s.issuer != "" {
			iss = s.issuer
		}
		meta := map[string]any{
			"issuer": iss, "authorization_endpoint": s.srv.URL + "/authorize",
			"token_endpoint": s.srv.URL + "/token", "registration_endpoint": s.srv.URL + "/register",
		}
		s.mu.Lock()
		if !s.noRevocation {
			meta["revocation_endpoint"] = s.srv.URL + "/revoke"
		}
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(meta)
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		s.revokes.Add(1)
		s.mu.Lock()
		s.revokeForm = r.PostForm
		status := s.revokeStatus
		s.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
		}
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		s.regs.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.lastForm = url.Values{"auth": {body["token_endpoint_auth_method"].(string)}, "redir": {body["redirect_uris"].([]any)[0].(string)}}
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "cid-1"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		s.mu.Lock()
		s.lastForm = r.PostForm
		s.mu.Unlock()
		if r.PostForm.Get("grant_type") == "refresh_token" {
			s.refreshes.Add(1)
			time.Sleep(50 * time.Millisecond)
			if s.refreshFn != nil {
				s.refreshFn(w)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "acc-2", "expires_in": 3600})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "acc-1", "refresh_token": "ref-1", "expires_in": 10, "scope": "read"})
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func newOAuth(t *testing.T, s *asStub) (*connectedapps.OAuth, *connectedapps.Store) {
	store, pool := newStore(t)
	reg := connectedapps.NewRegistry(config.Config{
		ConnectedAppsAaruMCPURL:   s.srv.URL + "/mcp",
		ConnectedAppsNorviqMCPURL: "https://norviq.test/mcp",
	})
	return connectedapps.NewOAuth(pool, reg, store, s.srv.Client(), permissive{}, "https://lumina.test/cb"), store
}

func stateOf(t *testing.T, authURL string) (string, url.Values) {
	u, err := url.Parse(authURL)
	require.NoError(t, err)
	return u.Query().Get("state"), u.Query()
}

func TestStartDiscoversRegistersAndBuildsURL(t *testing.T) {
	s := newAS(t)
	o, _ := newOAuth(t, s)
	got, err := o.Start(context.Background(), alice, "aaru", nil)
	require.NoError(t, err)
	_, q := stateOf(t, got)
	require.Equal(t, "code", q.Get("response_type"))
	require.Equal(t, "S256", q.Get("code_challenge_method"))
	require.NotEmpty(t, q.Get("code_challenge"))
	require.NotEmpty(t, q.Get("state"))
	require.Equal(t, s.srv.URL+"/mcp", q.Get("resource"))
	require.Equal(t, "cid-1", q.Get("client_id"))
	require.Equal(t, "https://lumina.test/cb", q.Get("redirect_uri"))
	s.mu.Lock()
	require.Equal(t, "none", s.lastForm.Get("auth"))
	require.Equal(t, "https://lumina.test/cb", s.lastForm.Get("redir"))
	s.mu.Unlock()

	_, err = o.Start(context.Background(), bob, "aaru", nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, s.regs.Load())
}

func TestStartModeAndGuard(t *testing.T) {
	s := newAS(t)
	o, _ := newOAuth(t, s)
	_, err := o.Start(context.Background(), alice, "norviq", nil)
	require.ErrorIs(t, err, connectedapps.ErrNotOAuth)
	_, err = o.Start(context.Background(), alice, "nope", nil)
	require.ErrorIs(t, err, connectedapps.ErrUnknownApp)

	store, pool := newStore(t)
	reg := connectedapps.NewRegistry(config.Config{ConnectedAppsAaruMCPURL: s.srv.URL + "/mcp"})
	rej := connectedapps.NewOAuth(pool, reg, store, s.srv.Client(), rejecting{}, "https://lumina.test/cb")
	_, err = rej.Start(context.Background(), alice, "aaru", nil)
	require.Error(t, err)
}

func TestStartRejectsIssuerMismatch(t *testing.T) {
	s := newAS(t)
	s.issuer = "https://evil.test"
	o, _ := newOAuth(t, s)
	_, err := o.Start(context.Background(), alice, "aaru", nil)
	require.ErrorContains(t, err, "issuer mismatch")
	require.EqualValues(t, 0, s.regs.Load())
}

func finish(t *testing.T, o *connectedapps.OAuth) {
	t.Helper()
	rt := "/lists"
	au, err := o.Start(context.Background(), alice, "aaru", &rt)
	require.NoError(t, err)
	st, _ := stateOf(t, au)
	app, ret, err := o.Finish(context.Background(), st, "the-code")
	require.NoError(t, err)
	require.Equal(t, "aaru", app)
	require.Equal(t, "/lists", *ret)
}

func TestFinishExchangesAndSeals(t *testing.T) {
	s := newAS(t)
	o, store := newOAuth(t, s)
	finish(t, o)
	s.mu.Lock()
	f := s.lastForm
	s.mu.Unlock()
	require.Equal(t, "authorization_code", f.Get("grant_type"))
	require.Equal(t, "the-code", f.Get("code"))
	require.NotEmpty(t, f.Get("code_verifier"))
	require.Equal(t, "cid-1", f.Get("client_id"))
	require.Equal(t, s.srv.URL+"/mcp", f.Get("resource"))
	require.Equal(t, "https://lumina.test/cb", f.Get("redirect_uri"))
	require.Empty(t, f.Get("client_secret"))

	a, err := store.Get(context.Background(), alice, "aaru")
	require.NoError(t, err)
	require.Equal(t, "acc-1", a.AccessToken)
	require.Equal(t, "ref-1", a.RefreshToken)
	require.NotNil(t, a.ExpiresAt)
}

func TestFinishRejectsUnknownOrReusedState(t *testing.T) {
	s := newAS(t)
	o, _ := newOAuth(t, s)
	_, _, err := o.Finish(context.Background(), "nope", "c")
	require.ErrorIs(t, err, connectedapps.ErrBadState)
	au, err := o.Start(context.Background(), alice, "aaru", nil)
	require.NoError(t, err)
	st, _ := stateOf(t, au)
	_, _, err = o.Finish(context.Background(), st, "c")
	require.NoError(t, err)
	_, _, err = o.Finish(context.Background(), st, "c")
	require.ErrorIs(t, err, connectedapps.ErrBadState)
}

func TestCallRefreshesOnce(t *testing.T) {
	s := newAS(t)
	o, store := newOAuth(t, s)
	finish(t, o) // expires in 10s
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := o.Fresh(context.Background(), alice, "aaru")
			require.NoError(t, err)
			require.Equal(t, "acc-2", tok)
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, s.refreshes.Load())
	a, err := store.Get(context.Background(), alice, "aaru")
	require.NoError(t, err)
	require.Equal(t, "ref-1", a.RefreshToken, "old refresh token kept when response omits it")
	s.mu.Lock()
	require.Equal(t, "ref-1", s.lastForm.Get("refresh_token"))
	s.mu.Unlock()
}

func TestFreshSkipsRefreshWhenValid(t *testing.T) {
	s := newAS(t)
	o, store := newOAuth(t, s)
	exp := time.Now().Add(time.Hour)
	require.NoError(t, store.Save(context.Background(), alice, connectedapps.Account{
		AppID: "aaru", AccessToken: "live", RefreshToken: "r", ExpiresAt: &exp,
	}))
	tok, err := o.Fresh(context.Background(), alice, "aaru")
	require.NoError(t, err)
	require.Equal(t, "live", tok)
	require.EqualValues(t, 0, s.refreshes.Load())
	_, err = o.Fresh(context.Background(), bob, "aaru")
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)
}

func TestCallMarksNeedsReauth(t *testing.T) {
	s := newAS(t)
	s.refreshFn = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	}
	o, store := newOAuth(t, s)
	finish(t, o)
	_, err := o.Fresh(context.Background(), alice, "aaru")
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)
	a, err := store.Get(context.Background(), alice, "aaru")
	require.NoError(t, err)
	require.Equal(t, "needs_reauth", a.Status)
}

func TestRefreshServerErrorLeavesRow(t *testing.T) {
	s := newAS(t)
	s.refreshFn = func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) }
	o, store := newOAuth(t, s)
	finish(t, o)
	_, err := o.Fresh(context.Background(), alice, "aaru")
	require.Error(t, err)
	require.NotErrorIs(t, err, connectedapps.ErrNotConnected)
	a, err := store.Get(context.Background(), alice, "aaru")
	require.NoError(t, err)
	require.Equal(t, "connected", a.Status)
}

func TestSavePATValidatesPrefix(t *testing.T) {
	s := newAS(t)
	o, store := newOAuth(t, s)
	ctx := context.Background()
	require.NoError(t, o.SavePAT(ctx, alice, "norviq", "nvq_pat_x"))
	require.ErrorIs(t, o.SavePAT(ctx, alice, "norviq", "abc"), connectedapps.ErrBadPAT)
	require.ErrorIs(t, o.SavePAT(ctx, alice, "aaru", "nvq_pat_x"), connectedapps.ErrNotOAuth)
	tok, err := o.Fresh(ctx, alice, "norviq")
	require.NoError(t, err)
	require.Equal(t, "nvq_pat_x", tok)
	_, err = store.Get(ctx, alice, "norviq")
	require.NoError(t, err)
}

func TestRefreshForcesOnceThenReusesRotatedToken(t *testing.T) {
	s := newAS(t)
	o, store := newOAuth(t, s)
	finish(t, o) // registers the client and stores acc-1
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)
	require.NoError(t, store.Save(ctx, alice, connectedapps.Account{
		AppID: "aaru", AccessToken: "live", RefreshToken: "r", ExpiresAt: &exp,
	}))

	tok, err := o.Refresh(ctx, alice, "aaru", "live")
	require.NoError(t, err)
	require.Equal(t, "acc-2", tok)
	require.EqualValues(t, 1, s.refreshes.Load())

	// A second caller that got a 401 on the same stale token reuses the new one.
	tok, err = o.Refresh(ctx, alice, "aaru", "live")
	require.NoError(t, err)
	require.Equal(t, "acc-2", tok)
	require.EqualValues(t, 1, s.refreshes.Load())

	_, err = o.Refresh(ctx, alice, "norviq", "x")
	require.ErrorIs(t, err, connectedapps.ErrNotOAuth)
	_, err = o.Refresh(ctx, bob, "aaru", "x")
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)
}

func TestRevokePostsRefreshTokenToRevocationEndpoint(t *testing.T) {
	s := newAS(t)
	o, _ := newOAuth(t, s)
	finish(t, o)
	require.NoError(t, o.Revoke(context.Background(), alice, "aaru"))
	require.EqualValues(t, 1, s.revokes.Load())
	s.mu.Lock()
	f := s.revokeForm
	s.mu.Unlock()
	require.Equal(t, "ref-1", f.Get("token"))
	require.Equal(t, "refresh_token", f.Get("token_type_hint"))
	require.Equal(t, "cid-1", f.Get("client_id"))

	// A failing endpoint is an error that never carries the token.
	s.mu.Lock()
	s.revokeStatus = http.StatusInternalServerError
	s.mu.Unlock()
	err := o.Revoke(context.Background(), alice, "aaru")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "ref-1")
}

func TestRevokeSkipsWhenNothingToRevoke(t *testing.T) {
	s := newAS(t)
	o, store := newOAuth(t, s)
	// Not connected: nothing to do.
	require.NoError(t, o.Revoke(context.Background(), alice, "aaru"))
	// Connected without a refresh token.
	require.NoError(t, store.Save(context.Background(), bob, connectedapps.Account{AppID: "aaru", AccessToken: "acc"}))
	require.NoError(t, o.Revoke(context.Background(), bob, "aaru"))
	// The server advertises no revocation endpoint.
	finish(t, o)
	s.mu.Lock()
	s.noRevocation = true
	s.mu.Unlock()
	require.NoError(t, o.Revoke(context.Background(), alice, "aaru"))
	require.Zero(t, s.revokes.Load())

	require.ErrorIs(t, o.Revoke(context.Background(), alice, "norviq"), connectedapps.ErrNotOAuth)
	require.ErrorIs(t, o.Revoke(context.Background(), alice, "nope"), connectedapps.ErrUnknownApp)
}

func TestFinishFailureNamesTheApp(t *testing.T) {
	s := newAS(t)
	o, _ := newOAuth(t, s)
	au, err := o.Start(context.Background(), alice, "aaru", nil)
	require.NoError(t, err)
	st, _ := stateOf(t, au)
	s.srv.Close() // the exchange cannot reach the token endpoint
	app, _, err := o.Finish(context.Background(), st, "c")
	require.Error(t, err)
	require.Equal(t, "aaru", app)
}

// R17: a 307 would make Go re-send the form (refresh token included) to the
// redirect target, which the guard never saw.
func TestTokenEndpointRedirectIsRefused(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "stolen", "expires_in": 3600})
	}))
	defer other.Close()
	s := newAS(t)
	s.refreshFn = func(w http.ResponseWriter) {
		w.Header().Set("Location", other.URL+"/token")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}
	o, store := newOAuth(t, s)
	caller := s.srv.Client()
	require.Nil(t, caller.CheckRedirect, "the caller's client is left alone")
	finish(t, o)
	_, err := o.Fresh(context.Background(), alice, "aaru")
	require.Error(t, err)
	require.NotContains(t, err.Error(), other.URL)
	require.NotContains(t, err.Error(), "ref-1")
	require.Zero(t, hits.Load(), "the redirect target must never be contacted")
	require.Nil(t, s.srv.Client().CheckRedirect)
	a, err := store.Get(context.Background(), alice, "aaru")
	require.NoError(t, err)
	require.Equal(t, "acc-1", a.AccessToken)
}

// A decline: the state is taken, the app and returnTo come back, and the
// token endpoint is never asked.
func TestFinishWithoutCodeConsumesState(t *testing.T) {
	s := newAS(t)
	o, _ := newOAuth(t, s)
	rt := "https://app.test/settings"
	au, err := o.Start(context.Background(), alice, "aaru", &rt)
	require.NoError(t, err)
	st, _ := stateOf(t, au)
	s.mu.Lock()
	s.lastForm = nil
	s.mu.Unlock()
	app, ret, err := o.Finish(context.Background(), st, "")
	require.ErrorIs(t, err, connectedapps.ErrNoCode)
	require.Equal(t, "aaru", app)
	require.Equal(t, rt, *ret)
	s.mu.Lock()
	require.Nil(t, s.lastForm, "no token request for a decline")
	s.mu.Unlock()
	_, _, err = o.Finish(context.Background(), st, "c")
	require.ErrorIs(t, err, connectedapps.ErrBadState)
}

// R20: Start says whether the remote side or this server failed.
func TestStartTagsUpstreamFailuresOnly(t *testing.T) {
	s := newAS(t)
	o, _ := newOAuth(t, s)
	// The guard rejecting the app's metadata URL is an upstream failure.
	bad := connectedapps.NewOAuth(nil, connectedapps.NewRegistry(config.Config{ConnectedAppsAaruMCPURL: s.srv.URL + "/mcp"}),
		nil, s.srv.Client(), rejecting{}, "https://lumina.test/cb")
	_, err := bad.Start(context.Background(), alice, "aaru", nil)
	require.ErrorIs(t, err, connectedapps.ErrUpstream)

	// A database failure while caching the client is local.
	_, err = o.Pool().Exec(context.Background(), `DROP TABLE connected_app_clients`)
	require.NoError(t, err)
	_, err = o.Start(context.Background(), alice, "aaru", nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, connectedapps.ErrUpstream)
}

// blockRefresh makes the stub's refresh answer wait for release. entered is
// closed once the first refresh request has arrived.
func blockRefresh(s *asStub) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.refreshFn = func(w http.ResponseWriter) {
		once.Do(func() { close(entered) })
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "acc-2", "refresh_token": "ref-2", "expires_in": 3600})
	}
	return entered, release
}

// A refresh that answers after the row was deleted must not write it back.
func TestRefreshNeverRecreatesADeletedRow(t *testing.T) {
	s := newAS(t)
	entered, release := blockRefresh(s)
	o, store := newOAuth(t, s)
	finish(t, o) // expires in 10s
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		_, err := o.Fresh(ctx, alice, "aaru")
		done <- err
	}()
	<-entered
	require.NoError(t, store.Delete(ctx, alice, "aaru"))
	close(release)

	require.ErrorIs(t, <-done, connectedapps.ErrNotConnected)
	_, err := store.Get(ctx, alice, "aaru")
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)
}

func TestRefreshTransientStatusLeavesRowConnected(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusForbidden} {
		s := newAS(t)
		s.refreshFn = func(w http.ResponseWriter) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
		}
		o, store := newOAuth(t, s)
		finish(t, o)
		_, err := o.Fresh(context.Background(), alice, "aaru")
		require.Error(t, err, status)
		require.NotErrorIs(t, err, connectedapps.ErrNotConnected, status)
		require.Contains(t, err.Error(), "token refresh failed", status)
		a, err := store.Get(context.Background(), alice, "aaru")
		require.NoError(t, err)
		require.Equal(t, "connected", a.Status, status)
		require.Equal(t, "acc-1", a.AccessToken, "row untouched")
	}
}

func TestRefreshUnauthorizedMarksNeedsReauth(t *testing.T) {
	s := newAS(t)
	s.refreshFn = func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) }
	o, store := newOAuth(t, s)
	finish(t, o)
	_, err := o.Fresh(context.Background(), alice, "aaru")
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)
	a, err := store.Get(context.Background(), alice, "aaru")
	require.NoError(t, err)
	require.Equal(t, "needs_reauth", a.Status)
}

// Disconnect waits for a refresh in flight, then revokes the token that
// refresh left behind and deletes the row: nothing writes it back.
func TestDisconnectDuringRefreshLeavesNoRow(t *testing.T) {
	s := newAS(t)
	entered, release := blockRefresh(s)
	o, store := newOAuth(t, s)
	finish(t, o) // expires in 10s
	ctx := context.Background()

	refreshed := make(chan error, 1)
	go func() {
		_, err := o.Fresh(ctx, alice, "aaru")
		refreshed <- err
	}()
	<-entered
	disconnected := make(chan [2]error, 1)
	go func() {
		revokeErr, err := o.Disconnect(ctx, alice, "aaru")
		disconnected <- [2]error{revokeErr, err}
	}()
	select {
	case <-disconnected:
		t.Fatal("disconnect did not wait for the refresh")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)

	require.NoError(t, <-refreshed)
	errs := <-disconnected
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	_, err := store.Get(ctx, alice, "aaru")
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)
	_, err = o.Fresh(ctx, alice, "aaru")
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)
	require.EqualValues(t, 1, s.revokes.Load())
	s.mu.Lock()
	require.Equal(t, "ref-2", s.revokeForm.Get("token"), "the token the refresh left, not the one it replaced")
	s.mu.Unlock()
}

// The delete does not depend on the revoke or on the caller staying around.
func TestDisconnectDeletesWhateverTheRevokeDid(t *testing.T) {
	s := newAS(t)
	s.revokeStatus = http.StatusInternalServerError
	o, store := newOAuth(t, s)
	finish(t, o)

	revokeErr, err := o.Disconnect(context.Background(), alice, "aaru")
	require.Error(t, revokeErr)
	require.NotContains(t, revokeErr.Error(), "ref-1")
	require.NoError(t, err)
	_, err = store.Get(context.Background(), alice, "aaru")
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)

	// A caller that has already gone: the revoke fails, the row still goes.
	finish(t, o)
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	revokeErr, err = o.Disconnect(gone, alice, "aaru")
	require.Error(t, revokeErr)
	require.NoError(t, err)
	_, err = store.Get(context.Background(), alice, "aaru")
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)

	// Never connected: nothing to revoke, nothing to delete, no error.
	revokeErr, err = o.Disconnect(context.Background(), bob, "aaru")
	require.NoError(t, revokeErr)
	require.NoError(t, err)
	_, err = o.Disconnect(context.Background(), alice, "evil")
	require.ErrorIs(t, err, connectedapps.ErrUnknownApp)
}

func TestDisconnectPATAppDeletesWithoutRevoke(t *testing.T) {
	s := newAS(t)
	o, store := newOAuth(t, s)
	ctx := context.Background()
	require.NoError(t, o.SavePAT(ctx, alice, "norviq", "nvq_pat_abc"))

	revokeErr, err := o.Disconnect(ctx, alice, "norviq")
	require.NoError(t, revokeErr)
	require.NoError(t, err)
	_, err = store.Get(ctx, alice, "norviq")
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)
	require.EqualValues(t, 0, s.revokes.Load())
}
