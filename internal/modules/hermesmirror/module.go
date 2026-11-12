// Package hermesmirror serves /v1/hermes/mirror/* (HermesMirrorController and
// the webhook credential), /v1/hermes/artifacts, /v1/hermes/workspace/* and the
// public push route POST /v1/hermes/mirror/webhook/:token. Swift mounts all of
// it only with a secret box and a Hermes endpoint resolver.
package hermesmirror

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/hermesdash"
	"lumina/internal/hermesmirror"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/modules/workflows"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"math"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Webhook envelope (HermesMirrorWebhookController statics).
const (
	SignatureHeader     = "X-Webhook-Signature-V2"
	TimestampHeader     = "X-Webhook-Timestamp"
	ReplayWindowSeconds = 300
	MaxWebhookBodyBytes = 1024 * 1024
	WebhookRoutePrefix  = "/v1/hermes/mirror/webhook"
)

// Options are the module's dependencies.
type Options struct {
	Identify echo.MiddlewareFunc
	Limits   *ratelimit.Store
	// BYO is the HermesResolutionMiddleware (hermesresolve.Resolver.Middleware()).
	BYO     echo.MiddlewareFunc
	Box     *secretbox.Box
	Service *hermesmirror.Service
	Now     func() time.Time
	Log     *slog.Logger
}

// Module is the route group.
type Module struct {
	pool *pgxpool.Pool
	opt  Options
}

// New builds the module.
func New(pool *pgxpool.Pool, opt Options) *Module {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	return &Module{pool: pool, opt: opt}
}

// Register mounts the groups: JWT, settingsByUser, then the BYO middleware.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	chain := []echo.MiddlewareFunc{
		m.opt.Identify,
		ratelimit.Middleware(m.opt.Limits, ratelimit.SettingsByUser, ratelimit.UserOrIPKey),
	}
	if m.opt.BYO != nil {
		chain = append(chain, m.opt.BYO)
	}
	const p = "/v1/hermes/mirror/"
	r.GET(p+"status", m.status, chain...)
	r.POST(p+"sync", m.sync, chain...)
	r.GET(p+"skills", m.skills, chain...)
	r.PUT(p+"skills/:name", m.toggleSkill, chain...)
	r.GET(p+"jobs", m.jobs, chain...)
	r.POST(p+"jobs", m.createJob, chain...)
	r.POST(p+"jobs/install-compile", m.installCompileJob, chain...)
	r.GET(p+"jobs/:id/runs", m.jobRuns, chain...)
	r.PUT(p+"jobs/:id", m.updateJob, chain...)
	r.POST(p+"jobs/:id/pause", m.pauseJob, chain...)
	r.POST(p+"jobs/:id/resume", m.resumeJob, chain...)
	r.POST(p+"jobs/:id/trigger", m.triggerJob, chain...)
	r.DELETE(p+"jobs/:id", m.deleteJob, chain...)
	r.POST(p+"jobs/:id/collect", m.collectJobRuns, chain...)
	r.POST(p+"vault/import", m.importVault, chain...)
	r.POST(p+"vault/create", m.createVault, chain...)
	r.POST(p+"vault/import-sessions", m.importSessions, chain...)
	r.GET(p+"webhook", m.currentWebhook, chain...)
	r.POST(p+"webhook", m.rotateWebhook, chain...)

	r.GET("/v1/hermes/artifacts", m.listArtifacts, chain...)
	r.GET("/v1/hermes/artifacts/:id", m.getArtifact, chain...)

	r.GET("/v1/hermes/workspace/status", m.workspaceStatus, chain...)
	r.GET("/v1/hermes/workspace/files", m.workspaceListing, chain...)
	r.GET("/v1/hermes/workspace/file", m.workspaceFile, chain...)
	r.GET("/v1/hermes/workspace/diff", m.workspaceDiff, chain...)

	r.POST(WebhookRoutePrefix+"/:token", m.ingestWebhook)
}

func tenantOf(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, wire.ErrUnauthenticated
	}
	return id, nil
}

// MapError is HermesMirrorController.mapErrors: a transport error becomes its
// stable code; anything else passes through.
func MapError(err error) error {
	var te *hermesdash.Error
	if !errors.As(err, &te) {
		return err
	}
	return wire.NewError(StatusFor(te), te.Code())
}

// StatusFor is HermesMirrorController.status(for:).
func StatusFor(e *hermesdash.Error) int {
	switch e.Kind {
	case hermesdash.ErrInvalidPath, hermesdash.ErrBodyTooLarge:
		return http.StatusBadRequest
	case hermesdash.ErrNotFound:
		return http.StatusNotFound
	case hermesdash.ErrNotConfigured:
		return http.StatusConflict
	case hermesdash.ErrUnsupported:
		return http.StatusNotImplemented
	default:
		return http.StatusBadGateway
	}
}

// respond runs a service call under mapErrors and writes 200 JSON.
func respond[T any](c *echo.Context, call func(ctx context.Context, tenant uuid.UUID) (T, error)) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	out, err := call(llm.RequestContext(c), tenant)
	if err != nil {
		return MapError(err)
	}
	return wire.JSON(c, http.StatusOK, out)
}

func jobID(c *echo.Context) (string, error) {
	raw := c.Param("id")
	if raw == "" {
		return "", wire.NewError(http.StatusBadRequest, "hermes_job_id_required")
	}
	return raw, nil
}

func (m *Module) status(c *echo.Context) error {
	return respond(c, m.opt.Service.Status)
}

type syncRequest struct {
	Scope *[]string `json:"scope,omitempty"`
}

func (m *Module) sync(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	scopes := map[string]bool{}
	for _, s := range hermesmirror.AllScopes {
		scopes[s] = true
	}
	if raw, err := wire.ReadBody(c); err == nil {
		var body syncRequest
		if wire.DecodeInto(raw, &body) == nil && body.Scope != nil && validScopes(*body.Scope) {
			scopes = map[string]bool{}
			for _, s := range *body.Scope {
				scopes[s] = true
			}
		}
	}
	out, err := m.opt.Service.Sync(llm.RequestContext(c), tenant, scopes)
	if err != nil {
		return MapError(err)
	}
	return wire.JSON(c, http.StatusOK, out)
}

func validScopes(scopes []string) bool {
	for _, s := range scopes {
		switch s {
		case hermesmirror.ScopeSkills, hermesmirror.ScopeJobs, hermesmirror.ScopeVault, hermesmirror.ScopeArtifacts:
		default:
			return false
		}
	}
	return true
}

func (m *Module) skills(c *echo.Context) error {
	return respond(c, func(ctx context.Context, tenant uuid.UUID) (hermesmirror.SkillsResponse, error) {
		skills, err := m.opt.Service.MirroredSkills(ctx, tenant)
		return hermesmirror.SkillsResponse{Skills: skills}, err
	})
}

type toggleRequest struct {
	Enabled bool `json:"enabled"`
}

func (m *Module) toggleSkill(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	name := c.Param("name")
	if name == "" {
		return wire.NewError(http.StatusBadRequest, "skill_name_required")
	}
	body, err := wire.Bind[toggleRequest](c)
	if err != nil {
		return err
	}
	out, err := m.opt.Service.ToggleSkill(llm.RequestContext(c), tenant, name, body.Enabled)
	if err != nil {
		return MapError(err)
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) jobs(c *echo.Context) error {
	return respond(c, m.opt.Service.Jobs)
}

func (m *Module) collectJobRuns(c *echo.Context) error {
	id, err := jobID(c)
	if err != nil {
		if _, terr := tenantOf(c); terr != nil {
			return terr
		}
		return err
	}
	return respond(c, func(ctx context.Context, tenant uuid.UUID) (hermesmirror.CollectResultDTO, error) {
		return m.opt.Service.CollectJobRuns(ctx, tenant, id)
	})
}

func (m *Module) jobRuns(c *echo.Context) error {
	id, err := jobID(c)
	if err != nil {
		return err
	}
	limit, ok := wire.QueryInt(c, "limit")
	if !ok {
		limit = hermesmirror.DefaultJobRunsLimit
	}
	return respond(c, func(ctx context.Context, tenant uuid.UUID) (hermesmirror.JobRunsResponse, error) {
		return m.opt.Service.JobRuns(ctx, tenant, id, limit)
	})
}

func (m *Module) createJob(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[hermesmirror.JobCreateRequest](c)
	if err != nil {
		return err
	}
	out, err := m.opt.Service.CreateJob(llm.RequestContext(c), tenant, *body)
	if err != nil {
		return MapError(err)
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) updateJob(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id, err := jobID(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[hermesmirror.JobUpdateRequest](c)
	if err != nil {
		return err
	}
	out, err := m.opt.Service.UpdateJob(llm.RequestContext(c), tenant, id, *body)
	if err != nil {
		return MapError(err)
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) jobAction(c *echo.Context, action func(context.Context, uuid.UUID, string) (hermesmirror.JobDTO, error)) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id, err := jobID(c)
	if err != nil {
		return err
	}
	out, err := action(llm.RequestContext(c), tenant, id)
	if err != nil {
		return MapError(err)
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) pauseJob(c *echo.Context) error  { return m.jobAction(c, m.opt.Service.PauseJob) }
func (m *Module) resumeJob(c *echo.Context) error { return m.jobAction(c, m.opt.Service.ResumeJob) }

func (m *Module) triggerJob(c *echo.Context) error { return m.jobAction(c, m.opt.Service.TriggerJob) }

func (m *Module) deleteJob(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id, err := jobID(c)
	if err != nil {
		return err
	}
	if err := m.opt.Service.DeleteJob(llm.RequestContext(c), tenant, id); err != nil {
		return MapError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (m *Module) installCompileJob(c *echo.Context) error {
	return respond(c, m.opt.Service.InstallCompileJob)
}

type vaultImportRequest struct {
	VaultPath *string `json:"vaultPath,omitempty"`
}

func (m *Module) importVault(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	var requested *string
	if raw, err := wire.ReadBody(c); err == nil {
		var body vaultImportRequest
		if wire.DecodeInto(raw, &body) == nil && body.VaultPath != nil {
			if trimmed := swiftstr.TrimWhitespacesAndNewlines(*body.VaultPath); trimmed != "" {
				requested = &trimmed
			}
		}
	}
	out, err := m.opt.Service.ImportVault(llm.RequestContext(c), tenant, requested)
	if err != nil {
		return MapError(err)
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) createVault(c *echo.Context) error {
	return respond(c, m.opt.Service.CreateVault)
}

func (m *Module) importSessions(c *echo.Context) error {
	return respond(c, m.opt.Service.ImportSessions)
}

// MARK: - Artifacts

func (m *Module) listArtifacts(c *echo.Context) error {
	q := hermesmirror.ArtifactQuery{Limit: 50}
	if v, ok := wire.QueryParam(c, "kind"); ok {
		q.Kind = &v
	}
	if v, ok := wire.QueryParam(c, "q"); ok {
		q.Query = &v
	}
	if v, ok := wire.QueryParam(c, "before"); ok {
		q.Before = hermesdash.ParseISO(v)
	}
	if n, ok := wire.QueryInt(c, "limit"); ok {
		q.Limit = n
	}
	if v, ok := wire.QueryParam(c, "sessionID"); ok {
		q.SessionID = &v
	}
	return respond(c, func(ctx context.Context, tenant uuid.UUID) (hermesmirror.ArtifactListResponse, error) {
		return m.opt.Service.Artifacts(ctx, tenant, q)
	})
}

func (m *Module) getArtifact(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id, err := ids.Parse(c.Param("id"))
	if err != nil {
		return wire.NewError(http.StatusBadRequest, "hermes_artifact_id_required")
	}
	out, err := m.opt.Service.Artifact(c.Request().Context(), tenant, id)
	if err != nil {
		return MapError(err)
	}
	return wire.JSON(c, http.StatusOK, out)
}

// MARK: - Workspace

// requiredPath: a missing or blank parameter is a 400, never a default.
func requiredPath(c *echo.Context, name string) (string, error) {
	raw, ok := wire.QueryParam(c, name)
	if !ok {
		return "", wire.NewError(http.StatusBadRequest, name+" required")
	}
	value := swiftstr.TrimWhitespacesAndNewlines(raw)
	if value == "" {
		return "", wire.NewError(http.StatusBadRequest, name+" required")
	}
	return value, nil
}

func (m *Module) workspaceStatus(c *echo.Context) error {
	if _, err := tenantOf(c); err != nil {
		return err
	}
	path, err := requiredPath(c, "path")
	if err != nil {
		return err
	}
	return respond(c, func(ctx context.Context, tenant uuid.UUID) (hermesmirror.WorkspaceStatusDTO, error) {
		return m.opt.Service.WorkspaceStatus(ctx, tenant, path)
	})
}

func (m *Module) workspaceListing(c *echo.Context) error {
	if _, err := tenantOf(c); err != nil {
		return err
	}
	path, err := requiredPath(c, "path")
	if err != nil {
		return err
	}
	return respond(c, func(ctx context.Context, tenant uuid.UUID) (hermesmirror.WorkspaceListingDTO, error) {
		return m.opt.Service.WorkspaceListing(ctx, tenant, path)
	})
}

func (m *Module) workspaceFile(c *echo.Context) error {
	if _, err := tenantOf(c); err != nil {
		return err
	}
	path, err := requiredPath(c, "path")
	if err != nil {
		return err
	}
	return respond(c, func(ctx context.Context, tenant uuid.UUID) (hermesmirror.WorkspaceFileDTO, error) {
		return m.opt.Service.WorkspaceFile(ctx, tenant, path)
	})
}

func (m *Module) workspaceDiff(c *echo.Context) error {
	if _, err := tenantOf(c); err != nil {
		return err
	}
	repoPath, err := requiredPath(c, "path")
	if err != nil {
		return err
	}
	file, err := requiredPath(c, "file")
	if err != nil {
		return err
	}
	return respond(c, func(ctx context.Context, tenant uuid.UUID) (hermesmirror.WorkspaceDiffDTO, error) {
		return m.opt.Service.WorkspaceDiff(ctx, tenant, repoPath, file)
	})
}

// MARK: - Webhook

// WebhookDTO is HermesMirrorWebhookDTO.
type WebhookDTO struct {
	Path                string     `json:"path"`
	Token               string     `json:"token"`
	Secret              *string    `json:"secret,omitempty"`
	SignatureHeader     string     `json:"signatureHeader"`
	TimestampHeader     string     `json:"timestampHeader"`
	ReplayWindowSeconds int        `json:"replayWindowSeconds"`
	RotatedAt           *wire.Time `json:"rotatedAt,omitempty"`
}

type webhookRow struct {
	ID, Tenant        uuid.UUID
	Token             string
	Ciphertext, Nonce []byte
	RotatedAt         time.Time
}

func webhookDTO(row webhookRow, secret *string) WebhookDTO {
	return WebhookDTO{
		Path: WebhookRoutePrefix + "/" + row.Token, Token: row.Token, Secret: secret,
		SignatureHeader: SignatureHeader, TimestampHeader: TimestampHeader,
		ReplayWindowSeconds: ReplayWindowSeconds, RotatedAt: &wire.Time{Time: row.RotatedAt},
	}
}

func (m *Module) webhookWhere(ctx context.Context, clause string, arg any) (*webhookRow, error) {
	var row webhookRow
	err := m.pool.QueryRow(ctx, `SELECT id, tenant_id, token, secret_ciphertext, secret_nonce, rotated_at
		FROM hermes_mirror_webhooks WHERE `+clause+` LIMIT 1`, arg).Scan(
		&row.ID, &row.Tenant, &row.Token, &row.Ciphertext, &row.Nonce, &row.RotatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (m *Module) currentWebhook(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	row, err := m.webhookWhere(c.Request().Context(), "tenant_id = $1", tenant)
	if err != nil {
		return err
	}
	if row == nil {
		return wire.NewError(http.StatusNotFound, "hermes_webhook_not_configured")
	}
	return wire.JSON(c, http.StatusOK, webhookDTO(*row, nil))
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// rotateWebhook mints a new token and secret; gated on admin_config_rw.
func (m *Module) rotateWebhook(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	ctx := llm.RequestContext(c)
	if !m.opt.Service.SupportsAdminConfig(ctx, tenant) {
		return wire.NewError(http.StatusConflict, "hermes_admin_config_rw_required")
	}
	secret := base64.StdEncoding.EncodeToString(randomBytes(32))
	sealed, err := m.opt.Box.Seal(secret, tenant)
	if err != nil {
		return err
	}
	existing, err := m.webhookWhere(ctx, "tenant_id = $1", tenant)
	if err != nil {
		return err
	}
	row := webhookRow{
		Tenant: tenant, Token: hex.EncodeToString(randomBytes(32)),
		Ciphertext: sealed.Ciphertext, Nonce: sealed.Nonce, RotatedAt: m.opt.Now(),
	}
	now := m.opt.Now()
	if existing != nil {
		row.ID = existing.ID
		_, err = m.pool.Exec(ctx, `UPDATE hermes_mirror_webhooks SET token = $2, secret_ciphertext = $3, secret_nonce = $4,
			rotated_at = $5, updated_at = $6 WHERE id = $1`, row.ID, row.Token, row.Ciphertext, row.Nonce, row.RotatedAt, now)
	} else {
		row.ID = uuid.New()
		_, err = m.pool.Exec(ctx, `INSERT INTO hermes_mirror_webhooks (id, tenant_id, token, secret_ciphertext, secret_nonce,
			rotated_at, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`,
			row.ID, tenant, row.Token, row.Ciphertext, row.Nonce, row.RotatedAt, now)
	}
	if err != nil {
		return err
	}
	m.opt.Log.InfoContext(ctx, "hermes mirror webhook rotated", "tenant", ids.Upper(tenant))
	return wire.JSON(c, http.StatusOK, webhookDTO(row, &secret))
}

var errWebhookUnauthorized = wire.NewError(http.StatusUnauthorized, "hermes_webhook_unauthorized")

// webhookPayload is HermesJobWebhookPayload; only job_id is used, but the
// rest must decode as Swift's JSONDecoder would.
type webhookPayload struct {
	JobID      string   `json:"job_id"`
	RunKey     *string  `json:"run_key,omitempty"`
	Status     *string  `json:"status,omitempty"`
	Output     *string  `json:"output,omitempty"`
	Error      *string  `json:"error,omitempty"`
	StartedAt  *float64 `json:"started_at,omitempty"`
	FinishedAt *float64 `json:"finished_at,omitempty"`
}

// ingestWebhook is the push: token, freshness, HMAC, then the same idempotent
// collect the poller runs. A collect failure is a zero result, never an error.
func (m *Module) ingestWebhook(c *echo.Context) error {
	ctx := c.Request().Context()
	token := c.Param("token")
	if token == "" || swiftstr.Count(token) > 128 {
		return errWebhookUnauthorized
	}
	row, err := m.webhookWhere(ctx, "token = $1", token)
	if err != nil {
		return err
	}
	if row == nil {
		return errWebhookUnauthorized
	}
	rawTimestamp, ok := wire.HeaderValue(c.Request(), TimestampHeader)
	if !ok {
		return errWebhookUnauthorized
	}
	ts, ok := wire.ParseDouble(rawTimestamp)
	now := float64(m.opt.Now().UnixNano()) / 1e9
	if !ok || !(math.Abs(now-ts) <= ReplayWindowSeconds) {
		return errWebhookUnauthorized
	}
	signature, ok := wire.HeaderValue(c.Request(), SignatureHeader)
	if !ok {
		return errWebhookUnauthorized
	}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, MaxWebhookBodyBytes+1))
	if err != nil {
		return err
	}
	if len(body) > MaxWebhookBodyBytes {
		return wire.ErrBodyTooLarge
	}
	secret, err := m.opt.Box.Open(secretbox.Sealed{Ciphertext: row.Ciphertext, Nonce: row.Nonce}, row.Tenant)
	if err != nil || !workflows.VerifyWebhookSignature(signature, secret, rawTimestamp, body) {
		return errWebhookUnauthorized
	}
	payload, err := wire.Decode[webhookPayload](body)
	if err != nil || !validRunStatus(payload.Status) {
		return wire.NewError(http.StatusBadRequest, "hermes_webhook_invalid_payload")
	}
	id, err := hermesdash.ValidateJobID(payload.JobID)
	if err != nil {
		return MapError(err)
	}
	result, err := m.opt.Service.CollectJobRuns(ctx, row.Tenant, id)
	if err != nil {
		m.opt.Log.DebugContext(ctx, "hermes mirror webhook collect deferred to the poller",
			"tenant", ids.Upper(row.Tenant), "job", id, "error", hermesmirror.Describe(err))
		return wire.JSON(c, http.StatusOK, hermesmirror.CollectResultDTO{HermesJobID: id})
	}
	return wire.JSON(c, http.StatusOK, result)
}

func validRunStatus(s *string) bool {
	if s == nil {
		return true
	}
	switch *s {
	case hermesdash.RunRunning, hermesdash.RunOK, hermesdash.RunError:
		return true
	}
	return false
}
