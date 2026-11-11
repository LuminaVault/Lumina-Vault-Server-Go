// Package integrations is /v1/integrations/xai and /v1/integrations/nous (wave 6E):
// the per-tenant xAI Grok and Nous Portal OAuth flows, which run the hermes CLI in
// the tenant's Hermes container (XaiOAuthController, NousOAuthController and their
// services in Sources/App/Integrations). Sessions and parked CLI processes are
// in-memory, so start and complete must reach the same process, and both pairs flip
// together.
package integrations

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/hermescontainers"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/wire"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Options configures Module.
type Options struct {
	Containers hermescontainers.Containers
	Xai        XaiBackend
	Nous       NousBackend
	// Credentials, when set, drops its cached xai credential after connect and
	// revoke, as UserCredentialStore.invalidate does.
	Credentials *llm.CredentialStore
	Now         func() time.Time
	Log         *slog.Logger
}

// Module serves both route groups behind JWT only.
type Module struct {
	pool         *pgxpool.Pool
	opt          Options
	xaiSessions  *sessionStore
	nousSessions *sessionStore
}

func New(pool *pgxpool.Pool, opt Options) *Module {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	return &Module{pool: pool, opt: opt, xaiSessions: newSessionStore(opt.Now), nousSessions: newSessionStore(opt.Now)}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.GET("/v1/integrations/xai", m.xaiStatus, authMW)
	r.POST("/v1/integrations/xai/start", m.xaiStart, authMW)
	r.POST("/v1/integrations/xai/complete", m.xaiComplete, authMW)
	r.DELETE("/v1/integrations/xai", m.xaiRevoke, authMW)
	r.GET("/v1/integrations/nous", m.nousStatus, authMW)
	r.POST("/v1/integrations/nous/start", m.nousStart, authMW)
	r.POST("/v1/integrations/nous/complete", m.nousComplete, authMW)
	r.DELETE("/v1/integrations/nous", m.nousRevoke, authMW)
}

// sessionStore is XaiOAuthSessionStore / NousOAuthSessionStore: session → tenant,
// expiring after ten minutes.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]session
	ttl      time.Duration
	now      func() time.Time
}

type session struct {
	tenant    uuid.UUID
	startedAt time.Time
}

func newSessionStore(now func() time.Time) *sessionStore {
	return &sessionStore{sessions: map[string]session{}, ttl: 10 * time.Minute, now: now}
}

func (s *sessionStore) put(id string, tenant uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	s.sessions[id] = session{tenant: tenant, startedAt: s.now()}
}

func (s *sessionStore) take(id string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	v, ok := s.sessions[id]
	delete(s.sessions, id)
	return v, ok
}

func (s *sessionStore) sweep() {
	cutoff := s.now().Add(-s.ttl)
	for id, v := range s.sessions {
		if v.startedAt.Before(cutoff) {
			delete(s.sessions, id)
		}
	}
}

func tenantOf(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, auth.ErrUnauthorized
	}
	return id, nil
}

func wireTime(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}

// ---- xAI ----

// XaiStatusResponse is XaiStatusResponse.
type XaiStatusResponse struct {
	Connected      bool       `json:"connected"`
	Tier           string     `json:"tier"`
	XaiConnectedAt *wire.Time `json:"xaiConnectedAt,omitempty"`
}

// XaiStartResponse is XaiStartResponse.
type XaiStartResponse struct {
	SessionID    string `json:"sessionID"`
	AuthorizeURL string `json:"authorizeURL"`
}

type xaiCompleteRequest struct {
	SessionID   string `json:"sessionID"`
	CallbackURL string `json:"callbackURL"`
}

func (m *Module) userTier(ctx context.Context, tenant uuid.UUID) (string, error) {
	var tier string
	err := m.pool.QueryRow(ctx, `SELECT tier FROM users WHERE id = $1`, tenant).Scan(&tier)
	if errors.Is(err, pgx.ErrNoRows) {
		return "free", nil
	}
	return tier, err
}

// XaiStatus is XaiOAuthService.status.
func (m *Module) XaiStatus(ctx context.Context, tenant uuid.UUID) (XaiStatusResponse, error) {
	h, err := m.opt.Containers.Handle(ctx, tenant)
	if err != nil {
		return XaiStatusResponse{}, err
	}
	tier, err := m.userTier(ctx, tenant)
	if err != nil {
		return XaiStatusResponse{}, err
	}
	out := XaiStatusResponse{Tier: tier}
	if h != nil && h.XaiConnectedAt != nil {
		out.Connected, out.XaiConnectedAt = true, wireTime(h.XaiConnectedAt)
	}
	return out, nil
}

// XaiStart is XaiOAuthService.start.
func (m *Module) XaiStart(ctx context.Context, tenant uuid.UUID) (XaiStartResponse, error) {
	h, err := m.opt.Containers.EnsureRunning(ctx, tenant)
	if err != nil {
		return XaiStartResponse{}, err
	}
	sessionID := ids.Upper(uuid.New())
	u, err := m.opt.Xai.RequestAuthorizeURL(ctx, h, sessionID)
	if err != nil {
		return XaiStartResponse{}, err
	}
	m.xaiSessions.put(sessionID, tenant)
	m.opt.Log.InfoContext(ctx, "xai oauth start", "tenantID", tenant, "sessionID", sessionID)
	return XaiStartResponse{SessionID: sessionID, AuthorizeURL: u}, nil
}

// XaiComplete is XaiOAuthService.complete: on success the tenant moves to the pro
// tier and gets an oauth marker credential for xai.
func (m *Module) XaiComplete(ctx context.Context, sessionID, callbackURL string) (XaiStatusResponse, error) {
	s, ok := m.xaiSessions.take(sessionID)
	if !ok {
		return XaiStatusResponse{}, ErrSessionNotFound
	}
	h, err := m.opt.Containers.EnsureRunning(ctx, s.tenant)
	if err != nil {
		return XaiStatusResponse{}, err
	}
	ok, err = m.opt.Xai.SubmitCallback(ctx, h, sessionID, callbackURL)
	if err != nil {
		return XaiStatusResponse{}, err
	}
	if !ok {
		return XaiStatusResponse{}, &BackendFailedError{Reason: "backend reported non-zero exit"}
	}
	at := m.opt.Now()
	if err := m.opt.Containers.RecordXaiConnected(ctx, s.tenant, &at); err != nil {
		return XaiStatusResponse{}, err
	}
	if _, err := m.pool.Exec(ctx, `UPDATE users SET tier = 'pro', updated_at = now() WHERE id = $1`, s.tenant); err != nil {
		return XaiStatusResponse{}, err
	}
	if err := m.upsertXaiMarker(ctx, s.tenant); err != nil {
		m.opt.Log.WarnContext(ctx, "failed to seed xai oauth provider marker", "error", err)
	}
	m.opt.Log.InfoContext(ctx, "xai oauth completed; user promoted to pro", "tenantID", s.tenant, "sessionID", sessionID)
	return m.XaiStatus(ctx, s.tenant)
}

// upsertXaiMarker is UserCredentialStore.upsert(provider: .xai, credentialKind:
// "oauth", label: "SuperGrok (linked)"): no secret, no base URL.
func (m *Module) upsertXaiMarker(ctx context.Context, tenant uuid.UUID) error {
	tag, err := m.pool.Exec(ctx, `UPDATE user_provider_credentials SET credential_kind = 'oauth', ciphertext = NULL, nonce = NULL,
		base_url = NULL, label = 'SuperGrok (linked)', verified_at = NULL, last_failure_at = NULL, last_failure_code = NULL, updated_at = now()
		WHERE tenant_id = $1 AND provider = 'xai'`, tenant)
	if err == nil && tag.RowsAffected() == 0 {
		_, err = m.pool.Exec(ctx, `INSERT INTO user_provider_credentials (id, tenant_id, provider, credential_kind, label, created_at, updated_at)
			VALUES ($1, $2, 'xai', 'oauth', 'SuperGrok (linked)', now(), now())`, uuid.New(), tenant)
	}
	if err == nil && m.opt.Credentials != nil {
		m.opt.Credentials.Invalidate(tenant, llm.XAI)
	}
	return err
}

// XaiRevoke is XaiOAuthService.revoke. It demotes to trial and deletes the xai
// credential whatever its kind, as Swift does.
func (m *Module) XaiRevoke(ctx context.Context, tenant uuid.UUID) (XaiStatusResponse, error) {
	h, err := m.opt.Containers.Handle(ctx, tenant)
	if err != nil {
		return XaiStatusResponse{}, err
	}
	if h != nil {
		_, _ = m.opt.Xai.Revoke(ctx, h)
	}
	if err := m.opt.Containers.RecordXaiConnected(ctx, tenant, nil); err != nil {
		return XaiStatusResponse{}, err
	}
	if _, err := m.pool.Exec(ctx, `UPDATE users SET tier = 'trial', updated_at = now() WHERE id = $1`, tenant); err != nil {
		return XaiStatusResponse{}, err
	}
	if _, err := m.pool.Exec(ctx, `DELETE FROM user_provider_credentials WHERE tenant_id = $1 AND provider = 'xai'`, tenant); err == nil && m.opt.Credentials != nil {
		m.opt.Credentials.Invalidate(tenant, llm.XAI)
	}
	return m.XaiStatus(ctx, tenant)
}

func (m *Module) xaiStatus(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	s, err := m.XaiStatus(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, s)
}

func (m *Module) xaiStart(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	s, err := m.XaiStart(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, s)
}

func (m *Module) xaiComplete(c *echo.Context) error {
	body, err := wire.Bind[xaiCompleteRequest](c)
	if err != nil {
		return err
	}
	if body.SessionID == "" {
		return wire.NewError(http.StatusBadRequest, "sessionID required")
	}
	if body.CallbackURL == "" {
		return wire.NewError(http.StatusBadRequest, "callbackURL required")
	}
	s, err := m.XaiComplete(c.Request().Context(), body.SessionID, body.CallbackURL)
	var failed *BackendFailedError
	switch {
	case errors.Is(err, ErrSessionNotFound):
		return wire.NewError(http.StatusNotFound, "session not found or expired")
	case errors.Is(err, ErrBackendNotYetImplemented):
		return wire.NewError(http.StatusNotImplemented, "xai oauth backend not enabled")
	case errors.As(err, &failed):
		m.opt.Log.WarnContext(c.Request().Context(), "xai oauth backend failed: "+failed.Reason)
		return wire.NewError(http.StatusBadGateway, "xai oauth backend failed")
	case err != nil:
		return err
	}
	return wire.JSON(c, http.StatusOK, s)
}

func (m *Module) xaiRevoke(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	s, err := m.XaiRevoke(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, s)
}

// ---- Nous ----

// NousStatusResponse is LuminaVaultShared.NousStatusResponse.
type NousStatusResponse struct {
	Connected       bool       `json:"connected"`
	NousConnectedAt *wire.Time `json:"nousConnectedAt,omitempty"`
	Plan            *string    `json:"plan,omitempty"`
}

// NousStartResponse is LuminaVaultShared.NousStartResponse.
type NousStartResponse struct {
	SessionID string  `json:"sessionID"`
	VerifyURL string  `json:"verifyURL"`
	UserCode  *string `json:"userCode,omitempty"`
}

type nousCompleteRequest struct {
	SessionID string `json:"sessionID"`
}

// NousStatus is NousOAuthService.status.
func (m *Module) NousStatus(ctx context.Context, tenant uuid.UUID) (NousStatusResponse, error) {
	h, err := m.opt.Containers.Handle(ctx, tenant)
	if err != nil || h == nil {
		return NousStatusResponse{}, err
	}
	out := NousStatusResponse{Connected: h.NousConnectedAt != nil, NousConnectedAt: wireTime(h.NousConnectedAt)}
	if out.Connected {
		out.Plan = m.opt.Nous.SubscriptionPlan(ctx, h)
	}
	return out, nil
}

// NousStart is NousOAuthService.start.
func (m *Module) NousStart(ctx context.Context, tenant uuid.UUID) (NousStartResponse, error) {
	h, err := m.opt.Containers.EnsureRunning(ctx, tenant)
	if err != nil {
		return NousStartResponse{}, err
	}
	sessionID := ids.Upper(uuid.New())
	verifyURL, code, err := m.opt.Nous.RequestVerification(ctx, h, sessionID)
	if err != nil {
		return NousStartResponse{}, err
	}
	m.nousSessions.put(sessionID, tenant)
	m.opt.Log.InfoContext(ctx, "nous oauth start", "tenantID", tenant, "sessionID", sessionID)
	return NousStartResponse{SessionID: sessionID, VerifyURL: verifyURL, UserCode: code}, nil
}

// NousComplete is NousOAuthService.complete. The tier is left alone.
func (m *Module) NousComplete(ctx context.Context, sessionID string) (NousStatusResponse, error) {
	s, ok := m.nousSessions.take(sessionID)
	if !ok {
		return NousStatusResponse{}, ErrSessionNotFound
	}
	h, err := m.opt.Containers.EnsureRunning(ctx, s.tenant)
	if err != nil {
		return NousStatusResponse{}, err
	}
	ok, err = m.opt.Nous.AwaitCompletion(ctx, h, sessionID)
	if err != nil {
		return NousStatusResponse{}, err
	}
	if !ok {
		return NousStatusResponse{}, &BackendFailedError{Reason: "backend reported non-zero exit"}
	}
	at := m.opt.Now()
	if err := m.opt.Containers.RecordNousConnected(ctx, s.tenant, &at); err != nil {
		return NousStatusResponse{}, err
	}
	m.opt.Log.InfoContext(ctx, "nous oauth completed", "tenantID", s.tenant, "sessionID", sessionID)
	return m.NousStatus(ctx, s.tenant)
}

// NousRevoke is NousOAuthService.revoke.
func (m *Module) NousRevoke(ctx context.Context, tenant uuid.UUID) (NousStatusResponse, error) {
	h, err := m.opt.Containers.Handle(ctx, tenant)
	if err != nil {
		return NousStatusResponse{}, err
	}
	if h != nil {
		_, _ = m.opt.Nous.Revoke(ctx, h)
	}
	if err := m.opt.Containers.RecordNousConnected(ctx, tenant, nil); err != nil {
		return NousStatusResponse{}, err
	}
	return m.NousStatus(ctx, tenant)
}

func (m *Module) nousStatus(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	s, err := m.NousStatus(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, s)
}

func (m *Module) nousStart(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	s, err := m.NousStart(c.Request().Context(), tenant)
	switch {
	case errors.Is(err, ErrVerifyURLMissing):
		return wire.NewError(http.StatusBadGateway, "nous oauth backend did not return a verification URL")
	case errors.Is(err, ErrBackendNotYetImplemented):
		return wire.NewError(http.StatusNotImplemented, "nous oauth backend not enabled")
	case err != nil:
		return err
	}
	return wire.JSON(c, http.StatusOK, s)
}

func (m *Module) nousComplete(c *echo.Context) error {
	body, err := wire.Bind[nousCompleteRequest](c)
	if err != nil {
		return err
	}
	if body.SessionID == "" {
		return wire.NewError(http.StatusBadRequest, "sessionID required")
	}
	s, err := m.NousComplete(c.Request().Context(), body.SessionID)
	var failed *BackendFailedError
	switch {
	case errors.Is(err, ErrSessionNotFound):
		return wire.NewError(http.StatusNotFound, "session not found or expired")
	case errors.As(err, &failed):
		m.opt.Log.WarnContext(c.Request().Context(), "nous oauth backend failed: "+failed.Reason)
		return wire.NewError(http.StatusBadGateway, "nous oauth backend failed")
	case err != nil:
		return err
	}
	return wire.JSON(c, http.StatusOK, s)
}

func (m *Module) nousRevoke(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	s, err := m.NousRevoke(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, s)
}
