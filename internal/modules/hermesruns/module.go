// Package hermesruns serves /v1/hermes/runs, the Go port of Swift's
// HermesRunsController (Sources/App/Hermes/Runs/HermesRunsController.swift):
// start an agent run on the tenant's Hermes, list and read runs, follow one
// live, answer its approval prompts and stop it. Every route reads the
// persisted copy: Hermes keeps runs in memory for 300 s only.
package hermesruns

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/hermesprofile"
	"lumina/internal/hermesruns"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/ratelimit"
	"lumina/internal/sse"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

const (
	// eventPageSize is the rows fetched per replay round trip on the feed.
	eventPageSize = 200
	// feedPollInterval is the backstop re-read: the bus is lossy and
	// in-process, so a slow or cross-process consumer still converges.
	feedPollInterval = 5 * time.Second
	eventName        = "hermes.run.event"
)

// Options are the module's dependencies.
type Options struct {
	Identify echo.MiddlewareFunc
	// Hermes is the BYO resolution middleware; nil without a resolver.
	Hermes echo.MiddlewareFunc
	// Profile is hermesprofile's middleware, which composes the session key.
	Profile echo.MiddlewareFunc
	Limits  *ratelimit.Store
	// Gate is EntitlementMiddleware(requires: .chat).
	Gate    echo.MiddlewareFunc
	Service *hermesruns.Service
	Log     *slog.Logger
	// PollInterval overrides feedPollInterval (tests).
	PollInterval time.Duration
	// KeepAlive overrides sse.DefaultKeepAlive (tests).
	KeepAlive time.Duration
}

// Module is the /v1/hermes/runs route group.
type Module struct {
	opts Options
}

// New builds the module.
func New(opts Options) *Module {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = feedPollInterval
	}
	if opts.KeepAlive == 0 {
		opts.KeepAlive = sse.DefaultKeepAlive
	}
	return &Module{opts: opts}
}

// Register mounts the group behind jwt, byo, hermesProfile, conversationByUser
// and the .chat entitlement, in Swift's order.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	var mw []echo.MiddlewareFunc
	for _, f := range []echo.MiddlewareFunc{
		m.opts.Identify, m.opts.Hermes, m.opts.Profile,
		ratelimit.Middleware(m.opts.Limits, ratelimit.ConversationByUser, ratelimit.UserOrIPKey),
		m.opts.Gate,
	} {
		if f != nil {
			mw = append(mw, f)
		}
	}
	r.POST("/v1/hermes/runs", m.start, mw...)
	r.GET("/v1/hermes/runs", m.list, mw...)
	r.GET("/v1/hermes/runs/:id", m.get, mw...)
	r.GET("/v1/hermes/runs/:id/events", m.events, mw...)
	r.Streaming(http.MethodGet, "/v1/hermes/runs/:id/events")
	r.POST("/v1/hermes/runs/:id/approval", m.approve, mw...)
	r.POST("/v1/hermes/runs/:id/stop", m.stop, mw...)
}

func requireTenant(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, wire.ErrUnauthenticated
	}
	return id, nil
}

// runID is ctx.parameters.require("id", as: UUID.self), on the raw segment.
func runID(c *echo.Context) (uuid.UUID, error) {
	segs := wire.PathSegments(c.Request())
	if len(segs) < 4 {
		return uuid.Nil, wire.NewError(http.StatusBadRequest, "Expected parameter does not exist")
	}
	id, err := ids.Parse(segs[3])
	if err != nil {
		return uuid.Nil, wire.NewError(http.StatusBadRequest, "Parameter '"+segs[3]+"' can not be converted to the expected type (UUID)")
	}
	return id, nil
}

func tenantAndRun(c *echo.Context) (tenant, run uuid.UUID, err error) {
	if tenant, err = requireTenant(c); err != nil {
		return
	}
	run, err = runID(c)
	return
}

// mapErr is withMappedErrors: service and gateway errors become stable codes.
func mapErr(err error) error {
	if se, ok := hermesruns.IsServiceError(err); ok {
		return wire.NewError(se.HTTPStatus(), se.StableCode())
	}
	var ce *hermesruns.ClientError
	if errors.As(err, &ce) {
		return wire.NewError(ce.HTTPStatus(), ce.StableCode())
	}
	return err
}

// startBody is HermesRunStartRequest.
type startBody struct {
	Prompt         string     `json:"prompt"`
	SessionID      *string    `json:"sessionID,omitempty"`
	Model          *string    `json:"model,omitempty"`
	ConversationID *wire.UUID `json:"conversationID,omitempty"`
}

// approvalBody is HermesRunApprovalRequest.
type approvalBody struct {
	Choice hermesruns.Choice `json:"choice"`
}

type listResponse struct {
	Runs []hermesruns.DTO `json:"runs"`
}

// start is POST /v1/hermes/runs: 202 once Hermes accepted the run.
func (m *Module) start(c *echo.Context) error {
	tenant, err := requireTenant(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[startBody](c)
	if err != nil {
		return err
	}
	req := hermesruns.StartRequest{Prompt: body.Prompt, SessionID: body.SessionID, Model: body.Model}
	if body.ConversationID != nil {
		id := body.ConversationID.UUID
		req.ConversationID = &id
	}
	key, _ := hermesprofile.SessionKey(c)
	run, err := m.opts.Service.Start(c.Request().Context(), tenant, req, key)
	if err != nil {
		return mapErr(err)
	}
	return wire.JSON(c, http.StatusAccepted, run)
}

// approve is POST /v1/hermes/runs/:id/approval.
func (m *Module) approve(c *echo.Context) error {
	tenant, id, err := tenantAndRun(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[approvalBody](c)
	if err != nil {
		return err
	}
	run, err := m.opts.Service.Approve(c.Request().Context(), tenant, id, body.Choice)
	if err != nil {
		return mapErr(err)
	}
	return wire.JSON(c, http.StatusOK, run)
}

// stop is POST /v1/hermes/runs/:id/stop.
func (m *Module) stop(c *echo.Context) error {
	tenant, id, err := tenantAndRun(c)
	if err != nil {
		return err
	}
	run, err := m.opts.Service.Stop(c.Request().Context(), tenant, id)
	if err != nil {
		return mapErr(err)
	}
	return wire.JSON(c, http.StatusOK, run)
}

// list is GET /v1/hermes/runs?limit=&conversationID=. An unparseable
// conversationID is treated as absent.
func (m *Module) list(c *echo.Context) error {
	tenant, err := requireTenant(c)
	if err != nil {
		return err
	}
	limit, ok := wire.QueryInt(c, "limit")
	if !ok {
		limit = 20
	}
	var conversation *uuid.UUID
	if raw, ok := wire.QueryParam(c, "conversationID"); ok {
		if id, err := ids.Parse(raw); err == nil {
			conversation = &id
		}
	}
	runs, err := m.opts.Service.List(c.Request().Context(), tenant, limit, conversation)
	if err != nil {
		return mapErr(err)
	}
	if runs == nil {
		runs = []hermesruns.DTO{}
	}
	return wire.JSON(c, http.StatusOK, listResponse{Runs: runs})
}

// get is GET /v1/hermes/runs/:id.
func (m *Module) get(c *echo.Context) error {
	tenant, id, err := tenantAndRun(c)
	if err != nil {
		return err
	}
	run, err := m.opts.Service.Get(c.Request().Context(), tenant, id)
	if err != nil {
		return mapErr(err)
	}
	return wire.JSON(c, http.StatusOK, run)
}

// events is GET /v1/hermes/runs/:id/events?after=<seq>: SSE, replay then live.
// It 404s before a byte of the stream is written.
func (m *Module) events(c *echo.Context) error {
	tenant, id, err := tenantAndRun(c)
	if err != nil {
		return err
	}
	after, ok := wire.QueryInt(c, "after")
	if !ok {
		after = 0
	}
	if _, err := m.opts.Service.Get(c.Request().Context(), tenant, id); err != nil {
		return mapErr(err)
	}
	stream := sse.Produce(c.Request().Context(), func(ctx context.Context, yield func(hermesruns.EventDTO) bool) error {
		return m.feed(ctx, tenant, id, after, yield)
	})
	return sse.ServeNamedStream(c, stream, eventName, m.opts.KeepAlive)
}

// feed is HermesRunsController.feed: every wake (a bus event for this run, or
// the backstop tick) re-reads from the cursor, so a dropped bus event only
// delays an event. It ends once the run is terminal and drained.
func (m *Module) feed(ctx context.Context, tenant, id uuid.UUID, cursor int, yield func(hermesruns.EventDTO) bool) error {
	// Subscribe before the first read, so an event persisted in between still wakes.
	bus, unsubscribe := m.opts.Service.Bus().Subscribe()
	defer unsubscribe()
	ticker := time.NewTicker(m.opts.PollInterval)
	defer ticker.Stop()
	for {
		batch, err := m.opts.Service.Events(ctx, tenant, id, cursor, eventPageSize)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			m.opts.Log.WarnContext(ctx, "hermes run feed read failed", "run", ids.Upper(id), "error", err.Error())
			return err
		}
		for _, e := range batch {
			if !yield(e) {
				return nil
			}
			cursor = e.Seq
		}
		if len(batch) == eventPageSize {
			continue
		}
		run, err := m.opts.Service.Get(ctx, tenant, id)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			m.opts.Log.WarnContext(ctx, "hermes run feed read failed", "run", ids.Upper(id), "error", err.Error())
			return err
		}
		if run.Status.IsTerminal() && cursor >= run.LastSeq {
			return nil
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				break wait
			case p := <-bus:
				if p.RunID == id {
					break wait
				}
			}
		}
	}
}
