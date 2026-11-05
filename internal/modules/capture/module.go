// Package capture serves POST /v1/capture/safari: the Go port of
// LuminaVaultServer Sources/App/Capture/CaptureController.swift, a thin shim over
// internal/capture.
//
// The DTOs are hand-written, not generated: api/openapi.yaml answers 200 where
// Swift answers 202, and Swift's behaviour wins.
package capture

import (
	"errors"
	"lumina/internal/auth"
	linkcapture "lumina/internal/capture"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/posthog"
	"lumina/internal/ratelimit"
	"lumina/internal/wire"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Module registers the route. Swift's group order is jwt, entitlement, rate limit.
type Module struct {
	pool      *pgxpool.Pool
	service   *linkcapture.Service
	gate      echo.MiddlewareFunc
	limits    *ratelimit.Store
	analytics *posthog.Client
}

// New builds the module. gate is EntitlementMiddleware(.capture), and limits is
// the process's shared store. A nil analytics client captures nothing.
func New(pool *pgxpool.Pool, service *linkcapture.Service, gate echo.MiddlewareFunc, limits *ratelimit.Store, analytics *posthog.Client) *Module {
	return &Module{pool: pool, service: service, gate: gate, limits: limits, analytics: analytics}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.POST("/v1/capture/safari", m.captureSafari, authMW, m.gate,
		ratelimit.TieredMiddleware(m.limits, ratelimit.CaptureByUser, ratelimit.UserOrIPKey, m.tier))
}

// tier is the identified caller's users.tier, which scales the limit. A failed
// read falls back to the flat limit.
func (m *Module) tier(c *echo.Context) (string, bool) {
	id, ok := auth.Identity(c)
	if !ok {
		return "", false
	}
	var tier string
	if err := m.pool.QueryRow(c.Request().Context(), `SELECT tier FROM users WHERE id = $1`, id).Scan(&tier); err != nil {
		return "", false
	}
	return tier, true
}

// safariRequest is CaptureSafariRequest. The Space's key is snake_case on the
// wire: a client sending spaceId has it decode as nil, and the link lands unfiled.
type safariRequest struct {
	URL     string     `json:"url"`
	Notes   *string    `json:"notes,omitempty"`
	SpaceID *wire.UUID `json:"space_id,omitempty"`
}

type safariResponse struct {
	ID     wire.UUID `json:"id"`
	Path   string    `json:"path"`
	Status string    `json:"status"`
}

func (m *Module) captureSafari(c *echo.Context) error {
	tenant := auth.UserID(c)
	body, err := wire.Bind[safariRequest](c)
	if err != nil {
		return err
	}
	var spaceID *uuid.UUID
	if body.SpaceID != nil {
		spaceID = &body.SpaceID.UUID
	}
	result, err := m.service.CaptureLink(c.Request().Context(), tenant, body.URL, body.Notes, spaceID)
	switch {
	case errors.Is(err, linkcapture.ErrInvalidURL):
		return wire.NewError(http.StatusBadRequest, "Invalid URL")
	case errors.Is(err, linkcapture.ErrNonPublicHost):
		return wire.NewError(http.StatusBadRequest, "URL host is not enrichable")
	case errors.Is(err, linkcapture.ErrUnknownSpace):
		return wire.NewError(http.StatusBadRequest, "`space_id` does not belong to the caller")
	case err != nil:
		return err
	}
	m.analytics.Capture("link_captured", ids.Upper(tenant), map[string]any{"has_space": spaceID != nil})
	return wire.JSON(c, http.StatusAccepted, safariResponse{
		ID: wire.UUID{UUID: result.FileID}, Path: result.RelativePath, Status: "accepted",
	})
}
