// Package marketplace serves /v1/marketplace: the Go port of LuminaVaultServer
// Sources/App/Marketplace (MarketplaceController.swift, MarketplaceService.swift,
// PluginRunnerClient.swift, MarketplaceCapabilityBroker.swift), plus the two
// PluginService methods the controller calls (installMarketplace,
// upgradeMarketplace), which touch only plugin_installs and SecretBox. The rest
// of Plugins moves in its own wave and shares that table, in the same format.
//
// Each handler runs Swift's checks in Swift's order: most writes decode the body
// before they require identity, so a malformed body is a 400 even without a
// token. The first-party catalog is seeded lazily, on the first list or detail
// read after an admin exists.
package marketplace

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/marketplace/oapi-codegen.yaml api/openapi.yaml"

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/marketplace/db"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	maxArtifactBytes = 10 * 1024 * 1024
	maxToolInput     = 1_048_576
	maxCapRounds     = 4
	minSigningKey    = 32
)

var (
	errAdminRequired     = wire.NewError(http.StatusForbidden, "admin_required")
	errPluginNotFound    = wire.NewError(http.StatusNotFound, "marketplace_plugin_not_found")
	errVersionNotFound   = wire.NewError(http.StatusNotFound, "marketplace_version_not_found")
	errPublisherNotFound = wire.NewError(http.StatusNotFound, "publisher_application_not_found")
	errSubmissionMissing = wire.NewError(http.StatusNotFound, "submission_not_found")
	errApprovedRequired  = wire.NewError(http.StatusForbidden, "approved_publisher_required")
	errIntegrityInvalid  = wire.NewError(http.StatusUnprocessableEntity, "plugin_artifact_integrity_invalid")
	errInvalidSlug       = wire.NewError(http.StatusUnprocessableEntity, "invalid_slug")
	errHTTPSRequired     = wire.NewError(http.StatusUnprocessableEntity, "https_url_required")
	errInvalidHost       = wire.NewError(http.StatusUnprocessableEntity, "invalid_network_host")
	errArtifactKey       = wire.NewError(http.StatusUnprocessableEntity, "invalid_artifact_key")
	errArtifactDigest    = wire.NewError(http.StatusUnprocessableEntity, "plugin_artifact_digest_mismatch")
	errArtifactMissing   = wire.NewError(http.StatusUnprocessableEntity, "plugin_artifact_unavailable")
	errManifestRequired  = wire.NewError(http.StatusUnprocessableEntity, "plugin_manifest_required")
	errManifestInvalid   = wire.NewError(http.StatusUnprocessableEntity, "invalid_plugin_manifest")
)

var (
	slugRunRe  = regexp.MustCompile(`[^a-z0-9]+`)
	slugRe     = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	semverRe   = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?$`)
	sha256Re   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	keyRe      = regexp.MustCompile(`^[a-zA-Z0-9/_-]+\.wasm$`)
	toolNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,63}$`)
)

// errVersionInsert is what every marketplace_versions INSERT does in Swift at
// fdbed9b0. MarketplaceVersion.configFields is a [PluginConfigField], which
// Fluent binds as jsonb[], while the column is jsonb, so Postgres refuses the
// statement: "column config_fields is of type jsonb but expression is of type
// jsonb[]". createVersion reports that as 409 version_already_exists, and the
// first-party catalog stops after its first listing, which then has no version.
// The port matches it (parity would not pass otherwise, and production must not
// change on a flip). Fixing it is SetVersionInserts(true) as the default, once
// the owner decides; see the PR.
var errVersionInsert = errors.New("marketplace: version insert fails as in Swift (config_fields jsonb[])")

// Options are the plugin.* settings MarketplaceService takes.
type Options struct {
	RunnerURL          string
	RunnerToken        string
	ArtifactRoot       string
	ArtifactSigningKey string
}

type Module struct {
	pool     *pgxpool.Pool
	q        *db.Queries
	box      *secretbox.Box
	opts     Options
	runner   Runner
	broker   Broker
	identify echo.MiddlewareFunc
	limits   *ratelimit.Store
	now      func() time.Time
	// versionInserts is false in production: see errVersionInsert.
	versionInserts bool
}

// CheckConfig is the guard in Swift's buildRouter: outside dev, a runner token
// with no usable signing key refuses to boot. An unset environment is dev.
func CheckConfig(environment string, opts Options) error {
	if opts.RunnerToken == "" || len(opts.ArtifactSigningKey) >= minSigningKey || environment == "" || environment == "dev" {
		return nil
	}
	return errors.New("PLUGIN_ARTIFACT_SIGNING_KEY must contain at least 32 bytes when the plugin runner is enabled. " +
		"Use a secret independent from PLUGIN_RUNNER_TOKEN")
}

// New returns nil without a usable LV_SECRET_MASTER_KEY: Swift mounts
// /v1/marketplace only inside the plugins block, which needs SecretBox.
func New(pool *pgxpool.Pool, masterKey string, opts Options, identify echo.MiddlewareFunc, limits *ratelimit.Store, paths vaultpaths.Paths) *Module {
	box, err := secretbox.New(masterKey)
	if err != nil {
		return nil
	}
	return &Module{
		pool: pool, q: db.New(pool), box: box, opts: opts,
		runner:   NewRunner(opts.RunnerURL, opts.RunnerToken),
		broker:   NewBroker(pool, paths),
		identify: identify, limits: limits, now: time.Now,
	}
}

// SetVersionInserts lets marketplace_versions inserts work, which they do not in
// Swift (see errVersionInsert). The Go tests use it to cover the code behind them.
func (m *Module) SetVersionInserts(work bool) { m.versionInserts = work }

// SetRunner replaces the plugin runner (tests).
func (m *Module) SetRunner(r Runner) { m.runner = r }

// SetBroker replaces the capability broker (tests).
func (m *Module) SetBroker(b Broker) { m.broker = b }

// Register ignores the shared auth middleware: the group is jwtAuthenticator +
// settingsByUser in Swift, with the 401 raised in the handler.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	mw := []echo.MiddlewareFunc{m.identify, ratelimit.Middleware(m.limits, ratelimit.SettingsByUser, ratelimit.UserOrIPKey)}
	r.GET("/v1/marketplace/plugins", m.list, mw...)
	r.GET("/v1/marketplace/plugins/:slug", m.detail, mw...)
	r.GET("/v1/marketplace/plugins/:slug/reviews", m.reviews, mw...)
	r.PUT("/v1/marketplace/plugins/:slug/rating", m.rate, mw...)
	r.POST("/v1/marketplace/plugins/:slug/install", m.install, mw...)
	r.POST("/v1/marketplace/plugins/:slug/upgrade", m.upgrade, mw...)
	r.POST("/v1/marketplace/plugins/:slug/tools/:toolName/run", m.runTool, mw...)

	r.POST("/v1/marketplace/publisher/apply", m.applyPublisher, mw...)
	r.GET("/v1/marketplace/publisher", m.publisher, mw...)
	r.POST("/v1/marketplace/publisher/plugins", m.createListing, mw...)
	r.POST("/v1/marketplace/publisher/plugins/:slug/versions", m.createVersion, mw...)
	r.POST("/v1/marketplace/publisher/plugins/:slug/artifacts", m.uploadArtifact, mw...)
	r.POST("/v1/marketplace/publisher/plugins/:slug/versions/:versionID/submit", m.submit, mw...)
	r.GET("/v1/marketplace/publisher/submissions", m.publisherSubmissions, mw...)

	r.GET("/v1/marketplace/admin/submissions", m.adminSubmissions, mw...)
	r.GET("/v1/marketplace/admin/publishers", m.adminPublishers, mw...)
	r.POST("/v1/marketplace/admin/submissions/:submissionID/decision", m.moderate, mw...)
	r.POST("/v1/marketplace/admin/publishers/:publisherID/decision", m.moderatePublisher, mw...)
	r.POST("/v1/marketplace/admin/versions/:versionID/revoke", m.revokeVersion, mw...)
}

// MARK: - Browsing

func (m *Module) list(c *echo.Context) error {
	if _, ok := auth.Identity(c); !ok {
		return wire.ErrUnauthenticated
	}
	ctx := c.Request().Context()
	query := c.QueryParams()
	var category *PluginCategory
	if raw := query.Get("category"); PluginCategory(raw).Valid() {
		category = new(PluginCategory(raw))
	}
	featured := parseBool(query.Get("featured"))
	limit := 24
	if raw, ok := param(query, "limit"); ok {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	limit = min(50, max(1, limit))

	if err := m.ensureFirstPartyCatalog(ctx); err != nil {
		return err
	}
	rows, err := m.q.PublishedListings(ctx)
	if err != nil {
		return err
	}
	normalized := ""
	if raw, ok := param(query, "query"); ok {
		normalized = strings.ToLower(swiftstr.TrimWhitespacesAndNewlines(raw))
	}
	filtered := rows[:0:0]
	for _, row := range rows {
		if (category == nil || row.Category == string(*category)) &&
			(featured == nil || row.Featured == *featured) &&
			(normalized == "" || strings.Contains(strings.ToLower(row.Name), normalized) || strings.Contains(strings.ToLower(row.Summary), normalized)) {
			filtered = append(filtered, row)
		}
	}
	offset := 0
	if raw, ok := param(query, "cursor"); ok {
		if n, err := strconv.Atoi(raw); err == nil {
			offset = n
		}
	}
	page := filtered[min(max(0, offset), len(filtered)):]
	page = page[:min(limit, len(page))]
	out := MarketplaceListResponse{Items: make([]MarketplacePluginDTO, len(page))}
	for i, row := range page {
		if out.Items[i], err = m.pluginDTO(ctx, row, nil); err != nil {
			return err
		}
	}
	if offset < len(filtered)-len(page) {
		next := strconv.Itoa(offset + len(page))
		out.NextCursor = &next
	}
	return wire.JSON(c, http.StatusOK, out)
}

func param(q url.Values, name string) (string, bool) {
	vals, ok := q[name]
	if !ok || len(vals) == 0 {
		return "", false
	}
	return vals[0], true
}

func parseBool(s string) *bool {
	switch strings.ToLower(s) {
	case "true", "1":
		return new(true)
	case "false", "0":
		return new(false)
	}
	return nil
}

func (m *Module) detail(c *echo.Context) error {
	if _, ok := auth.Identity(c); !ok {
		return wire.ErrUnauthenticated
	}
	ctx := c.Request().Context()
	if err := m.ensureFirstPartyCatalog(ctx); err != nil {
		return err
	}
	row, err := m.q.PublishedListingBySlug(ctx, c.Param("slug"))
	if errors.Is(err, pgx.ErrNoRows) {
		return errPluginNotFound
	}
	if err != nil {
		return err
	}
	dto, err := m.pluginDTO(ctx, row, nil)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, dto)
}

func (m *Module) reviews(c *echo.Context) error {
	if _, ok := auth.Identity(c); !ok {
		return wire.ErrUnauthenticated
	}
	ctx := c.Request().Context()
	listing, err := m.requireListing(ctx, c.Param("slug"))
	if err != nil {
		return err
	}
	rows, err := m.q.Reviews(ctx, listing.ID)
	if err != nil {
		return err
	}
	out := MarketplaceReviewsResponse{Items: make([]MarketplaceReviewDTO, len(rows))}
	for i, r := range rows {
		out.Items[i] = MarketplaceReviewDTO{
			Id: wire.UUID{UUID: r.ID}, Rating: int(r.Rating), Body: r.Body, AuthorUsername: r.Username,
			VerifiedInstall: true, CreatedAt: wireTime(r.CreatedAt), UpdatedAt: wireTime(r.UpdatedAt),
		}
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) rate(c *echo.Context) error {
	req, err := wire.Bind[ratingRequest](c)
	if err != nil {
		return err
	}
	slug := c.Param("slug")
	userID, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	if req.Rating < 1 || req.Rating > 5 {
		return wire.NewError(http.StatusUnprocessableEntity, "rating_must_be_1_to_5")
	}
	var body *string
	if req.Body != nil {
		trimmed := swiftstr.TrimWhitespacesAndNewlines(*req.Body)
		if swiftstr.Count(trimmed) > 2000 {
			return wire.NewError(http.StatusRequestEntityTooLarge, "review_too_long")
		}
		if trimmed != "" {
			body = &trimmed
		}
	}
	ctx := c.Request().Context()
	listing, err := m.requireListing(ctx, slug)
	if err != nil {
		return err
	}
	if _, err := m.q.InstallBySlug(ctx, db.InstallBySlugParams{TenantID: userID, PluginSlug: slug}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return wire.NewError(http.StatusForbidden, "verified_install_required")
		}
		return err
	}
	publisher, err := m.q.PublisherByID(ctx, listing.PublisherID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err != nil || publisher.OwnerUserID == userID {
		return wire.NewError(http.StatusForbidden, "publishers_cannot_rate_own_plugin")
	}
	now := m.now()
	var row db.MarketplaceRating
	existing, err := m.q.RatingByUser(ctx, db.RatingByUserParams{ListingID: listing.ID, UserID: userID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		row, err = m.q.InsertRating(ctx, db.InsertRatingParams{
			ID: uuid.New(), ListingID: listing.ID, UserID: userID, Rating: int64(req.Rating), Body: body, At: &now,
		})
	case err == nil:
		row, err = m.q.UpdateRating(ctx, db.UpdateRatingParams{Rating: int64(req.Rating), Body: body, At: &now, ID: existing.ID})
	}
	if err != nil {
		return err
	}
	username, err := m.q.Username(ctx, userID)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, MarketplaceReviewDTO{
		Id: wire.UUID{UUID: row.ID}, Rating: int(row.Rating), Body: row.Body, AuthorUsername: username,
		VerifiedInstall: true, CreatedAt: wireTime(row.CreatedAt), UpdatedAt: wireTime(row.UpdatedAt),
	})
}

// MARK: - Installs (PluginService.installMarketplace / upgradeMarketplace)

func (m *Module) install(c *echo.Context) error {
	req, err := wire.Bind[installRequest](c)
	if err != nil {
		return err
	}
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	dto, err := m.installMarketplace(c.Request().Context(), tenant, c.Param("slug"), req.VersionID.UUID, req.GrantedPermissions, req.Config)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, dto)
}

func (m *Module) upgrade(c *echo.Context) error {
	req, err := wire.Bind[upgradeRequest](c)
	if err != nil {
		return err
	}
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	slug := c.Param("slug")
	ctx := c.Request().Context()
	row, err := m.q.InstallBySlug(ctx, db.InstallBySlugParams{TenantID: tenant, PluginSlug: slug})
	if errors.Is(err, pgx.ErrNoRows) {
		return wire.NewError(http.StatusNotFound, "install_not_found")
	}
	if err != nil {
		return err
	}
	if row.MarketplaceVersionID == nil || *row.MarketplaceVersionID != req.FromVersionID.UUID {
		return wire.NewError(http.StatusConflict, "marketplace_install_version_changed")
	}
	if req.FromVersionID.UUID == req.ToVersionID.UUID {
		return wire.NewError(http.StatusConflict, "marketplace_version_already_installed")
	}
	config, err := m.openConfig(row, tenant)
	if err != nil {
		return err
	}
	maps.Copy(config, req.Config)
	dto, err := m.installMarketplace(ctx, tenant, slug, req.ToVersionID.UUID, req.GrantedPermissions, config)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, dto)
}

// installMarketplace installs an approved immutable version. Permission grants
// must exactly match the reviewed version, so a client cannot silently omit
// consent or grant capabilities the package never declared.
func (m *Module) installMarketplace(ctx context.Context, tenant uuid.UUID, slug string, versionID uuid.UUID, granted []PluginPermission, config map[string]string) (PluginInstallDTO, error) {
	listing, err := m.q.PublishedListingBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return PluginInstallDTO{}, errVersionNotFound
	}
	if err != nil {
		return PluginInstallDTO{}, err
	}
	version, err := m.q.ApprovedListingVersion(ctx, db.ApprovedListingVersionParams{ID: versionID, ListingID: listing.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return PluginInstallDTO{}, errVersionNotFound
	}
	if err != nil {
		return PluginInstallDTO{}, err
	}
	grantedSet := make([]string, 0, len(granted))
	for _, p := range granted {
		grantedSet = append(grantedSet, string(p))
	}
	slices.Sort(grantedSet)
	grantedSet = slices.Compact(grantedSet)
	declared := slices.Sorted(slices.Values(version.Permissions))
	if !slices.Equal(slices.Compact(declared), grantedSet) {
		return PluginInstallDTO{}, wire.NewError(http.StatusUnprocessableEntity, "permission_consent_mismatch")
	}
	fields, err := configFields(version)
	if err != nil {
		return PluginInstallDTO{}, err
	}
	allowed := map[string]bool{}
	for _, f := range fields {
		allowed[f.Key] = true
	}
	for key := range config {
		if !allowed[key] {
			return PluginInstallDTO{}, wire.NewError(http.StatusBadRequest, "unknown_field")
		}
	}
	existing, err := m.q.InstallBySlug(ctx, db.InstallBySlugParams{TenantID: tenant, PluginSlug: slug})
	hasExisting := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PluginInstallDTO{}, err
	}
	effective := map[string]string{}
	if hasExisting {
		if effective, err = m.openConfig(existing, tenant); err != nil {
			return PluginInstallDTO{}, err
		}
	}
	maps.Copy(effective, config)
	for _, f := range fields {
		if !f.IsRequired {
			continue
		}
		value, ok := effective[f.Key]
		if !ok || swiftstr.TrimWhitespacesAndNewlines(value) == "" {
			return PluginInstallDTO{}, wire.NewError(http.StatusBadRequest, "missing_field:"+f.Key)
		}
	}
	sealed, err := m.seal(effective, tenant)
	if err != nil {
		return PluginInstallDTO{}, err
	}
	now := m.now()
	var row db.PluginInstall
	if hasExisting {
		row, err = m.q.UpdateInstall(ctx, db.UpdateInstallParams{
			ConfigCiphertext: sealed.Ciphertext, ConfigNonce: sealed.Nonce, MarketplaceVersionID: &versionID,
			GrantedPermissions: grantedSet, At: &now, ID: existing.ID,
		})
	} else {
		row, err = m.q.InsertInstall(ctx, db.InsertInstallParams{
			ID: uuid.New(), TenantID: tenant, PluginSlug: slug,
			ConfigCiphertext: sealed.Ciphertext, ConfigNonce: sealed.Nonce, MarketplaceVersionID: &versionID,
			GrantedPermissions: grantedSet, At: &now,
		})
	}
	if err != nil {
		return PluginInstallDTO{}, err
	}
	return installDTO(row), nil
}

func (m *Module) seal(config map[string]string, tenant uuid.UUID) (secretbox.Sealed, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(config); err != nil { // map keys are sorted, as Swift's .sortedKeys
		return secretbox.Sealed{}, err
	}
	sealed, err := m.box.Seal(strings.TrimSuffix(buf.String(), "\n"), tenant)
	if err != nil {
		slog.ErrorContext(context.Background(), "plugin config encryption failed", "error", err)
		return secretbox.Sealed{}, wire.NewError(http.StatusInternalServerError, "encryption_failed")
	}
	return sealed, nil
}

func (m *Module) openConfig(row db.PluginInstall, tenant uuid.UUID) (map[string]string, error) {
	plaintext, err := m.box.Open(secretbox.Sealed{Ciphertext: row.ConfigCiphertext, Nonce: row.ConfigNonce}, tenant)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(plaintext), &out); err != nil || out == nil {
		return map[string]string{}, nil //nolint:nilerr // Swift: (try? decode) ?? [:]
	}
	return out, nil
}

func installDTO(r db.PluginInstall) PluginInstallDTO {
	granted := make([]PluginPermission, 0, len(r.GrantedPermissions))
	for _, p := range r.GrantedPermissions {
		if PluginPermission(p).Valid() {
			granted = append(granted, PluginPermission(p))
		}
	}
	status := PluginInstallDTOStatus(r.Status)
	if status != "enabled" && status != "disabled" {
		status = "enabled"
	}
	out := PluginInstallDTO{
		Id: wire.UUID{UUID: r.ID}, PluginSlug: r.PluginSlug, Status: status, HasConfig: len(r.ConfigCiphertext) > 0,
		CreatedAt: wireTime(r.CreatedAt), LastSyncAt: wireTime(r.LastSyncAt), GrantedPermissions: &granted,
	}
	if r.MarketplaceVersionID != nil {
		out.MarketplaceVersionId = &wire.UUID{UUID: *r.MarketplaceVersionID}
	}
	return out
}

// MARK: - Publishers

func (m *Module) applyPublisher(c *echo.Context) error {
	req, err := wire.Bind[publisherApplicationRequest](c)
	if err != nil {
		return err
	}
	userID, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	handle, err := normalizedSlug(req.Handle)
	if err != nil {
		return err
	}
	displayName := swiftstr.TrimWhitespacesAndNewlines(req.DisplayName)
	if swiftstr.Count(displayName) < 2 {
		return wire.NewError(http.StatusUnprocessableEntity, "publisher_display_name_required")
	}
	ctx := c.Request().Context()
	existing, err := m.q.PublisherByOwner(ctx, userID)
	if err == nil {
		return wire.JSON(c, http.StatusOK, publisherDTO(existing))
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	website, err := validHTTPSURL(req.WebsiteURL)
	if err != nil {
		return err
	}
	id := uuid.New()
	now := m.now()
	if err := m.q.InsertPublisher(ctx, db.InsertPublisherParams{
		ID: id, OwnerUserID: userID, Handle: handle, DisplayName: displayName,
		Bio: req.Bio, WebsiteUrl: website, Status: "pending", Verified: false, At: &now,
	}); err != nil {
		return wire.NewError(http.StatusConflict, "publisher_handle_unavailable")
	}
	row, err := m.q.PublisherByID(ctx, id)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, publisherDTO(row))
}

func (m *Module) publisher(c *echo.Context) error {
	userID, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	row, err := m.q.PublisherByOwner(c.Request().Context(), userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errPublisherNotFound
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, publisherDTO(row))
}

func (m *Module) createListing(c *echo.Context) error {
	req, err := wire.Bind[listingCreateRequest](c)
	if err != nil {
		return err
	}
	userID, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	ctx := c.Request().Context()
	publisher, err := m.requireApprovedPublisher(ctx, userID)
	if err != nil {
		return err
	}
	slug, err := normalizedSlug(req.Slug)
	if err != nil {
		return err
	}
	name := swiftstr.TrimWhitespacesAndNewlines(req.Name)
	summary := swiftstr.TrimWhitespacesAndNewlines(req.Summary)
	description := swiftstr.TrimWhitespacesAndNewlines(req.Description)
	icon, err := validHTTPSURL(req.IconURL)
	if err != nil {
		return err
	}
	screenshots := []string{}
	if req.Screenshots != nil {
		for _, s := range *req.Screenshots {
			if !isHTTPSURL(s) {
				return errHTTPSRequired
			}
			screenshots = append(screenshots, s)
		}
	}
	if name == "" || summary == "" || description == "" {
		return wire.NewError(http.StatusUnprocessableEntity, "listing_metadata_required")
	}
	now := m.now()
	row, err := m.q.InsertListing(ctx, db.InsertListingParams{
		ID: uuid.New(), PublisherID: publisher.ID, Slug: slug, Name: name, Summary: summary, Description: description,
		Category: string(req.Category), IconUrl: icon, Screenshots: screenshots, Status: "draft", Featured: false, At: &now,
	})
	if err != nil {
		return wire.NewError(http.StatusConflict, "plugin_slug_unavailable")
	}
	// Not saved: a draft listing has no version yet, but the DTO needs one.
	placeholderVersion := db.MarketplaceVersion{
		ID: uuid.New(), ListingID: row.ID, Version: "0.0.0", Status: "draft", RuntimeKind: "declarative",
		Permissions: []string{}, NetworkHosts: []string{}, ConfigFields: []byte("[]"),
	}
	dto, err := m.pluginDTO(ctx, row, &placeholderVersion)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, dto)
}

func (m *Module) createVersion(c *echo.Context) error {
	req, err := wire.Bind[versionCreateRequest](c)
	if err != nil {
		return err
	}
	userID, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	ctx := c.Request().Context()
	listing, _, err := m.requireOwnedListing(ctx, userID, c.Param("slug"))
	if err != nil {
		return err
	}
	if !semverRe.MatchString(req.Version) {
		return wire.NewError(http.StatusUnprocessableEntity, "invalid_semver")
	}
	if req.RuntimeKind == Native {
		return wire.NewError(http.StatusForbidden, "native_runtime_reserved_for_first_party_plugins")
	}
	if req.ManifestJSON != nil {
		var object map[string]any
		if err := json.Unmarshal([]byte(*req.ManifestJSON), &object); err != nil || object == nil {
			return errManifestInvalid
		}
	}
	permissions := make([]string, 0, len(req.Permissions))
	for _, p := range req.Permissions {
		permissions = append(permissions, string(p))
	}
	slices.Sort(permissions)
	permissions = slices.Compact(permissions)
	hosts := []string{}
	if req.NetworkHosts != nil {
		for _, h := range *req.NetworkHosts {
			host, err := validHost(h)
			if err != nil {
				return err
			}
			hosts = append(hosts, host)
		}
		slices.Sort(hosts)
	}
	if !slices.Contains(permissions, string(NetworkFetch)) && len(hosts) > 0 {
		return wire.NewError(http.StatusUnprocessableEntity, "network_hosts_require_network_fetch")
	}
	if req.RuntimeKind == Wasm {
		if req.ArtifactSHA256 == nil || !sha256Re.MatchString(*req.ArtifactSHA256) ||
			req.ArtifactKey == nil || *req.ArtifactKey == "" || req.ArtifactSignature == nil ||
			!VerifyArtifactSignature(*req.ArtifactSignature, *req.ArtifactKey, *req.ArtifactSHA256, m.opts.ArtifactSigningKey) {
			return wire.NewError(http.StatusUnprocessableEntity, "wasm_artifact_required")
		}
		if err := m.verifyArtifact(*req.ArtifactKey, *req.ArtifactSHA256); err != nil {
			return err
		}
	}
	if !m.versionInserts {
		return wire.NewError(http.StatusConflict, "version_already_exists")
	}
	fields := []PluginConfigField{}
	if req.ConfigFields != nil {
		for _, f := range *req.ConfigFields {
			fields = append(fields, PluginConfigField{Key: f.Key, Label: f.Label, Placeholder: f.Placeholder, Kind: f.Kind, IsRequired: f.IsRequired})
		}
	}
	args := db.InsertVersionParams{
		ID: uuid.New(), ListingID: listing.ID, Version: req.Version, Status: "draft", RuntimeKind: string(req.RuntimeKind),
		Permissions: permissions, NetworkHosts: hosts, ConfigFields: mustJSON(fields), Changelog: req.Changelog,
		ArtifactKey: req.ArtifactKey, ArtifactSha256: req.ArtifactSHA256, ArtifactSignature: req.ArtifactSignature,
		At: new(m.now()),
	}
	if req.ManifestJSON != nil {
		args.ManifestJson = []byte(*req.ManifestJSON)
	}
	row, err := m.q.InsertVersion(ctx, args)
	if err != nil {
		return wire.NewError(http.StatusConflict, "version_already_exists")
	}
	return wire.JSON(c, http.StatusOK, versionDTO(row))
}

func (m *Module) uploadArtifact(c *echo.Context) error {
	req, err := wire.Bind[artifactUploadRequest](c)
	if err != nil {
		return err
	}
	userID, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	slug := c.Param("slug")
	_, publisher, err := m.requireOwnedListing(c.Request().Context(), userID, slug)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.ToLower(req.FileName), ".wasm") || strings.ContainsAny(req.FileName, `/\`) {
		return wire.NewError(http.StatusUnprocessableEntity, "wasm_file_required")
	}
	data, ok := decodeBase64(req.BytesBase64)
	if !ok || len(data) == 0 || len(data) > maxArtifactBytes {
		return wire.NewError(http.StatusRequestEntityTooLarge, "wasm_artifact_too_large")
	}
	if !bytes.HasPrefix(data, []byte{0x00, 0x61, 0x73, 0x6D}) {
		return wire.NewError(http.StatusUnprocessableEntity, "invalid_wasm_artifact")
	}
	digest := sha256Hex(data)
	key := publisher.Handle + "/" + slug + "/" + digest + ".wasm"
	if len(m.opts.ArtifactSigningKey) < minSigningKey {
		return wire.NewError(http.StatusServiceUnavailable, "plugin_artifact_signing_unavailable")
	}
	path, ok := m.artifactPath(key)
	if !ok {
		return errArtifactKey
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // Foundation's default, shared with the runner's volume
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := writeAtomic(path, data); err != nil {
			return err
		}
	}
	return wire.JSON(c, http.StatusOK, MarketplaceArtifactUploadResponse{
		ArtifactKey: key, Sha256: digest, SizeBytes: len(data),
		Signature: SignArtifact(key, digest, m.opts.ArtifactSigningKey),
	})
}

func (m *Module) submit(c *echo.Context) error {
	userID, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	slug := c.Param("slug")
	versionID, err := uuidParam(c, "versionID")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	listing, _, err := m.requireOwnedListing(ctx, userID, slug)
	if err != nil {
		return err
	}
	version, err := m.q.ListingVersion(ctx, db.ListingVersionParams{ID: versionID, ListingID: listing.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return errVersionNotFound
	}
	if err != nil {
		return err
	}
	if version.Status != string(MarketplaceVersionStatusDraft) && version.Status != string(MarketplaceVersionStatusRejected) {
		return wire.NewError(http.StatusConflict, "version_not_submittable")
	}
	validation := ValidateVersion(version)
	if version.RuntimeKind == string(Wasm) && !m.artifactIntegrityIsValid(version) {
		validation = append(validation, "plugin_artifact_integrity_invalid")
	}
	status := string(MarketplaceVersionStatusDraft)
	if len(validation) == 0 {
		status = string(MarketplaceVersionStatusInReview)
	}
	now := m.now()
	if err := m.q.SetVersionStatus(ctx, db.SetVersionStatusParams{ID: versionID, Status: status, UpdatedAt: &now, PublishedAt: version.PublishedAt}); err != nil {
		return err
	}
	var submittedAt *time.Time
	if len(validation) == 0 {
		submittedAt = &now
	}
	var row db.MarketplaceSubmission
	existing, err := m.q.SubmissionByVersion(ctx, versionID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		row, err = m.q.InsertSubmission(ctx, db.InsertSubmissionParams{
			ID: uuid.New(), VersionID: versionID, PublisherUserID: userID, Status: status,
			ValidationErrors: validation, SubmittedAt: submittedAt, At: &now,
		})
	case err == nil:
		row, err = m.q.ResubmitSubmission(ctx, db.ResubmitSubmissionParams{
			PublisherUserID: userID, Status: status, ValidationErrors: validation, SubmittedAt: submittedAt, At: &now, ID: existing.ID,
		})
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, submissionDTO(row, slug))
}

func (m *Module) publisherSubmissions(c *echo.Context) error {
	userID, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	rows, err := m.q.PublisherSubmissions(c.Request().Context(), userID)
	if err != nil {
		return err
	}
	out := MarketplaceSubmissionsResponse{Items: make([]MarketplaceSubmissionDTO, len(rows))}
	for i, r := range rows {
		out.Items[i] = submissionDTO(db.MarketplaceSubmission{
			ID: r.ID, VersionID: r.VersionID, Status: r.Status, ValidationErrors: r.ValidationErrors,
			ReviewNote: r.ReviewNote, SubmittedAt: r.SubmittedAt, ReviewedAt: r.ReviewedAt,
		}, r.Slug)
	}
	return wire.JSON(c, http.StatusOK, out)
}

// MARK: - Admin

func (m *Module) requireAdmin(ctx context.Context, userID uuid.UUID) error {
	admin, err := m.q.IsAdmin(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !admin) {
		return errAdminRequired
	}
	return err
}

func (m *Module) adminSubmissions(c *echo.Context) error {
	userID, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	ctx := c.Request().Context()
	if err := m.requireAdmin(ctx, userID); err != nil {
		return err
	}
	rows, err := m.q.AllSubmissions(ctx)
	if err != nil {
		return err
	}
	out := MarketplaceSubmissionsResponse{Items: make([]MarketplaceSubmissionDTO, len(rows))}
	for i, r := range rows {
		out.Items[i] = submissionDTO(db.MarketplaceSubmission{
			ID: r.ID, VersionID: r.VersionID, Status: r.Status, ValidationErrors: r.ValidationErrors,
			ReviewNote: r.ReviewNote, SubmittedAt: r.SubmittedAt, ReviewedAt: r.ReviewedAt,
		}, r.Slug)
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) adminPublishers(c *echo.Context) error {
	userID, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	ctx := c.Request().Context()
	if err := m.requireAdmin(ctx, userID); err != nil {
		return err
	}
	rows, err := m.q.PendingPublishers(ctx)
	if err != nil {
		return err
	}
	out := MarketplacePublishersResponse{Items: make([]MarketplacePublisherDTO, len(rows))}
	for i, r := range rows {
		out.Items[i] = publisherDTO(r)
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) moderate(c *echo.Context) error {
	req, err := wire.Bind[moderationRequest](c)
	if err != nil {
		return err
	}
	submissionID, err := uuidParam(c, "submissionID")
	if err != nil {
		return err
	}
	adminID, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	ctx := c.Request().Context()
	if err := m.requireAdmin(ctx, adminID); err != nil {
		return err
	}
	submission, err := m.q.SubmissionByID(ctx, submissionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errSubmissionMissing
	}
	if err != nil {
		return err
	}
	version, err := m.q.VersionByID(ctx, submission.VersionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errSubmissionMissing
	}
	if err != nil {
		return err
	}
	listing, err := m.q.ListingByID(ctx, version.ListingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errSubmissionMissing
	}
	if err != nil {
		return err
	}
	if submission.Status != string(MarketplaceVersionStatusInReview) {
		return wire.NewError(http.StatusConflict, "submission_not_in_review")
	}
	status := string(MarketplaceVersionStatusRejected)
	if req.Approved {
		status = string(MarketplaceVersionStatusApproved)
		if version.RuntimeKind == string(Wasm) && !m.artifactIntegrityIsValid(version) {
			return errIntegrityInvalid
		}
	}
	now := m.now()
	row, err := m.q.ReviewSubmission(ctx, db.ReviewSubmissionParams{
		Status: status, ReviewNote: req.Note, ReviewedByUserID: &adminID, At: &now, ID: submissionID,
	})
	if err != nil {
		return err
	}
	publishedAt := version.PublishedAt
	if req.Approved {
		publishedAt = &now
	}
	if err := m.q.SetVersionStatus(ctx, db.SetVersionStatusParams{ID: version.ID, Status: status, UpdatedAt: &now, PublishedAt: publishedAt}); err != nil {
		return err
	}
	if req.Approved {
		if err := m.q.PublishListing(ctx, db.PublishListingParams{ID: listing.ID, Status: "published", UpdatedAt: &now}); err != nil {
			return err
		}
	}
	return wire.JSON(c, http.StatusOK, submissionDTO(row, listing.Slug))
}

func (m *Module) moderatePublisher(c *echo.Context) error {
	req, err := wire.Bind[moderationRequest](c)
	if err != nil {
		return err
	}
	publisherID, err := uuidParam(c, "publisherID")
	if err != nil {
		return err
	}
	adminID, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	ctx := c.Request().Context()
	if err := m.requireAdmin(ctx, adminID); err != nil {
		return err
	}
	status := "rejected"
	if req.Approved {
		status = "approved"
	}
	row, err := m.q.SetPublisherStatus(ctx, db.SetPublisherStatusParams{ID: publisherID, Status: status, Verified: req.Approved, UpdatedAt: new(m.now())})
	if errors.Is(err, pgx.ErrNoRows) {
		return errPublisherNotFound
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, publisherDTO(row))
}

func (m *Module) revokeVersion(c *echo.Context) error {
	req, err := wire.Bind[revokeRequest](c)
	if err != nil {
		return err
	}
	versionID, err := uuidParam(c, "versionID")
	if err != nil {
		return err
	}
	adminID, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	ctx := c.Request().Context()
	if err := m.requireAdmin(ctx, adminID); err != nil {
		return err
	}
	if swiftstr.Count(swiftstr.TrimWhitespacesAndNewlines(req.Reason)) < 5 {
		return wire.NewError(http.StatusUnprocessableEntity, "revocation_reason_required")
	}
	version, err := m.q.VersionByID(ctx, versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errVersionNotFound
	}
	if err != nil {
		return err
	}
	now := m.now()
	if err := m.q.SetVersionStatus(ctx, db.SetVersionStatusParams{ID: versionID, Status: "revoked", UpdatedAt: &now, PublishedAt: version.PublishedAt}); err != nil {
		return err
	}
	if err := m.q.DisableVersionInstalls(ctx, db.DisableVersionInstallsParams{At: &now, VersionID: &versionID}); err != nil {
		return err
	}
	slog.WarnContext(ctx, "marketplace version revoked", "version_id", ids.Upper(versionID), "reason", req.Reason)
	version.Status = "revoked"
	return wire.JSON(c, http.StatusOK, versionDTO(version))
}

// MARK: - Internals

// ensureFirstPartyCatalog: migrations usually run before the first admin is
// bootstrapped, so the built-in catalog is seeded on first read once a stable
// system owner exists.
func (m *Module) ensureFirstPartyCatalog(ctx context.Context) error {
	seeded, err := m.q.PublisherExistsByHandle(ctx, "luminavault")
	if err != nil || seeded {
		return err
	}
	admin, err := m.q.FirstAdmin(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	publisherID := uuid.MustParse("00000000-0000-4000-8000-000000000043")
	now := m.now()
	if err := m.q.InsertPublisher(ctx, db.InsertPublisherParams{
		ID: publisherID, OwnerUserID: admin, Handle: "luminavault", DisplayName: "LuminaVault",
		Status: "approved", Verified: true, At: &now,
	}); err != nil {
		// Another request won the race.
		won, checkErr := m.q.PublisherExistsByHandle(ctx, "luminavault")
		if checkErr != nil {
			return checkErr
		}
		if !won {
			return err
		}
		return nil
	}
	for _, entry := range firstPartyCatalog {
		listing, err := m.q.InsertListing(ctx, db.InsertListingParams{
			ID: uuid.New(), PublisherID: publisherID, Slug: entry.slug, Name: entry.name, Summary: entry.summary,
			Description: entry.description, Category: entry.category, Screenshots: []string{}, Status: "published",
			Featured: entry.featured, At: &now,
		})
		if err != nil {
			return err
		}
		if !m.versionInserts {
			return errVersionInsert
		}
		if _, err := m.q.InsertVersion(ctx, db.InsertVersionParams{
			ID: uuid.New(), ListingID: listing.ID, Version: "1.0.0", Status: "approved", RuntimeKind: "native",
			Permissions: []string{}, NetworkHosts: []string{}, ConfigFields: mustJSON(entry.configFields),
			PublishedAt: &now, At: &now,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (m *Module) pluginDTO(ctx context.Context, row db.MarketplaceListing, override *db.MarketplaceVersion) (MarketplacePluginDTO, error) {
	publisher, err := m.q.PublisherByID(ctx, row.PublisherID)
	if errors.Is(err, pgx.ErrNoRows) {
		return MarketplacePluginDTO{}, wire.NewError(http.StatusInternalServerError, "marketplace_publisher_missing")
	}
	if err != nil {
		return MarketplacePluginDTO{}, err
	}
	var version db.MarketplaceVersion
	if override != nil {
		version = *override
	} else {
		version, err = m.q.LatestApprovedVersion(ctx, row.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return MarketplacePluginDTO{}, errVersionNotFound
		}
		if err != nil {
			return MarketplacePluginDTO{}, err
		}
	}
	ratings, err := m.q.RatingsForListing(ctx, row.ID)
	if err != nil {
		return MarketplacePluginDTO{}, err
	}
	installs, err := m.q.InstallCount(ctx, row.Slug)
	if err != nil {
		return MarketplacePluginDTO{}, err
	}
	average := 0.0
	if len(ratings) > 0 {
		var sum int64
		for _, r := range ratings {
			sum += r
		}
		average = float64(sum) / float64(len(ratings))
	}
	fields, err := configFields(version)
	if err != nil {
		return MarketplacePluginDTO{}, err
	}
	category := PluginCategory(row.Category)
	if !category.Valid() {
		category = Skill
	}
	return MarketplacePluginDTO{
		Slug: row.Slug, Name: row.Name, Summary: row.Summary, Description: row.Description, Category: category,
		IconURL: row.IconUrl, Screenshots: nonNil(row.Screenshots), Publisher: publisherDTO(publisher),
		LatestVersion: versionDTO(version), Featured: row.Featured, RatingAverage: average, RatingCount: len(ratings),
		InstallCount: int(installs), ConfigFields: fields,
	}, nil
}

func configFields(v db.MarketplaceVersion) ([]PluginConfigField, error) {
	out := []PluginConfigField{}
	if err := json.Unmarshal(v.ConfigFields, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (m *Module) requireListing(ctx context.Context, slug string) (db.MarketplaceListing, error) {
	row, err := m.q.ListingBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, errPluginNotFound
	}
	return row, err
}

func (m *Module) requireOwnedListing(ctx context.Context, userID uuid.UUID, slug string) (db.MarketplaceListing, db.MarketplacePublisher, error) {
	publisher, err := m.requireApprovedPublisher(ctx, userID)
	if err != nil {
		return db.MarketplaceListing{}, publisher, err
	}
	listing, err := m.q.OwnedListing(ctx, db.OwnedListingParams{Slug: slug, PublisherID: publisher.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return listing, publisher, errPluginNotFound
	}
	return listing, publisher, err
}

func (m *Module) requireApprovedPublisher(ctx context.Context, userID uuid.UUID) (db.MarketplacePublisher, error) {
	row, err := m.q.PublisherByOwner(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.Status != "approved") {
		return row, errApprovedRequired
	}
	return row, err
}

func publisherDTO(r db.MarketplacePublisher) MarketplacePublisherDTO {
	status := MarketplacePublisherDTOStatus(r.Status)
	return MarketplacePublisherDTO{
		Id: wire.UUID{UUID: r.ID}, Handle: r.Handle, DisplayName: r.DisplayName, Bio: r.Bio,
		WebsiteURL: r.WebsiteUrl, Verified: r.Verified, Status: &status,
	}
}

func versionDTO(r db.MarketplaceVersion) MarketplaceVersionDTO {
	status := MarketplaceVersionStatus(r.Status)
	if !status.Valid() {
		status = MarketplaceVersionStatusDraft
	}
	kind := MarketplaceRuntimeKind(r.RuntimeKind)
	if !kind.Valid() {
		kind = Declarative
	}
	permissions := make([]PluginPermission, 0, len(r.Permissions))
	for _, p := range r.Permissions {
		if PluginPermission(p).Valid() {
			permissions = append(permissions, PluginPermission(p))
		}
	}
	tools := []MarketplaceToolManifest{}
	if manifest, err := ParseManifest(r); err == nil {
		tools = manifest.Tools
	}
	return MarketplaceVersionDTO{
		Id: wire.UUID{UUID: r.ID}, Version: r.Version, Status: status, RuntimeKind: kind, Permissions: permissions,
		NetworkHosts: nonNil(r.NetworkHosts), Changelog: r.Changelog, PublishedAt: wireTime(r.PublishedAt), Tools: &tools,
	}
}

func submissionDTO(r db.MarketplaceSubmission, slug string) MarketplaceSubmissionDTO {
	status := MarketplaceVersionStatus(r.Status)
	if !status.Valid() {
		status = MarketplaceVersionStatusDraft
	}
	return MarketplaceSubmissionDTO{
		Id: wire.UUID{UUID: r.ID}, PluginSlug: slug, VersionId: wire.UUID{UUID: r.VersionID}, Status: status,
		ValidationErrors: nonNil(r.ValidationErrors), ReviewNote: r.ReviewNote,
		SubmittedAt: wireTime(r.SubmittedAt), ReviewedAt: wireTime(r.ReviewedAt),
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func wireTime(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}

func uuidParam(c *echo.Context, name string) (uuid.UUID, error) {
	raw := c.Param(name)
	id, err := ids.Parse(raw)
	if err != nil || len(raw) != 36 {
		return uuid.Nil, wire.NewError(http.StatusBadRequest, "invalid_"+name)
	}
	return id, nil
}

// decodeBase64 is Data(base64Encoded:): padded standard alphabet and nothing
// else. Go's decoder would also skip CR and LF.
func decodeBase64(s string) ([]byte, bool) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, false
	}
	data, err := base64.StdEncoding.DecodeString(s)
	return data, err == nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func normalizedSlug(input string) (string, error) {
	normalized := strings.Trim(slugRunRe.ReplaceAllString(strings.ToLower(input), "-"), "-")
	if n := len(normalized); n < 2 || n > 64 || !slugRe.MatchString(normalized) {
		return "", errInvalidSlug
	}
	return normalized, nil
}

func validHost(value string) (string, error) {
	host := swiftstr.TrimWhitespacesAndNewlines(strings.ToLower(value))
	if host == "" || strings.ContainsAny(host, "/:") {
		return "", errInvalidHost
	}
	if u, err := url.Parse("https://" + host); err != nil || u.Host != host {
		return "", errInvalidHost
	}
	return host, nil
}

func validHTTPSURL(value *string) (*string, error) {
	if value == nil || *value == "" {
		return nil, nil //nolint:nilnil // absent is valid
	}
	if !isHTTPSURL(*value) {
		return nil, errHTTPSRequired
	}
	return value, nil
}

// isHTTPSURL is requireHTTPSURL: URL.scheme is reported as written, so
// HTTPS:// is not https.
func isHTTPSURL(value string) bool {
	if !strings.HasPrefix(value, "https:") {
		return false
	}
	u, err := url.Parse(value)
	return err == nil && u.Hostname() != ""
}

// Manifest is MarketplacePluginManifest.
type Manifest struct {
	SchemaVersion int
	Tools         []MarketplaceToolManifest
}

// ParseManifest is MarketplaceService.manifest: every key is required, and a
// wrong type or a missing key is invalid_plugin_manifest.
func ParseManifest(v db.MarketplaceVersion) (Manifest, error) {
	if v.ManifestJson == nil {
		return Manifest{}, errManifestRequired
	}
	var raw struct {
		SchemaVersion *float64 `json:"schemaVersion"`
		Tools         *[]struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(v.ManifestJson, &raw); err != nil || raw.SchemaVersion == nil || raw.Tools == nil ||
		*raw.SchemaVersion != float64(int(*raw.SchemaVersion)) {
		return Manifest{}, errManifestInvalid
	}
	out := Manifest{SchemaVersion: int(*raw.SchemaVersion), Tools: make([]MarketplaceToolManifest, len(*raw.Tools))}
	for i, t := range *raw.Tools {
		if t.Name == nil || t.Description == nil {
			return Manifest{}, errManifestInvalid
		}
		out.Tools[i] = MarketplaceToolManifest{Name: *t.Name, Description: *t.Description}
	}
	return out, nil
}

// ValidateVersion is MarketplaceService.validate; errors in Swift's order.
func ValidateVersion(v db.MarketplaceVersion) []string {
	errs := []string{}
	if !semverRe.MatchString(v.Version) {
		errs = append(errs, "invalid_semver")
	}
	if v.RuntimeKind == string(Wasm) {
		if v.ArtifactKey == nil || v.ArtifactSha256 == nil || v.ArtifactSignature == nil {
			errs = append(errs, "wasm_artifact_required")
		}
		manifest, err := ParseManifest(v)
		if err != nil {
			errs = append(errs, "invalid_plugin_manifest")
		} else {
			if manifest.SchemaVersion != 1 || len(manifest.Tools) == 0 {
				errs = append(errs, "plugin_tools_required")
			}
			names := map[string]bool{}
			badName := false
			for _, t := range manifest.Tools {
				names[t.Name] = true
				badName = badName || !toolNameRe.MatchString(t.Name)
			}
			if len(names) != len(manifest.Tools) {
				errs = append(errs, "duplicate_plugin_tools")
			}
			if badName {
				errs = append(errs, "invalid_plugin_tool_name")
			}
		}
	}
	if len(v.NetworkHosts) > 0 && !slices.Contains(v.Permissions, string(NetworkFetch)) {
		errs = append(errs, "network_hosts_require_network_fetch")
	}
	return errs
}

// MARK: - Artifacts

// SignArtifact is MarketplaceArtifactIntegrity.sign: the signature binds both
// the storage key and the digest.
func SignArtifact(key, digest, signingKey string) string {
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write([]byte(key + ":" + digest))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyArtifactSignature fails closed for weak keys.
func VerifyArtifactSignature(signature, key, digest, signingKey string) bool {
	if len(signingKey) < minSigningKey || !sha256Re.MatchString(signature) {
		return false
	}
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write([]byte(key + ":" + digest))
	return hmac.Equal(got, mac.Sum(nil))
}

// artifactPath is the key under the artifact root, or false if it escapes.
func (m *Module) artifactPath(key string) (string, bool) {
	root, err := filepath.Abs(m.opts.ArtifactRoot)
	if err != nil {
		return "", false
	}
	path := filepath.Join(root, key)
	return path, strings.HasPrefix(path, root+string(filepath.Separator))
}

func safeArtifactKey(key string) bool {
	return !strings.Contains(key, "..") && !strings.HasPrefix(key, "/") && keyRe.MatchString(key)
}

func (m *Module) verifyArtifact(key, expected string) error {
	if !safeArtifactKey(key) {
		return errArtifactKey
	}
	data, ok := m.readArtifact(key)
	if !ok {
		return errArtifactMissing
	}
	if sha256Hex(data) != expected {
		return errArtifactDigest
	}
	return nil
}

func (m *Module) readArtifact(key string) ([]byte, bool) {
	path, ok := m.artifactPath(key)
	if !ok {
		return nil, false
	}
	data, err := os.ReadFile(path) //nolint:gosec // key checked by safeArtifactKey and artifactPath
	if err != nil || len(data) > maxArtifactBytes {
		return nil, false
	}
	return data, true
}

func (m *Module) artifactIntegrityIsValid(v db.MarketplaceVersion) bool {
	if v.ArtifactKey == nil || v.ArtifactSha256 == nil || v.ArtifactSignature == nil ||
		!VerifyArtifactSignature(*v.ArtifactSignature, *v.ArtifactKey, *v.ArtifactSha256, m.opts.ArtifactSigningKey) {
		return false
	}
	return m.verifyArtifact(*v.ArtifactKey, *v.ArtifactSha256) == nil
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { //nolint:gosec // Foundation's default for a written file
		return err
	}
	return os.Rename(tmp.Name(), path)
}
