// Package workflows serves /v1/workflows, /v1/workflow-templates and
// POST /v1/workflow-hooks/:hookID: the Go port of LuminaVaultServer
// Sources/App/Workflows (WorkflowController, WorkflowTemplateController,
// WorkflowWebhookController, WorkflowService, WorkflowValidator,
// WorkflowSchedulePolicy, WorkflowTemplateCatalog and the read half of
// WorkflowSpendService).
//
// Only the routes are here. WorkflowEngine, WorkflowScheduler,
// WorkflowMaintenanceService and LegacyJobWorkflowMigrator stay in Swift and poll
// the rows these routes write.
package workflows

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/workflows/oapi-codegen.yaml api/openapi.yaml"

import (
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/modules/workflows/db"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/selfimprove"
	"lumina/internal/sse"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const eventStreamPath = "/v1/workflows/runs/:runID/events/stream"

// Options are the module's collaborators, each a Swift dependency of the same name.
type Options struct {
	Identify echo.MiddlewareFunc
	Limits   *ratelimit.Store
	// Gate is EntitlementMiddleware(requires: .workflowAutomation). It covers
	// /v1/workflows only: /v1/workflow-templates has no gate and no rate limit.
	Gate echo.MiddlewareFunc
	// Box seals webhook secrets. Nil without LV_SECRET_MASTER_KEY: both webhook
	// routes answer 503, and the rest work.
	Box         *secretbox.Box
	Preferences *llm.PreferenceRepository
	Activity    *selfimprove.Activity
	// ManagedInferenceAvailable is a platform OpenRouter key being configured.
	ManagedInferenceAvailable bool
	// DecodeEventRows makes the event routes return stored events. Off, they
	// fail on any row as Swift does; see errEventRows. It's the one-line fix once
	// the owner chooses to diverge.
	DecodeEventRows bool
}

// Module is the workflow route group. Each handler keeps Swift's order: most
// decode the body before they read the identity, so a bad body from an anonymous
// caller is a 400 rather than a 401, and an anonymous request still counts against
// the rate limiter's "ip:" bucket.
type Module struct {
	pool *pgxpool.Pool
	q    *db.Queries
	opts Options
	now  func() time.Time
}

// New builds the module.
func New(pool *pgxpool.Pool, opts Options) *Module {
	return &Module{pool: pool, q: db.New(pool), opts: opts, now: time.Now}
}

// Enabled is reader.bool(forKey: "cerberus.studio.enabled", default: true) over the
// raw env value: swift-configuration's decodeBool, and the default for anything it
// can't read. Off means no routes at all, in both servers.
func Enabled(raw string) bool {
	switch strings.ToLower(raw) {
	case "no", "0", "false":
		return false
	}
	return true
}

// Register ignores the shared auth middleware on purpose; see Module.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	r.POST("/v1/workflow-hooks/:hookID", m.ingest)

	studio := []echo.MiddlewareFunc{
		m.opts.Identify,
		ratelimit.TieredMiddleware(m.opts.Limits, ratelimit.WorkflowsByUser, ratelimit.UserOrIPKey, ratelimit.PGTier(m.pool)),
	}
	if m.opts.Gate != nil {
		studio = append(studio, m.opts.Gate)
	}
	r.GET("/v1/workflows", m.list, studio...)
	r.POST("/v1/workflows", m.create, studio...)
	r.POST("/v1/workflows/validate", m.validate, studio...)
	r.GET("/v1/workflows/limits", m.limits, studio...)
	r.GET("/v1/workflows/runs", m.allRuns, studio...)
	r.GET("/v1/workflows/approvals", m.approvals, studio...)
	r.GET("/v1/workflows/runs/:runID", m.runDetail, studio...)
	r.GET("/v1/workflows/runs/:runID/events", m.runEvents, studio...)
	r.GET(eventStreamPath, m.runEventStream, studio...)
	r.Streaming(http.MethodGet, eventStreamPath)
	r.POST("/v1/workflows/runs/:runID/cancel", m.cancel, studio...)
	r.POST("/v1/workflows/runs/:runID/retry", m.retry, studio...)
	r.POST("/v1/workflows/runs/:runID/resume", m.resume, studio...)
	r.POST("/v1/workflows/approvals/:approvalID/decision", m.decide, studio...)
	r.GET("/v1/workflows/:id", m.detail, studio...)
	r.PUT("/v1/workflows/:id/draft", m.update, studio...)
	r.POST("/v1/workflows/:id/publish", m.publish, studio...)
	r.POST("/v1/workflows/:id/runs", m.run, studio...)
	r.POST("/v1/workflows/:id/webhook/rotate", m.rotate, studio...)
	r.GET("/v1/workflows/:id/runs", m.workflowRuns, studio...)
	r.GET("/v1/workflows/:id/versions", m.versions, studio...)
	r.POST("/v1/workflows/:id/versions/:version/restore", m.restore, studio...)

	templates := m.opts.Identify
	r.GET("/v1/workflow-templates", m.templates, templates)
	r.POST("/v1/workflow-templates/:templateID/instantiate", m.instantiate, templates)
	r.POST("/v1/workflow-templates/:templateID/runs", m.templateRun, templates)
}

// tenantOf is ctx.requireTenantID().
func tenantOf(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, wire.ErrUnauthenticated
	}
	return id, nil
}

// pathID is WorkflowController.pathID.
func pathID(c *echo.Context, name string) (uuid.UUID, error) {
	id, err := ids.Parse(c.Param(name))
	if err != nil {
		return uuid.Nil, wire.NewError(http.StatusBadRequest, "invalid_"+name)
	}
	return id, nil
}

// tenantAndID reads the identity, then the path id: the order of Swift's arguments.
func tenantAndID(c *echo.Context, name string) (tenant, id uuid.UUID, err error) {
	if tenant, err = tenantOf(c); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if id, err = pathID(c, name); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return tenant, id, nil
}

// respond writes out, or the error with WorkflowController.mapErrors applied.
func respond(c *echo.Context, out any, err error) error {
	if err != nil {
		return mapErrors(err)
	}
	return wire.JSON(c, http.StatusOK, out)
}

// respondTemplate is respond with WorkflowTemplateController's narrower mapErrors.
func respondTemplate(c *echo.Context, out any, err error) error {
	if err != nil {
		return mapTemplateErrors(err)
	}
	return wire.JSON(c, http.StatusOK, out)
}

// MARK: - WorkflowController

// list has no mapErrors in Swift, so a tier without read access is a bare 500.
func (m *Module) list(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	out, err := m.listWorkflows(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) create(c *echo.Context) error {
	body, err := wire.Bind[createRequest](c)
	if err != nil {
		return err
	}
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	out, err := m.createWorkflow(ctx, tenant, body.Name, body.DescriptionText, body.Definition.definition())
	if err != nil {
		return mapErrors(err)
	}
	m.opts.Activity.NoteActivity(ctx, tenant, m.now())
	m.opts.Activity.TriggerComplexSession(ctx, tenant, 0, true)
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) validate(c *echo.Context) error {
	body, err := wire.Bind[definitionRequest](c)
	if err != nil {
		return err
	}
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, m.validateForTenant(c.Request().Context(), tenant, body.definition()))
}

// limits has no mapErrors in Swift either.
func (m *Module) limits(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	tier, err := m.effectiveTier(ctx, tenant)
	if err != nil {
		return err
	}
	if tier == tierArchived {
		return errForbidden
	}
	return wire.JSON(c, http.StatusOK, m.limitsFor(ctx, tenant, tier))
}

func (m *Module) detail(c *echo.Context) error {
	tenant, id, err := tenantAndID(c, "id")
	if err != nil {
		return err
	}
	out, err := m.workflowDetail(c.Request().Context(), tenant, id)
	return respond(c, out, err)
}

func (m *Module) update(c *echo.Context) error {
	body, err := wire.Bind[draftUpdateRequest](c)
	if err != nil {
		return err
	}
	tenant, id, err := tenantAndID(c, "id")
	if err != nil {
		return err
	}
	out, err := m.updateDraft(c.Request().Context(), tenant, id, *body)
	return respond(c, out, err)
}

func (m *Module) publish(c *echo.Context) error {
	tenant, id, err := tenantAndID(c, "id")
	if err != nil {
		return err
	}
	out, err := m.publishWorkflow(c.Request().Context(), tenant, id)
	return respond(c, out, err)
}

func (m *Module) run(c *echo.Context) error {
	body, err := wire.Bind[runRequest](c)
	if err != nil {
		return err
	}
	tenant, id, err := tenantAndID(c, "id")
	if err != nil {
		return err
	}
	out, err := m.enqueue(c.Request().Context(), tenant, id, Manual, body.Input, conversationOf(body.ConversationID), nil)
	return respond(c, out, err)
}

func conversationOf(id *wire.UUID) *uuid.UUID {
	if id == nil {
		return nil
	}
	return &id.UUID
}

func (m *Module) rotate(c *echo.Context) error {
	tenant, id, err := tenantAndID(c, "id")
	if err != nil {
		return err
	}
	out, err := m.rotateWebhook(c.Request().Context(), tenant, id)
	return respond(c, out, err)
}

// allRuns has no mapErrors in Swift.
func (m *Module) allRuns(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	out, err := m.listRuns(c.Request().Context(), tenant, nil)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, out)
}

// workflowRuns has no mapErrors in Swift, and a workflow that isn't the caller's
// lists as empty.
func (m *Module) workflowRuns(c *echo.Context) error {
	tenant, id, err := tenantAndID(c, "id")
	if err != nil {
		return err
	}
	out, err := m.listRuns(c.Request().Context(), tenant, &id)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) runDetail(c *echo.Context) error {
	tenant, id, err := tenantAndID(c, "runID")
	if err != nil {
		return err
	}
	out, err := m.runByID(c.Request().Context(), tenant, id)
	return respond(c, out, err)
}

func (m *Module) runEvents(c *echo.Context) error {
	// Hummingbird reads the query before anything here can fail, but nothing
	// observable depends on it: a bad value is 0.
	var after int64
	if raw, ok := wire.QueryParam(c, "after"); ok {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			after = n
		}
	}
	tenant, id, err := tenantAndID(c, "runID")
	if err != nil {
		return err
	}
	out, err := m.eventList(c.Request().Context(), tenant, id, after)
	return respond(c, out, err)
}

func (m *Module) runEventStream(c *echo.Context) error {
	tenant, id, err := tenantAndID(c, "runID")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if _, err := m.runByID(ctx, tenant, id); err != nil {
		return mapErrors(err)
	}
	hidden, err := m.modelHidden(ctx, tenant)
	if err != nil {
		return err
	}
	return sse.Serve(c, &eventSource{m: m, tenant: tenant, run: id, hidden: hidden}, sse.HeaderFirstCursor(c), eventStreamOptions)
}

func (m *Module) retry(c *echo.Context) error {
	tenant, id, err := tenantAndID(c, "runID")
	if err != nil {
		return err
	}
	out, err := m.retryRun(c.Request().Context(), tenant, id)
	return respond(c, out, err)
}

func (m *Module) resume(c *echo.Context) error {
	tenant, id, err := tenantAndID(c, "runID")
	if err != nil {
		return err
	}
	out, err := m.resumeRun(c.Request().Context(), tenant, id)
	return respond(c, out, err)
}

func (m *Module) versions(c *echo.Context) error {
	tenant, id, err := tenantAndID(c, "id")
	if err != nil {
		return err
	}
	out, err := m.listVersions(c.Request().Context(), tenant, id)
	return respond(c, out, err)
}

func (m *Module) restore(c *echo.Context) error {
	version, err := strconv.Atoi(c.Param("version"))
	if err != nil || version <= 0 {
		return wire.NewError(http.StatusBadRequest, "invalid_version")
	}
	tenant, id, err := tenantAndID(c, "id")
	if err != nil {
		return err
	}
	out, err := m.restoreVersion(c.Request().Context(), tenant, id, version)
	return respond(c, out, err)
}

// approvals has no mapErrors in Swift.
func (m *Module) approvals(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	out, err := m.pendingApprovals(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) cancel(c *echo.Context) error {
	tenant, id, err := tenantAndID(c, "runID")
	if err != nil {
		return err
	}
	if err := m.cancelRun(c.Request().Context(), tenant, id); err != nil {
		return mapErrors(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (m *Module) decide(c *echo.Context) error {
	body, err := wire.Bind[decisionRequest](c)
	if err != nil {
		return err
	}
	tenant, id, err := tenantAndID(c, "approvalID")
	if err != nil {
		return err
	}
	if err := m.decideApproval(c.Request().Context(), tenant, id, *body); err != nil {
		return mapErrors(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// MARK: - WorkflowTemplateController

func (m *Module) templates(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	tier, err := m.effectiveTier(c.Request().Context(), tenant)
	if err == nil && tier == tierArchived {
		err = errForbidden
	}
	return respondTemplate(c, WorkflowTemplatesResponse{Templates: templateCatalog}, err)
}

func (m *Module) instantiate(c *echo.Context) error {
	body, err := wire.Bind[instantiateRequest](c)
	if err != nil {
		return err
	}
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	out, err := m.instantiateTemplate(c.Request().Context(), tenant, c.Param("templateID"), body.Name)
	return respondTemplate(c, out, err)
}

func (m *Module) templateRun(c *echo.Context) error {
	body, err := wire.Bind[runRequest](c)
	if err != nil {
		return err
	}
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	out, err := m.runTemplate(c.Request().Context(), tenant, c.Param("templateID"), *body)
	return respondTemplate(c, out, err)
}
