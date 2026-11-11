package ws_test

import (
	"context"
	"lumina/internal/httpserver"
	"lumina/internal/ws"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	wsroute "lumina/internal/modules/ws"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")

// fakeIdentify attaches alice when the bearer token is "alice", as
// auth.Identify does for a valid token, and never rejects.
func fakeIdentify(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if c.Request().Header.Get("Authorization") == "Bearer alice" {
			c.Set("lv.auth.userID", alice)
		}
		return next(c)
	}
}

type router struct{ e *echo.Echo }

func (r router) GET(p string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo {
	return r.e.GET(p, h, m...)
}

func (r router) POST(p string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo {
	return r.e.POST(p, h, m...)
}

func (r router) PUT(p string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo {
	return r.e.PUT(p, h, m...)
}

func (r router) PATCH(p string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo {
	return r.e.PATCH(p, h, m...)
}

func (r router) DELETE(p string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo {
	return r.e.DELETE(p, h, m...)
}

func (r router) HEAD(p string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo {
	return r.e.HEAD(p, h, m...)
}
func (router) Streaming(string, string) {}

var _ httpserver.Router = router{}

func newServer(t *testing.T, sockets *ws.Manager) *httptest.Server {
	t.Helper()
	e := echo.New()
	m := wsroute.NewWithLoader(sockets, fakeIdentify, func(context.Context, uuid.UUID) (string, error) { return "alice", nil })
	m.Register(router{e}, nil)
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv
}

func TestNoIdentityIs405(t *testing.T) {
	srv := newServer(t, ws.NewManager())
	for _, upgrade := range []bool{false, true} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+wsroute.Path, nil)
		if upgrade {
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "upgrade=%v", upgrade)
	}
}

func TestPlainGetWithIdentityIs200(t *testing.T) {
	srv := newServer(t, ws.NewManager())
	req, _ := http.NewRequest(http.MethodGet, srv.URL+wsroute.Path, nil)
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Zero(t, resp.ContentLength)
}

func TestUpgradeRegistersAndRelays(t *testing.T) {
	sockets := ws.NewManager()
	srv := newServer(t, sockets)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dial := func() *websocket.Conn {
		c, resp, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):]+wsroute.Path, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": {"Bearer alice"}},
		})
		require.NoError(t, err)
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
		return c
	}
	a, b := dial(), dial()
	defer func() { _ = a.CloseNow(); _ = b.CloseNow() }()
	require.Eventually(t, func() bool { return len(sockets.Connections(alice.String())) == 2 }, 5*time.Second, 10*time.Millisecond)

	// A valid frame reaches every socket of the tenant, the sender's own included.
	require.NoError(t, a.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)))
	for _, c := range []*websocket.Conn{a, b} {
		typ, data, err := c.Read(ctx)
		require.NoError(t, err)
		require.Equal(t, websocket.MessageText, typ)
		require.JSONEq(t, `{"type":"ping"}`, string(data))
	}

	// Server-side broadcasts (kb-compile progress) reach the sockets too.
	sockets.Broadcast(ctx, alice.String(), `{"type":"memorySaved"}`)
	_, data, err := b.Read(ctx)
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"memorySaved"}`, string(data))

	require.NoError(t, a.Close(websocket.StatusNormalClosure, ""))
	require.Eventually(t, func() bool { return len(sockets.Connections(alice.String())) == 1 }, 5*time.Second, 10*time.Millisecond)
}
