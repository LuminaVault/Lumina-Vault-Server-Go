// Package connectedapps serves the settings API for connected apps: a user
// lists, connects and disconnects their own Norviq, Aaru, Khepri and Loci
// accounts. The work is in lumina/internal/connectedapps; this package is the
// HTTP surface.
//
// Every route acts on the user id of the session token (auth.Identity), never
// on a vault id. Only the OAuth callback is unauthenticated: its state names
// the user. No response, redirect or log line carries a token, an OAuth code
// or text from a remote server.
package connectedapps

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"lumina/internal/auth"
	core "lumina/internal/connectedapps"
	"lumina/internal/httpserver"
	"lumina/internal/modules/calendar"
	"lumina/internal/wire"
)

const (
	statusConnected    = "connected"
	statusDisconnected = "disconnected"

	// appCallbackBase is where the callback sends the iOS app.
	appCallbackBase = "luminavault://connected-apps"
)

// Forgetter drops a user's cached tool list for an app; *core.Service is one.
type Forgetter interface {
	Forget(user uuid.UUID, appID string)
}

type Options struct {
	Registry core.Registry
	Store    *core.Store
	OAuth    *core.OAuth
	Service  Forgetter
	// OAuthReady is false when the server has no public base URL to build the
	// OAuth callback from. OAuth connects then answer 503; PAT connects work.
	OAuthReady bool
	// WebReturnOrigins is the returnTo allowlist, the same list calendar uses.
	WebReturnOrigins []string
	// Log is where failures go. Nil means slog.Default().
	Log *slog.Logger
}

type Module struct {
	registry   core.Registry
	store      *core.Store
	oauth      *core.OAuth
	service    Forgetter
	oauthReady bool
	webReturn  []string
	log        *slog.Logger
}

func New(opts Options) *Module {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Module{
		registry: opts.Registry, store: opts.Store, oauth: opts.OAuth, service: opts.Service,
		oauthReady: opts.OAuthReady, webReturn: opts.WebReturnOrigins, log: log,
	}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.GET("/v1/me/connected-apps", m.list, authMW)
	r.POST("/v1/me/connected-apps/:app/connect", m.connect, authMW)
	r.DELETE("/v1/me/connected-apps/:app", m.disconnect, authMW)
	r.GET("/v1/connected-apps/oauth/callback", m.oauthCallback)
}

func userOf(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, wire.ErrUnauthenticated
	}
	return id, nil
}

var errUnknownApp = wire.NewError(http.StatusNotFound, "Unknown app")

func (m *Module) appOf(c *echo.Context) (core.App, error) {
	app, ok := m.registry.Get(c.Param("app"))
	if !ok {
		return core.App{}, errUnknownApp
	}
	return app, nil
}

// MARK: GET /v1/me/connected-apps

type appDTO struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Blurb       string     `json:"blurb"`
	Auth        string     `json:"auth"`
	Status      string     `json:"status"`
	ConnectedAt *wire.Time `json:"connectedAt,omitempty"`
}

// list answers every app in the registry with the user's status for it.
func (m *Module) list(c *echo.Context) error {
	user, err := userOf(c)
	if err != nil {
		return err
	}
	accounts, err := m.store.List(c.Request().Context(), user) // no tokens
	if err != nil {
		return err
	}
	byApp := make(map[string]core.Account, len(accounts))
	for _, a := range accounts {
		byApp[a.AppID] = a
	}
	apps := m.registry.All()
	out := make([]appDTO, len(apps))
	for i, app := range apps {
		dto := appDTO{ID: app.ID, Name: app.Name, Blurb: app.Blurb, Auth: string(app.Auth), Status: statusDisconnected}
		if acct, ok := byApp[app.ID]; ok {
			dto.Status = acct.Status
			if acct.Status == statusConnected {
				dto.ConnectedAt = &wire.Time{Time: acct.ConnectedAt}
			}
		}
		out[i] = dto
	}
	return wire.JSON(c, http.StatusOK, out)
}

// MARK: POST /v1/me/connected-apps/:app/connect

type connectRequest struct {
	ReturnTo *string `json:"returnTo,omitempty"`
	Token    *string `json:"token,omitempty"`
}

type connectResponse struct {
	AuthorizeURL string `json:"authorizeUrl"`
}

var (
	errTokenRequired   = wire.NewError(http.StatusBadRequest, "This app connects with an access token")
	errTokenNotAllowed = wire.NewError(http.StatusBadRequest, "This app connects by signing in, not with a token")
	errBadToken        = wire.NewError(http.StatusBadRequest, "That token is not in this app's format")
	errInvalidReturn   = wire.NewError(http.StatusBadRequest, "invalid_return_to")
	errOAuthNotReady   = wire.NewError(http.StatusServiceUnavailable, "Connecting apps is not configured on this server")
)

// connect stores a pasted token for a PAT app, or starts the OAuth flow and
// answers the URL to open.
func (m *Module) connect(c *echo.Context) error {
	user, err := userOf(c)
	if err != nil {
		return err
	}
	app, err := m.appOf(c)
	if err != nil {
		return err
	}
	raw, err := wire.ReadBody(c)
	if err != nil {
		return err
	}
	req := &connectRequest{}
	if len(bytes.TrimSpace(raw)) > 0 { // an OAuth connect may send no body
		if req, err = wire.Decode[connectRequest](raw); err != nil {
			return err
		}
	}
	ctx := c.Request().Context()

	if app.Auth == core.AuthPAT {
		if req.Token == nil {
			return errTokenRequired
		}
		switch err := m.oauth.SavePAT(ctx, user, app.ID, *req.Token); {
		case errors.Is(err, core.ErrBadPAT):
			return errBadToken
		case errors.Is(err, core.ErrNotOAuth):
			return errTokenNotAllowed
		case errors.Is(err, core.ErrUnknownApp):
			return errUnknownApp
		case err != nil:
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}

	if req.Token != nil {
		return errTokenNotAllowed
	}
	if !m.oauthReady {
		return errOAuthNotReady
	}
	var returnTo *string
	if req.ReturnTo != nil {
		valid, ok := calendar.ValidReturn(*req.ReturnTo, m.webReturn)
		if !ok {
			return errInvalidReturn
		}
		returnTo = &valid
	}
	authorizeURL, err := m.oauth.Start(ctx, user, app.ID, returnTo)
	switch {
	case errors.Is(err, core.ErrUnknownApp):
		return errUnknownApp
	case errors.Is(err, core.ErrNotOAuth):
		return errTokenRequired
	case err != nil:
		// Discovery or registration failed. OAuth errors hold no token and no
		// remote body, but none of their text goes to the client.
		m.log.ErrorContext(ctx, "connected app connect failed", "app", app.ID, "error", err)
		return wire.NewError(http.StatusBadGateway, "Could not reach "+app.Name+". Try again later.")
	}
	return wire.JSON(c, http.StatusOK, connectResponse{AuthorizeURL: authorizeURL})
}

// MARK: DELETE /v1/me/connected-apps/:app

// disconnect revokes the refresh token where the app allows it, deletes the
// user's row and drops the cached tools. Disconnecting an app the user never
// connected is 204 as well.
func (m *Module) disconnect(c *echo.Context) error {
	user, err := userOf(c)
	if err != nil {
		return err
	}
	app, err := m.appOf(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if app.Auth == core.AuthOAuth {
		// Best effort: whatever happens here, the row below still goes.
		if err := m.oauth.Revoke(ctx, user, app.ID); err != nil {
			m.log.WarnContext(ctx, "connected app revoke failed", "app", app.ID, "kind", errorKind(err))
		}
	}
	if err := m.store.Delete(ctx, user, app.ID); err != nil {
		return err
	}
	m.service.Forget(user, app.ID)
	return c.NoContent(http.StatusNoContent)
}

// errorKind is a coarse label for a log line, so the error's own text (which
// may name a host) stays out of it.
func errorKind(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	return "failed"
}

// MARK: GET /v1/connected-apps/oauth/callback

// oauthCallback finishes the flow the app's authorization server sent back.
// Every outcome is a redirect, and the redirect carries a status only: never
// the error, the code or a token.
func (m *Module) oauthCallback(c *echo.Context) error {
	state, _ := wire.QueryParam(c, "state")
	code, _ := wire.QueryParam(c, "code")
	if state == "" || code == "" {
		return c.Redirect(http.StatusFound, appRedirect("", "error"))
	}
	// The browser leaving must not abandon an exchange the app already consumed.
	ctx := context.WithoutCancel(c.Request().Context())
	appID, returnTo, err := m.oauth.Finish(ctx, state, code)
	if _, known := m.registry.Get(appID); !known {
		appID = ""
	}
	if err != nil {
		if !errors.Is(err, core.ErrBadState) {
			m.log.ErrorContext(ctx, "connected app oauth exchange failed", "app", appID, "kind", errorKind(err))
		}
		return c.Redirect(http.StatusFound, appRedirect(appID, "error"))
	}
	if returnTo != nil {
		// Checked at connect; checked again so a bad value can never leave here.
		if valid, ok := calendar.ValidReturn(*returnTo, m.webReturn); ok {
			return c.Redirect(http.StatusFound, valid)
		}
	}
	return c.Redirect(http.StatusFound, appRedirect(appID, statusConnected))
}

// appRedirect is the deep link back into the iOS app. appID is a registry id
// (lowercase letters) or empty when the app is unknown.
func appRedirect(appID, status string) string {
	base := appCallbackBase
	if appID != "" {
		base += "/" + appID
	}
	return base + "?status=" + status
}
