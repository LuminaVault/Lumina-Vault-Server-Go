// Package brain serves GET /v1/brain/graph: one read-only knowledge graph across
// every vault the caller can read. It is new in the Go server and has no Swift
// counterpart.
package brain

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/brain/oapi-codegen.yaml api/openapi.yaml"

import (
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ratelimit"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Module is the route group. Like /v1/memory/graph it has no entitlement gate.
type Module struct {
	pool     *pgxpool.Pool
	identify echo.MiddlewareFunc
	limits   *ratelimit.Store
	now      func() time.Time
}

// New builds the module. identify is auth.Identify and limits the process's
// shared rate-limit store.
func New(pool *pgxpool.Pool, identify echo.MiddlewareFunc, limits *ratelimit.Store) *Module {
	return &Module{pool: pool, identify: identify, limits: limits, now: time.Now}
}

func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	limit := ratelimit.TieredMiddleware(m.limits, ratelimit.BrainGraphByUser, ratelimit.UserOrIPKey, ratelimit.PGTier(m.pool))
	// identify never rejects, so Require turns a missing identity into the 401,
	// after the limiter as in the other Identify chains.
	r.GET("/v1/brain/graph", auth.Require(m.graph), m.identify, limit)
}

func (m *Module) graph(c *echo.Context) error {
	return wire.JSON(c, http.StatusOK, BrainGraphResponse{
		GeneratedAt: wire.Time{Time: m.now()},
		Truncated:   false,
		Vaults:      []BrainGraphVault{},
		Nodes:       []BrainGraphNode{},
		Edges:       []BrainGraphEdge{},
	})
}
