// Package selfimprovement serves /v1/me/improvement: the Go port of
// LuminaVaultServer Sources/App/SelfImprovement's controller and the request half of
// SelfImprovementService. SelfImprovementScheduler stays in Swift: curator/runs and
// soul/reviews only queue rows, which it claims with SKIP LOCKED, and its review LLM
// goes straight to the gateway.
package selfimprovement

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/selfimprovement/oapi-codegen.yaml api/openapi.yaml"

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/selfimprovement/db"
	"lumina/internal/selfimprove"
	"lumina/internal/skills"
	"lumina/internal/soul"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	defaultRunsLimit = 50
	maxRunsLimit     = 100
	firstReviewDelay = 168 * time.Hour

	messageDisabled = "Self-improvement is temporarily disabled by the operator."
	messageReadOnly = "Self-improvement is read-only when using a bring-your-own Hermes endpoint."
)

var (
	errDisabled        = wire.NewError(http.StatusServiceUnavailable, "self_improvement_disabled")
	errReadOnly        = wire.NewError(http.StatusConflict, "self_improvement_read_only_on_byo_hermes")
	errInvalidSettings = wire.NewError(http.StatusBadRequest, "invalid_self_improvement_settings")
	errInvalidID       = wire.NewError(http.StatusBadRequest, "invalid_id")
	errInvalidResource = wire.NewError(http.StatusBadRequest, "invalid_resource")
	errInvalidName     = wire.NewError(http.StatusBadRequest, "invalid_resource_name")
	errManualTrigger   = wire.NewError(http.StatusBadRequest, "manual_trigger_required")
	errCuratorDisabled = wire.NewError(http.StatusConflict, "curator_disabled")
	errSoulDisabled    = wire.NewError(http.StatusConflict, "soul_review_disabled")
	errSoulUnsupported = wire.NewError(http.StatusConflict, "soul_review_unsupported_on_byo_hermes")
	errRunActive       = wire.NewError(http.StatusConflict, "self_improvement_run_already_active")
	errRunNotFound     = wire.NewError(http.StatusNotFound, "self_improvement_run_not_found")
	errChangeNotFound  = wire.NewError(http.StatusNotFound, "self_improvement_change_not_found")
	errAlreadyDecided  = wire.NewError(http.StatusConflict, "self_improvement_change_already_decided")
	errSoulOnBYO       = wire.NewError(http.StatusConflict, "soul_unsupported_on_byo_hermes")
	errSoulChanged     = wire.NewError(http.StatusConflict, "soul_changed_since_review")
	errProposalMissing = wire.NewError(http.StatusConflict, "soul_proposal_missing")
	errNoSnapshot      = wire.NewError(http.StatusConflict, "rollback_snapshot_unavailable")
	errResourceChanged = wire.NewError(http.StatusConflict, "resource_changed_since_curator_run")
	errResourceMissing = wire.NewError(http.StatusNotFound, "curator_resource_not_found")
	errKindMismatch    = wire.NewError(http.StatusConflict, "resource_kind_mismatch")
)

// Options are the module's dependencies.
type Options struct {
	Identify echo.MiddlewareFunc
	Paths    vaultpaths.Paths
	Soul     *soul.Service
	Catalog  *skills.Catalog
	// IsUserOverride is selfimprove.ResolverOverride. Nil means never, Swift's
	// capabilities service being absent without a SecretBox.
	IsUserOverride func(context.Context, uuid.UUID) bool
	// EconomyModel is selfImprovement.economyModel; empty is reported as unavailable.
	EconomyModel string
	// GloballyEnabled is selfImprovement.enabled.
	GloballyEnabled bool
}

// Module is the route group. Swift's group has jwtAuthenticator and nothing else, so
// no rate limit, and each handler reaches requireIdentity() where Swift does: body
// and path checks that come first answer 400 without a token.
type Module struct {
	q        *db.Queries
	identify echo.MiddlewareFunc
	paths    vaultpaths.Paths
	soul     *soul.Service
	catalog  *skills.Catalog
	override func(context.Context, uuid.UUID) bool
	economy  string
	enabled  bool
	now      func() time.Time
}

// New builds the module.
func New(pool *pgxpool.Pool, opts Options) *Module {
	return &Module{
		q:        db.New(pool),
		identify: opts.Identify,
		paths:    opts.Paths,
		soul:     opts.Soul,
		catalog:  opts.Catalog,
		override: opts.IsUserOverride,
		economy:  opts.EconomyModel,
		enabled:  opts.GloballyEnabled,
		now:      time.Now,
	}
}

// Register ignores the shared auth middleware on purpose; see Module.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	const base = "/v1/me/improvement"
	r.GET(base, m.getStatus, m.identify)
	r.PUT(base, m.putSettings, m.identify)
	r.POST(base+"/curator/runs", m.runCurator, m.identify)
	r.GET(base+"/runs", m.listRuns, m.identify)
	r.GET(base+"/runs/:id", m.getRun, m.identify)
	r.POST(base+"/runs/:id/rollback", m.rollback, m.identify)
	r.GET(base+"/resources", m.listResources, m.identify)
	r.PATCH(base+"/resources/:kind/:name", m.pinResource, m.identify)
	r.POST(base+"/soul/reviews", m.reviewSoul, m.identify)
	r.GET(base+"/changes", m.listChanges, m.identify)
	r.POST(base+"/changes/:id/approve", m.approveChange, m.identify)
	r.POST(base+"/changes/:id/reject", m.rejectChange, m.identify)
}

// MARK: - Status and settings

func (m *Module) getStatus(c *echo.Context) error {
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	return m.respondStatus(c, tenant)
}

func (m *Module) putSettings(c *echo.Context) error {
	body, err := wire.Bind[settingsUpdateRequest](c)
	if err != nil {
		return err
	}
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	dto := body.Settings
	if !validSettings(dto) {
		return errInvalidSettings
	}
	ctx := c.Request().Context()
	if err := m.requireManaged(ctx, tenant); err != nil {
		return err
	}
	row, err := m.settings(ctx, tenant)
	if err != nil {
		return err
	}
	// Saving the cadence is also an explicit reschedule. Otherwise a changed interval
	// would keep its old due date until the next run.
	now := m.now()
	next := now.Add(time.Duration(dto.IntervalHours) * time.Hour)
	if _, err := m.q.UpdateSettings(ctx, db.UpdateSettingsParams{
		ID:                      row.ID,
		Enabled:                 dto.Enabled,
		CuratorEnabled:          dto.CuratorEnabled,
		IntervalHours:           int32(dto.IntervalHours),    //nolint:gosec // validSettings bounds it
		MinimumIdleHours:        int32(dto.MinimumIdleHours), //nolint:gosec // validSettings bounds it
		Consolidate:             dto.Consolidate,
		PruneBuiltins:           dto.PruneBuiltins,
		BackupKeep:              int32(dto.BackupKeep), //nolint:gosec // validSettings bounds it
		SoulReviewEnabled:       dto.SoulReviewEnabled,
		ReviewComplexSessions:   dto.ReviewComplexSessions,
		SoulReviewWindowDays:    int32(dto.SoulReviewWindowDays),    //nolint:gosec // validSettings bounds it
		SoulReviewCooldownHours: int32(dto.SoulReviewCooldownHours), //nolint:gosec // validSettings bounds it
		ModelMode:               string(dto.ModelMode),
		NextReviewAt:            &next,
		UpdatedAt:               now,
	}); err != nil {
		return err
	}
	return m.respondStatus(c, tenant)
}

// validSettings is SelfImprovementService.validate.
func validSettings(s settingsRequest) bool {
	return s.IntervalHours >= 24 && s.IntervalHours <= 720 &&
		s.MinimumIdleHours >= 0 && s.MinimumIdleHours <= 72 &&
		s.BackupKeep >= 1 && s.BackupKeep <= 20 &&
		s.SoulReviewWindowDays >= 7 && s.SoulReviewWindowDays <= 14 &&
		s.SoulReviewCooldownHours >= 1 && s.SoulReviewCooldownHours <= 168 &&
		!s.PruneBuiltins
}

func (m *Module) respondStatus(c *echo.Context, tenant uuid.UUID) error {
	ctx := c.Request().Context()
	row, err := m.settings(ctx, tenant)
	if err != nil {
		return err
	}
	pending, err := m.q.CountPendingChanges(ctx, tenant)
	if err != nil {
		return err
	}
	availability, message := Managed, (*string)(nil)
	switch {
	case !m.enabled:
		availability, message = Unavailable, ptr(messageDisabled)
	case m.isUserOverride(ctx, tenant):
		availability, message = ReadOnly, ptr(messageReadOnly)
	}
	return wire.JSON(c, http.StatusOK, ImprovementStatusDTO{
		Settings:              settingsDTO(row),
		Availability:          availability,
		EconomyModelAvailable: m.economy != "",
		PendingChanges:        int(pending),
		LastCuratorReviewAt:   wireTime(row.LastCuratorReviewAt),
		LastSoulReviewAt:      wireTime(row.LastSoulReviewAt),
		NextReviewAt:          wireTime(row.NextReviewAt),
		Message:               message,
	})
}

// settings finds the tenant's row or creates it with the safe defaults. Swift has no
// upsert here: losing the race on tenant_id is an error.
func (m *Module) settings(ctx context.Context, tenant uuid.UUID) (db.SelfImprovementSetting, error) {
	row, err := m.q.Settings(ctx, tenant)
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return row, err
	}
	now := m.now()
	next := now.Add(firstReviewDelay)
	return m.q.InsertSettings(ctx, db.InsertSettingsParams{ID: uuid.New(), TenantID: tenant, NextReviewAt: &next, CreatedAt: now})
}

// MARK: - Queue

func (m *Module) runCurator(c *echo.Context) error {
	// try? in Swift: anything that doesn't decode, an empty body included, is the
	// default, which is a dry run.
	dryRun := true
	if body, err := wire.Bind[runRequest](c); err == nil {
		dryRun = body.DryRun
	}
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if !dryRun {
		row, err := m.settings(ctx, tenant)
		if err != nil {
			return err
		}
		if !row.Enabled || !row.CuratorEnabled {
			return errCuratorDisabled
		}
	}
	return m.enqueue(c, tenant, Curator, dryRun)
}

func (m *Module) reviewSoul(c *echo.Context) error {
	// try? in Swift, so an unknown trigger is a decoding failure and means manual.
	if body, err := wire.Bind[soulReviewRequest](c); err == nil {
		if t := ImprovementTrigger(body.Trigger); t.Valid() && t != Manual {
			return errManualTrigger
		}
	}
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	row, err := m.settings(ctx, tenant)
	if err != nil {
		return err
	}
	if !row.Enabled || !row.SoulReviewEnabled {
		return errSoulDisabled
	}
	if m.isUserOverride(ctx, tenant) {
		return errSoulUnsupported
	}
	return m.enqueue(c, tenant, Soul, false)
}

// enqueue is the private enqueue with trigger .manual, the only one these routes use.
// An active run of the same mode is the answer; one of the other mode is a conflict.
func (m *Module) enqueue(c *echo.Context, tenant uuid.UUID, kind ImprovementChangeKind, dryRun bool) error {
	ctx := c.Request().Context()
	if err := m.requireManaged(ctx, tenant); err != nil {
		return err
	}
	active, err := m.q.ActiveRun(ctx, db.ActiveRunParams{TenantID: tenant, Kind: string(kind)})
	switch {
	case err == nil && active.DryRun == dryRun:
		return wire.JSON(c, http.StatusAccepted, ImprovementRunAcceptedResponse{Run: runDTO(active)})
	case err == nil:
		return errRunActive
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	run, err := m.q.InsertRun(ctx, db.InsertRunParams{
		ID: uuid.New(), TenantID: tenant, Kind: string(kind), Trigger: string(Manual), DryRun: dryRun, CreatedAt: m.now(),
	})
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusAccepted, ImprovementRunAcceptedResponse{Run: runDTO(run)})
}

// MARK: - Runs

func (m *Module) listRuns(c *echo.Context) error {
	limit, ok := wire.QueryInt(c, "limit")
	if !ok {
		limit = defaultRunsLimit
	}
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	rows, err := m.q.ListRuns(c.Request().Context(), db.ListRunsParams{TenantID: tenant, Limit: int32(min(max(limit, 1), maxRunsLimit))}) //nolint:gosec // clamped to 1...100
	if err != nil {
		return err
	}
	runs := make([]ImprovementRunDTO, 0, len(rows))
	for _, row := range rows {
		runs = append(runs, runDTO(row))
	}
	return wire.JSON(c, http.StatusOK, ImprovementRunsResponse{Runs: runs})
}

func (m *Module) getRun(c *echo.Context) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	row, err := m.q.Run(c.Request().Context(), db.RunParams{ID: id, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return errRunNotFound
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, runDTO(row))
}

// resourceSnapshot is one entry of self_improvement_runs.snapshot_json, which Swift's
// curator writes.
type resourceSnapshot struct {
	name, markdown, state string
	afterSHA              *string
	archivedPath          *string
}

// decodeSnapshots is JSONDecoder on [ResourceSnapshot] under try?. Keys match exactly,
// and name, markdown and state must be strings.
func decodeSnapshots(raw string) ([]resourceSnapshot, bool) {
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, false
	}
	out := make([]resourceSnapshot, 0, len(entries))
	for _, entry := range entries {
		var s resourceSnapshot
		var ok bool
		if s.name, ok = requiredString(entry["name"]); !ok {
			return nil, false
		}
		if s.markdown, ok = requiredString(entry["markdown"]); !ok {
			return nil, false
		}
		if s.state, ok = requiredString(entry["state"]); !ok {
			return nil, false
		}
		if s.afterSHA, ok = optionalString(entry["afterSHA"]); !ok {
			return nil, false
		}
		if s.archivedPath, ok = optionalString(entry["archivedPath"]); !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func requiredString(raw json.RawMessage) (string, bool) {
	s, ok := optionalString(raw)
	if !ok || s == nil {
		return "", false
	}
	return *s, true
}

func optionalString(raw json.RawMessage) (*string, bool) {
	if raw == nil || string(raw) == "null" {
		return nil, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, false
	}
	return &s, true
}

// rollback is SelfImprovementService.rollback. An unknown or foreign run, and a run
// with no usable snapshot, are all the one 409. Nothing here is a transaction, in
// Swift either: a failure partway leaves the earlier snapshots restored and the run's
// status unchanged.
func (m *Module) rollback(c *echo.Context) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := m.requireManaged(ctx, tenant); err != nil {
		return err
	}
	run, err := m.q.Run(ctx, db.RunParams{ID: id, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoSnapshot
	}
	if err != nil {
		return err
	}
	if run.SnapshotJson == nil {
		return errNoSnapshot
	}
	snapshots, ok := decodeSnapshots(*run.SnapshotJson)
	if !ok || len(snapshots) == 0 {
		return errNoSnapshot
	}
	for _, snapshot := range snapshots {
		if m.changedSinceRun(tenant, snapshot) {
			return errResourceChanged
		}
	}
	for _, snapshot := range snapshots {
		if err := m.restore(ctx, tenant, snapshot); err != nil {
			return err
		}
	}
	rolledBack, err := m.q.SetRunStatus(ctx, db.SetRunStatusParams{ID: run.ID, Status: string(ImprovementRunStatusRolledBack)})
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, ImprovementRollbackResponse{Run: runDTO(rolledBack)})
}

// changedSinceRun says the active file is no longer what the curator left. A missing
// or unreadable file, or a snapshot with no afterSHA, doesn't count.
func (m *Module) changedSinceRun(tenant uuid.UUID, snapshot resourceSnapshot) bool {
	active := m.skillFile(tenant, snapshot.name)
	if snapshot.afterSHA == nil || !vaultfiles.Exists(active) {
		return false
	}
	data, err := os.ReadFile(active) //nolint:gosec // skillFile: Swift checks nothing here either, see there
	if err != nil {
		return false
	}
	current, ok := vaultfiles.DecodeUTF8(data)
	return ok && selfimprove.SHA256(current) != *snapshot.afterSHA
}

func (m *Module) restore(ctx context.Context, tenant uuid.UUID, snapshot resourceSnapshot) error {
	active := m.skillFile(tenant, snapshot.name)
	// os.MkdirAll on the uncleaned directory, because createDirectory creates each
	// component of what it's given, a directory before a ".." included.
	if err := os.MkdirAll(strings.TrimSuffix(active, "/SKILL.md"), 0o755); err != nil { //nolint:gosec // the mode Foundation's createDirectory leaves; Hermes reads this tree
		return err
	}
	if err := vaultfiles.WriteAtomic(active, []byte(snapshot.markdown)); err != nil {
		return err
	}
	if snapshot.archivedPath != nil && *snapshot.archivedPath != "" {
		// try? in Swift. The path is absolute and goes unchecked, as in Swift: the
		// curator wrote it, and only that row of the database names it.
		_ = os.RemoveAll(*snapshot.archivedPath)
	}
	_, err := m.q.VaultSkillState(ctx, db.VaultSkillStateParams{TenantID: tenant, Name: snapshot.name})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return m.q.InsertSkillState(ctx, db.InsertSkillStateParams{TenantID: tenant, Name: snapshot.name, CuratorState: snapshot.state})
	case err != nil:
		return err
	}
	return m.q.SetTenantSkillsCuratorState(ctx, db.SetTenantSkillsCuratorStateParams{TenantID: tenant, CuratorState: snapshot.state})
}

// skillFile is <tenant>/skills/<name>/SKILL.md. Concatenated rather than joined so a
// ".." in name reaches the filesystem as it does through Swift's
// appendingPathComponent. Swift runs name through no path check at this site, and
// this keeps that (D-wave4-prep): it skips vaultfiles.SanitizePath and ResolveInside.
// Its only source is a snapshot's name, which is a manifest name from a vault
// SKILL.md, never the request.
func (m *Module) skillFile(tenant uuid.UUID, name string) string {
	return m.paths.TenantRoot(tenant) + "/skills/" + name + "/SKILL.md"
}

// MARK: - Resources

func (m *Module) listResources(c *echo.Context) error {
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	states, err := m.q.VaultSkillStates(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	stateByName := make(map[string]db.VaultSkillStatesRow, len(states))
	for _, state := range states {
		stateByName[swiftstr.CanonicalKey(state.Name)] = state
	}
	items := []ImprovementSkillDTO{}
	visible := map[string]bool{}
	for _, manifest := range m.catalog.Manifests(tenant) {
		if manifest.Source != skills.SourceVault {
			continue
		}
		key := swiftstr.CanonicalKey(manifest.Name)
		visible[key] = true
		state, known := stateByName[key]
		item := ImprovementSkillDTO{
			Name: manifest.Name, Title: manifest.Description, Kind: manifestKind(manifest),
			State: ImprovementResourceStateActive, CuratorManaged: true,
		}
		if known {
			item.State = resourceState(state.CuratorState)
			item.Pinned = state.CuratorPinned
			item.LastActivityAt = wireTime(state.CuratorLastActivityAt)
		}
		items = append(items, item)
	}
	for _, state := range states {
		if visible[swiftstr.CanonicalKey(state.Name)] || state.CuratorState != string(ImprovementResourceStateArchived) {
			continue
		}
		kind := ImprovementResourceKindSkill
		if swiftstr.HasASCIIPrefix(state.Name, "job-") {
			kind = ImprovementResourceKindJob
		}
		items = append(items, ImprovementSkillDTO{
			Name: state.Name, Title: state.Name, Kind: kind, State: ImprovementResourceStateArchived,
			Pinned: state.CuratorPinned, CuratorManaged: true, LastActivityAt: wireTime(state.CuratorLastActivityAt),
		})
	}
	slices.SortFunc(items, func(a, b ImprovementSkillDTO) int {
		return cmp.Compare(swiftstr.CanonicalKey(a.Name), swiftstr.CanonicalKey(b.Name))
	})
	return wire.JSON(c, http.StatusOK, ImprovementSkillsResponse{Skills: items})
}

func (m *Module) pinResource(c *echo.Context) error {
	kind := ImprovementResourceKind(c.Param("kind"))
	if !kind.Valid() {
		return errInvalidResource
	}
	name := c.Param("name")
	body, err := wire.Bind[pinRequest](c)
	if err != nil {
		return err
	}
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	if !selfimprove.ValidResourceName(name) {
		return errInvalidName
	}
	ctx := c.Request().Context()
	if err := m.requireManaged(ctx, tenant); err != nil {
		return err
	}
	manifest, hasManifest := m.catalog.Manifest(name, tenant)
	existing, err := m.q.VaultSkillState(ctx, db.VaultSkillStateParams{TenantID: tenant, Name: name})
	hasState := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	archived := hasState && existing.CuratorState == string(ImprovementResourceStateArchived)
	if (!hasManifest || manifest.Source != skills.SourceVault) && !archived {
		return errResourceMissing
	}
	if hasManifest && manifestKind(manifest) != kind {
		return errKindMismatch
	}
	out := ImprovementSkillDTO{
		Name: name, Title: name, Kind: kind, State: ImprovementResourceStateActive,
		Pinned: body.Pinned, CuratorManaged: true,
	}
	if hasManifest {
		out.Title = manifest.Description
	}
	if hasState {
		if err := m.q.SetTenantSkillsPinned(ctx, db.SetTenantSkillsPinnedParams{TenantID: tenant, CuratorPinned: body.Pinned}); err != nil {
			return err
		}
		out.State = resourceState(existing.CuratorState)
		out.LastActivityAt = wireTime(existing.CuratorLastActivityAt)
	} else if err := m.q.InsertSkillState(ctx, db.InsertSkillStateParams{
		TenantID: tenant, Name: name, CuratorPinned: body.Pinned, CuratorState: string(ImprovementResourceStateActive),
	}); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, out)
}

func manifestKind(manifest skills.Manifest) ImprovementResourceKind {
	if swiftstr.HasASCIIPrefix(manifest.Name, "job-") || manifest.Schedule != nil {
		return ImprovementResourceKindJob
	}
	return ImprovementResourceKindSkill
}

func resourceState(raw string) ImprovementResourceState {
	if s := ImprovementResourceState(raw); s.Valid() {
		return s
	}
	return ImprovementResourceStateActive
}

// MARK: - Changes

func (m *Module) listChanges(c *echo.Context) error {
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	rows, err := m.q.ListChanges(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	changes := make([]ImprovementChangeDTO, 0, len(rows))
	for _, row := range rows {
		changes = append(changes, changeDTO(row))
	}
	return wire.JSON(c, http.StatusOK, ImprovementChangesResponse{Changes: changes})
}

func (m *Module) approveChange(c *echo.Context) error { return m.decide(c, true) }

func (m *Module) rejectChange(c *echo.Context) error { return m.decide(c, false) }

// decide is SelfImprovementService.decide. A stale base or a missing proposal is
// stored and then answered as a conflict. A failed SOUL write answers 500 and leaves
// the change pending, though the vault copy may already be written.
func (m *Module) decide(c *echo.Context, approve bool) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := m.requireManaged(ctx, tenant); err != nil {
		return err
	}
	change, err := m.q.Change(ctx, db.ChangeParams{ID: id, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return errChangeNotFound
	}
	if err != nil {
		return err
	}
	if change.State != string(ImprovementChangeStatePending) {
		return errAlreadyDecided
	}
	decidedAt := m.now()
	update := db.UpdateChangeParams{ID: change.ID, DecidedAt: &decidedAt, FailureReason: change.FailureReason, AppliedAt: change.AppliedAt}
	if !approve {
		update.State = string(ImprovementChangeStateRejected)
		return m.saveChange(c, update)
	}
	// Unreachable while requireManaged has just passed, short of the config changing
	// between the two reads.
	if m.isUserOverride(ctx, tenant) {
		return errSoulOnBYO
	}
	username, err := m.q.Username(ctx, tenant)
	if err != nil {
		return err
	}
	current := m.soul.Read(tenant, username, m.now())
	if change.BaseSha256 == nil || selfimprove.SHA256(current) != *change.BaseSha256 {
		update.State = string(ImprovementChangeStateStale)
		if _, err := m.q.UpdateChange(ctx, update); err != nil {
			return err
		}
		return errSoulChanged
	}
	if change.ProposedMarkdown == nil {
		update.State = string(ImprovementChangeStateFailed)
		update.FailureReason = ptr("proposal payload missing")
		if _, err := m.q.UpdateChange(ctx, update); err != nil {
			return err
		}
		return errProposalMissing
	}
	if _, err := m.soul.Write(tenant, username, *change.ProposedMarkdown); err != nil {
		return err
	}
	appliedAt := m.now()
	update.State = string(ImprovementChangeStateApplied)
	update.AppliedAt = &appliedAt
	return m.saveChange(c, update)
}

func (m *Module) saveChange(c *echo.Context, update db.UpdateChangeParams) error {
	saved, err := m.q.UpdateChange(c.Request().Context(), update)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, ImprovementDecisionResponse{Change: changeDTO(saved)})
}

// MARK: - Helpers

// requireManaged is SelfImprovementService.requireManaged.
func (m *Module) requireManaged(ctx context.Context, tenant uuid.UUID) error {
	if !m.enabled {
		return errDisabled
	}
	if m.isUserOverride(ctx, tenant) {
		return errReadOnly
	}
	return nil
}

func (m *Module) isUserOverride(ctx context.Context, tenant uuid.UUID) bool {
	return m.override != nil && m.override(ctx, tenant)
}

func requireIdentity(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, auth.ErrUnauthorized
	}
	return id, nil
}

// idParam is SelfImprovementController.id, UUID(uuidString:) on the raw segment.
func idParam(c *echo.Context) (uuid.UUID, error) {
	id, err := ids.Parse(c.Param("id"))
	if err != nil {
		return uuid.Nil, errInvalidID
	}
	return id, nil
}

func ptr(s string) *string { return &s }
