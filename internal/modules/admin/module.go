// Package admin is wave 7B, "admin rest": the operator routes behind
// X-Admin-Token, and /internal/metrics behind METRICS_SECRET. It ports
// Admin/AdminController.swift (hermes-profiles), Admin/HealthAdminController.swift,
// Admin/MemoryAdminController.swift, Admin/UsageEventsAdminController.swift and
// Admin/InternalMetricsController.swift, with the services they call.
//
// Nothing in the cluster calls the sweep routes today (plan B6). They are ported as
// routes only; scheduling them is decision O3.
package admin

import (
	"crypto/subtle"
	"lumina/internal/chunkindex"
	"lumina/internal/embedding"
	"lumina/internal/hermesprofile"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

var (
	errAdminDisabled   = wire.NewError(http.StatusNotFound, "admin disabled")
	errAdminToken      = wire.NewError(http.StatusUnauthorized, "admin token invalid")
	errMetricsDisabled = wire.NewError(http.StatusNotFound, "metrics disabled")
	errMetricsSecret   = wire.NewError(http.StatusUnauthorized, "metrics secret invalid")
	errInvalidUserID   = wire.NewError(http.StatusBadRequest, "invalid userID")
	errUserNotFound    = wire.NewError(http.StatusNotFound, "user not found")
)

// Options wires the module.
type Options struct {
	AdminToken    string
	MetricsSecret string
	Reconciler    *hermesprofile.Reconciler
	Backfill      *chunkindex.Backfiller
	// Health correlation: the routed transport, as Swift's routedTransport.
	Transport    llm.ChatTransport
	Embeddings   embedding.Service
	Memories     *memorystore.Repository
	DefaultModel string
	// USAGE_EVENTS_RETENTION_DAYS and USAGE_EVENTS_RETENTION_BATCH_SIZE.
	RetentionDays      int
	RetentionBatchSize int
}

// Module serves the 12 routes.
type Module struct {
	pool        *pgxpool.Pool
	opt         Options
	scoring     Scoring
	pruning     Pruning
	retention   Retention
	correlation *Correlation
	now         func() time.Time
}

// New returns the module.
func New(pool *pgxpool.Pool, opt Options) *Module {
	return &Module{
		pool:      pool,
		opt:       opt,
		scoring:   Scoring{Pool: pool, Config: DefaultScoringConfig},
		pruning:   Pruning{Pool: pool, Config: DefaultPruningConfig},
		retention: Retention{Pool: pool, Policy: NewRetentionPolicy(opt.RetentionDays, opt.RetentionBatchSize)},
		correlation: &Correlation{
			Pool: pool, Transport: opt.Transport, Embeddings: opt.Embeddings, Memories: opt.Memories,
			DefaultModel: opt.DefaultModel,
		},
		now: time.Now,
	}
}

func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	r.GET("/internal/metrics", m.metrics, m.requireMetricsSecret)

	admin := m.requireAdminToken
	r.GET("/v1/admin/hermes-profiles/health", m.profilesHealth, admin)
	r.POST("/v1/admin/hermes-profiles/reconcile", m.profilesReconcile, admin)
	r.POST("/v1/admin/hermes-profiles/reap-orphans", m.profilesReap, admin)

	r.POST("/v1/admin/health/correlate", m.correlateAll, admin)
	r.POST("/v1/admin/health/correlate/:userID", m.correlateOne, admin)

	r.POST("/v1/admin/memory/recompute", m.recomputeAll, admin)
	r.POST("/v1/admin/memory/recompute/:userID", m.recomputeOne, admin)
	r.POST("/v1/admin/memory/prune", m.pruneAll, admin)
	r.POST("/v1/admin/memory/prune/:userID", m.pruneOne, admin)
	// Swift always builds a ChunkBackfillService, so this is always registered.
	r.POST("/v1/admin/memory/chunks/backfill/:userID", m.backfillChunks, admin)

	r.POST("/v1/admin/usage-events/prune", m.usageEventsPrune, admin)
}

// requireAdminToken is AdminTokenMiddleware: X-Admin-Token against ADMIN_TOKEN.
// An empty token rejects everything.
func (m *Module) requireAdminToken(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if m.opt.AdminToken == "" {
			return errAdminDisabled
		}
		got := c.Request().Header.Get("X-Admin-Token")
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(m.opt.AdminToken)) != 1 {
			return errAdminToken
		}
		return next(c)
	}
}

// requireMetricsSecret is MetricsSecretMiddleware: "Authorization: Bearer
// <METRICS_SECRET>", compared in constant time. An empty secret is a 404.
func (m *Module) requireMetricsSecret(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if m.opt.MetricsSecret == "" {
			return errMetricsDisabled
		}
		auth, ok := wire.HeaderValue(c.Request(), "Authorization")
		const prefix = "Bearer "
		if !ok || !strings.HasPrefix(auth, prefix) ||
			subtle.ConstantTimeCompare([]byte(auth[len(prefix):]), []byte(m.opt.MetricsSecret)) != 1 {
			return errMetricsSecret
		}
		return next(c)
	}
}

// metrics is InternalMetricsController.metrics: JSON, not Prometheus.
func (m *Module) metrics(c *echo.Context) error {
	var users int
	if err := m.pool.QueryRow(c.Request().Context(), `SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, struct {
		Users int `json:"users"`
	}{users})
}

func (m *Module) profilesHealth(c *echo.Context) error {
	h, err := m.opt.Reconciler.Health(c.Request().Context())
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, h)
}

func (m *Module) profilesReconcile(c *echo.Context) error {
	s, err := m.opt.Reconciler.Reconcile(c.Request().Context())
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, s)
}

func (m *Module) profilesReap(c *echo.Context) error {
	s, err := m.opt.Reconciler.ReapOrphans(c.Request().Context())
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, s)
}

// parseUserID is parseUserID: UUID(uuidString:) on the path segment.
func parseUserID(c *echo.Context) (uuid.UUID, error) {
	id, err := ids.Parse(c.Param("userID"))
	if err != nil {
		return uuid.Nil, errInvalidUserID
	}
	return id, nil
}

// recomputeResponse is MemoryRecomputeResponse; tenantID is left out when nil.
type recomputeResponse struct {
	TenantID    *wire.UUID `json:"tenantID,omitempty"`
	RowsUpdated int        `json:"rowsUpdated"`
}

func (m *Module) recomputeAll(c *echo.Context) error {
	rows, err := m.scoring.RecomputeAll(c.Request().Context(), m.now())
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, recomputeResponse{RowsUpdated: rows})
}

func (m *Module) recomputeOne(c *echo.Context) error {
	tenant, err := parseUserID(c)
	if err != nil {
		return err
	}
	rows, err := m.scoring.RecomputeForTenant(c.Request().Context(), tenant, m.now())
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, recomputeResponse{TenantID: &wire.UUID{UUID: tenant}, RowsUpdated: rows})
}

func (m *Module) pruneAll(c *echo.Context) error {
	s, err := RunPruningSweep(c.Request().Context(), m.pool, m.scoring, m.pruning, m.now())
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, s)
}

// pruneOne recomputes first so the prune predicate sees fresh scores.
func (m *Module) pruneOne(c *echo.Context) error {
	tenant, err := parseUserID(c)
	if err != nil {
		return err
	}
	ctx, now := c.Request().Context(), m.now()
	if _, err := m.scoring.RecomputeForTenant(ctx, tenant, now); err != nil {
		return err
	}
	r, err := m.pruning.PruneForTenant(ctx, tenant, now)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, r)
}

// backfillChunks indexes one batch of a tenant's un-chunked memories. A batchSize
// that isn't an Int is the default, as `flatMap { Int($0) } ?? default`.
func (m *Module) backfillChunks(c *echo.Context) error {
	tenant, err := parseUserID(c)
	if err != nil {
		return err
	}
	batch, ok := wire.QueryInt(c, "batchSize")
	if !ok {
		batch = chunkindex.DefaultBackfillBatchSize
	}
	r, err := m.opt.Backfill.Backfill(c.Request().Context(), tenant, batch)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, r)
}

func (m *Module) usageEventsPrune(c *echo.Context) error {
	s, err := m.retention.Sweep(c.Request().Context(), m.now())
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, s)
}

func (m *Module) correlateAll(c *echo.Context) error {
	s, err := m.correlation.RunForAllUsers(c.Request().Context(), m.now())
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, s)
}

// correlateResponse is HealthCorrelationRunResponse.
type correlateResponse struct {
	Status   string     `json:"status"`
	MemoryID *wire.UUID `json:"memoryId,omitempty"`
}

func (m *Module) correlateOne(c *echo.Context) error {
	tenant, err := parseUserID(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var exists bool
	if err := m.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, tenant).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errUserNotFound
	}
	out, err := m.correlation.Correlate(ctx, tenant, m.now())
	if err != nil {
		return err
	}
	resp := correlateResponse{Status: out.Status}
	if out.MemoryID != nil {
		resp.MemoryID = &wire.UUID{UUID: *out.MemoryID}
	}
	return wire.JSON(c, http.StatusOK, resp)
}
