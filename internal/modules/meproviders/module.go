// Package meproviders serves /v1/me/providers, /v1/me/preferences/llm and
// /v1/me/connections: the Go port of LuminaVaultServer Sources/App/Me
// ProvidersController.swift, LLMPreferencesController.swift and
// ConnectionsController.swift, with the write half of UserCredentialStore and
// UserLLMPreferenceRepository.
//
// Reads and writes go through the stack's one CredentialStore and
// PreferenceRepository, so a write here drops the cached row the router reads,
// as Swift's single actor does. A write through the other server is not seen
// until the 600 s cache expires: see the PR's flip-blocker note.
package meproviders

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/meproviders/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/modules/meproviders/db"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Options wires the module. Stack is the process's one LLM stack. Box is the
// SecretBox: without it /v1/me/providers is not mounted, as in Swift, where the
// controller needs userCredentialStore.
type Options struct {
	Identify echo.MiddlewareFunc
	Limits   *ratelimit.Store
	Stack    *llm.Stack
	Box      *secretbox.Box
	// Client is the probe session for /test and /models. Nil means a default client.
	Client *http.Client
}

// Module is the route group. Every route is jwtAuthenticator + settingsByUser in
// Swift, and each handler requires identity itself.
type Module struct {
	pool   *pgxpool.Pool
	q      *db.Queries
	opts   Options
	client *http.Client
	log    *slog.Logger
	now    func() time.Time
	// probes tracks detached tool probes, so tests and shutdown can wait for them.
	probes sync.WaitGroup
	// probeCtx is the probes' context: they outlive the request, as a Swift Task does.
	probeCtx context.Context
}

func New(pool *pgxpool.Pool, opts Options) *Module {
	client := opts.Client
	if client == nil {
		client = &http.Client{}
	}
	return &Module{
		pool: pool, q: db.New(pool), opts: opts, client: client,
		log: slog.Default().With("module", "me-providers"), now: time.Now,
		probeCtx: context.Background(),
	}
}

// SetClock replaces the clock (tests).
func (m *Module) SetClock(now func() time.Time) { m.now = now }

// Wait blocks until every detached tool probe has stored its verdict.
func (m *Module) Wait() { m.probes.Wait() }

func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	mw := []echo.MiddlewareFunc{m.opts.Identify, ratelimit.Middleware(m.opts.Limits, ratelimit.SettingsByUser, ratelimit.UserOrIPKey), auth.Require}
	if m.opts.Box != nil && m.opts.Stack != nil && m.opts.Stack.Credentials != nil {
		r.GET("/v1/me/providers/catalog", m.catalog, mw...)
		r.GET("/v1/me/providers", m.list, mw...)
		r.PUT("/v1/me/providers/:provider", m.put, mw...)
		r.DELETE("/v1/me/providers/:provider", m.delete, mw...)
		r.POST("/v1/me/providers/:provider/test", m.test, mw...)
		r.GET("/v1/me/providers/:provider/models", m.models, mw...)
		r.GET("/v1/me/providers/:provider/pool", m.listPool, mw...)
		r.POST("/v1/me/providers/:provider/pool", m.addPool, mw...)
		r.DELETE("/v1/me/providers/:provider/pool/:keyID", m.deletePool, mw...)
	}
	r.GET("/v1/me/preferences/llm", m.getPreferences, mw...)
	r.PUT("/v1/me/preferences/llm", m.putPreferences, mw...)
	r.GET("/v1/me/connections", m.summary, mw...)
	r.POST("/v1/me/connections/test-all", m.testAll, mw...)
	r.GET("/v1/me/connections/events", m.events, mw...)
}
