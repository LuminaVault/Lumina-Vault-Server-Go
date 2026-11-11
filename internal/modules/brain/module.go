// Package brain serves GET /v1/brain/graph: one read-only knowledge graph across
// every vault the caller can read. It is new in the Go server and has no Swift
// counterpart.
package brain

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/brain/oapi-codegen.yaml api/openapi.yaml"

import (
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/brain/db"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Module is the route group. Like /v1/memory/graph it has no entitlement gate.
type Module struct {
	pool     *pgxpool.Pool
	q        *db.Queries
	access   *vaultaccess.Service
	identify echo.MiddlewareFunc
	limits   *ratelimit.Store
	now      func() time.Time
}

// New builds the module. identify is auth.Identify and limits the process's
// shared rate-limit store.
func New(pool *pgxpool.Pool, identify echo.MiddlewareFunc, limits *ratelimit.Store) *Module {
	return &Module{pool: pool, q: db.New(pool), access: vaultaccess.New(pool), identify: identify, limits: limits, now: time.Now}
}

func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	limit := ratelimit.TieredMiddleware(m.limits, ratelimit.BrainGraphByUser, ratelimit.UserOrIPKey, ratelimit.PGTier(m.pool))
	// identify never rejects, so Require turns a missing identity into the 401,
	// after the limiter as in the other Identify chains.
	r.GET("/v1/brain/graph", auth.Require(m.graph), m.identify, limit)
}

// params is the parsed query string.
type params struct {
	vaults []uuid.UUID
}

var errInvalidVaults = wire.NewError(http.StatusBadRequest, "invalid vaults")

// parseParams reads the query string. X-Vault-ID is deliberately not read: this
// route spans vaults and takes its scope only from `vaults`.
func parseParams(c *echo.Context) (params, error) {
	var p params
	if raw := c.QueryParam("vaults"); raw != "" {
		seen := map[uuid.UUID]bool{}
		for _, part := range strings.Split(raw, ",") {
			id, err := ids.Parse(part)
			if err != nil {
				return params{}, errInvalidVaults
			}
			if !seen[id] {
				seen[id] = true
				p.vaults = append(p.vaults, id)
			}
		}
	}
	return p, nil
}

func (m *Module) graph(c *echo.Context) error {
	p, err := parseParams(c)
	if err != nil {
		return err
	}
	userID, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	scopes, err := m.scopes(c.Request().Context(), userID, p.vaults)
	if err != nil {
		return err
	}
	vaults := make([]BrainGraphVault, 0, len(scopes))
	for _, s := range scopes {
		vaults = append(vaults, BrainGraphVault{Id: wire.UUID{UUID: s.ID}, Name: s.Name, Personal: s.Personal})
	}
	return wire.JSON(c, http.StatusOK, BrainGraphResponse{
		GeneratedAt: wire.Time{Time: m.now()},
		Truncated:   false,
		Vaults:      vaults,
		Nodes:       []BrainGraphNode{},
		Edges:       []BrainGraphEdge{},
	})
}
