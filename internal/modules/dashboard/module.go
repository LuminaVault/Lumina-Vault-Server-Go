// Package dashboard serves /v1/dashboard, /v1/analytics, /v1/insights and
// /v1/tasks: the Go port of LuminaVaultServer Sources/App/Dashboard (wave 3).
// These routes only read and write rows: AnalyticsMaintenanceService and
// SynthesisWorker, which prune and produce them, stay in Swift (jobs.md).
package dashboard

import (
	"context"
	"errors"
	"lumina/internal/httpserver"
	"lumina/internal/modules/dashboard/db"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Module holds the dashboard routes.
type Module struct {
	pool     *pgxpool.Pool
	q        *db.Queries
	vaults   *vaultaccess.Service
	identify echo.MiddlewareFunc
}

// New builds the module. identify is auth.Identify: the analytics routes that
// resolve a vault read X-Vault-ID (and decode the body) before requiring identity.
func New(pool *pgxpool.Pool, identify echo.MiddlewareFunc) *Module {
	return &Module{pool: pool, q: db.New(pool), vaults: vaultaccess.New(pool), identify: identify}
}

// Register mounts the routes. Swift's groups add only jwtAuthenticator; there is
// no rate limit on any of them.
func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.GET("/v1/dashboard/stats", m.stats, authMW)
	r.GET("/v1/dashboard/profile", m.profile, authMW)
	r.GET("/v1/dashboard/home", m.home, authMW)
	r.GET("/v1/dashboard/activity", m.activity, authMW)

	r.GET("/v1/analytics/usage-summary", m.usageSummary, authMW)
	r.GET("/v1/analytics/retrieval-health", m.retrievalHealth, authMW)
	r.GET("/v1/analytics/overview", m.overview, m.identify)
	r.GET("/v1/analytics/models", m.models, m.identify)
	r.GET("/v1/analytics/team", m.team, m.identify)
	r.POST("/v1/analytics/events", m.recordEvent, m.identify)
	r.POST("/v1/analytics/model-feedback", m.recordModelFeedback, m.identify)
	r.PATCH("/v1/analytics/recommendations", m.updateRecommendation, m.identify)

	r.GET("/v1/tasks", m.tasks, authMW)

	r.GET("/v1/insights", m.listInsights, authMW)
	r.GET("/v1/insights/synthesis/latest", m.latestSynthesis, authMW)
	r.POST("/v1/insights/:id/dismiss", m.dismissInsight, authMW)
}

// orZero maps "no row" to the zero value: Swift's `?.field ?? default`.
func orZero[T any](v T, err error) (T, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		var zero T
		return zero, nil
	}
	return v, err
}

// clampedLimit is the parseLimit shared by TasksController and
// InsightsController: not an Int → fallback, otherwise clamped to 1...maxLimit.
func clampedLimit(raw string, fallback, maxLimit int) int {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return max(1, min(n, maxLimit))
}

// nonNil keeps an empty list as [] on the wire.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func notFound(msg string) error { return wire.NewError(http.StatusNotFound, msg) }

func requestContext(c *echo.Context) context.Context { return c.Request().Context() }
