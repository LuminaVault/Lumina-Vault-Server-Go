package skillscron

import (
	"context"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// Module is CronBridgeController. Swift mounts it on /v1/me/hermes/cron behind
// jwtAuthenticator only (no rate limit), and only when the SecretBox branch
// built a container manager.
type Module struct {
	svc *Service
}

// New builds the module.
func New(svc *Service) *Module { return &Module{svc: svc} }

// Service is the bridge, for the dashboard's cron preview.
func (m *Module) Service() *Service { return m.svc }

// Register mounts the routes.
func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	const p = "/v1/me/hermes/cron"
	r.GET(p, m.list, authMW)
	r.POST(p, m.create, authMW)
	r.POST(p+"/preview", m.preview, authMW)
	r.PUT(p+"/config", m.setConfig, authMW)
	r.POST(p+"/:id/pause", m.pause, authMW)
	r.POST(p+"/:id/resume", m.resume, authMW)
	r.DELETE(p+"/:id", m.remove, authMW)
}

type configRequest struct {
	DashboardURL   string `json:"dashboardUrl"`
	DashboardToken string `json:"dashboardToken"`
}

type previewRequest struct {
	Text string `json:"text"`
}

// preview: natural language → a cron spec for the user to confirm (no write).
func (m *Module) preview(c *echo.Context) error {
	tenant := auth.UserID(c)
	body, err := wire.Bind[previewRequest](c)
	if err != nil {
		return err
	}
	spec, ok := m.svc.Preview(c.Request().Context(), tenant, body.Text)
	if !ok {
		return wire.NewError(http.StatusUnprocessableEntity, "not_a_schedulable_job")
	}
	return wire.JSON(c, http.StatusOK, spec)
}

// setConfig stores the BYO dashboard cron endpoint, then returns the
// now-listable jobs.
func (m *Module) setConfig(c *echo.Context) error {
	tenant := auth.UserID(c)
	body, err := wire.Bind[configRequest](c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := m.svc.SetBYOConfig(ctx, tenant, body.DashboardURL, body.DashboardToken); err != nil {
		return err
	}
	return respond(c)(m.svc.List(ctx, tenant))
}

func (m *Module) list(c *echo.Context) error {
	return respond(c)(m.svc.List(c.Request().Context(), auth.UserID(c)))
}

func (m *Module) create(c *echo.Context) error {
	tenant := auth.UserID(c)
	spec, err := wire.Bind[CreateSpec](c)
	if err != nil {
		return err
	}
	// trimmingCharacters(in: .whitespaces): spaces and tabs, not newlines.
	if swiftstr.TrimWhitespaces(spec.Schedule) == "" {
		return wire.NewError(http.StatusBadRequest, "schedule_required")
	}
	return respond(c)(m.svc.Create(c.Request().Context(), tenant, *spec))
}

func (m *Module) pause(c *echo.Context) error  { return m.mutate(c, "pause") }
func (m *Module) resume(c *echo.Context) error { return m.mutate(c, "resume") }
func (m *Module) remove(c *echo.Context) error { return m.mutate(c, "remove") }

func (m *Module) mutate(c *echo.Context, action string) error {
	tenant := auth.UserID(c)
	id := c.Param("id")
	if id == "" {
		return wire.NewError(http.StatusBadRequest, "id_required")
	}
	jobs, err := m.svc.MutateManaged(c.Request().Context(), tenant, action, id)
	return respond(c)(ListResponse{Source: "managed", Jobs: jobs}, err)
}

// respond writes a service answer as 200 JSON, or returns its error.
func respond(c *echo.Context) func(ListResponse, error) error {
	return func(list ListResponse, err error) error {
		if err != nil {
			return err
		}
		return wire.JSON(c, http.StatusOK, list)
	}
}

// CronPreview is CronBridgePreview.cronPreview: the first five jobs from
// whichever source the tenant has, and no jobs on any error.
func (s *Service) CronPreview(ctx context.Context, tenant uuid.UUID) []Job {
	list, err := s.List(ctx, tenant)
	if err != nil {
		return []Job{}
	}
	return list.Jobs[:min(len(list.Jobs), 5)]
}
