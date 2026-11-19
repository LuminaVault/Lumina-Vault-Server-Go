// Package mcp serves /v1/mcp (MCP over Streamable HTTP) and
// /v1/me/agent-connections: the Go port of LuminaVaultServer Sources/App/MCP
// (wave 5, group 5F). The tool set composes the same services the HTTP
// routes use: vaultindex, chunkindex, memorystore, soul and devicetools.
//
// The vault a call acts on comes from the caller's identity (and X-Vault-ID)
// on every request, never from a tool argument.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/connectedapps"
	"lumina/internal/httpserver"
	"lumina/internal/ratelimit"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// maxBody is MCPController's `collect(upTo: 1024 * 1024)`.
const maxBody = 1 << 20

// DefaultPublicBaseURL is App+build's fallback when neither mcp.publicBaseUrl
// nor ingestion.publicBaseUrl is set.
const DefaultPublicBaseURL = "https://api.luminavault.com"

// Options are the module's dependencies.
type Options struct {
	Identify    echo.MiddlewareFunc // auth.Identify: JWTAuthenticator
	Limits      *ratelimit.Store
	VaultAccess *vaultaccess.Service
	Connections *Connections
	Service     *Service
	// PublicBaseURL is mcpPublicBaseURL, resolved by cmd/App.
	PublicBaseURL string
	// ConnectedApps proxies the caller's connected-app tools. Nil (connected
	// apps off) means no app tools and no grants on the wire. Never assign a
	// typed nil.
	ConnectedApps ConnectedApps
	// ConnectedAppIDs is the registry's app ids: the only values a key's
	// connectedAppGrants may hold.
	ConnectedAppIDs []string
}

// ConnectedApps is the user's connected first-party apps as tool sources
// (connectedapps.Service). user is always the caller's identity, never a vault.
type ConnectedApps interface {
	Tools(ctx context.Context, user uuid.UUID, onlyApps []string) ([]connectedapps.AppTool, error)
	Call(ctx context.Context, user uuid.UUID, name string, args map[string]any) (string, error)
}

// Module is the two route groups.
type Module struct{ opt Options }

// New returns the module.
func New(opt Options) *Module {
	if opt.PublicBaseURL == "" {
		opt.PublicBaseURL = DefaultPublicBaseURL
	}
	return &Module{opt: opt}
}

func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	mcpChain := []echo.MiddlewareFunc{
		originGuard,
		ratelimit.Middleware(m.opt.Limits, ratelimit.MCPAnonymousByIP, ratelimit.IPOnlyKey),
		m.authenticate,
		ratelimit.Middleware(m.opt.Limits, ratelimit.MCPByUser, ratelimit.UserOrIPKey),
	}
	r.POST("/v1/mcp", m.handle, mcpChain...)

	settings := []echo.MiddlewareFunc{
		m.opt.Identify,
		ratelimit.Middleware(m.opt.Limits, ratelimit.SettingsByUser, ratelimit.UserOrIPKey),
	}
	r.GET("/v1/me/agent-connections", m.list, settings...)
	r.POST("/v1/me/agent-connections", m.issue, settings...)
	r.GET("/v1/me/agent-connections/preview", m.preview, settings...)
	r.PATCH("/v1/me/agent-connections/:id", m.update, settings...)
	r.DELETE("/v1/me/agent-connections/:id", m.revoke, settings...)
}

var errForbidden = wire.NewError(http.StatusForbidden, "forbidden")

// originGuard is MCPOriginGuard with no allowed origins: any non-empty Origin
// is a 403.
func originGuard(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if origin, ok := wire.HeaderValue(c.Request(), "Origin"); ok && origin != "" {
			slog.WarnContext(c.Request().Context(), "mcp request rejected: unrecognised origin", "origin", origin)
			return errForbidden
		}
		return next(c)
	}
}

// bearer is the trimmed token after "Bearer ", or false.
func bearer(r *http.Request) (string, bool) {
	header, ok := wire.HeaderValue(r, "Authorization")
	if !ok {
		return "", false
	}
	rest, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return "", false
	}
	token := swiftstr.TrimWhitespaces(rest)
	return token, token != ""
}

// agentToken is MCPController.agentToken: the bearer, when it is an lv_ key.
func agentToken(r *http.Request) (string, bool) {
	token, ok := bearer(r)
	if !ok || !strings.HasPrefix(token, TokenPrefix) {
		return "", false
	}
	return token, true
}

// authenticate is MCPAuthenticator: an lv_ key, else the JWT authenticator.
// It never rejects; the handler's requireIdentity does.
func (m *Module) authenticate(next echo.HandlerFunc) echo.HandlerFunc {
	jwt := m.opt.Identify(next)
	return func(c *echo.Context) error {
		token, ok := bearer(c.Request())
		if !ok {
			return next(c)
		}
		if !strings.HasPrefix(token, TokenPrefix) {
			return jwt(c)
		}
		user, found, err := m.opt.Connections.Authenticate(c.Request().Context(), token)
		if err != nil {
			return err
		}
		if found {
			auth.SetIdentity(c, user)
		}
		return next(c)
	}
}

func (m *Module) handle(c *echo.Context) error {
	if _, ok := auth.Identity(c); !ok {
		return auth.ErrUnauthorized
	}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxBody {
		return wire.ErrBodyTooLarge
	}
	rpc, err := decodeRequest(body)
	if err != nil {
		return writeRPC(c, failure(nil, parseError("request is not valid JSON-RPC")))
	}
	notification := rpc.id == nil

	result, err := m.dispatch(c, rpc)
	if err != nil {
		if notification {
			return c.NoContent(http.StatusAccepted)
		}
		var rpcErr *rpcError
		if errors.As(err, &rpcErr) {
			return writeRPC(c, failure(rpc.id, rpcErr))
		}
		slog.ErrorContext(c.Request().Context(), fmt.Sprintf("mcp.internalError method=%s: %v", rpc.method, err))
		return writeRPC(c, failure(rpc.id, internalError("tool execution failed")))
	}
	if result == nil || notification {
		return c.NoContent(http.StatusAccepted)
	}
	return writeRPC(c, success(rpc.id, result))
}

func writeRPC(c *echo.Context, r response) error {
	b, err := wire.MarshalFoundation(r)
	if err != nil {
		return err
	}
	return c.Blob(http.StatusOK, "application/json", b)
}

// instructions is initialize's text: Instructions as it always was, plus the
// connected-apps paragraph only when this server offers those tools.
func (m *Module) instructions() string {
	if m.opt.ConnectedApps == nil {
		return Instructions
	}
	return Instructions + "\n\n" + ConnectedAppInstructions
}

// dispatch returns nil for a notification that is accepted and ignored.
func (m *Module) dispatch(c *echo.Context, rpc *request) (any, error) {
	switch rpc.method {
	case "initialize":
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": ServerName, "title": ServerTitle, "version": ServerVersion},
			"instructions":    m.instructions(),
		}, nil
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": append(toolListing(), m.connectedAppListing(c)...)}, nil
	case "tools/call":
		return m.callTool(c, rpc)
	}
	return nil, methodNotFound("method '" + rpc.method + "' is not supported")
}

func (m *Module) callTool(c *echo.Context, rpc *request) (any, error) {
	params, _ := rpc.params.(map[string]any)
	name, ok := params["name"].(string)
	if params == nil || !ok {
		return nil, invalidParams("tools/call requires a `name`")
	}
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	// Lumina's own tools come first, so an app tool can never shadow one.
	t, ok := lookupTool(name)
	if !ok {
		if appID, isApp := m.connectedApp(name); isApp {
			return m.callConnectedApp(c, appID, name, args)
		}
		return nil, methodNotFound("unknown tool '" + name + "'")
	}
	if unknown, found := unknownArgument(t, args); found {
		return nil, invalidParams("unknown argument '" + unknown + "' for tool '" + name + "'")
	}
	ctx := c.Request().Context()
	if t.writes {
		if token, ok := agentToken(c.Request()); ok {
			allowed, err := m.opt.Connections.AllowsWrites(ctx, token)
			if err != nil {
				return nil, err
			}
			if !allowed {
				return toolContent(map[string]any{
					"isError": true,
					"message": "this agent key is read-only — the user can give it write access in LuminaVault Settings → Agent connections",
				}), nil
			}
		}
	}
	if t.personal {
		return m.callPersonal(c, t, args)
	}
	perm := vaultaccess.Read
	if t.writes {
		perm = vaultaccess.Write
	}
	access, err := m.opt.VaultAccess.Resolve(c, perm)
	if err != nil {
		return nil, err
	}
	who, err := m.caller(c)
	if err != nil {
		return nil, err
	}
	result, err := m.opt.Service.call(ctx, name, args, access.VaultID, who)
	if err != nil {
		return nil, err
	}
	return toolContent(result), nil
}

// callPersonal ignores X-Vault-ID: personal data is always the caller's own.
func (m *Module) callPersonal(c *echo.Context, t tool, args map[string]any) (any, error) {
	user, ok := auth.Identity(c)
	if !ok {
		return nil, auth.ErrUnauthorized
	}
	if token, ok := agentToken(c.Request()); ok {
		allowed, err := m.opt.Connections.AllowsPersonalData(c.Request().Context(), token)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return toolContent(map[string]any{
				"isError": true,
				"message": "this agent key is not allowed personal data — the user can turn on " +
					"\"Health, Calendar & Reminders\" for it in LuminaVault Settings → Agent connections",
			}), nil
		}
	}
	result, err := m.opt.Service.callPersonal(c.Request().Context(), t.name, args, user)
	if err != nil {
		return nil, err
	}
	return toolContent(result), nil
}

// connectedApp is the registry app a "<app>__<tool>" name belongs to. It is
// false for every name when connected apps are off.
func (m *Module) connectedApp(name string) (string, bool) {
	if m.opt.ConnectedApps == nil {
		return "", false
	}
	appID, _, ok := connectedapps.SplitName(name)
	if !ok || !slices.Contains(m.opt.ConnectedAppIDs, appID) {
		return "", false
	}
	return appID, true
}

// connectedAppListing is the caller's connected-app tools for tools/list: for
// an agent key only the apps the user granted that key, for a JWT session all
// of the user's own. The user is the caller's identity; X-Vault-ID plays no
// part. A failure lists no app tools and leaves Lumina's own list intact.
func (m *Module) connectedAppListing(c *echo.Context) []any {
	user, ok := auth.Identity(c)
	if m.opt.ConnectedApps == nil || !ok {
		return nil
	}
	ctx := c.Request().Context()
	// nil means every app the user connected; that is for JWT sessions only.
	var only []string
	if token, ok := agentToken(c.Request()); ok {
		grants, err := m.opt.Connections.connectedAppGrants(ctx, token)
		if err != nil {
			slog.WarnContext(ctx, "mcp: connected-app grants lookup failed")
			return nil
		}
		if len(grants) == 0 {
			return nil
		}
		only = grants
	}
	tools, err := m.opt.ConnectedApps.Tools(ctx, user, only)
	if err != nil {
		slog.WarnContext(ctx, "mcp: connected-app tools unavailable")
		return nil
	}
	out := []any{}
	for _, t := range tools {
		if l, ok := m.connectedAppToolListing(t, only); ok {
			out = append(out, l)
		}
	}
	return out
}

// connectedAppToolListing is one app tool as tools/list shows it, or false for
// a tool that must not be listed: a name that is not "<app>__<tool>" for a
// registry app (and, for a key, a granted one), a name Lumina's own catalog
// has, or an input schema that is not a JSON object schema.
func (m *Module) connectedAppToolListing(t connectedapps.AppTool, only []string) (map[string]any, bool) {
	appID, isApp := m.connectedApp(t.Name)
	if !isApp || appID != t.App.ID || (only != nil && !slices.Contains(only, appID)) {
		return nil, false
	}
	if _, static := lookupTool(t.Name); static {
		return nil, false
	}
	var schema map[string]any
	if err := json.Unmarshal(t.Remote.InputSchema, &schema); err != nil || schema["type"] != "object" {
		return nil, false
	}
	description := "From the user's connected " + t.App.Name + " account (read-only)."
	if t.Remote.Description != "" {
		description += " " + t.Remote.Description
	}
	return map[string]any{
		"name":        t.Name,
		"title":       t.App.Name + ": " + t.Remote.Name,
		"description": description,
		"inputSchema": schema,
		"annotations": map[string]any{
			"readOnlyHint":    true,
			"destructiveHint": false,
			"idempotentHint":  true,
		},
	}, true
}

// callConnectedApp runs one connected-app tool as the caller. An agent key
// needs the user's grant for that app, checked here on every call whether or
// not the key ever listed the tool; a JWT session is the user and needs none.
// The grant is separate from allowPersonalData. The refusal is decided before
// the service is asked anything, so it says nothing about whether the user
// connected the app. Arguments and results are never logged.
func (m *Module) callConnectedApp(c *echo.Context, appID, name string, args map[string]any) (any, error) {
	user, ok := auth.Identity(c)
	if !ok {
		return nil, auth.ErrUnauthorized
	}
	ctx := c.Request().Context()
	if token, ok := agentToken(c.Request()); ok {
		allowed, err := m.opt.Connections.AllowsConnectedApp(ctx, token, appID)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return toolContent(map[string]any{
				"isError": true,
				"message": "this agent key is not allowed to use connected apps it was not granted — " +
					"the user can grant it an app in LuminaVault Settings → Agent connections",
			}), nil
		}
	}
	out, err := m.opt.ConnectedApps.Call(ctx, user, name, args)
	if errors.Is(err, connectedapps.ErrToolNotAllowed) {
		return nil, methodNotFound("unknown tool '" + name + "'")
	}
	if err != nil {
		// The service's errors are fixed sentences with no remote text.
		return toolContent(map[string]any{"isError": true, "message": err.Error()}), nil
	}
	// The app's answer is data, not this server's verdict: a field named
	// isError in it stays in structuredContent and cannot set the MCP flag.
	return toolContentWith(connectedAppResult(out), false), nil
}

// connectedAppResult wraps an app tool's text as personalResult wraps a
// device's: a JSON object is the structured result, anything else is {"text"}.
func connectedAppResult(raw string) map[string]any {
	var object map[string]any
	if err := json.Unmarshal([]byte(raw), &object); err != nil || object == nil {
		return map[string]any{"text": raw}
	}
	return object
}

func (m *Module) caller(c *echo.Context) (caller, error) {
	user, ok := auth.Identity(c)
	if !ok {
		return caller{}, auth.ErrUnauthorized
	}
	token, ok := agentToken(c.Request())
	if !ok {
		return caller{userID: user}, nil
	}
	conn, err := m.opt.Connections.byToken(c.Request().Context(), token)
	if err != nil {
		return caller{}, err
	}
	if conn == nil {
		return caller{userID: user}, nil
	}
	return caller{userID: user, connectionID: &conn.ID, connectionName: &conn.Name, clientKind: &conn.ClientKind}, nil
}

// Agent connections (AgentConnectionsController).

type issueRequest struct {
	Name              string     `json:"name"`
	ClientKind        clientKind `json:"clientKind"`
	AllowPersonalData *bool      `json:"allowPersonalData,omitempty"`
	Access            *access    `json:"access,omitempty"`
}

type updateRequest struct {
	AllowPersonalData *bool   `json:"allowPersonalData,omitempty"`
	Access            *access `json:"access,omitempty"`
	// ConnectedAppGrants replaces the key's grants: omitted or null leaves
	// them, [] clears them.
	ConnectedAppGrants *[]string `json:"connectedAppGrants,omitempty"`
}

// surface is a connection as this server shows it: with connected apps off,
// its grants are not on the wire.
func (m *Module) surface(d ConnectionDTO) ConnectionDTO {
	if m.opt.ConnectedApps == nil {
		d.ConnectedAppGrants = nil
	}
	return d
}

// appGrants checks a PATCH's connectedAppGrants: registry ids only, each once,
// in the order given. With connected apps off no id is valid.
func (m *Module) appGrants(in []string) ([]string, error) {
	out := []string{}
	for _, id := range in {
		if m.opt.ConnectedApps == nil || !slices.Contains(m.opt.ConnectedAppIDs, id) {
			return nil, wire.NewError(http.StatusBadRequest, "invalid_connected_app")
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

func (m *Module) tenant(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, auth.ErrUnauthorized
	}
	return id, nil
}

func (m *Module) list(c *echo.Context) error {
	tenant, err := m.tenant(c)
	if err != nil {
		return err
	}
	conns, err := m.opt.Connections.List(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	for i := range conns {
		conns[i] = m.surface(conns[i])
	}
	return wire.JSON(c, http.StatusOK, map[string]any{"connections": conns})
}

func (m *Module) issue(c *echo.Context) error {
	tenant, err := m.tenant(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[issueRequest](c)
	if err != nil {
		return err
	}
	allowPersonal := body.AllowPersonalData != nil && *body.AllowPersonalData
	acc := AccessRead
	if body.Access != nil {
		acc = string(*body.Access)
	}
	conn, token, err := m.opt.Connections.Issue(c.Request().Context(), tenant, body.Name, string(body.ClientKind), allowPersonal, acc)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, map[string]any{
		"connection": m.surface(conn),
		"token":      token,
		"setup":      setupInstructions(string(body.ClientKind), m.opt.PublicBaseURL, token),
	})
}

func (m *Module) preview(c *echo.Context) error {
	if _, err := m.tenant(c); err != nil {
		return err
	}
	kind, ok := wire.QueryParam(c, "clientKind")
	if !ok {
		kind = KindOther
	}
	if !validKind(kind) {
		return wire.NewError(http.StatusBadRequest, "invalid_client_kind")
	}
	return wire.JSON(c, http.StatusOK, setupPreview(kind, m.opt.PublicBaseURL))
}

func (m *Module) pathID(c *echo.Context) (uuid.UUID, error) {
	// Hummingbird's trie prefers the constant "preview" segment over :id, and
	// that node only has GET, so PATCH/DELETE .../preview is an empty 404.
	if c.Param("id") == "preview" {
		return uuid.Nil, echo.ErrNotFound
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil || len(c.Param("id")) != 36 {
		return uuid.Nil, wire.NewError(http.StatusBadRequest, "invalid_id")
	}
	return id, nil
}

func (m *Module) update(c *echo.Context) error {
	tenant, err := m.tenant(c)
	if err != nil {
		return err
	}
	id, err := m.pathID(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[updateRequest](c)
	if err != nil {
		return err
	}
	if m.opt.ConnectedApps == nil {
		// Connected apps are off: the field is not part of the API, and is
		// ignored like any other unknown field.
		body.ConnectedAppGrants = nil
	}
	if body.AllowPersonalData == nil && body.Access == nil && body.ConnectedAppGrants == nil {
		return wire.NewError(http.StatusBadRequest, "nothing_to_update")
	}
	var grants *[]string
	if body.ConnectedAppGrants != nil {
		checked, err := m.appGrants(*body.ConnectedAppGrants)
		if err != nil {
			return err
		}
		grants = &checked
	}
	var acc *string
	if body.Access != nil {
		s := string(*body.Access)
		acc = &s
	}
	conn, err := m.opt.Connections.Update(c.Request().Context(), tenant, id, body.AllowPersonalData, acc, grants)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, m.surface(conn))
}

func (m *Module) revoke(c *echo.Context) error {
	tenant, err := m.tenant(c)
	if err != nil {
		return err
	}
	id, err := m.pathID(c)
	if err != nil {
		return err
	}
	if err := m.opt.Connections.Revoke(c.Request().Context(), tenant, id); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
