// Package ingestion serves /v1/ingestions and /v1/ingestion-sources: the routes of
// LuminaVaultServer Sources/App/Ingestion (MultimodalIngestionController and
// MultimodalIngestionService), and the MultimodalIngestionWorker job (worker.go).
// The routes only enqueue rows and write chunks; capability checks, LLM calls, link
// capture and push all happen in the worker's processNext, which claims rows with
// SKIP LOCKED, so it can run on either server while the other's copy is off.
//
// The worker builds the /v1/ingestion-sources/<token> URL it hands to BYO Hermes
// from INGESTION_PUBLIC_BASE_URL, which points at the API host, so Hermes reaches
// the source route through the ingress whichever server serves it.
package ingestion

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/ingestion/oapi-codegen.yaml api/openapi.yaml"

import (
	"errors"
	"lumina/internal/diskguard"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/ingestion/db"
	"lumina/internal/ratelimit"
	"lumina/internal/sse"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	chunkSize     = 8 * 1024 * 1024
	maxItems      = 50
	maxFileBytes  = int64(2 * 1024 * 1024 * 1024)
	maxBatchBytes = int64(5 * 1024 * 1024 * 1024)

	stateAwaitingUpload    = "awaiting_upload"
	stateQueued            = "queued"
	stateBlockedCapability = "blocked_capability"
	stateCompleted         = "completed"
	stateFailed            = "failed"
	stateCancelled         = "cancelled"

	eventProgress     = "progress"
	eventStateChanged = "state_changed"

	eventsPath = "/v1/ingestions/:batchID/events"
	sourcePath = "/v1/ingestion-sources/:token"
)

var (
	errBatchNotFound = wire.NewError(http.StatusNotFound, "ingestion batch not found")
	errItemNotFound  = wire.NewError(http.StatusNotFound, "ingestion item not found")

	// uploadPolicy is RateLimitPolicy.ingestionUploadByUser: a 2 GiB file is 256
	// chunks of 8 MiB, so the ceiling is high. Only this module uses it.
	uploadPolicy = ratelimit.Policy{Name: "ingestionUploadByUser", Max: 600, Window: 60 * time.Second}
)

// Module is the ingestion route group. Swift wraps /v1/ingestions in
// jwtAuthenticator + RateLimitMiddleware(.ingestionUploadByUser), and each
// handler resolves vault access itself, so identify + limits reproduce it: a bad
// X-Vault-ID is a 400 before the missing token is a 401. The source route has no
// middleware at all: its token is the only gate.
type Module struct {
	pool     *pgxpool.Pool
	q        *db.Queries
	access   *vaultaccess.Service
	files    *vaultfiles.Store
	paths    vaultpaths.Paths
	quota    Quota
	disk     *diskguard.Guard
	identify echo.MiddlewareFunc
	limits   *ratelimit.Store
	now      func() time.Time
}

// New builds the module. disk is the process's one guard on the vault root.
func New(pool *pgxpool.Pool, identify echo.MiddlewareFunc, limits *ratelimit.Store, paths vaultpaths.Paths, disk *diskguard.Guard, quota Quota) *Module {
	return &Module{
		pool:     pool,
		q:        db.New(pool),
		access:   vaultaccess.New(pool),
		files:    vaultfiles.NewStore(pool, paths),
		paths:    paths,
		quota:    quota,
		disk:     disk,
		identify: identify,
		limits:   limits,
		now:      time.Now,
	}
}

// Register ignores the shared auth middleware on purpose; see Module.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	mw := []echo.MiddlewareFunc{m.identify, ratelimit.Middleware(m.limits, uploadPolicy, ratelimit.UserOrIPKey)}
	r.POST("/v1/ingestions", m.create, mw...)
	r.GET("/v1/ingestions", m.list, mw...)
	r.GET("/v1/ingestions/:batchID", m.getDetail, mw...)
	r.GET(eventsPath, m.events, mw...)
	r.Streaming(http.MethodGet, eventsPath)
	r.PUT("/v1/ingestions/:batchID/items/:itemID/chunks/:index", m.uploadChunk, mw...)
	r.POST("/v1/ingestions/:batchID/items/:itemID/complete", m.complete, mw...)
	r.POST("/v1/ingestions/:batchID/items/:itemID/retry", m.retry, mw...)
	r.DELETE("/v1/ingestions/:batchID/items/:itemID", m.cancel, mw...)
	r.GET(sourcePath, m.source)
	r.HEAD(sourcePath, m.source)
}

// param is the controller's parameter(_:ctx:): a strict UUID, or a 400 naming it.
func param(c *echo.Context, name string) (uuid.UUID, error) {
	id, err := ids.Parse(c.Param(name))
	if err != nil {
		return uuid.Nil, wire.NewError(http.StatusBadRequest, "invalid "+name)
	}
	return id, nil
}

// itemParams reads batchID, then itemID, for the routes that take both.
func itemParams(c *echo.Context) (batchID, itemID uuid.UUID, err error) {
	if batchID, err = param(c, "batchID"); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if itemID, err = param(c, "itemID"); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return batchID, itemID, nil
}

func (m *Module) getDetail(c *echo.Context) error {
	acc, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	batchID, err := param(c, "batchID")
	if err != nil {
		return err
	}
	return m.respondDetail(c, acc.VaultID, batchID)
}

func (m *Module) list(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	batches, err := m.q.ListBatches(ctx, acc.VaultID)
	if err != nil {
		return err
	}
	out := batchListDTO{Batches: make([]batchDTO, 0, len(batches))}
	for _, b := range batches {
		d, err := m.batchDetail(ctx, b)
		if err != nil {
			return err
		}
		out.Batches = append(out.Batches, d)
	}
	return wire.JSON(c, http.StatusOK, out)
}

// events checks the batch before the response commits to 200. Unlike the other
// routes it parses the id before it resolves access, so a bad id beats a missing
// token.
func (m *Module) events(c *echo.Context) error {
	batchID, err := param(c, "batchID")
	if err != nil {
		return err
	}
	acc, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	if _, err := m.detail(c.Request().Context(), acc.VaultID, batchID); err != nil {
		return err
	}
	return sse.Serve(c, eventSource{m: m, tenant: acc.VaultID, batch: batchID}, sse.Cursor(c), sse.Ingestion())
}

func (m *Module) respondDetail(c *echo.Context, tenant, batchID uuid.UUID) error {
	d, err := m.detail(c.Request().Context(), tenant, batchID)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, d)
}

// notFound maps pgx's no-rows onto err, as a nil Fluent first() does.
func notFound(queryErr, err error) error {
	if errors.Is(queryErr, pgx.ErrNoRows) {
		return err
	}
	return queryErr
}
