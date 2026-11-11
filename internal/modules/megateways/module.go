// Package megateways is /v1/me/hermes-gateways and /v1/gateways/photon/inbound
// (wave 6D): the messaging gateways a tenant's Hermes container runs
// (HermesGatewaysController and Sources/App/HermesGateways). Apply jobs persist in
// hermes_gateway_apply_jobs; the apply, WhatsApp pairing and Photon setup streams
// and sessions are in-memory, so the whole group flips together. Every container
// operation goes through hermescontainers, which forks docker as Swift does: in a
// pod with no docker those calls fail as Swift's do.
package megateways

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/hermescontainers"
	"lumina/internal/hermesresolve"
	"lumina/internal/httpserver"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/wire"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Options configures Module.
type Options struct {
	Box        *secretbox.Box
	Limits     *ratelimit.Store
	Containers hermescontainers.Containers
	Docker     hermescontainers.Docker
	// Resolver supplies the tenant's Hermes for /:id/test (byoHermesMiddleware).
	Resolver *hermesresolve.Resolver
	// SidecarToken is photon.sidecarToken. Empty disables the inbound webhook.
	SidecarToken string
	Log          *slog.Logger
}

// Module serves the gateway routes.
type Module struct {
	pool    *pgxpool.Pool
	opt     Options
	pairing *pairing
	apply   *applier
	photon  *photon
	health  *http.Client
	now     func() time.Time
}

func New(pool *pgxpool.Pool, opt Options) *Module {
	if opt.Log == nil {
		opt.Log = slog.Default().With("module", "megateways")
	}
	m := &Module{pool: pool, opt: opt, health: &http.Client{Timeout: 5 * time.Second}, now: time.Now}
	m.pairing = newPairing(opt.Containers, livePairBackend{docker: opt.Docker, sessionDir: "/opt/data/.hermes/platforms/whatsapp/session"}, opt.Log)
	m.apply = &applier{
		pool: pool, containers: opt.Containers, probe: defaultHealthProbe, healthTimeout: 60 * time.Second,
		log: opt.Log, now: m.nowFn, subs: map[uuid.UUID]map[int]chan applyEvent{},
	}
	m.photon = newPhoton(pool, opt.Box, opt.Log)
	return m
}

func (m *Module) nowFn() time.Time { return m.now() }

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	chain := []echo.MiddlewareFunc{authMW, ratelimit.Middleware(m.opt.Limits, ratelimit.SettingsByUser, ratelimit.UserOrIPKey)}
	if m.opt.Resolver != nil {
		chain = append(chain, m.opt.Resolver.Middleware())
	}
	const base = "/v1/me/hermes-gateways"
	r.GET(base, m.list, chain...)
	r.POST(base+"/whatsapp/pair", m.whatsAppStartPair, chain...)
	r.GET(base+"/whatsapp/pair/:sessionID/stream", m.whatsAppPairStream, chain...)
	r.DELETE(base+"/whatsapp/session", m.whatsAppUnlink, chain...)
	r.POST(base+"/photon/setup", m.photonStartSetup, chain...)
	r.POST(base+"/photon/setup/:sessionID/phone", m.photonSubmitPhone, chain...)
	r.GET(base+"/photon/setup/:sessionID/stream", m.photonSetupStream, chain...)
	r.GET(base+"/:id", m.getOne, chain...)
	r.PUT(base+"/:id", m.put, chain...)
	r.DELETE(base+"/:id", m.delete, chain...)
	r.POST(base+"/:id/test", m.test, chain...)
	r.POST(base+"/apply", m.startApply, chain...)
	r.GET(base+"/apply/current", m.applyCurrent, chain...)
	r.GET(base+"/apply/:jobID", m.applyStatus, chain...)
	r.GET(base+"/apply/:jobID/stream", m.applyStream, chain...)
	r.POST("/v1/gateways/photon/inbound", m.photonInbound, m.sidecarToken)
}

// CatalogEntry is HermesGatewayCatalogEntry.
type CatalogEntry struct {
	ID              string     `json:"id"`
	DisplayName     string     `json:"displayName"`
	IconSlug        string     `json:"iconSlug"`
	Description     string     `json:"description"`
	RequiredFields  []Field    `json:"requiredFields"`
	Status          string     `json:"status"`
	HasConfig       bool       `json:"hasConfig"`
	VerifiedAt      *wire.Time `json:"verifiedAt,omitempty"`
	LastFailureCode *string    `json:"lastFailureCode,omitempty"`
	PairingKind     *string    `json:"pairingKind,omitempty"`
}

type listResponse struct {
	Items []CatalogEntry `json:"items"`
}

type putRequest struct {
	Config map[string]string `json:"config"`
}

type testResponse struct {
	OK           bool       `json:"ok"`
	VerifiedAt   *wire.Time `json:"verifiedAt,omitempty"`
	ErrorCode    *string    `json:"errorCode,omitempty"`
	ErrorMessage *string    `json:"errorMessage,omitempty"`
}

type gatewayRow struct {
	id              uuid.UUID
	status          string
	verifiedAt      *time.Time
	lastFailureCode *string
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

func (m *Module) loadRow(ctx context.Context, tenant uuid.UUID, gateway string) (*gatewayRow, error) {
	var r gatewayRow
	err := m.pool.QueryRow(ctx, `SELECT id, status, verified_at, last_failure_code FROM user_hermes_gateways
		WHERE tenant_id = $1 AND gateway_id = $2 LIMIT 1`, tenant, gateway).Scan(&r.id, &r.status, &r.verifiedAt, &r.lastFailureCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // Swift's Optional
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

var knownStatus = map[string]bool{"not_configured": true, "configured": true, "verified": true, "error": true}

func (m *Module) entryDTO(ctx context.Context, id string, tenant uuid.UUID, row *gatewayRow) CatalogEntry {
	e := catalog[id]
	out := CatalogEntry{
		ID: id, DisplayName: e.DisplayName, IconSlug: e.IconSlug, Description: e.Description,
		RequiredFields: e.RequiredFields, Status: "not_configured", PairingKind: e.PairingKind,
	}
	if e.PairingKind != nil {
		if m.pairing.isPaired(ctx, tenant) {
			out.Status, out.HasConfig = "verified", true
		}
	} else if row != nil {
		out.HasConfig = true
		if knownStatus[row.status] {
			out.Status = row.status
		}
	}
	if row != nil {
		out.VerifiedAt, out.LastFailureCode = wireTime(row.verifiedAt), row.lastFailureCode
	}
	return out
}

func (m *Module) list(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	rows, err := m.pool.Query(ctx, `SELECT gateway_id, id, status, verified_at, last_failure_code FROM user_hermes_gateways WHERE tenant_id = $1`, tenant)
	if err != nil {
		return err
	}
	byID := map[string]*gatewayRow{}
	for rows.Next() {
		var gid string
		var r gatewayRow
		if err := rows.Scan(&gid, &r.id, &r.status, &r.verifiedAt, &r.lastFailureCode); err != nil {
			rows.Close()
			return err
		}
		byID[gid] = &r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	items := make([]CatalogEntry, 0, len(gatewayIDs))
	for _, id := range gatewayIDs {
		items = append(items, m.entryDTO(ctx, id, tenant, byID[id]))
	}
	return wire.JSON(c, http.StatusOK, listResponse{Items: items})
}

// parseID is parseID: an id outside the catalog is 404 unsupported_gateway.
// "apply" is a constant segment in Hummingbird's trie, which wins over :id and
// answers any other method with an empty 404.
func parseID(c *echo.Context) (string, error) {
	raw := c.Param("id")
	if raw == "apply" {
		return "", echo.ErrNotFound
	}
	if _, ok := catalog[raw]; !ok {
		return "", wire.NewError(http.StatusNotFound, "unsupported_gateway")
	}
	return raw, nil
}

func rejectPairing(id string) error {
	if catalog[id].PairingKind != nil {
		return wire.NewError(http.StatusBadRequest, "unsupported_gateway")
	}
	return nil
}

func (m *Module) getOne(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c)
	if err != nil {
		return err
	}
	row, err := m.loadRow(c.Request().Context(), tenant, id)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, m.entryDTO(c.Request().Context(), id, tenant, row))
}

// upsertGatewayRow seals config into the tenant's row for gateway, resetting its
// status to configured.
func upsertGatewayRow(ctx context.Context, pool *pgxpool.Pool, tenant uuid.UUID, gateway string, sealed secretbox.Sealed) error {
	tag, err := pool.Exec(ctx, `UPDATE user_hermes_gateways SET config_ciphertext = $3, config_nonce = $4, status = 'configured',
		verified_at = NULL, last_failure_at = NULL, last_failure_code = NULL, updated_at = now()
		WHERE tenant_id = $1 AND gateway_id = $2`, tenant, gateway, sealed.Ciphertext, sealed.Nonce)
	if err != nil || tag.RowsAffected() > 0 {
		return err
	}
	_, err = pool.Exec(ctx, `INSERT INTO user_hermes_gateways (id, tenant_id, gateway_id, config_ciphertext, config_nonce, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'configured', now(), now())`, uuid.New(), tenant, gateway, sealed.Ciphertext, sealed.Nonce)
	return err
}

func (m *Module) put(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c)
	if err != nil {
		return err
	}
	if err := rejectPairing(id); err != nil {
		return err
	}
	body, err := wire.Bind[putRequest](c)
	if err != nil {
		return err
	}
	if key, ok := validate(id, body.Config); !ok {
		return wire.NewError(http.StatusBadRequest, "missing_field:"+key)
	}
	// JSONEncoder with .sortedKeys.
	plain, err := wire.MarshalFoundation(body.Config)
	if err != nil {
		return err
	}
	sealed, err := m.opt.Box.Seal(string(plain), tenant)
	if err != nil {
		m.opt.Log.ErrorContext(c.Request().Context(), "hermes gateway config encryption failed", "error", err)
		return wire.NewError(http.StatusInternalServerError, "encryption_failed")
	}
	if err := upsertGatewayRow(c.Request().Context(), m.pool, tenant, id, sealed); err != nil {
		return err
	}
	e := catalog[id]
	return wire.JSON(c, http.StatusOK, CatalogEntry{
		ID: id, DisplayName: e.DisplayName, IconSlug: e.IconSlug, Description: e.Description,
		RequiredFields: e.RequiredFields, Status: "configured", HasConfig: true,
	})
}

func (m *Module) delete(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c)
	if err != nil {
		return err
	}
	if err := rejectPairing(id); err != nil {
		return err
	}
	if _, err := m.pool.Exec(c.Request().Context(), `DELETE FROM user_hermes_gateways WHERE tenant_id = $1 AND gateway_id = $2`, tenant, id); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (m *Module) test(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c)
	if err != nil {
		return err
	}
	if err := rejectPairing(id); err != nil {
		return err
	}
	res, ok := hermesresolve.FromContext(c)
	if !ok {
		return wire.NewError(http.StatusBadRequest, "hermes_not_configured")
	}
	ctx := c.Request().Context()
	code := m.probeHealth(ctx, res.BaseURL, res.AuthHeader)
	row, err := m.loadRow(ctx, tenant, id)
	if err != nil {
		return err
	}
	now := m.now()
	if code == nil {
		if row != nil {
			if _, err := m.pool.Exec(ctx, `UPDATE user_hermes_gateways SET verified_at = $2, last_failure_at = NULL, last_failure_code = NULL,
				updated_at = $2 WHERE id = $1`, row.id, now); err != nil {
				return err
			}
		}
		return wire.JSON(c, http.StatusOK, testResponse{OK: true, VerifiedAt: &wire.Time{Time: now}})
	}
	if row != nil {
		if _, err := m.pool.Exec(ctx, `UPDATE user_hermes_gateways SET last_failure_at = $2, last_failure_code = $3, updated_at = $2
			WHERE id = $1`, row.id, now, *code); err != nil {
			return err
		}
	}
	return wire.JSON(c, http.StatusOK, testResponse{OK: false, ErrorCode: code})
}

// probeHealth is HermesGatewayClient.health: nil when reachable, else its error code.
func (m *Module) probeHealth(ctx context.Context, base *url.URL, authHeader *string) *string {
	u := *base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/v1/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return ptr("hermes_unreachable:network")
	}
	if authHeader != nil && *authHeader != "" {
		req.Header.Set("Authorization", *authHeader)
	}
	resp, err := m.health.Do(req)
	if err != nil {
		return ptr("hermes_unreachable:" + classifyNetError(err))
	}
	_ = resp.Body.Close()
	switch s := resp.StatusCode; {
	case s >= 200 && s < 300:
		return nil
	case s == 401 || s == 403:
		return ptr("hermes_unauthorized")
	case s == 404:
		return ptr("hermes_unreachable:not_found")
	case s >= 500 && s < 600:
		return ptr("hermes_unreachable:http_5xx")
	default:
		return ptr("hermes_unreachable:http_" + strconv.Itoa(s))
	}
}

// classifyNetError maps a transport failure onto URLError's codes as Swift does.
func classifyNetError(err error) string {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var netErr net.Error
	switch {
	case errors.As(err, &dnsErr):
		return "dns"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.As(err, &certErr), errors.As(err, &unknownAuth):
		return "tls"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	default:
		return "network"
	}
}

func (m *Module) startApply(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	snap, err := m.apply.startApply(c.Request().Context(), tenant)
	if errors.Is(err, errApplyRunning) {
		return wire.NewError(http.StatusConflict, "apply_in_progress")
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, struct {
		JobID wire.UUID `json:"jobID"`
		State string    `json:"state"`
	}{snap.JobID, snap.State})
}

func (m *Module) applyCurrent(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	r, err := m.apply.currentJob(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	if r == nil {
		return wire.NewError(http.StatusNotFound, "no_apply_job")
	}
	return wire.JSON(c, http.StatusOK, r.snapshot())
}

// optionalUUID is ctx.parameters.get(name, as: UUID.self).
func optionalUUID(c *echo.Context, name string) (uuid.UUID, bool) {
	raw := c.Param(name)
	if len(raw) != 36 {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	return id, err == nil
}

func (m *Module) requireJobID(c *echo.Context) (uuid.UUID, error) {
	id, ok := optionalUUID(c, "jobID")
	if !ok {
		return uuid.Nil, wire.NewError(http.StatusBadRequest, "invalid_job_id")
	}
	return id, nil
}

func (m *Module) applyStatus(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id, err := m.requireJobID(c)
	if err != nil {
		return err
	}
	r, err := m.apply.job(c.Request().Context(), id, tenant)
	if err != nil {
		return err
	}
	if r == nil {
		return wire.NewError(http.StatusNotFound, "job_not_found")
	}
	return wire.JSON(c, http.StatusOK, r.snapshot())
}

func (m *Module) applyStream(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id, err := m.requireJobID(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	r, err := m.apply.job(ctx, id, tenant)
	if err != nil {
		return err
	}
	if r == nil {
		return wire.NewError(http.StatusNotFound, "job_not_found")
	}
	events, cancel := m.apply.subscribe(ctx, id, tenant)
	defer cancel()
	return streamSSE(c, events)
}

func (m *Module) whatsAppStartPair(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id, err := m.pairing.startPairing(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, struct {
		SessionID wire.UUID `json:"sessionID"`
	}{wire.UUID{UUID: id}})
}

func (m *Module) whatsAppPairStream(c *echo.Context) error {
	if _, err := tenantOf(c); err != nil {
		return err
	}
	id, ok := optionalUUID(c, "sessionID")
	if !ok {
		return wire.NewError(http.StatusBadRequest, "invalid_session_id")
	}
	events, cancel := m.pairing.subscribe(id)
	defer cancel()
	return streamSSE(c, events)
}

func (m *Module) whatsAppUnlink(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if _, err := m.pairing.unlink(ctx, tenant); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, m.entryDTO(ctx, "whatsapp", tenant, nil))
}

func (m *Module) photonStartSetup(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id, err := m.photon.startSetup(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, struct {
		SessionID wire.UUID `json:"sessionID"`
	}{wire.UUID{UUID: id}})
}

type phoneBody struct {
	Phone string `json:"phone"`
}

func (m *Module) photonSubmitPhone(c *echo.Context) error {
	if _, err := tenantOf(c); err != nil {
		return err
	}
	raw := c.Param("sessionID")
	id, ok := optionalUUID(c, "sessionID")
	if !ok {
		return wire.NewError(http.StatusBadRequest, "Parameter '"+raw+"' can not be converted to the expected type (UUID)")
	}
	body, err := wire.Bind[phoneBody](c)
	if err != nil {
		return err
	}
	if err := m.photon.submitPhone(id, body.Phone); err != nil {
		return err
	}
	return c.NoContent(http.StatusOK)
}

func (m *Module) photonSetupStream(c *echo.Context) error {
	if _, err := tenantOf(c); err != nil {
		return err
	}
	id, ok := optionalUUID(c, "sessionID")
	if !ok {
		return wire.NewError(http.StatusBadRequest, "invalid_session_id")
	}
	events, cancel := m.photon.subscribe(id)
	defer cancel()
	return streamSSE(c, events)
}

// sidecarToken is SidecarTokenMiddleware: an empty configured token disables the
// webhook; otherwise X-Lumina-Sidecar-Token, or Authorization with or without
// "Bearer ", must match.
func (m *Module) sidecarToken(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if m.opt.SidecarToken == "" {
			return wire.NewError(http.StatusNotFound, "sidecar webhook disabled")
		}
		presented, ok := presentedToken(c.Request())
		if !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(m.opt.SidecarToken)) != 1 {
			return wire.NewError(http.StatusUnauthorized, "sidecar token invalid")
		}
		return next(c)
	}
}

func presentedToken(r *http.Request) (string, bool) {
	if v := r.Header.Values("X-Lumina-Sidecar-Token"); len(v) > 0 {
		return v[0], true
	}
	v := r.Header.Values("Authorization")
	if len(v) == 0 {
		return "", false
	}
	return strings.TrimPrefix(v[0], "Bearer "), true
}

// photonInbound is handlePhotonInbound with no delivery service: the sidecar
// client is not ported (production sets no sidecar), so it is always 503.
func (m *Module) photonInbound(c *echo.Context) error {
	m.opt.Log.WarnContext(c.Request().Context(), "photon inbound received but delivery service not available")
	return c.NoContent(http.StatusServiceUnavailable)
}

// streamSSE writes one `data:` frame per event, Foundation-encoded, until the
// channel closes or the client leaves.
func streamSSE[T any](c *echo.Context, events <-chan T) error {
	w := c.Response()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	ctx := c.Request().Context()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			b, err := wire.MarshalFoundation(ev)
			if err != nil {
				return nil
			}
			if _, err := w.Write(append(append([]byte("data: "), b...), '\n', '\n')); err != nil {
				return nil
			}
			_ = rc.Flush()
		}
	}
}
