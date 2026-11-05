// Package kb serves GET /v1/memory-compile/pending: the read half of Swift's
// Sources/App/KB/MemoryCompileController. POST /v1/memory-compile is the
// kb-compile module (group 4F), which must flip together with /v1/ws.
package kb

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/kb/oapi-codegen.yaml api/openapi.yaml"

import (
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/modules/kb/db"
	"lumina/internal/wire"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Module is the pending probe. Swift mounts it on a jwt-only group, so polling
// on screen focus spends no compile rate limit and meets no entitlement gate.
type Module struct {
	q *db.Queries
}

func New(pool *pgxpool.Pool) *Module {
	return &Module{q: db.New(pool)}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.GET("/v1/memory-compile/pending", m.pending, authMW)
}

// pending counts by the caller's own tenant. X-Vault-ID is not read.
func (m *Module) pending(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	count, err := m.q.CountPendingFiles(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, KBCompilePendingResponse{PendingFiles: int(count)})
}
