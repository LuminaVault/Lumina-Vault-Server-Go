// Package mcp serves /v1/mcp (MCP over Streamable HTTP) and
// /v1/me/agent-connections: the Go port of LuminaVaultServer Sources/App/MCP
// (wave 5, group 5F). The tool set composes the same services the HTTP
// routes use: vaultindex, chunkindex, memorystore, soul and devicetools.
//
// The vault a call acts on comes from the caller's identity (and X-Vault-ID)
// on every request, never from a tool argument.
package mcp

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ratelimit"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"net/http"
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

// dispatch returns nil for a notification that is accepted and ignored.
func (m *Module) dispatch(c *echo.Context, rpc *request) (any, error) {
	switch rpc.method {
	case "initialize":
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": ServerName, "title": ServerTitle, "version": ServerVersion},
			"instructions":    Instructions,
		}, nil
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolListing()}, nil
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
	t, ok := lookupTool(name)
	if !ok {
		return nil, methodNotFound("unknown tool '" + name + "'")
	}
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
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
		"connection": conn,
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
	if body.AllowPersonalData == nil && body.Access == nil {
		return wire.NewError(http.StatusBadRequest, "nothing_to_update")
	}
	var acc *string
	if body.Access != nil {
		s := string(*body.Access)
		acc = &s
	}
	conn, err := m.opt.Connections.Update(c.Request().Context(), tenant, id, body.AllowPersonalData, acc)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, conn)
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
