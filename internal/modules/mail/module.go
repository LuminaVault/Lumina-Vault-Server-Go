// Package mail serves /v1/mail/gmail: the Go port of LuminaVaultServer
// Sources/App/Mail/GmailController. Gmail read access rides on the Calendar
// Google grant as an incremental gmail.readonly scope: same OAuth client, same
// callback (/v1/calendar/oauth/callback) and same calendar_accounts row. So the
// service is calendar.Google, and mail flips with calendar.
//
// Not here: GmailClient and GmailInboxService, the inbox reader Muse chat uses.
// No route of this module reads mail.
package mail

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/mail/oapi-codegen.yaml api/openapi.yaml"

import (
	"errors"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/calendar"
	"lumina/internal/posthog"
	"lumina/internal/wire"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type Module struct {
	google    *calendar.Google
	analytics *posthog.Client
}

// New builds the module. A nil google (no LV_SECRET_MASTER_KEY) registers
// nothing, as in Swift.
func New(google *calendar.Google, analytics *posthog.Client) *Module {
	return &Module{google: google, analytics: analytics}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	if m.google == nil {
		return
	}
	r.GET("/v1/mail/gmail/status", m.status, authMW)
	r.POST("/v1/mail/gmail/connect", m.connect, authMW)
	r.POST("/v1/mail/gmail/disconnect", m.disconnect, authMW)
}

func tenantOf(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, wire.ErrUnauthenticated
	}
	return id, nil
}

func (m *Module) status(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	s, err := m.google.GmailStatus(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, GmailStatusResponse{
		Connected: s.Connected, NeedsReauth: s.NeedsReauth, AccountEmail: s.AccountEmail, CalendarConnected: s.CalendarConnected,
	})
}

func (m *Module) connect(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	url, err := m.google.Start(c.Request().Context(), tenant, calendar.ReturnToParam(c), calendar.PurposeGmail)
	switch {
	case errors.Is(err, calendar.ErrNotConfigured):
		return wire.NewError(http.StatusServiceUnavailable, "Google sign-in is not configured on this server")
	case errors.Is(err, calendar.ErrInvalidReturn):
		return wire.NewError(http.StatusBadRequest, "invalid_return_to")
	case err != nil:
		return err
	}
	m.analytics.Capture("gmail_connect_started", ids.Upper(tenant), nil)
	return wire.JSON(c, http.StatusOK, CalendarConnectStartResponse{AuthorizeURL: url})
}

func (m *Module) disconnect(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	if err := m.google.DisconnectGmail(c.Request().Context(), tenant); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
