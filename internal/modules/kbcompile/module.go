// Package kbcompile holds POST /v1/memory-compile, the heavy Hermes learning loop.
// The loop itself is in internal/memorycompile, which Smart Import shares.
package kbcompile

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/achievementsink"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/memorycompile"
	"lumina/internal/modules/achievements"
	"lumina/internal/modules/kbcompile/db"
	"lumina/internal/onboardlatch"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultfiles"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// compileRequest is KBCompileRequest. The spec marks forceFullRecompile optional;
// Swift's synthesized decoder requires it, Bool being non-optional.
type compileRequest struct {
	VaultFileIDs []wire.UUID `json:"vaultFileIds,omitempty"`
	ForceFull    bool        `json:"forceFullRecompile"`
}

// Options are the route's dependencies.
type Options struct {
	Identify     echo.MiddlewareFunc
	Idempotency  echo.MiddlewareFunc
	Hermes       echo.MiddlewareFunc // nil without a secret box
	Gate         echo.MiddlewareFunc // entitlement memoryCompile
	Limits       *ratelimit.Store
	Service      *memorycompile.Service
	Latches      *onboardlatch.Latches
	Achievements *achievementsink.Sink
}

// Module is the route.
type Module struct {
	pool *pgxpool.Pool
	q    *db.Queries
	opts Options
}

// New builds the module.
func New(pool *pgxpool.Pool, opts Options) *Module {
	return &Module{pool: pool, q: db.New(pool), opts: opts}
}

// Register mounts Swift's stack, in order: JWT, idempotency, BYO Hermes, entitlement,
// the memory-compile limit.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	mws := []echo.MiddlewareFunc{m.opts.Identify, m.opts.Idempotency}
	if m.opts.Hermes != nil {
		mws = append(mws, m.opts.Hermes)
	}
	if m.opts.Gate != nil {
		mws = append(mws, m.opts.Gate)
	}
	mws = append(mws, ratelimit.TieredMiddleware(m.opts.Limits, ratelimit.MemoryCompileByUser, ratelimit.UserOrIPKey, m.tier))
	r.POST("/v1/memory-compile", m.compile, mws...)
}

func (m *Module) tier(c *echo.Context) (string, bool) {
	id, ok := auth.Identity(c)
	if !ok {
		return "", false
	}
	tier, err := m.q.UserTier(c.Request().Context(), id)
	if err != nil {
		return "", false
	}
	return tier, true
}

func (m *Module) compile(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	body, err := wire.Bind[compileRequest](c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	progress := m.opts.Service.Progress()
	runID := uuid.New()
	rows, err := ResolveRows(ctx, m.pool, tenant, body.VaultFileIDs, body.ForceFull)
	if err != nil {
		return err
	}
	progress.Publish(ctx, memorycompile.StartedEvent(runID, len(rows)), tenant)

	if len(rows) == 0 {
		zero := 0
		empty := memorycompile.Response{
			MemoriesIngested: 0, MemoriesUpdated: &zero, DurationMs: &zero,
			RunID: wire.UUID{UUID: runID}, PendingMemoryIDs: []wire.UUID{},
		}
		progress.Publish(ctx, memorycompile.CompletedEvent(runID, empty), tenant)
		m.recordUsage(ctx, tenant, runID, 0)
		return wire.JSON(c, http.StatusOK, empty)
	}

	started := time.Now()
	result, err := m.opts.Service.CompileExistingVaultFiles(llm.RequestContext(c), tenant, ids.Upper(tenant), rows, nil, runID)
	if err != nil {
		progress.Publish(ctx, memorycompile.ErrorEvent(runID, fmt.Sprint(err)), tenant)
		return err
	}
	elapsed := int(time.Since(started).Milliseconds())

	// Deliberately after the empty return: an empty compile learned nothing, so
	// guided-start step 2 stays incomplete.
	if m.opts.Latches != nil {
		m.opts.Latches.Latch(ctx, onboardlatch.FirstMemoryCompile, tenant)
	}
	if m.opts.Achievements != nil {
		m.opts.Achievements.Enqueue(tenant, achievements.EventKBCompiled)
	}
	pending := make([]wire.UUID, len(result.Memories))
	for i, memory := range result.Memories {
		pending[i] = wire.UUID{UUID: memory.ID}
	}
	response := memorycompile.Response{
		MemoriesIngested: len(result.Memories), DurationMs: &elapsed,
		RunID: wire.UUID{UUID: runID}, PendingMemoryIDs: pending,
	}
	progress.Publish(ctx, memorycompile.CompletedEvent(runID, response), tenant)
	m.recordUsage(ctx, tenant, runID, len(result.WrittenFiles))
	return wire.JSON(c, http.StatusOK, response)
}

func (m *Module) recordUsage(ctx context.Context, tenant, runID uuid.UUID, files int) {
	if err := memorycompile.RecordMemoryCompile(ctx, m.pool, tenant, runID, files); err != nil {
		slog.WarnContext(ctx, "memory compile usage insert failed", "error", err)
	}
}

// ResolveRows is resolveRows. Explicit ids win over forceFullRecompile; neither means
// the unprocessed rows. No ORDER BY, as in Swift.
func ResolveRows(ctx context.Context, pool *pgxpool.Pool, tenant uuid.UUID, fileIDs []wire.UUID, forceFull bool) ([]*vaultfiles.VaultFile, error) {
	query := `SELECT ` + vaultfiles.Columns + ` FROM vault_files WHERE tenant_id = $1`
	args := []any{tenant}
	switch {
	case len(fileIDs) > 0:
		list := make([]uuid.UUID, len(fileIDs))
		for i, id := range fileIDs {
			list[i] = id.UUID
		}
		query += ` AND id = ANY($2)`
		args = append(args, list)
	case forceFull:
	default:
		query += ` AND processed_at IS NULL`
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*vaultfiles.VaultFile{}
	for rows.Next() {
		f, err := vaultfiles.Scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
