// Package me serves the wave-3 part of /v1/me: the Go port of LuminaVaultServer
// Sources/App/Me (MeTodayController/Service/Cache, MeBriefController/Service,
// MeRecapController/Service, PeriodRecapController/Service/Builder/Window and
// the request half of PeriodRecapThemes, MeResurfaceController/Service,
// MeLearnedController/Service, MeCommitmentsController + CommitmentsService
// reads, LastKnownLocationController and ApnsCategoryPrefsController).
//
// Not here (decisions.md D-me-split): the Hermes gateway, provider, LLM
// preference and connection controllers. Not here either (they stay in
// Swift, jobs.md): BriefComposer (written by the daily-brief skill run),
// CommitmentsService.record (kb-compile) and PeriodRecapThemesWorker.
//
// Two things are per process (coupling.md):
//   - The /v1/me/today cache (MeTodayCache): same 300 s TTL, but Swift
//     invalidates it from the in-process EventBus (memory_upserted,
//     achievement_unlocked), which Go never hears. A write handled by Swift
//     shows on a Go-served /v1/me/today only after the TTL.
//   - The recap themes card: GET /v1/me/recap/period claims the
//     recap_themes row exactly as Swift does, but Swift's
//     PeriodRecapThemesWorker drains an in-process inbox, so a row Go claims
//     is never generated. Swift's next claim after the 120 s in-flight cutoff
//     (attempts permitting) picks it up again.
//
// Routes and middleware follow App+build.swift: today, recap, recap/period,
// resurface, learned, commitments and brief sit behind jwtAuthenticator +
// RateLimitMiddleware(.meTodayByUser); location sits in the suggestions group
// with the same policy (so it shares the bucket); apns-categories is JWT only.
package me

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/me/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/modules/achievements"
	"lumina/internal/modules/me/db"
	"lumina/internal/ratelimit"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"

	_ "time/tzdata" // users.timezone is resolved in-process; the runtime image has no zoneinfo
)

// Options are the Swift config keys the module reads.
type Options struct {
	// RecapThemesEnabled is recap.themesEnabled (RECAP_THEMES_ENABLED, default true).
	RecapThemesEnabled bool
}

type Module struct {
	pool     *pgxpool.Pool
	q        *db.Queries
	identify echo.MiddlewareFunc
	limits   *ratelimit.Store
	opts     Options
	today    *todayCache
	subs     map[string]achievements.Sub
	now      func() time.Time
}

// New builds the module. identify is auth.Identify and limits the process-wide
// rate-limit store.
func New(pool *pgxpool.Pool, identify echo.MiddlewareFunc, limits *ratelimit.Store, opts Options) *Module {
	return &Module{
		pool:     pool,
		q:        db.New(pool),
		identify: identify,
		limits:   limits,
		opts:     opts,
		today:    newTodayCache(todayTTL),
		subs:     achievements.Current.SubsByKey(),
		now:      time.Now,
	}
}

// SetClock replaces the clock (tests).
func (m *Module) SetClock(now func() time.Time) { m.now = now }

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	limit := ratelimit.Middleware(m.limits, ratelimit.MeTodayByUser, ratelimit.UserOrIPKey)
	chain := []echo.MiddlewareFunc{m.identify, limit, auth.Require}
	r.GET("/v1/me/today", m.getToday, chain...)
	r.GET("/v1/me/recap", m.getRecap, chain...)
	r.GET("/v1/me/resurface", m.getResurface, chain...)
	r.GET("/v1/me/learned", m.getLearned, chain...)
	r.GET("/v1/me/commitments", m.listCommitments, chain...)
	r.PATCH("/v1/me/commitments/:id", m.patchCommitment, chain...)
	r.GET("/v1/me/brief", m.getBrief, chain...)
	r.POST("/v1/me/brief/:briefId/cards/:cardId/feedback", m.postFeedback, chain...)
	r.GET("/v1/me/recap/period", m.getPeriodRecap, chain...)
	r.GET("/v1/me/location", m.getLocation, chain...)
	r.DELETE("/v1/me/location", m.deleteLocation, chain...)
	r.GET("/v1/me/apns-categories", m.getApns, authMW)
	r.PUT("/v1/me/apns-categories", m.putApns, authMW)
}

// user is the slice of Swift's User model these handlers read.
type user struct {
	id        uuid.UUID
	tier      string
	timezone  string
	createdAt *time.Time
}

// loadUser is ctx.requireIdentity(): the users row of the authenticated caller.
func (m *Module) loadUser(c *echo.Context) (user, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return user{}, wire.ErrUnauthenticated
	}
	row, err := m.q.MeUser(c.Request().Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return user{}, wire.ErrUnauthenticated
	}
	if err != nil {
		return user{}, err
	}
	return user{id: id, tier: row.Tier, timezone: row.Timezone, createdAt: row.CreatedAt}, nil
}

func badRequest(msg string) *wire.Error   { return wire.NewError(http.StatusBadRequest, msg) }
func notFound(msg string) *wire.Error     { return wire.NewError(http.StatusNotFound, msg) }
func ctx(c *echo.Context) context.Context { return c.Request().Context() }

func wt(t time.Time) wire.Time { return wire.Time{Time: t} }

func wtp(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}

func wup(id *uuid.UUID) *wire.UUID {
	if id == nil {
		return nil
	}
	return &wire.UUID{UUID: *id}
}

// orZero is Swift decoding a nullable timestamptz into a non-optional Date.
// Swift would fail the whole request on NULL; seeds never contain one.
func orZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
