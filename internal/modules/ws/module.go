// Package ws serves GET /v1/ws: the Go port of the WebSocket route in
// LuminaVaultServer Sources/App/App+build.swift (the `websocketGroup` block,
// wave 5, group 5J). The socket map, broadcast guard and frame loop are
// internal/ws (P5); this package is only the route and its upgrade decision.
//
// Hummingbird's `router.ws` (hummingbird-websocket 2.7.0, WebSocketRouter.swift)
// registers a plain GET whose handler runs shouldUpgrade:
//   - .dontUpgrade (Swift's closure returns it when JWTAuthenticator attached no
//     user) answers 405 with an empty body, upgrade headers or not;
//   - .upgrade answers 200 with an empty body, and the channel switches protocols
//     only when the request asked for a WebSocket. A plain GET with a valid token
//     therefore gets 200 and nothing else.
//
// The device result route (POST /v1/devices/command/{id}/result) is already
// served by modules/apple on the same devicerpc.Broker; it moves to Go with this
// route because the broker's waiters and the sockets must live in one process.
package ws

import (
	"context"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ws"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Path is the route Swift mounts with `router.group("/v1/ws").ws("")`.
const Path = "/v1/ws"

// UsernameLoader reads the username Swift's ConnectionManager.register logs.
type UsernameLoader func(ctx context.Context, id uuid.UUID) (string, error)

// Module is the /v1/ws route.
type Module struct {
	sockets  *ws.Manager
	identify echo.MiddlewareFunc
	username UsernameLoader
}

// New returns the route on sockets. identify is auth.Identify: Swift's
// JWTAuthenticator, which attaches a user and never rejects.
func New(pool *pgxpool.Pool, sockets *ws.Manager, identify echo.MiddlewareFunc) *Module {
	return &Module{sockets: sockets, identify: identify, username: func(ctx context.Context, id uuid.UUID) (string, error) {
		var name string
		err := pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, id).Scan(&name)
		return name, err
	}}
}

// NewWithLoader is New with a custom username loader, for tests.
func NewWithLoader(sockets *ws.Manager, identify echo.MiddlewareFunc, username UsernameLoader) *Module {
	return &Module{sockets: sockets, identify: identify, username: username}
}

func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	r.Streaming(http.MethodGet, Path)
	r.GET(Path, m.serve, m.identify)
}

func (m *Module) serve(c *echo.Context) error {
	user, ok := auth.Identity(c)
	if !ok {
		return c.NoContent(http.StatusMethodNotAllowed)
	}
	req := c.Request()
	if !isUpgrade(req) {
		return c.NoContent(http.StatusOK)
	}
	username, err := m.username(req.Context(), user)
	if err != nil {
		return err
	}
	conn, err := ws.Accept(c.Response(), req)
	if err != nil {
		// Accept has already written its own error response.
		slog.DebugContext(req.Context(), "websocket upgrade failed", "error", err)
		return nil
	}
	m.sockets.Serve(req.Context(), conn, user, username)
	return nil
}

// isUpgrade is the check NIO's HTTP upgrader makes before switching protocols.
func isUpgrade(r *http.Request) bool {
	return headerHasToken(r.Header, "Connection", "upgrade") && headerHasToken(r.Header, "Upgrade", "websocket")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}
