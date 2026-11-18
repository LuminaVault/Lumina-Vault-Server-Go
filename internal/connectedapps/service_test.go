package connectedapps_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"lumina/internal/config"
	"lumina/internal/connectedapps"
)

// mcpApp is a stub first-party app: MCP endpoint at /mcp plus the OAuth
// documents and token endpoint, so one server serves discovery and calls.
type mcpApp struct {
	srv       *httptest.Server
	mu        sync.Mutex
	requests  int      // every HTTP request to /mcp
	bearers   []string // Authorization header of each /mcp request
	listCalls atomic.Int32
	okTokens  map[string]bool // bearer values that are accepted
	tools     []string
	result    string
}

func newMCPApp(t *testing.T, ok ...string) *mcpApp {
	a := &mcpApp{okTokens: map[string]bool{}, tools: []string{"read_library", "apply_library_patch"}, result: "the-secret-result"}
	for _, o := range ok {
		a.okTokens[o] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"authorization_servers": []string{a.srv.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": a.srv.URL, "token_endpoint": a.srv.URL + "/token"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "alice-token-2", "expires_in": 3600})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.requests++
		a.bearers = append(a.bearers, r.Header.Get("Authorization"))
		a.mu.Unlock()
		if !a.okTokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": map[string]any{}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			a.listCalls.Add(1)
			var ts []map[string]any
			for _, n := range a.tools {
				ts = append(ts, map[string]any{"name": n, "description": "d", "inputSchema": map[string]any{"type": "object"}})
			}
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": map[string]any{"tools": ts}})
		case "tools/call":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": a.result}}}})
		}
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

func (a *mcpApp) count() int { a.mu.Lock(); defer a.mu.Unlock(); return a.requests }

type svcEnv struct {
	svc   *connectedapps.Service
	store *connectedapps.Store
	oauth *connectedapps.OAuth
	app   *mcpApp
}

func newSvc(t *testing.T, app *mcpApp, log *slog.Logger) *svcEnv {
	store, pool := newStore(t)
	reg := connectedapps.NewRegistry(config.Config{
		ConnectedAppsAaruMCPURL:   app.srv.URL + "/mcp",
		ConnectedAppsNorviqMCPURL: app.srv.URL + "/mcp",
	})
	oauth := connectedapps.NewOAuth(pool, reg, store, app.srv.Client(), permissive{}, "https://lumina.test/cb")
	_, err := pool.Exec(context.Background(), `INSERT INTO connected_app_clients (app_id, client_id) VALUES ('aaru','cid')`)
	require.NoError(t, err)
	client := &connectedapps.Client{HTTP: app.srv.Client(), Guard: permissive{}}
	return &svcEnv{connectedapps.NewService(reg, store, oauth, client, log), store, oauth, app}
}

func saveAlice(t *testing.T, e *svcEnv, token string) {
	t.Helper()
	exp := time.Now().Add(time.Hour)
	require.NoError(t, e.store.Save(context.Background(), alice, connectedapps.Account{
		AppID: "aaru", AccessToken: token, RefreshToken: "r", ExpiresAt: &exp}))
}

func TestSplitName(t *testing.T) {
	a, tool, ok := connectedapps.SplitName("aaru__read_library")
	require.True(t, ok)
	require.Equal(t, "aaru", a)
	require.Equal(t, "read_library", tool)
	a, tool, ok = connectedapps.SplitName("aaru__a__b")
	require.True(t, ok)
	require.Equal(t, "aaru", a)
	require.Equal(t, "a__b", tool)
	for _, bad := range []string{"", "aaru", "__x", "aaru__", "__"} {
		_, _, ok = connectedapps.SplitName(bad)
		require.False(t, ok, bad)
	}
}

func TestToolsOnlyConnectedApps(t *testing.T) {
	e := newSvc(t, newMCPApp(t, "alice-token"), nil)
	saveAlice(t, e, "alice-token")
	got, err := e.svc.Tools(context.Background(), alice, nil)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "aaru__read_library", got[0].Name)
	require.Equal(t, "aaru", got[0].App.ID)
	require.Equal(t, "read_library", got[0].Remote.Name)

	got, err = e.svc.Tools(context.Background(), bob, nil)
	require.NoError(t, err)
	require.Empty(t, got)

	got, err = e.svc.Tools(context.Background(), alice, []string{"norviq"})
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestToolsDropUnlistedNames(t *testing.T) {
	app := newMCPApp(t, "alice-token")
	app.tools = []string{"search_titles", "apply_library_patch", "read_library", "list_airs"}
	e := newSvc(t, app, nil)
	saveAlice(t, e, "alice-token")
	got, err := e.svc.Tools(context.Background(), alice, nil)
	require.NoError(t, err)
	var names []string
	for _, g := range got {
		names = append(names, g.Name)
	}
	require.Equal(t, []string{"aaru__list_airs", "aaru__read_library", "aaru__search_titles"}, names, "sorted, write tool dropped")
}

func TestToolsCachedAndForgotten(t *testing.T) {
	e := newSvc(t, newMCPApp(t, "alice-token"), nil)
	saveAlice(t, e, "alice-token")
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, err := e.svc.Tools(ctx, alice, nil)
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, e.app.listCalls.Load())
	e.svc.Forget(alice, "aaru")
	_, err := e.svc.Tools(ctx, alice, nil)
	require.NoError(t, err)
	require.EqualValues(t, 2, e.app.listCalls.Load())
}

func TestToolsSkipsFailingAppAndNeedsReauthRows(t *testing.T) {
	e := newSvc(t, newMCPApp(t), nil) // accepts no token: every request is 401
	require.NoError(t, e.oauth.SavePAT(context.Background(), alice, "norviq", "nvq_pat_x"))
	saveAlice(t, e, "alice-token")
	got, err := e.svc.Tools(context.Background(), alice, nil)
	require.NoError(t, err, "a failing app never fails the call")
	require.Empty(t, got)

	// Both rows now need reauth (401s) and are skipped without any request.
	before := e.app.count()
	got, err = e.svc.Tools(context.Background(), alice, nil)
	require.NoError(t, err)
	require.Empty(t, got)
	require.Equal(t, before, e.app.count())
}

func TestCallUsesCallersToken(t *testing.T) {
	app := newMCPApp(t, "alice-token")
	e := newSvc(t, app, nil)
	saveAlice(t, e, "alice-token")
	out, err := e.svc.Call(context.Background(), alice, "aaru__read_library", map[string]any{"q": "x"})
	require.NoError(t, err)
	require.Equal(t, "the-secret-result", out)
	for _, b := range app.bearers {
		require.Equal(t, "Bearer alice-token", b)
	}

	before := app.count()
	_, err = e.svc.Call(context.Background(), bob, "aaru__read_library", nil)
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)
	require.Equal(t, before, app.count(), "no request for a user without a row")
}

func TestCallRejectsUnlistedTool(t *testing.T) {
	app := newMCPApp(t, "alice-token")
	e := newSvc(t, app, nil)
	saveAlice(t, e, "alice-token")
	for _, name := range []string{"aaru__apply_library_patch", "nope__read_library", "aaru", "aaru__"} {
		_, err := e.svc.Call(context.Background(), alice, name, nil)
		require.Error(t, err, name)
		require.False(t, strings.Contains(err.Error(), "alice-token"))
	}
	_, err := e.svc.Call(context.Background(), alice, "aaru__apply_library_patch", nil)
	require.ErrorIs(t, err, connectedapps.ErrToolNotAllowed)
	require.Zero(t, app.count())
}

func TestCallNeedsReauthRowMakesNoRequest(t *testing.T) {
	app := newMCPApp(t, "alice-token")
	e := newSvc(t, app, nil)
	saveAlice(t, e, "alice-token")
	require.NoError(t, e.store.MarkNeedsReauth(context.Background(), alice, "aaru", "invalid_grant"))
	_, err := e.svc.Call(context.Background(), alice, "aaru__read_library", nil)
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)
	require.EqualError(t, err, "Reconnect Aaru in Lumina Settings.")
	require.Zero(t, app.count())
}

func TestDisconnectInvalidatesCache(t *testing.T) {
	e := newSvc(t, newMCPApp(t, "alice-token"), nil)
	saveAlice(t, e, "alice-token")
	ctx := context.Background()
	got, err := e.svc.Tools(ctx, alice, nil)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NoError(t, e.store.Delete(ctx, alice, "aaru"))
	e.svc.Forget(alice, "aaru")
	_, err = e.svc.Call(ctx, alice, "aaru__read_library", nil)
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)
	got, err = e.svc.Tools(ctx, alice, nil)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestCallLogsNoSecrets(t *testing.T) {
	var buf bytes.Buffer
	e := newSvc(t, newMCPApp(t, "alice-token"), slog.New(slog.NewJSONHandler(&buf, nil)))
	saveAlice(t, e, "alice-token")
	_, err := e.svc.Call(context.Background(), alice, "aaru__read_library", map[string]any{"q": "secret-arg-value"})
	require.NoError(t, err)
	out := buf.String()
	require.Equal(t, 1, strings.Count(out, "connected_app_call"))
	require.Contains(t, out, `"app":"aaru"`)
	require.Contains(t, out, `"tool":"read_library"`)
	require.Contains(t, out, `"ok":true`)
	for _, secret := range []string{"alice-token", "secret-arg-value", "the-secret-result"} {
		require.NotContains(t, out, secret)
	}

	buf.Reset()
	_, err = e.svc.Call(context.Background(), alice, "aaru__apply_library_patch", nil)
	require.Error(t, err)
	require.Equal(t, 1, strings.Count(buf.String(), "connected_app_call"))
	require.Contains(t, buf.String(), `"ok":false`)
}

func TestCallRetriesOnceOn401(t *testing.T) {
	// "old" is rejected, the refreshed "alice-token-2" is accepted.
	app := newMCPApp(t, "alice-token-2")
	e := newSvc(t, app, nil)
	saveAlice(t, e, "old") // still valid by expiry, so Fresh will not refresh
	out, err := e.svc.Call(context.Background(), alice, "aaru__read_library", nil)
	require.NoError(t, err)
	require.Equal(t, "the-secret-result", out)
	acct, err := e.store.Get(context.Background(), alice, "aaru")
	require.NoError(t, err)
	require.Equal(t, "alice-token-2", acct.AccessToken)
	require.Equal(t, "connected", acct.Status)
}

func TestCallSecond401MarksNeedsReauth(t *testing.T) {
	app := newMCPApp(t) // rejects every token, including the refreshed one
	e := newSvc(t, app, nil)
	saveAlice(t, e, "old")
	_, err := e.svc.Call(context.Background(), alice, "aaru__read_library", nil)
	require.EqualError(t, err, "Reconnect Aaru in Lumina Settings.")
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)
	acct, err := e.store.Get(context.Background(), alice, "aaru")
	require.NoError(t, err)
	require.Equal(t, "needs_reauth", acct.Status)
}

func TestPATCallDoesNotRetryOn401(t *testing.T) {
	app := newMCPApp(t) // rejects everything
	app.tools = []string{"list_goals", "evil_write"}
	e := newSvc(t, app, nil)
	require.NoError(t, e.oauth.SavePAT(context.Background(), alice, "norviq", "nvq_pat_x"))
	_, err := e.svc.Call(context.Background(), alice, "norviq__list_goals", nil)
	require.EqualError(t, err, "Reconnect Norviq in Lumina Settings.")
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)
	require.Equal(t, 1, app.count(), "one initialize, no retry")
	acct, err := e.store.Get(context.Background(), alice, "norviq")
	require.NoError(t, err)
	require.Equal(t, "needs_reauth", acct.Status)
}

func TestToolCacheExpiresAfterTTL(t *testing.T) {
	e := newSvc(t, newMCPApp(t, "alice-token"), nil)
	saveAlice(t, e, "alice-token")
	clock := time.Now()
	e.svc.SetNow(func() time.Time { return clock })
	ctx := context.Background()
	_, err := e.svc.Tools(ctx, alice, nil)
	require.NoError(t, err)
	clock = clock.Add(4 * time.Minute)
	_, err = e.svc.Tools(ctx, alice, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, e.app.listCalls.Load())
	clock = clock.Add(2 * time.Minute)
	_, err = e.svc.Tools(ctx, alice, nil)
	require.NoError(t, err)
	require.EqualValues(t, 2, e.app.listCalls.Load())
}
