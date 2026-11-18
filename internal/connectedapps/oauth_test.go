package connectedapps_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"lumina/internal/config"
	"lumina/internal/connectedapps"
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
}

func newAS(t *testing.T) *asStub {
	s := &asStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"authorization_servers": []string{s.srv.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		iss := s.srv.URL
		if s.issuer != "" {
			iss = s.issuer
		}
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": iss, "authorization_endpoint": s.srv.URL + "/authorize",
			"token_endpoint": s.srv.URL + "/token", "registration_endpoint": s.srv.URL + "/register",
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		s.regs.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.lastForm = url.Values{"auth": {body["token_endpoint_auth_method"].(string)}, "redir": {body["redirect_uris"].([]any)[0].(string)}}
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"client_id": "cid-1"})
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
			json.NewEncoder(w).Encode(map[string]any{"access_token": "acc-2", "expires_in": 3600})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "acc-1", "refresh_token": "ref-1", "expires_in": 10, "scope": "read"})
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
		AppID: "aaru", AccessToken: "live", RefreshToken: "r", ExpiresAt: &exp}))
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
		json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
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
