// Package settings serves /v1/settings/hermes (HermesConfigController: the
// tenant's BYO Hermes endpoint) and /v1/me/hermes/capabilities
// (HermesCapabilitiesController). Both exist only with a secret box, as in Swift.
package settings

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/hermescaps"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/ssrfguard"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Options are the module's dependencies.
type Options struct {
	Identify     echo.MiddlewareFunc
	Limits       *ratelimit.Store
	Box          *secretbox.Box
	Guard        ssrfguard.Guard
	Capabilities *hermescaps.Service
	// MirrorNudge is HermesMirrorNudging: a saved gateway is refreshed within
	// seconds instead of at the next 15-minute tick. Nil means no nudge.
	MirrorNudge MirrorNudger
	Log         *slog.Logger
}

// Module is the route group.
// MirrorNudger is HermesMirrorNudging.
type MirrorNudger interface {
	Enqueue(tenant uuid.UUID)
}

// UseMirrorNudge wires the mirror worker once it exists. Call it before the
// server starts: the option is read without a lock.
func (m *Module) UseMirrorNudge(n MirrorNudger) {
	m.opt.MirrorNudge = n
}

type Module struct {
	pool  *pgxpool.Pool
	opt   Options
	probe *http.Client
	now   func() time.Time
}

// New builds the module.
func New(pool *pgxpool.Pool, opt Options) *Module {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	return &Module{
		pool: pool, opt: opt, now: time.Now,
		probe: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Register mounts both groups behind JWT and settingsByUser.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	chain := []echo.MiddlewareFunc{
		m.opt.Identify,
		ratelimit.Middleware(m.opt.Limits, ratelimit.SettingsByUser, ratelimit.UserOrIPKey),
	}
	r.GET("/v1/settings/hermes", m.get, chain...)
	r.PUT("/v1/settings/hermes", m.put, chain...)
	r.DELETE("/v1/settings/hermes", m.delete, chain...)
	r.POST("/v1/settings/hermes/test", m.test, chain...)
	if m.opt.Capabilities != nil {
		r.GET("/v1/me/hermes/capabilities", m.capabilities, chain...)
	}
}

// getResponse is HermesConfigController.GetResponse.
type getResponse struct {
	BaseURL       string     `json:"baseUrl"`
	HasAuthHeader bool       `json:"hasAuthHeader"`
	VerifiedAt    *wire.Time `json:"verifiedAt,omitempty"`
	Name          *string    `json:"name,omitempty"`
}

type putRequest struct {
	BaseURL    string  `json:"baseUrl"`
	AuthHeader *string `json:"authHeader,omitempty"`
	Name       *string `json:"name,omitempty"`
}

type testResponse struct {
	VerifiedAt wire.Time `json:"verifiedAt"`
}

// Stable error codes for POST .../test; upstream bodies are never forwarded.
const (
	errTimeout       = "timeout"
	errTLS           = "tls_error"
	errHTTP4xx       = "http_4xx"
	errHTTP5xx       = "http_5xx"
	errSSRFRejected  = "ssrf_rejected"
	errDecryptFailed = "decrypt_failed"
	errUnreachable   = "unreachable"
)

var errNoConfig = wire.NewError(http.StatusNotFound, "no_hermes_config")

type row struct {
	id                uuid.UUID
	baseURL           string
	ciphertext, nonce []byte
	verifiedAt        *time.Time
	name              *string
}

func (m *Module) load(ctx context.Context, tenant uuid.UUID) (*row, error) {
	var r row
	err := m.pool.QueryRow(ctx, `SELECT id, base_url, auth_header_ciphertext, auth_header_nonce, verified_at, name
		FROM user_hermes_config WHERE tenant_id = $1 LIMIT 1`, tenant).
		Scan(&r.id, &r.baseURL, &r.ciphertext, &r.nonce, &r.verifiedAt, &r.name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func tenantOf(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, wire.ErrUnauthenticated
	}
	return id, nil
}

func wtp(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}

func (r *row) response() getResponse {
	return getResponse{BaseURL: r.baseURL, HasAuthHeader: r.ciphertext != nil, VerifiedAt: wtp(r.verifiedAt), Name: r.name}
}

// GET /v1/settings/hermes

func (m *Module) get(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	r, err := m.load(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	if r == nil {
		return errNoConfig
	}
	return wire.JSON(c, http.StatusOK, r.response())
}

// PUT /v1/settings/hermes

func (m *Module) put(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[putRequest](c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	validated, err := m.opt.Guard.Validate(ctx, body.BaseURL)
	if err != nil {
		var rejection *ssrfguard.Rejection
		if errors.As(err, &rejection) {
			return wire.NewError(http.StatusBadRequest, "invalid_base_url:"+rejection.Error())
		}
		return err
	}
	var ciphertext, nonce []byte
	if body.AuthHeader != nil && *body.AuthHeader != "" {
		sealed, err := m.opt.Box.Seal(*body.AuthHeader, tenant)
		if err != nil {
			m.opt.Log.ErrorContext(ctx, "secretbox seal failed", "tenant", ids.Upper(tenant))
			return wire.NewError(http.StatusInternalServerError, "seal_failed")
		}
		ciphertext, nonce = sealed.Ciphertext, sealed.Nonce
	}
	var name *string
	if body.Name != nil {
		if trimmed := swiftstr.TrimWhitespacesAndNewlines(*body.Name); trimmed != "" {
			name = &trimmed
		}
	}
	baseURL := validated.String()
	existing, err := m.load(ctx, tenant)
	if err != nil {
		return err
	}
	if existing == nil {
		_, err = m.pool.Exec(ctx, `INSERT INTO user_hermes_config
			(id, tenant_id, base_url, auth_header_ciphertext, auth_header_nonce, verified_at, name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, NULL, $6, NOW(), NOW())`, uuid.New(), tenant, baseURL, ciphertext, nonce, name)
	} else {
		_, err = m.pool.Exec(ctx, `UPDATE user_hermes_config SET base_url = $2, auth_header_ciphertext = $3,
			auth_header_nonce = $4, name = $5, verified_at = NULL, updated_at = NOW() WHERE id = $1`,
			existing.id, baseURL, ciphertext, nonce, name)
	}
	if err != nil {
		return err
	}
	if m.opt.Capabilities != nil {
		_ = m.opt.Capabilities.Capabilities(ctx, tenant, true)
	}
	// Do not wait on the upstream list: a down VPS must not stall Save.
	if m.opt.MirrorNudge != nil {
		m.opt.MirrorNudge.Enqueue(tenant)
	}
	return wire.JSON(c, http.StatusOK, getResponse{BaseURL: baseURL, HasAuthHeader: ciphertext != nil, Name: name})
}

// DELETE /v1/settings/hermes

func (m *Module) delete(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	r, err := m.load(ctx, tenant)
	if err != nil {
		return err
	}
	if r != nil {
		if _, err := m.pool.Exec(ctx, `DELETE FROM user_hermes_config WHERE id = $1`, r.id); err != nil {
			return err
		}
	}
	return c.NoContent(http.StatusNoContent)
}

// POST /v1/settings/hermes/test

func (m *Module) test(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	r, err := m.load(ctx, tenant)
	if err != nil {
		return err
	}
	if r == nil {
		return errNoConfig
	}
	base, err := m.opt.Guard.Validate(ctx, r.baseURL)
	if err != nil {
		var rejection *ssrfguard.Rejection
		if !errors.As(err, &rejection) {
			return err
		}
		m.opt.Log.WarnContext(ctx, "ssrf rejection during test", "tenant", ids.Upper(tenant), "rejection", rejection.Error())
		return wire.NewError(http.StatusBadGateway, errSSRFRejected)
	}
	var authHeader *string
	if r.ciphertext != nil && r.nonce != nil {
		plain, err := m.opt.Box.Open(secretbox.Sealed{Ciphertext: r.ciphertext, Nonce: r.nonce}, tenant)
		if err != nil {
			return wire.NewError(http.StatusBadGateway, errDecryptFailed)
		}
		authHeader = &plain
	}
	if code := m.probeBase(ctx, base, authHeader); code != "" {
		return wire.NewError(http.StatusBadGateway, code)
	}
	now := m.now()
	if _, err := m.pool.Exec(ctx, `UPDATE user_hermes_config SET verified_at = $2, updated_at = NOW() WHERE id = $1`, r.id, now); err != nil {
		return err
	}
	if m.opt.Capabilities != nil {
		_ = m.opt.Capabilities.Capabilities(ctx, tenant, true)
	}
	return wire.JSON(c, http.StatusOK, testResponse{VerifiedAt: wire.Time{Time: now}})
}

// probeBase tries <base>/v1/models, then <base>/health on a 4xx.
func (m *Module) probeBase(ctx context.Context, base *url.URL, authHeader *string) string {
	code := m.probeOne(ctx, appendPath(base, "v1/models"), authHeader)
	if code == errHTTP4xx {
		return m.probeOne(ctx, appendPath(base, "health"), authHeader)
	}
	return code
}

func appendPath(base *url.URL, component string) string {
	u := *base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + component
	u.RawPath = ""
	return u.String()
}

func (m *Module) probeOne(ctx context.Context, rawURL string, authHeader *string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return errUnreachable
	}
	if authHeader != nil {
		req.Header.Set("Authorization", *authHeader)
	}
	resp, err := m.probe.Do(req)
	if err != nil {
		return classify(err)
	}
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return ""
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return errHTTP4xx
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		return errHTTP5xx
	}
	return errUnreachable
}

// classify maps a transport failure onto URLError's codes as the controller reads them.
func classify(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errTimeout
	}
	var certErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	if errors.As(err, &certErr) || errors.As(err, &unknownAuthority) || errors.As(err, &hostname) ||
		errors.As(err, &invalid) || errors.As(err, &record) {
		return errTLS
	}
	return errUnreachable
}

// GET /v1/me/hermes/capabilities

func (m *Module) capabilities(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	refresh := wire.Query(c, "refresh")
	return wire.JSON(c, http.StatusOK, m.opt.Capabilities.Capabilities(c.Request().Context(), tenant, refresh == "true"))
}
