// Package llmrouter serves /v1/router: the Go port of LuminaVaultServer
// Sources/App/LLM/Routing/RouterController.swift (profiles, bindings, catalog,
// dashboard), RouterPlaceholderRoutes.swift, and ParallelController.swift with
// the read side of ParallelExecutionStore.swift.
//
// Profiles and bindings go through the stack's one RouterProfileRepository, the
// one the router resolves from.
//
// ParallelExecutor is not ported (wave-5 owner decision 7): production runs with
// cerberus.parallelEnabled off, so every parallel route answers 403 there. With
// it on, the stream hands the request to the transport with the parallel values
// bound, as Swift does, and writes token, done and error frames only.
package llmrouter

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/llmrouter/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/embedding"
	"lumina/internal/entitlement"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/modules/llmrouter/db"
	"lumina/internal/ratelimit"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Options wires the module. Stack is the process's one LLM stack.
type Options struct {
	Identify   echo.MiddlewareFunc
	Limits     *ratelimit.Store
	Stack      *llm.Stack
	Memories   *memorystore.Repository
	Embeddings embedding.Service
	// FixDashboard serves /v1/router/dashboard. Off, it is Swift's bare 500:
	// RouterController decodes SUM(bigint), which Postgres returns as numeric,
	// into Int64, so the route fails on every call.
	FixDashboard bool
}

// Module is the /v1/router group: jwtAuthenticator + settingsByUser.
type Module struct {
	pool *pgxpool.Pool
	q    *db.Queries
	opts Options
	log  *slog.Logger
	now  func() time.Time
}

func New(pool *pgxpool.Pool, opts Options) *Module {
	return &Module{pool: pool, q: db.New(pool), opts: opts, log: slog.Default().With("module", "llm-router"), now: time.Now}
}

// SetClock replaces the clock (tests).
func (m *Module) SetClock(now func() time.Time) { m.now = now }

func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	mw := []echo.MiddlewareFunc{m.opts.Identify, ratelimit.Middleware(m.opts.Limits, ratelimit.SettingsByUser, ratelimit.UserOrIPKey), auth.Require}
	r.GET("/v1/router", m.list, mw...)
	r.POST("/v1/router", m.create, mw...)
	r.GET("/v1/router/catalog", m.catalog, mw...)
	r.GET("/v1/router/bindings", m.bindings, mw...)
	r.PUT("/v1/router/bindings/:scope/:scopeID", m.putBinding, mw...)
	r.DELETE("/v1/router/bindings/:scope/:scopeID", m.deleteBinding, mw...)
	r.GET("/v1/router/dashboard", m.dashboard, mw...)
	r.PUT("/v1/router/:id", m.update, mw...)
	r.DELETE("/v1/router/:id", m.delete, mw...)

	r.POST("/v1/router/parallel/executions/stream", m.stream, mw...)
	r.Streaming(http.MethodPost, "/v1/router/parallel/executions/stream")
	r.GET("/v1/router/parallel/executions", m.listExecutions, mw...)
	r.GET("/v1/router/parallel/executions/:executionID", m.executionDetail, mw...)
	r.DELETE("/v1/router/parallel/executions/:executionID", m.deleteExecution, mw...)
	r.GET("/v1/router/synthesis-presets", m.presets, mw...)
	r.POST("/v1/router/synthesis-presets", m.createPreset, mw...)
	r.PUT("/v1/router/synthesis-presets/:presetID", m.updatePreset, mw...)
	r.DELETE("/v1/router/synthesis-presets/:presetID", m.deletePreset, mw...)
}

// identity is ctx.requireIdentity(): the user row, for its tier.
type identity struct {
	id   uuid.UUID
	tier string // effective, after the override
	user *llm.User
}

func (m *Module) requireIdentity(c *echo.Context) (*identity, error) {
	id := auth.UserID(c)
	row, err := m.q.UserTier(c.Request().Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, wire.ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	return &identity{
		id: id, tier: entitlement.EffectiveTier(row.Tier, row.TierOverride),
		user: &llm.User{ID: id, Tier: row.Tier, TierOverride: row.TierOverride, PrivacyNoCNOrigin: row.PrivacyNoCnOrigin},
	}, nil
}

func (i *identity) proOrUltimate() bool { return i.tier == "pro" || i.tier == "ultimate" }
func (i *identity) ultimate() bool      { return i.tier == "ultimate" }

// pathID is RouterController.pathID / ParallelController.pathID.
func pathID(c *echo.Context, name string) (uuid.UUID, error) {
	raw := c.Param(name)
	id, err := ids.Parse(raw)
	if err != nil {
		return uuid.Nil, wire.NewError(http.StatusBadRequest, "invalid_"+name)
	}
	return id, nil
}

// preferences is the stack's preference repository, nil-safe for tests.
func (m *Module) preference(ctx context.Context, tenant uuid.UUID) (*llm.Preference, error) {
	if m.opts.Stack == nil || m.opts.Stack.Preferences == nil {
		return nil, nil
	}
	return m.opts.Stack.Preferences.Get(ctx, tenant)
}
