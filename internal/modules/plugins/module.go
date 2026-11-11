// Package plugins is /v1/plugins (wave 6F): PluginController and, with a container
// manager, HermesHubSkillsController (Sources/App/Plugins). The marketplace and the
// news ticker, which share plugin_installs, are already their own Go modules.
package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/hermescontainers"
	"lumina/internal/hermesresolve"
	"lumina/internal/httpserver"
	"lumina/internal/modules/imports"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/skills"
	"lumina/internal/wire"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Importer is ImportService.importLinks.
type Importer interface {
	ImportLinks(ctx context.Context, tenant uuid.UUID, sourceType string, urls []string) (imports.StageResult, error)
}

// DockerExec is the one container operation hub install needs.
type DockerExec interface {
	Exec(ctx context.Context, container string, command []string) (exitCode int, err error)
}

// NoDocker is the default DockerExec: the pod has no docker binary, so the
// process never launches. Swift's Process.run throws there, and the handler
// answers 500 (owner decision O1: port the routes, waive what needs docker).
type NoDocker struct{}

var errNoDocker = errors.New("docker exec unavailable: no docker binary in this process")

func (NoDocker) Exec(context.Context, string, []string) (int, error) { return -1, errNoDocker }

// Options wire the module.
type Options struct {
	Identify echo.MiddlewareFunc
	Limits   *ratelimit.Store
	// BYO is the Hermes resolution middleware; nil leaves GET hermes-skills empty.
	BYO        echo.MiddlewareFunc
	Box        *secretbox.Box
	Importer   Importer
	Connectors Registry
	Catalog    *skills.Catalog
	// Containers mounts the hub install routes, as HermesHubSkillsService exists
	// only beside a container manager.
	Containers *hermescontainers.Manager
	Docker     DockerExec
	// HermesHTTP fetches GET /v1/skills; nil means a 5 s client.
	HermesHTTP *http.Client
	Now        func() time.Time
}

// Module serves /v1/plugins.
type Module struct {
	pool *pgxpool.Pool
	opt  Options
}

func New(pool *pgxpool.Pool, opt Options) *Module {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.HermesHTTP == nil {
		opt.HermesHTTP = &http.Client{Timeout: 5 * time.Second}
	}
	if opt.Docker == nil {
		opt.Docker = NoDocker{}
	}
	return &Module{pool: pool, opt: opt}
}

// Register mounts jwt, settingsByUser and, when present, the BYO middleware.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	chain := []echo.MiddlewareFunc{
		m.opt.Identify,
		ratelimit.Middleware(m.opt.Limits, ratelimit.SettingsByUser, ratelimit.UserOrIPKey),
	}
	if m.opt.BYO != nil {
		chain = append(chain, m.opt.BYO)
	}
	r.GET("/v1/plugins/catalog", m.catalog, chain...)
	r.GET("/v1/plugins/hermes-skills", m.hermesSkills, chain...)
	r.GET("/v1/plugins/installs", m.listInstalls, chain...)
	r.POST("/v1/plugins/installs", m.install, chain...)
	r.PATCH("/v1/plugins/installs/:id", m.update, chain...)
	r.DELETE("/v1/plugins/installs/:id", m.delete, chain...)
	r.POST("/v1/plugins/installs/:id/sync", m.sync, chain...)
	if m.opt.Containers != nil {
		r.POST("/v1/plugins/hermes-skills/install", m.hubInstall, chain...)
		r.DELETE("/v1/plugins/hermes-skills/:name", m.hubUninstall, chain...)
	}
}

// Error codes are PluginService.ErrorCode.
var (
	errUnknownPlugin  = wire.NewError(http.StatusNotFound, "unknown_plugin")
	errInstallMissing = wire.NewError(http.StatusNotFound, "install_not_found")
	errNotAConnector  = wire.NewError(http.StatusBadRequest, "not_a_connector")
	errNoConnector    = wire.NewError(http.StatusServiceUnavailable, "connector_unavailable")
	errUnauthorized   = wire.NewError(http.StatusBadGateway, "connector_unauthorized")
	errUpstream       = wire.NewError(http.StatusBadGateway, "connector_upstream_failure")
	errDisabled       = wire.NewError(http.StatusConflict, "install_disabled")
	errPremium        = wire.NewError(http.StatusPaymentRequired, "premium_required")
	errInstallID      = wire.NewError(http.StatusBadRequest, "invalid plugin install id")
)

func fieldError(code, key string) error {
	return wire.NewError(http.StatusBadRequest, code+":"+key)
}

// InstallDTO is PluginInstallDTO.
type InstallDTO struct {
	ID                   wire.UUID  `json:"id"`
	PluginSlug           string     `json:"pluginSlug"`
	Status               string     `json:"status"`
	HasConfig            bool       `json:"hasConfig"`
	CreatedAt            *wire.Time `json:"createdAt,omitempty"`
	LastSyncAt           *wire.Time `json:"lastSyncAt,omitempty"`
	MarketplaceVersionID *wire.UUID `json:"marketplaceVersionId,omitempty"`
	GrantedPermissions   []string   `json:"grantedPermissions"`
}

var permissions = map[string]bool{"memory.read": true, "memory.write": true, "network.fetch": true, "output.emit": true, "vault.read": true, "vault.write": true}

type installRow struct {
	id                   uuid.UUID
	slug, status         string
	ciphertext, nonce    []byte
	createdAt, lastSync  *time.Time
	marketplaceVersionID *uuid.UUID
	granted              []string
}

const installColumns = `id, plugin_slug, status, config_ciphertext, config_nonce, created_at, last_sync_at, marketplace_version_id, granted_permissions`

func scanInstall(row pgx.Row) (installRow, error) {
	var r installRow
	err := row.Scan(&r.id, &r.slug, &r.status, &r.ciphertext, &r.nonce, &r.createdAt, &r.lastSync, &r.marketplaceVersionID, &r.granted)
	return r, err
}

func wireTime(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}

func (r installRow) dto() InstallDTO {
	status := r.status
	if status != "enabled" && status != "disabled" {
		status = "enabled"
	}
	granted := []string{}
	for _, p := range r.granted {
		if permissions[p] {
			granted = append(granted, p)
		}
	}
	out := InstallDTO{
		ID: wire.UUID{UUID: r.id}, PluginSlug: r.slug, Status: status, HasConfig: len(r.ciphertext) > 0,
		CreatedAt: wireTime(r.createdAt), LastSyncAt: wireTime(r.lastSync), GrantedPermissions: granted,
	}
	if r.marketplaceVersionID != nil {
		out.MarketplaceVersionID = &wire.UUID{UUID: *r.marketplaceVersionID}
	}
	return out
}

func tenantOf(c *echo.Context) (uuid.UUID, error) {
	tenant, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, wire.ErrUnauthenticated
	}
	return tenant, nil
}

func installID(c *echo.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil || len(c.Param("id")) != 36 {
		return uuid.Nil, errInstallID
	}
	return id, nil
}

func boolQuery(c *echo.Context, key string) *bool {
	raw, ok := wire.QueryParam(c, key)
	if !ok {
		return nil
	}
	v := raw == "true" || raw == "1"
	return &v
}

type listResponse struct {
	Items []CatalogEntryDTO `json:"items"`
}

func (m *Module) catalog(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	category := wire.Query(c, "category")
	if !categories[category] {
		category = ""
	}
	featured, premium := boolQuery(c, "featured"), boolQuery(c, "premium")
	entries := catalogDTOs(category, featured, premium)
	if (category == "" || category == "skill") && featured == nil && premium == nil && m.opt.Catalog != nil {
		for _, mf := range m.opt.Catalog.Manifests(tenant) {
			entries = append(entries, skillEntry(mf.Name, mf.Description))
		}
		sortBySlug(entries)
	}
	return wire.JSON(c, http.StatusOK, listResponse{Items: entries})
}

func (m *Module) hermesSkills(c *echo.Context) error {
	if _, err := tenantOf(c); err != nil {
		return err
	}
	res, ok := hermesresolve.FromContext(c)
	if !ok || res.BaseURL == nil {
		return wire.JSON(c, http.StatusOK, listResponse{Items: []CatalogEntryDTO{}})
	}
	return wire.JSON(c, http.StatusOK, listResponse{Items: m.installedSkills(c.Request().Context(), res.BaseURL, res.AuthHeader)})
}

const hermesSkillsBodyCap = 2 * 1024 * 1024

// installedSkills is HermesSkillsClient.installedSkills: best effort, empty on
// any failure.
func (m *Module) installedSkills(ctx context.Context, base *url.URL, authHeader *string) []CatalogEntryDTO {
	target := strings.TrimSuffix(base.String(), "/") + "/v1/skills"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return []CatalogEntryDTO{}
	}
	req.Header.Set("Accept", "application/json")
	if authHeader != nil && *authHeader != "" {
		req.Header.Set("Authorization", *authHeader)
	}
	resp, err := m.opt.HermesHTTP.Do(req)
	if err != nil {
		slog.DebugContext(ctx, "hermes /v1/skills fetch failed", "error", err)
		return []CatalogEntryDTO{}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return []CatalogEntryDTO{}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, hermesSkillsBodyCap+1))
	if err != nil || len(body) > hermesSkillsBodyCap {
		return []CatalogEntryDTO{}
	}
	return parseHermesSkills(body)
}

type installsResponse struct {
	Items []InstallDTO `json:"items"`
}

func (m *Module) listInstalls(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	rows, err := m.pool.Query(c.Request().Context(), `SELECT `+installColumns+` FROM plugin_installs
		WHERE tenant_id = $1 ORDER BY created_at ASC`, tenant)
	if err != nil {
		return err
	}
	defer rows.Close()
	items := []InstallDTO{}
	for rows.Next() {
		r, err := scanInstall(rows)
		if err != nil {
			return err
		}
		items = append(items, r.dto())
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, installsResponse{Items: items})
}

type installRequest struct {
	PluginSlug string            `json:"pluginSlug"`
	Config     map[string]string `json:"config"`
}

func (m *Module) install(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[installRequest](c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if isPremium(body.PluginSlug) {
		entitled, err := m.entitledToPremium(ctx, tenant)
		if err != nil {
			return err
		}
		if !entitled {
			return errPremium
		}
	}
	var dto InstallDTO
	if name, ok := lvSkillName(body.PluginSlug); ok {
		dto, err = m.installSkill(ctx, tenant, body.PluginSlug, name)
	} else {
		dto, err = m.installConnector(ctx, tenant, body.PluginSlug, body.Config)
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, dto)
}

// entitledToPremium is PluginController.entitledToPremium: pro or ultimate,
// tier_override first unless it is "none".
func (m *Module) entitledToPremium(ctx context.Context, tenant uuid.UUID) (bool, error) {
	var tier, override string
	if err := m.pool.QueryRow(ctx, `SELECT tier, COALESCE(tier_override, 'none') FROM users WHERE id = $1`, tenant).
		Scan(&tier, &override); err != nil {
		return false, err
	}
	effective := tier
	if override != "none" {
		effective = override
	}
	return effective == "pro" || effective == "ultimate", nil
}

func validationError(v validation) error {
	switch v.kind {
	case "unknown_plugin":
		return errUnknownPlugin
	case "missing":
		return fieldError("missing_field", v.key)
	case "unknown_field":
		return fieldError("unknown_field", v.key)
	}
	return nil
}

func (m *Module) installConnector(ctx context.Context, tenant uuid.UUID, slug string, config map[string]string) (InstallDTO, error) {
	if err := validationError(validate(slug, config)); err != nil {
		return InstallDTO{}, err
	}
	sealed, err := m.seal(ctx, config, tenant)
	if err != nil {
		return InstallDTO{}, err
	}
	r, err := m.upsertEnabled(ctx, tenant, slug, sealed)
	if err != nil {
		return InstallDTO{}, err
	}
	slog.InfoContext(ctx, "plugin installed", "tenant", tenant, "slug", slug)
	return r.dto(), nil
}

// installSkill is PluginService.installSkill: the manifest must exist, the skill
// is enabled, and a config-less install row records it.
func (m *Module) installSkill(ctx context.Context, tenant uuid.UUID, slug, name string) (InstallDTO, error) {
	if m.opt.Catalog == nil {
		return InstallDTO{}, errUnknownPlugin
	}
	if _, ok := m.opt.Catalog.Manifest(name, tenant); !ok {
		return InstallDTO{}, errUnknownPlugin
	}
	if err := m.setSkillEnabled(ctx, tenant, name, true); err != nil {
		return InstallDTO{}, err
	}
	sealed, err := m.seal(ctx, map[string]string{}, tenant)
	if err != nil {
		return InstallDTO{}, err
	}
	r, err := m.upsertEnabled(ctx, tenant, slug, sealed)
	if err != nil {
		return InstallDTO{}, err
	}
	return r.dto(), nil
}

// upsertEnabled saves the tenant's row for slug with the sealed config, enabled.
func (m *Module) upsertEnabled(ctx context.Context, tenant uuid.UUID, slug string, sealed secretbox.Sealed) (installRow, error) {
	now := m.opt.Now()
	r, err := scanInstall(m.pool.QueryRow(ctx, `UPDATE plugin_installs
		SET config_ciphertext = $3, config_nonce = $4, status = 'enabled', updated_at = $5
		WHERE id = (SELECT id FROM plugin_installs WHERE tenant_id = $1 AND plugin_slug = $2 LIMIT 1)
		RETURNING `+installColumns, tenant, slug, sealed.Ciphertext, sealed.Nonce, now))
	if !errors.Is(err, pgx.ErrNoRows) {
		return r, err
	}
	return scanInstall(m.pool.QueryRow(ctx, `INSERT INTO plugin_installs
		(id, tenant_id, plugin_slug, status, config_ciphertext, config_nonce, created_at, updated_at)
		VALUES ($1, $2, $3, 'enabled', $4, $5, $6, $6)
		RETURNING `+installColumns, uuid.New(), tenant, slug, sealed.Ciphertext, sealed.Nonce, now))
}

// setSkillEnabled is PluginService.setSkillEnabled. SkillsState's Fluent @ID is
// tenant_id alone, so saving a loaded row updates every skills_state row of the
// tenant. Kept from Swift (decisions.md D-security-after-archive).
func (m *Module) setSkillEnabled(ctx context.Context, tenant uuid.UUID, name string, enabled bool) error {
	var exists bool
	if err := m.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM skills_state
		WHERE tenant_id = $1 AND source = 'builtin' AND name = $2)`, tenant, name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		_, err := m.pool.Exec(ctx, `UPDATE skills_state SET enabled = $2 WHERE tenant_id = $1`, tenant, enabled)
		return err
	}
	_, err := m.pool.Exec(ctx, `INSERT INTO skills_state (tenant_id, source, name, enabled, schedule_override,
		run_at, last_run_at, last_status, last_error, apns_category)
		VALUES ($1, 'builtin', $2, $3, NULL, NULL, NULL, NULL, NULL, NULL)`, tenant, name, enabled)
	return err
}

// seal is PluginService.seal: sorted-key JSON, sealed for the tenant.
func (m *Module) seal(ctx context.Context, config map[string]string, tenant uuid.UUID) (secretbox.Sealed, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(config); err != nil {
		return secretbox.Sealed{}, err
	}
	sealed, err := m.opt.Box.Seal(strings.TrimSuffix(buf.String(), "\n"), tenant)
	if err != nil {
		slog.ErrorContext(ctx, "plugin config encryption failed", "error", err)
		return secretbox.Sealed{}, wire.NewError(http.StatusInternalServerError, "encryption_failed")
	}
	return sealed, nil
}

func (m *Module) openConfig(r installRow, tenant uuid.UUID) (map[string]string, error) {
	plaintext, err := m.opt.Box.Open(secretbox.Sealed{Ciphertext: r.ciphertext, Nonce: r.nonce}, tenant)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(plaintext), &out); err != nil || out == nil {
		return map[string]string{}, nil //nolint:nilerr // Swift: (try? decode) ?? [:]
	}
	return out, nil
}

func (m *Module) requireInstall(ctx context.Context, tenant, id uuid.UUID) (installRow, error) {
	r, err := scanInstall(m.pool.QueryRow(ctx, `SELECT `+installColumns+` FROM plugin_installs
		WHERE tenant_id = $1 AND id = $2 LIMIT 1`, tenant, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, errInstallMissing
	}
	return r, err
}

// statusValue is PluginInstallStatus on the way in.
type statusValue string

func (s *statusValue) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if v != "enabled" && v != "disabled" {
		return &wire.CorruptedError{Message: "Cannot initialize PluginInstallStatus from invalid String value " + v}
	}
	*s = statusValue(v)
	return nil
}

type updateRequest struct {
	Config *map[string]string `json:"config,omitempty"`
	Status *statusValue       `json:"status,omitempty"`
}

func (m *Module) update(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id, err := installID(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[updateRequest](c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	r, err := m.requireInstall(ctx, tenant, id)
	if err != nil {
		return err
	}
	var sets []string
	args := []any{r.id}
	set := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, col+" = $"+strconv.Itoa(len(args)))
	}
	if name, ok := lvSkillName(r.slug); ok {
		if body.Status != nil {
			r.status = string(*body.Status)
			set("status", r.status)
			if err := m.setSkillEnabled(ctx, tenant, name, r.status == "enabled"); err != nil {
				return err
			}
		}
	} else {
		if body.Config != nil {
			if err := validationError(validate(r.slug, *body.Config)); err != nil {
				return err
			}
			sealed, err := m.seal(ctx, *body.Config, tenant)
			if err != nil {
				return err
			}
			r.ciphertext, r.nonce = sealed.Ciphertext, sealed.Nonce
			set("config_ciphertext", sealed.Ciphertext)
			set("config_nonce", sealed.Nonce)
		}
		if body.Status != nil {
			r.status = string(*body.Status)
			set("status", r.status)
		}
	}
	if len(sets) > 0 {
		set("updated_at", m.opt.Now())
		if _, err := m.pool.Exec(ctx, `UPDATE plugin_installs SET `+strings.Join(sets, ", ")+` WHERE id = $1`, args...); err != nil {
			return err
		}
	}
	return wire.JSON(c, http.StatusOK, r.dto())
}

func (m *Module) delete(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id, err := installID(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	r, err := m.requireInstall(ctx, tenant, id)
	if err != nil {
		return err
	}
	if name, ok := lvSkillName(r.slug); ok {
		if err := m.setSkillEnabled(ctx, tenant, name, false); err != nil {
			return err
		}
	}
	if _, err := m.pool.Exec(ctx, `DELETE FROM plugin_installs WHERE id = $1`, r.id); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

type syncResponse struct {
	InstallID wire.UUID `json:"installId"`
	SessionID wire.UUID `json:"sessionId"`
	Status    string    `json:"status"`
	Total     int       `json:"total"`
	Staged    int       `json:"staged"`
	Skipped   int       `json:"skipped"`
}

func (m *Module) sync(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id, err := installID(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	r, err := m.requireInstall(ctx, tenant, id)
	if err != nil {
		return err
	}
	if r.status != "enabled" {
		return errDisabled
	}
	e, ok := catalog[r.slug]
	if !ok {
		return errUnknownPlugin
	}
	if e.dto.CapabilityKind != "connector" {
		return errNotAConnector
	}
	connector, ok := m.opt.Connectors[e.binding]
	if !ok {
		return errNoConnector
	}
	config, err := m.openConfig(r, tenant)
	if err != nil {
		return err
	}
	urls, err := connector.FetchURLs(ctx, config, tenant)
	if err != nil {
		var ce *ConnectorError
		if !errors.As(err, &ce) {
			return err
		}
		switch ce.Kind {
		case "missingConfig":
			return fieldError("missing_field", ce.Key)
		case "invalidConfig":
			return fieldError("invalid_field", ce.Key)
		case "unauthorized":
			return errUnauthorized
		default:
			return errUpstream
		}
	}
	result, err := m.opt.Importer.ImportLinks(ctx, tenant, "connector:"+r.slug, urls[:min(len(urls), imports.MaxBatch)])
	if err != nil {
		return err
	}
	if _, err := m.pool.Exec(ctx, `UPDATE plugin_installs SET last_sync_at = $2, updated_at = $2 WHERE id = $1`, r.id, m.opt.Now()); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, syncResponse{
		InstallID: wire.UUID{UUID: r.id}, SessionID: wire.UUID{UUID: result.SessionID}, Status: "enriching",
		Total: result.Total, Staged: result.Staged, Skipped: result.Skipped,
	})
}

// validatedRef is HermesHubSkillsService.validatedRef.
func validatedRef(raw string) (string, error) {
	ref := strings.TrimFunc(raw, unicode.IsSpace)
	bad := wire.NewError(http.StatusBadRequest, "invalid_skill_ref")
	if ref == "" || len([]rune(ref)) > 512 || strings.HasPrefix(ref, "-") {
		return "", bad
	}
	if strings.IndexFunc(ref, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", bad
	}
	return ref, nil
}

func (m *Module) hubInstall(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	id := wire.Query(c, "id")
	if id == "" {
		return wire.NewError(http.StatusBadRequest, "missing id query parameter")
	}
	return m.hubRun(c, tenant, id, "install", "hermes_install_failed")
}

func (m *Module) hubUninstall(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	name := c.Param("name")
	if name == "" {
		return wire.NewError(http.StatusBadRequest, "missing skill name")
	}
	return m.hubRun(c, tenant, name, "uninstall", "hermes_uninstall_failed")
}

// hubRun is HermesHubSkillsService.install/uninstall: hermes skills <verb> <ref>
// in the tenant's container, then the refreshed installed list.
func (m *Module) hubRun(c *echo.Context, tenant uuid.UUID, raw, verb, failure string) error {
	ref, err := validatedRef(raw)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	handle, err := m.opt.Containers.Handle(ctx, tenant)
	if err != nil {
		return err
	}
	if handle == nil {
		return wire.NewError(http.StatusConflict, "hermes_container_not_found")
	}
	code, err := m.opt.Docker.Exec(ctx, handle.ContainerName, []string{"hermes", "skills", verb, ref})
	if err != nil {
		return err
	}
	if code != 0 {
		slog.ErrorContext(ctx, "hermes skills "+verb+" failed", "tenant", tenant, "exit", code)
		return wire.NewError(http.StatusBadGateway, failure)
	}
	base, err := url.Parse(handle.BaseURL())
	if err != nil {
		return wire.JSON(c, http.StatusOK, listResponse{Items: []CatalogEntryDTO{}})
	}
	auth := "Bearer " + handle.APIServerKey
	return wire.JSON(c, http.StatusOK, listResponse{Items: m.installedSkills(ctx, base, &auth)})
}
