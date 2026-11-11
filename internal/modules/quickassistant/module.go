package quickassistant

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/devicetools"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/quickassistant/db"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// What the user reads after Confirm or Cancel, QuickAssistantActionsController's
// verbatim. Each is also the line the conversation keeps.
const (
	accessLost     = "You no longer have access to make that change, so nothing was done."
	calendarOff    = "Calendar changes are turned off, so nothing was added. Allow them in Data Access and ask again."
	phoneSilent    = "Your iPhone didn't answer, so I can't tell whether the event was added. Check Calendar before trying again."
	calendarFailed = "The event could not be added. Check Calendar before trying again."
	memoryGone     = "That memory no longer exists, so nothing was forgotten."
	genericFailure = "That didn't work, and nothing was changed."
	cancelledLine  = "Cancelled. Nothing was changed."
)

// errActionNotFound is every 404 here: a bad id, another user's action, or one
// already swept.
var errActionNotFound = wire.NewError(http.StatusNotFound, "action not found")

// Options are the middleware the routes run behind.
type Options struct {
	// Identify authenticates the request (Swift's jwtAuthenticator). Nil falls
	// back to the server's auth middleware.
	Identify echo.MiddlewareFunc
	// Gate is chat's EntitlementMiddleware(requires: .memoryQuery). It checks
	// the tier and consumes nothing (ruling 29). Nil mounts none.
	Gate echo.MiddlewareFunc
	// Limits backs RateLimitMiddleware(policy: .conversationByUser). Nil
	// mounts none.
	Limits *ratelimit.Store
}

// Module is QuickAssistantActionsController: it resolves the confirm cards the
// Quick Assistant shows.
//
//   - POST /v1/assistant/actions/:id/confirm runs the stored action, once.
//   - POST /v1/assistant/actions/:id/cancel drops it.
//
// A row belongs to the user who was asked, so another user's id is a 404.
// Confirm acts on the vault stored with the row, never on X-Vault-ID, and asks
// again whether the user may still write there (ruling 25). It is not a chat
// message: it takes no free-lane grant and no budget reservation (ruling 29).
type Module struct {
	pool  *pgxpool.Pool
	deps  Deps
	store *Store
	q     *db.Queries
	opts  Options
	tier  func(*echo.Context) (string, bool)
	log   *slog.Logger
	// afterClaim runs right after a successful claim. Tests use it to drop the
	// request at that moment.
	afterClaim func()
}

// New builds the module. d.Devices nil means a confirmed calendar_create
// fails generically (Decision 3); such a row cannot be made without a device.
func New(pool *pgxpool.Pool, d Deps, opts Options) *Module {
	store := d.Store
	if store == nil {
		store = NewStore(pool)
	}
	return &Module{
		pool: pool, deps: d, store: store, q: db.New(pool), opts: opts,
		tier: ratelimit.PGTier(pool), log: slog.Default().With("module", "quickassistant"),
	}
}

// Register mounts Swift's group: jwt, conversationByUser, then Entitlement
// .memoryQuery. It is called only when Quick Assistant is enabled.
func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	identify := m.opts.Identify
	if identify == nil {
		identify = authMW
	}
	chain := []echo.MiddlewareFunc{identify}
	if m.opts.Limits != nil {
		chain = append(chain, ratelimit.TieredMiddleware(m.opts.Limits, ratelimit.ConversationByUser, ratelimit.UserOrIPKey, m.tier))
	}
	if m.opts.Gate != nil {
		chain = append(chain, m.opts.Gate)
	}
	r.POST("/v1/assistant/actions/:id/confirm", m.confirm, chain...)
	r.POST("/v1/assistant/actions/:id/cancel", m.cancel, chain...)
}

func (m *Module) confirm(c *echo.Context) error {
	user, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	id, err := ids.Parse(c.Param("id"))
	if err != nil {
		return errActionNotFound
	}
	res, err := m.confirmAction(c.Request().Context(), id, user)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, res)
}

// confirmAction claims the action and runs it. Once claimed the row says
// confirmed, so the run and its bookkeeping go on under a context the request
// cannot cancel (ruling 31).
func (m *Module) confirmAction(ctx context.Context, id, user uuid.UUID) (Resolution, error) {
	claimed, err := m.store.Claim(ctx, id, user)
	if err != nil {
		return Resolution{}, err
	}
	if claimed == nil {
		return m.unclaimed(ctx, id, user)
	}
	if m.afterClaim != nil {
		m.afterClaim()
	}
	ctx = context.WithoutCancel(ctx)

	ok, message := m.authorizeAndExecute(ctx, claimed, user)
	if !ok {
		if err := m.store.MarkFailed(ctx, id); err != nil {
			m.log.WarnContext(ctx, "quick assistant action not marked failed", "error_type", fmt.Sprintf("%T", err))
		}
	}
	m.appendLine(ctx, claimed.ConversationID, message)
	status := StatusConfirmed
	if !ok {
		status = StatusFailed
	}
	return Resolution{Status: status, Message: message}, nil
}

// unclaimed is the answer for an action Claim did not take, by its status.
func (m *Module) unclaimed(ctx context.Context, id, user uuid.UUID) (Resolution, error) {
	status, found, err := m.store.Status(ctx, id, user)
	if err != nil {
		return Resolution{}, err
	}
	if !found {
		return Resolution{}, errActionNotFound
	}
	switch status {
	case "pending":
		return Resolution{Status: StatusExpired, Message: "This request expired. Ask again if you still want it."}, nil
	case "confirmed":
		return Resolution{Status: StatusAlreadyResolved, Message: "This was already confirmed. If it didn't happen, ask again."}, nil
	case "cancelled":
		return Resolution{Status: StatusAlreadyResolved, Message: "This was cancelled earlier, so nothing was changed."}, nil
	default:
		return Resolution{Status: StatusAlreadyResolved, Message: "This already failed and nothing was changed. Ask again if you still want it."}, nil
	}
}

func (m *Module) cancel(c *echo.Context) error {
	user, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	id, err := ids.Parse(c.Param("id"))
	if err != nil {
		return errActionNotFound
	}
	ctx := c.Request().Context()
	conversation, err := m.store.Cancel(ctx, id, user)
	if err != nil {
		return err
	}
	if conversation != nil {
		// The conversation must not end on "waiting for your confirmation",
		// which invites the model to propose it again. The row already says
		// cancelled, so a hang-up must not drop the line (ruling 55).
		m.appendLine(context.WithoutCancel(ctx), *conversation, cancelledLine)
		return wire.JSON(c, http.StatusOK, Resolution{Status: StatusCancelled, Message: cancelledLine})
	}
	_, found, err := m.store.Status(ctx, id, user)
	if err != nil {
		return err
	}
	if !found {
		return errActionNotFound
	}
	return wire.JSON(c, http.StatusOK, Resolution{Status: StatusAlreadyResolved, Message: "This was already dealt with."})
}

// MARK: - Execution

// authorizeAndExecute re-checks write access to the stored vault, then runs
// the stored action. A lost access runs nothing.
func (m *Module) authorizeAndExecute(ctx context.Context, action *ClaimedAction, user uuid.UUID) (bool, string) {
	if m.deps.Access == nil {
		return false, accessLost
	}
	access, err := m.deps.Access.ResolveVault(ctx, user, action.VaultID, vaultaccess.Write)
	if err != nil {
		return false, accessLost
	}
	switch action.Tool {
	case "calendar_create":
		return m.createEvent(ctx, action.Args, user)
	case "memory_forget":
		return m.forgetMemory(ctx, action.Args, access.VaultID, user)
	default:
		return false, genericFailure
	}
}

// createEvent writes the event to the phone once. No queue sits behind it: an
// event the phone cannot take now must not appear days later (ruling 26).
func (m *Module) createEvent(ctx context.Context, args map[string]any, user uuid.UUID) (bool, string) {
	title, hasTitle := args["title"].(string)
	start, hasStart := args["start"].(string)
	if !hasTitle || !hasStart || m.deps.Devices == nil {
		return false, genericFailure
	}
	tools := devicetools.New(m.pool, boundedDevice{m.deps.Devices}, nil)
	raw := tools.CalendarCreate(ctx, user, title, start, optionalString(args, "end"), optionalString(args, "location"))
	var result struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal([]byte(raw), &result)
	switch {
	case result.Status == "ok":
		return true, "Added “" + Shortened(title, 80) + "” to your calendar."
	case strings.Contains(result.Reason, "not allowed"):
		return false, calendarOff
	case strings.Contains(result.Reason, "did not respond"):
		return false, phoneSilent
	default:
		return false, calendarFailed
	}
}

// forgetMemory runs memory_forget through the MCP service on the stored vault,
// as Swift's MCPToolGate.dispatch with that vault does.
func (m *Module) forgetMemory(ctx context.Context, args map[string]any, vault, user uuid.UUID) (bool, string) {
	if m.deps.MCP == nil {
		return false, genericFailure
	}
	out, err := m.deps.MCP.CallAsSession(ctx, "memory_forget", args, vault, user)
	if err != nil {
		m.log.WarnContext(ctx, "quick assistant forget failed", "error_type", fmt.Sprintf("%T", err))
		return false, genericFailure
	}
	if forgotten, _ := out["forgotten"].(bool); forgotten {
		return true, "Forgot that memory."
	}
	return false, memoryGone
}

// appendLine is the line the conversation keeps. Written straight to the
// transcript, so it never goes through the indexing a chat reply gets (ruling
// 28). A failure is logged: the action has already been resolved.
func (m *Module) appendLine(ctx context.Context, conversation uuid.UUID, text string) {
	if err := m.q.AppendAssistantMessage(ctx, db.AppendAssistantMessageParams{
		ID: uuid.New(), ConversationID: conversation, Content: text,
	}); err != nil {
		m.log.WarnContext(ctx, "quick assistant confirmation line not saved", "error_type", fmt.Sprintf("%T", err))
	}
}

// optionalString is JSONValue.stringValue as an optional.
func optionalString(args map[string]any, key string) *string {
	if s, ok := args[key].(string); ok {
		return &s
	}
	return nil
}
