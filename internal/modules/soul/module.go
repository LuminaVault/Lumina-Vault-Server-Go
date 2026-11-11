// Package soul serves /v1/soul: the Go port of LuminaVaultServer
// Sources/App/Auth/SoulController.swift (wave 8, group 8D). The file work is
// internal/soul (SOULService, SOULCore, SOULComposer).
//
// Compose is SOULComposer.render, which is deterministic: it calls no model, so
// this module doesn't use internal/llm.
//
// Not ported: RouteTelemetry.observe("soul.*"), Swift's per-route timer, as in the
// other modules. SOULCoreMigrationJob stays in Swift (jobs.md).
package soul

import (
	"context"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/modules/achievements"
	"lumina/internal/modules/soul/db"
	"lumina/internal/ratelimit"
	"lumina/internal/soul"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Achievements is the AchievementsWorker inbox (achievementsink.Sink).
type Achievements interface {
	Enqueue(tenant uuid.UUID, event achievements.Event)
}

// Options holds the module's collaborators.
type Options struct {
	Identify echo.MiddlewareFunc // auth.Identify
	Limits   *ratelimit.Store    // the process-wide store
	Soul     *soul.Service
	// Achievements is nil when Swift's AchievementsWorker is (no database).
	Achievements Achievements
	// IsUserOverride is HermesRemoteCapabilitiesService.isUserOverride. Nil means
	// the service is nil in Swift (no LV_SECRET_MASTER_KEY): every tenant is managed.
	IsUserOverride func(context.Context, uuid.UUID) bool
	// Now stamps created_at in composed and reset documents. Defaults to time.Now.
	Now func() time.Time
}

type Module struct {
	q    *db.Queries
	opts Options
}

func New(pool *pgxpool.Pool, opts Options) *Module {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Module{q: db.New(pool), opts: opts}
}

// Register mounts Swift's chain for /v1/soul: jwtAuthenticator, then
// RateLimitMiddleware(.soulByUser), then requireIdentity in each handler. An
// unauthenticated request counts against its ip: bucket and can get a 429 before
// the 401.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	limit := ratelimit.Middleware(m.opts.Limits, ratelimit.SoulByUser, ratelimit.UserOrIPKey)
	r.GET("/v1/soul", m.get, m.opts.Identify, limit, auth.Require)
	r.PUT("/v1/soul", m.put, m.opts.Identify, limit, auth.Require)
	r.POST("/v1/soul/compose", m.compose, m.opts.Identify, limit, auth.Require)
	r.DELETE("/v1/soul", m.delete, m.opts.Identify, limit, auth.Require)
}

// errBYOHermes is assertWritableSoul's 409: SOUL.md lives on the tenant's own box,
// which has no write surface, so writes are refused rather than silently landing
// on a managed profile nobody reads.
var errBYOHermes = wire.NewError(http.StatusConflict, "soul_unsupported_on_byo_hermes")

func (m *Module) assertWritable(c *echo.Context, tenant uuid.UUID) error {
	if m.opts.IsUserOverride != nil && m.opts.IsUserOverride(c.Request().Context(), tenant) {
		return errBYOHermes
	}
	return nil
}

func (m *Module) username(c *echo.Context, tenant uuid.UUID) (string, error) {
	return m.q.Username(c.Request().Context(), tenant)
}

func (m *Module) respond(c *echo.Context, tenant uuid.UUID, markdown string) error {
	out := Response{Markdown: markdown}
	if at, ok := m.opts.Soul.UpdatedAt(tenant); ok {
		out.UpdatedAt = &wire.Time{Time: at}
	}
	return writeJSON(c, out)
}

// writeJSON encodes as Foundation's JSONEncoder does: SOUL.md is full of "<!--"
// and "/", which encoding/json would escape differently.
func writeJSON(c *echo.Context, v Response) error {
	b, err := wire.MarshalFoundation(v)
	if err != nil {
		return err
	}
	return c.Blob(http.StatusOK, wire.ContentTypeJSON, b)
}

func (m *Module) get(c *echo.Context) error {
	tenant := auth.UserID(c)
	username, err := m.username(c, tenant)
	if err != nil {
		return err
	}
	return m.respond(c, tenant, m.opts.Soul.Read(tenant, username, m.opts.Now()))
}

func (m *Module) put(c *echo.Context) error {
	tenant := auth.UserID(c)
	// Swift checks BYO Hermes before it decodes the body.
	if err := m.assertWritable(c, tenant); err != nil {
		return err
	}
	body, err := wire.Bind[putRequest](c)
	if err != nil {
		return err
	}
	username, err := m.username(c, tenant)
	if err != nil {
		return err
	}
	enforced, err := m.opts.Soul.Write(tenant, username, body.Markdown)
	if err != nil {
		return writeError(err, "SOUL.md too large after core covenant injection: ")
	}
	m.unlock(tenant)
	return m.respond(c, tenant, enforced)
}

func (m *Module) compose(c *echo.Context) error {
	tenant := auth.UserID(c)
	body, err := wire.Bind[composeRequest](c)
	if err != nil {
		return err
	}
	username, err := m.username(c, tenant)
	if err != nil {
		return err
	}
	markdown := soul.Compose(body.input(), username, m.opts.Now())
	// dry_run: render only, no write and no achievement, BYO tenants included.
	if body.DryRun != nil && *body.DryRun {
		return writeJSON(c, Response{Markdown: markdown})
	}
	if err := m.assertWritable(c, tenant); err != nil {
		return err
	}
	enforced, err := m.opts.Soul.Write(tenant, username, markdown)
	if err != nil {
		return writeError(err, "SOUL.md too large: ")
	}
	m.unlock(tenant)
	return m.respond(c, tenant, enforced)
}

func (m *Module) delete(c *echo.Context) error {
	tenant := auth.UserID(c)
	if err := m.assertWritable(c, tenant); err != nil {
		return err
	}
	username, err := m.username(c, tenant)
	if err != nil {
		return err
	}
	// Swift's delete catches nothing: a HermesDataError here is a bare 500.
	if _, err := m.opts.Soul.Reset(tenant, username, m.opts.Now()); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// writeError maps SOULService.write's failures as the put and compose handlers
// do: tooLarge is a 413, HermesDataError a 503 with its description, anything
// else a 500.
func writeError(err error, tooLargePrefix string) error {
	var tooLarge *soul.TooLargeError
	if errors.As(err, &tooLarge) {
		return wire.NewError(http.StatusRequestEntityTooLarge,
			tooLargePrefix+strconv.Itoa(tooLarge.Bytes)+" bytes > "+strconv.Itoa(tooLarge.Limit))
	}
	var notWritable *soul.NotWritableError
	if errors.As(err, &notWritable) {
		return wire.NewError(http.StatusServiceUnavailable, notWritable.Error())
	}
	return err
}

func (m *Module) unlock(tenant uuid.UUID) {
	if m.opts.Achievements != nil {
		m.opts.Achievements.Enqueue(tenant, achievements.EventSoulConfigured)
	}
}
