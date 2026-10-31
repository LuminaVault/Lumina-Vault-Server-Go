// Package teams serves /v1/teams, /v1/vaults and /v1/team-invitations: the
// Go port of LuminaVaultServer Sources/App/Teams/TeamController.swift (the
// team and shared-vault control plane). Vault access checks are
// internal/vaultaccess (VaultAccessService.swift); activity rows and the live
// stream are internal/vaultactivity (VaultActivityPublisher.swift).
//
// Each handler runs Swift's checks in Swift's order: path ids are parsed
// before identity is required (a bad id is a 400 even without a token), and
// GET /v1/teams purges expired archives before anything else.
package teams

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/teams/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/teams/db"
	"lumina/internal/ratelimit"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultactivity"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	archiveRetention = 30 * 24 * time.Hour
	invitationTTL    = 7 * 24 * time.Hour
)

var (
	errTeamAccess        = wire.NewError(http.StatusForbidden, "team access denied")
	errTeamName          = wire.NewError(http.StatusUnprocessableEntity, "team name must be 2-80 characters")
	errVaultName         = wire.NewError(http.StatusUnprocessableEntity, "vault name must be 2-80 characters")
	errTeamNotFound      = wire.NewError(http.StatusNotFound, "team not found")
	errBadVaultRole      = wire.NewError(http.StatusUnprocessableEntity, "invalid vault role")
	errMemberNotFound    = wire.NewError(http.StatusNotFound, "vault member not found")
	errRemoveOwner       = wire.NewError(http.StatusConflict, "transfer team ownership before removing the owner")
	errTransferNotOwner  = wire.NewError(http.StatusForbidden, "only the team owner can transfer ownership")
	errBadNewOwner       = wire.NewError(http.StatusUnprocessableEntity, "new owner must be an existing team member")
	errArchiveNotOwner   = wire.NewError(http.StatusForbidden, "only the team owner can archive the team")
	errNotRestorable     = wire.NewError(http.StatusGone, "team is not restorable")
	errOwnerLeaving      = wire.NewError(http.StatusConflict, "transfer ownership before leaving")
	errInviteFields      = wire.NewError(http.StatusUnprocessableEntity, "email and at least one vault grant are required")
	errInvitePending     = wire.NewError(http.StatusConflict, "a pending invitation already exists for this email")
	errInviteGrant       = wire.NewError(http.StatusUnprocessableEntity, "invalid invitation vault grant")
	errPendingNotFound   = wire.NewError(http.StatusNotFound, "pending invitation not found")
	errResendFailed      = wire.NewError(http.StatusServiceUnavailable, "invitation could not be resent")
	errVerifyFirst       = wire.NewError(http.StatusForbidden, "verify email before accepting")
	errInvitationInvalid = wire.NewError(http.StatusGone, "invitation is invalid or expired")
)

var vaultRoles = []string{"viewer", "editor", "admin"}

type Module struct {
	pool     *pgxpool.Pool
	q        *db.Queries
	access   *vaultaccess.Service
	activity *vaultactivity.Recorder
	hub      *vaultactivity.Hub
	paths    vaultpaths.Paths
	identify echo.MiddlewareFunc
	limits   *ratelimit.Store
	sender   Sender
	now      func() time.Time
}

// New builds the module. identify is auth.Identify (ids are checked before
// identity); limits is the process's shared rate-limit store; paths is the
// vault root; sender delivers invitation tokens (NewSender).
func New(pool *pgxpool.Pool, identify echo.MiddlewareFunc, limits *ratelimit.Store, paths vaultpaths.Paths, sender Sender) *Module {
	return &Module{
		pool:     pool,
		q:        db.New(pool),
		access:   vaultaccess.New(pool),
		activity: vaultactivity.New(pool),
		hub:      vaultactivity.Default,
		paths:    paths,
		identify: identify,
		limits:   limits,
		sender:   sender,
		now:      time.Now,
	}
}

// SetClock replaces the clock (tests).
func (m *Module) SetClock(now func() time.Time) { m.now = now }

const streamPath = "/v1/vaults/:vaultID/activity/stream"

// Register ignores the shared auth middleware: every group is
// jwtAuthenticator + settingsByUser in Swift, and handlers require identity
// themselves, after parsing path ids.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	mw := []echo.MiddlewareFunc{m.identify, ratelimit.Middleware(m.limits, ratelimit.SettingsByUser, ratelimit.UserOrIPKey)}
	r.GET("/v1/teams", m.listTeams, mw...)
	r.POST("/v1/teams", m.createTeam, mw...)
	r.GET("/v1/teams/:teamID/vaults", m.listTeamVaults, mw...)
	r.POST("/v1/teams/:teamID/vaults", m.createVault, mw...)
	r.POST("/v1/teams/:teamID/invitations", m.invite, mw...)
	r.GET("/v1/teams/:teamID/invitations", m.listInvitations, mw...)
	r.POST("/v1/teams/:teamID/invitations/:invitationID/resend", m.resendInvitation, mw...)
	r.DELETE("/v1/teams/:teamID/invitations/:invitationID", m.revokeInvitation, mw...)
	r.PUT("/v1/teams/:teamID/owner", m.transferOwnership, mw...)
	r.POST("/v1/teams/:teamID/archive", m.archiveTeam, mw...)
	r.POST("/v1/teams/:teamID/restore", m.restoreTeam, mw...)
	r.POST("/v1/teams/:teamID/leave", m.leaveTeam, mw...)

	r.GET("/v1/vaults", m.listVaults, mw...)
	r.GET("/v1/vaults/:vaultID/members", m.listMembers, mw...)
	r.PUT("/v1/vaults/:vaultID/members/:userID", m.updateMember, mw...)
	r.DELETE("/v1/vaults/:vaultID/members/:userID", m.removeMember, mw...)
	r.GET("/v1/vaults/:vaultID/activity", m.listActivity, mw...)
	r.GET(streamPath, m.activityStream, mw...)
	r.Streaming(http.MethodGet, streamPath)

	r.POST("/v1/team-invitations/:token/accept", m.acceptInvitation, mw...)
}

// MARK: - Teams

func (m *Module) listTeams(c *echo.Context) error {
	ctx := c.Request().Context()
	if err := m.purgeExpiredArchives(ctx); err != nil {
		return err
	}
	userID, err := requireIdentity(c)
	if err != nil {
		return err
	}
	memberships, err := m.q.TeamMembershipsByUser(ctx, userID)
	if err != nil {
		return err
	}
	out := make([]TeamSummary, 0, len(memberships))
	for _, ms := range memberships {
		team, err := m.q.GetTeam(ctx, ms.TeamID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		out = append(out, teamSummary(team, ms.Role, team.ArchivedAt))
	}
	return wire.JSON(c, http.StatusOK, out)
}

type nameBody struct {
	Name string `json:"name"`
}

func (m *Module) createTeam(c *echo.Context) error {
	userID, err := requireIdentity(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[nameBody](c)
	if err != nil {
		return err
	}
	name := swiftstr.TrimWhitespacesAndNewlines(body.Name)
	if !validName(name) {
		return errTeamName
	}
	ctx := c.Request().Context()
	now := m.now()
	id := uuid.New()
	if err := m.q.CreateTeam(ctx, db.CreateTeamParams{ID: id, Name: name, OwnerUserID: userID, CreatedAt: &now}); err != nil {
		return err
	}
	if err := m.q.CreateTeamMembership(ctx, db.CreateTeamMembershipParams{
		ID: uuid.New(), TeamID: id, UserID: userID, Role: "owner", CreatedAt: &now,
	}); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, TeamSummary{Id: wire.UUID{UUID: id}, Name: name, Role: "owner"})
}

func (m *Module) transferOwnership(c *echo.Context) error {
	teamID, err := uuidParam(c, "teamID")
	if err != nil {
		return err
	}
	actorID, err := requireIdentity(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	current, err := m.requireTeamRole(c, teamID, true)
	if err != nil {
		return err
	}
	if current.Role != "owner" {
		return errTransferNotOwner
	}
	body, err := wire.Bind[struct {
		UserID wire.UUID `json:"userID"`
	}](c)
	if err != nil {
		return err
	}
	newOwner := body.UserID.UUID
	if newOwner == actorID {
		return errBadNewOwner
	}
	target, err := m.q.GetTeamMembership(ctx, db.GetTeamMembershipParams{TeamID: teamID, UserID: newOwner})
	if errors.Is(err, pgx.ErrNoRows) {
		return errBadNewOwner
	}
	if err != nil {
		return err
	}
	team, err := m.q.GetTeam(ctx, teamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errBadNewOwner
	}
	if err != nil {
		return err
	}
	// One transaction, in Swift's order: team, current owner, new owner.
	err = pgx.BeginFunc(ctx, m.pool, func(tx pgx.Tx) error {
		q := m.q.WithTx(tx)
		now := m.now()
		if err := q.UpdateTeamOwner(ctx, db.UpdateTeamOwnerParams{ID: teamID, OwnerUserID: newOwner, UpdatedAt: &now}); err != nil {
			return err
		}
		now = m.now()
		if err := q.UpdateTeamMembershipRole(ctx, db.UpdateTeamMembershipRoleParams{ID: current.ID, Role: "admin", UpdatedAt: &now}); err != nil {
			return err
		}
		now = m.now()
		return q.UpdateTeamMembershipRole(ctx, db.UpdateTeamMembershipRoleParams{ID: target.ID, Role: "owner", UpdatedAt: &now})
	})
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, teamSummary(team, "admin", team.ArchivedAt))
}

func (m *Module) archiveTeam(c *echo.Context) error {
	teamID, err := uuidParam(c, "teamID")
	if err != nil {
		return err
	}
	membership, err := m.requireTeamRole(c, teamID, true)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if membership.Role != "owner" {
		return errArchiveNotOwner
	}
	team, err := m.q.GetTeam(ctx, teamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errArchiveNotOwner
	}
	if err != nil {
		return err
	}
	archivedAt := m.now()
	if err := m.setArchived(ctx, teamID, &archivedAt); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, teamSummary(team, membership.Role, &archivedAt))
}

func (m *Module) restoreTeam(c *echo.Context) error {
	teamID, err := uuidParam(c, "teamID")
	if err != nil {
		return err
	}
	membership, err := m.requireTeamRole(c, teamID, true)
	if err != nil {
		return err
	}
	if membership.Role != "owner" {
		return errNotRestorable
	}
	ctx := c.Request().Context()
	team, err := m.q.GetTeam(ctx, teamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotRestorable
	}
	if err != nil {
		return err
	}
	if team.ArchivedAt == nil || !team.ArchivedAt.After(m.now().Add(-archiveRetention)) {
		return errNotRestorable
	}
	if err := m.setArchived(ctx, teamID, nil); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, teamSummary(team, membership.Role, nil))
}

// setArchived saves the team, then each of its vaults, as Swift does model by model.
func (m *Module) setArchived(ctx context.Context, teamID uuid.UUID, at *time.Time) error {
	now := m.now()
	if err := m.q.SetTeamArchived(ctx, db.SetTeamArchivedParams{ID: teamID, ArchivedAt: at, UpdatedAt: &now}); err != nil {
		return err
	}
	vaults, err := m.q.VaultsByTeam(ctx, &teamID)
	if err != nil {
		return err
	}
	for _, v := range vaults {
		now := m.now()
		if err := m.q.SetVaultArchived(ctx, db.SetVaultArchivedParams{ID: v.ID, ArchivedAt: at, UpdatedAt: &now}); err != nil {
			return err
		}
	}
	return nil
}

func (m *Module) leaveTeam(c *echo.Context) error {
	teamID, err := uuidParam(c, "teamID")
	if err != nil {
		return err
	}
	userID, err := requireIdentity(c)
	if err != nil {
		return err
	}
	membership, err := m.requireTeamRole(c, teamID, false)
	if err != nil {
		return err
	}
	if membership.Role == "owner" {
		return errOwnerLeaving
	}
	ctx := c.Request().Context()
	vaults, err := m.q.VaultsByTeam(ctx, &teamID)
	if err != nil {
		return err
	}
	for _, v := range vaults {
		if err := m.q.DeleteUserVaultMemberships(ctx, db.DeleteUserVaultMembershipsParams{VaultID: v.ID, UserID: userID}); err != nil {
			return err
		}
	}
	if err := m.q.DeleteTeamMembership(ctx, membership.ID); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// purgeExpiredArchives deletes teams archived over 30 days ago (their vaults,
// memberships and invitations cascade) and their vault folders. Swift runs it
// at the top of GET /v1/teams, before the token is checked.
func (m *Module) purgeExpiredArchives(ctx context.Context) error {
	cutoff := m.now().Add(-archiveRetention)
	teams, err := m.q.ExpiredArchivedTeams(ctx, &cutoff)
	if err != nil {
		return err
	}
	for _, id := range teams {
		vaults, err := m.q.VaultsByTeam(ctx, &id)
		if err != nil {
			return err
		}
		for _, v := range vaults {
			_ = os.RemoveAll(m.paths.TenantRoot(v.ID)) // try? in Swift
		}
		if err := m.q.DeleteTeam(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// MARK: - Vaults

func (m *Module) listVaults(c *echo.Context) error {
	userID, err := requireIdentity(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	memberships, err := m.q.VaultMembershipsByUser(ctx, userID)
	if err != nil {
		return err
	}
	out := make([]VaultSummary, 0, len(memberships))
	for _, ms := range memberships {
		v, err := m.q.GetVault(ctx, ms.VaultID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		out = append(out, vaultSummary(v, v.TeamID, v.PersonalOwnerUserID != nil, ms))
	}
	// Personal first, then localizedCaseInsensitiveCompare on the name; Swift's
	// sort is stable, so equal names keep membership order.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsPersonal != out[j].IsPersonal {
			return out[i].IsPersonal
		}
		return caseInsensitiveLess(out[i].Name, out[j].Name)
	})
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) listTeamVaults(c *echo.Context) error {
	teamID, err := uuidParam(c, "teamID")
	if err != nil {
		return err
	}
	if _, err := m.requireTeamRole(c, teamID, false); err != nil {
		return err
	}
	userID, _ := auth.Identity(c)
	ctx := c.Request().Context()
	vaults, err := m.q.VaultsByTeam(ctx, &teamID)
	if err != nil {
		return err
	}
	out := make([]VaultSummary, 0, len(vaults))
	for _, v := range vaults {
		ms, err := m.q.GetVaultMembership(ctx, db.GetVaultMembershipParams{VaultID: v.ID, UserID: userID})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		out = append(out, vaultSummary(v, &teamID, false, ms))
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) createVault(c *echo.Context) error {
	teamID, err := uuidParam(c, "teamID")
	if err != nil {
		return err
	}
	if _, err := m.requireTeamRole(c, teamID, true); err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := m.liveTeam(ctx, teamID, errTeamNotFound); err != nil {
		return err
	}
	userID, _ := auth.Identity(c)
	body, err := wire.Bind[nameBody](c)
	if err != nil {
		return err
	}
	name := swiftstr.TrimWhitespacesAndNewlines(body.Name)
	if !validName(name) {
		return errVaultName
	}
	now := m.now()
	vaultID := uuid.New()
	if err := m.q.CreateVault(ctx, db.CreateVaultParams{ID: vaultID, TeamID: &teamID, Name: name, CreatedAt: &now}); err != nil {
		return err
	}
	now = m.now()
	ms := db.VaultMembership{ID: uuid.New(), VaultID: vaultID, UserID: userID, Role: "admin", CanUseAi: true, CreatedByUserID: userID}
	if err := m.q.CreateVaultMembership(ctx, db.CreateVaultMembershipParams{
		ID: ms.ID, VaultID: vaultID, UserID: userID, Role: ms.Role, CanUseAi: true, CreatedByUserID: userID, CreatedAt: &now,
	}); err != nil {
		return err
	}
	if err := m.paths.EnsureTenantDirectories(vaultID); err != nil {
		return err
	}
	if err := m.record(ctx, vaultID, userID, "vault.created", "vault", &vaultID, &name); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, vaultSummary(db.Vault{ID: vaultID, Name: name}, &teamID, false, ms))
}

// MARK: - Members

func (m *Module) listMembers(c *echo.Context) error {
	vaultID, err := uuidParam(c, "vaultID")
	if err != nil {
		return err
	}
	userID, err := m.resolve(c, vaultID, vaultaccess.Read)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	_, adminErr := m.access.ResolveVault(ctx, userID, vaultID, vaultaccess.Admin)
	maySeeEmail := adminErr == nil
	memberships, err := m.q.VaultMembershipsByVault(ctx, vaultID)
	if err != nil {
		return err
	}
	out := make([]VaultMember, 0, len(memberships))
	for _, ms := range memberships {
		u, err := m.q.GetUser(ctx, ms.UserID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		email := ""
		if maySeeEmail {
			email = u.Email
		}
		out = append(out, vaultMember(ms, u.Username, email))
	}
	return wire.JSON(c, http.StatusOK, out)
}

// membershipBody is Swift's MembershipBody; field order is decode order.
type membershipBody struct {
	Role     string `json:"role"`
	CanUseAI bool   `json:"canUseAI"`
}

func (m *Module) updateMember(c *echo.Context) error {
	vaultID, err := uuidParam(c, "vaultID")
	if err != nil {
		return err
	}
	actor, err := m.resolve(c, vaultID, vaultaccess.Admin)
	if err != nil {
		return err
	}
	targetID, err := uuidParam(c, "userID")
	if err != nil {
		return err
	}
	body, err := wire.Bind[membershipBody](c)
	if err != nil {
		return err
	}
	if !slices.Contains(vaultRoles, body.Role) {
		return errBadVaultRole
	}
	ctx := c.Request().Context()
	ms, err := m.q.GetVaultMembership(ctx, db.GetVaultMembershipParams{VaultID: vaultID, UserID: targetID})
	if errors.Is(err, pgx.ErrNoRows) {
		return errMemberNotFound
	}
	if err != nil {
		return err
	}
	target, err := m.q.GetUser(ctx, targetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errMemberNotFound
	}
	if err != nil {
		return err
	}
	now := m.now()
	if err := m.q.UpdateVaultMembership(ctx, db.UpdateVaultMembershipParams{
		ID: ms.ID, Role: body.Role, CanUseAi: body.CanUseAI, UpdatedAt: &now,
	}); err != nil {
		return err
	}
	ms.Role, ms.CanUseAi = body.Role, body.CanUseAI
	if err := m.record(ctx, vaultID, actor, "membership.updated", "member", &targetID, &target.Username); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, vaultMember(ms, target.Username, target.Email))
}

func (m *Module) removeMember(c *echo.Context) error {
	vaultID, err := uuidParam(c, "vaultID")
	if err != nil {
		return err
	}
	actor, err := m.resolve(c, vaultID, vaultaccess.Admin)
	if err != nil {
		return err
	}
	targetID, err := uuidParam(c, "userID")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	// Any failure here is the owner conflict in Swift, a personal vault included.
	v, err := m.q.GetVault(ctx, vaultID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && v.TeamID == nil) {
		return errRemoveOwner
	}
	if err != nil {
		return err
	}
	team, err := m.q.GetTeam(ctx, *v.TeamID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && team.OwnerUserID == targetID) {
		return errRemoveOwner
	}
	if err != nil {
		return err
	}
	ms, err := m.q.GetVaultMembership(ctx, db.GetVaultMembershipParams{VaultID: vaultID, UserID: targetID})
	if errors.Is(err, pgx.ErrNoRows) {
		return errMemberNotFound
	}
	if err != nil {
		return err
	}
	targetName := "Former member"
	if u, err := m.q.GetUser(ctx, targetID); err == nil {
		targetName = u.Username
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err := m.q.DeleteVaultMembership(ctx, ms.ID); err != nil {
		return err
	}
	if err := m.record(ctx, vaultID, actor, "membership.removed", "member", &targetID, &targetName); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// MARK: - Invitations

// inviteGrant is Swift's InviteGrant (role, then canUseAI: decode and encode order).
type inviteGrant struct {
	Role     string `json:"role"`
	CanUseAI bool   `json:"canUseAI"`
}

type inviteBody struct {
	Email       string                 `json:"email"`
	VaultGrants map[string]inviteGrant `json:"vaultGrants"`
}

func (m *Module) invite(c *echo.Context) error {
	teamID, err := uuidParam(c, "teamID")
	if err != nil {
		return err
	}
	if _, err := m.requireTeamRole(c, teamID, true); err != nil {
		return err
	}
	ctx := c.Request().Context()
	team, err := m.q.GetTeam(ctx, teamID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && team.ArchivedAt != nil) {
		return errTeamNotFound
	}
	if err != nil {
		return err
	}
	actorID, _ := auth.Identity(c)
	body, err := wire.Bind[inviteBody](c)
	if err != nil {
		return err
	}
	email := strings.ToLower(swiftstr.TrimWhitespacesAndNewlines(body.Email))
	if !strings.Contains(email, "@") || len(body.VaultGrants) == 0 {
		return errInviteFields
	}
	pending, err := m.q.PendingInvitationExists(ctx, db.PendingInvitationExistsParams{TeamID: teamID, Email: email, ExpiresAt: m.now()})
	if err != nil {
		return err
	}
	if pending {
		return errInvitePending
	}
	// Swift walks the dictionary in its (random) order; every failure is the
	// same 422, so the order is not observable.
	for raw, grant := range body.VaultGrants {
		vaultID, ok := parseUUID(raw)
		if !ok {
			return errInviteGrant
		}
		v, err := m.q.GetVault(ctx, vaultID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errInviteGrant
		}
		if err != nil {
			return err
		}
		if v.TeamID == nil || *v.TeamID != teamID || !slices.Contains(vaultRoles, grant.Role) {
			return errInviteGrant
		}
	}
	actor, err := m.q.GetUser(ctx, actorID)
	if err != nil {
		return err
	}
	grants, err := json.Marshal(body.VaultGrants) // keys sorted; Swift's order is per-process random
	if err != nil {
		return err
	}
	token := newToken()
	now := m.now()
	inv := db.TeamInvitation{ID: uuid.New(), TeamID: teamID, Email: email, ExpiresAt: now.Add(invitationTTL)}
	if err := m.q.CreateInvitation(ctx, db.CreateInvitationParams{
		ID: inv.ID, TeamID: teamID, Email: email, TokenHash: hashToken(token), VaultGrants: string(grants),
		InvitedByUserID: actorID, ExpiresAt: inv.ExpiresAt, CreatedAt: &now,
	}); err != nil {
		return err
	}
	if err := m.sender.Send(ctx, email, team.Name, actor.Username, token, inv.ExpiresAt); err != nil {
		_ = m.q.DeleteInvitation(context.WithoutCancel(ctx), inv.ID) // try? in Swift
		return err
	}
	return wire.JSON(c, http.StatusOK, invitation(inv))
}

func (m *Module) listInvitations(c *echo.Context) error {
	teamID, err := uuidParam(c, "teamID")
	if err != nil {
		return err
	}
	if _, err := m.requireTeamRole(c, teamID, true); err != nil {
		return err
	}
	rows, err := m.q.PendingInvitations(c.Request().Context(), teamID)
	if err != nil {
		return err
	}
	out := make([]TeamInvitation, 0, len(rows))
	for _, inv := range rows {
		out = append(out, invitation(inv))
	}
	return wire.JSON(c, http.StatusOK, out)
}

// pendingInvitation loads the team's invitation that is neither accepted nor revoked.
func (m *Module) pendingInvitation(c *echo.Context, teamID uuid.UUID) (db.TeamInvitation, error) {
	invitationID, err := uuidParam(c, "invitationID")
	if err != nil {
		return db.TeamInvitation{}, err
	}
	inv, err := m.q.GetInvitation(c.Request().Context(), invitationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.TeamInvitation{}, errPendingNotFound
	}
	if err != nil {
		return db.TeamInvitation{}, err
	}
	if inv.TeamID != teamID || inv.AcceptedAt != nil || inv.RevokedAt != nil {
		return db.TeamInvitation{}, errPendingNotFound
	}
	return inv, nil
}

func (m *Module) resendInvitation(c *echo.Context) error {
	teamID, err := uuidParam(c, "teamID")
	if err != nil {
		return err
	}
	if _, err := m.requireTeamRole(c, teamID, true); err != nil {
		return err
	}
	inv, err := m.pendingInvitation(c, teamID)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	team, err := m.q.GetTeam(ctx, teamID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && team.ArchivedAt != nil) {
		return errPendingNotFound
	}
	if err != nil {
		return err
	}
	actorID, _ := auth.Identity(c)
	actor, err := m.q.GetUser(ctx, actorID)
	if err != nil {
		return err
	}
	token := newToken()
	previousHash, previousExpiry := inv.TokenHash, inv.ExpiresAt
	inv.ExpiresAt = m.now().Add(invitationTTL)
	if err := m.q.SetInvitationToken(ctx, db.SetInvitationTokenParams{ID: inv.ID, TokenHash: hashToken(token), ExpiresAt: inv.ExpiresAt}); err != nil {
		return err
	}
	if err := m.sender.Send(ctx, inv.Email, team.Name, actor.Username, token, inv.ExpiresAt); err != nil {
		_ = m.q.SetInvitationToken(context.WithoutCancel(ctx), db.SetInvitationTokenParams{
			ID: inv.ID, TokenHash: previousHash, ExpiresAt: previousExpiry,
		})
		return errResendFailed
	}
	return wire.JSON(c, http.StatusOK, invitation(inv))
}

func (m *Module) revokeInvitation(c *echo.Context) error {
	teamID, err := uuidParam(c, "teamID")
	if err != nil {
		return err
	}
	if _, err := m.requireTeamRole(c, teamID, true); err != nil {
		return err
	}
	inv, err := m.pendingInvitation(c, teamID)
	if err != nil {
		return err
	}
	now := m.now()
	if err := m.q.RevokeInvitation(c.Request().Context(), db.RevokeInvitationParams{ID: inv.ID, RevokedAt: &now}); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (m *Module) acceptInvitation(c *echo.Context) error {
	token := c.Param("token")
	userID, err := requireIdentity(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	user, err := m.q.GetUser(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return wire.ErrUnauthenticated
	}
	if err != nil {
		return err
	}
	if !user.IsVerified {
		return errVerifyFirst
	}
	inv, err := m.q.InvitationByTokenHash(ctx, hashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return errInvitationInvalid
	}
	if err != nil {
		return err
	}
	if inv.AcceptedAt != nil || inv.RevokedAt != nil || !inv.ExpiresAt.After(m.now()) ||
		inv.Email != strings.ToLower(user.Email) {
		return errInvitationInvalid
	}
	team, err := m.q.GetTeam(ctx, inv.TeamID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && team.ArchivedAt != nil) {
		return errInvitationInvalid
	}
	if err != nil {
		return err
	}
	_, err = m.q.GetTeamMembership(ctx, db.GetTeamMembershipParams{TeamID: inv.TeamID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		now := m.now()
		err = m.q.CreateTeamMembership(ctx, db.CreateTeamMembershipParams{
			ID: uuid.New(), TeamID: inv.TeamID, UserID: userID, Role: "member", CreatedAt: &now,
		})
	}
	if err != nil {
		return err
	}
	var grants map[string]inviteGrant
	if err := json.Unmarshal([]byte(inv.VaultGrants), &grants); err != nil {
		return err
	}
	// Swift iterates the decoded dictionary in random order; Go uses key order.
	keys := make([]string, 0, len(grants))
	for k := range grants {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, raw := range keys {
		grant := grants[raw]
		vaultID, ok := parseUUID(raw)
		if !ok {
			continue
		}
		if err := m.grantSeat(ctx, vaultID, userID, grant, inv.InvitedByUserID); err != nil {
			return err
		}
		if err := m.record(ctx, vaultID, userID, "member.joined", "member", &userID, &user.Username); err != nil {
			return err
		}
	}
	now := m.now()
	if err := m.q.AcceptInvitation(ctx, db.AcceptInvitationParams{ID: inv.ID, AcceptedAt: &now}); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, teamSummary(team, "member", team.ArchivedAt))
}

// grantSeat updates the user's seat in vaultID or creates it.
func (m *Module) grantSeat(ctx context.Context, vaultID, userID uuid.UUID, grant inviteGrant, invitedBy uuid.UUID) error {
	existing, err := m.q.GetVaultMembership(ctx, db.GetVaultMembershipParams{VaultID: vaultID, UserID: userID})
	now := m.now()
	switch {
	case err == nil:
		return m.q.UpdateVaultMembership(ctx, db.UpdateVaultMembershipParams{
			ID: existing.ID, Role: grant.Role, CanUseAi: grant.CanUseAI, UpdatedAt: &now,
		})
	case errors.Is(err, pgx.ErrNoRows):
		return m.q.CreateVaultMembership(ctx, db.CreateVaultMembershipParams{
			ID: uuid.New(), VaultID: vaultID, UserID: userID, Role: grant.Role, CanUseAi: grant.CanUseAI,
			CreatedByUserID: invitedBy, CreatedAt: &now,
		})
	default:
		return err
	}
}

// MARK: - Activity

func (m *Module) listActivity(c *echo.Context) error {
	vaultID, err := uuidParam(c, "vaultID")
	if err != nil {
		return err
	}
	if _, err := m.resolve(c, vaultID, vaultaccess.Read); err != nil {
		return err
	}
	rows, err := m.q.RecentActivity(c.Request().Context(), vaultID)
	if err != nil {
		return err
	}
	out := make([]vaultactivity.Activity, 0, len(rows))
	for _, r := range rows {
		out = append(out, vaultactivity.Activity{
			ID:          wire.UUID{UUID: r.ID},
			VaultID:     wire.UUID{UUID: vaultID},
			ActorUserID: optUUID(r.ActorUserID),
			ActorName:   r.ActorName,
			Action:      r.Action,
			TargetType:  r.TargetType,
			TargetID:    optUUID(r.TargetID),
			TargetTitle: r.TargetTitle,
			CreatedAt:   wire.Time{Time: r.CreatedAt},
		})
	}
	return wire.JSON(c, http.StatusOK, out)
}

// MARK: - Helpers

// requireTeamRole is Swift's requireTeamRole: identity (401), then a team
// membership, with owner or admin when admin is set (403 otherwise).
func (m *Module) requireTeamRole(c *echo.Context, teamID uuid.UUID, admin bool) (db.TeamMembership, error) {
	userID, err := requireIdentity(c)
	if err != nil {
		return db.TeamMembership{}, err
	}
	ms, err := m.q.GetTeamMembership(c.Request().Context(), db.GetTeamMembershipParams{TeamID: teamID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.TeamMembership{}, errTeamAccess
	}
	if err != nil {
		return db.TeamMembership{}, err
	}
	if admin && ms.Role != "owner" && ms.Role != "admin" {
		return db.TeamMembership{}, errTeamAccess
	}
	return ms, nil
}

// resolve is VaultAccessService.resolve(vaultID:): identity first, then the
// path's vault (X-Vault-ID plays no part). It returns the caller.
func (m *Module) resolve(c *echo.Context, vaultID uuid.UUID, perm vaultaccess.Permission) (uuid.UUID, error) {
	userID, err := requireIdentity(c)
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := m.access.ResolveVault(c.Request().Context(), userID, vaultID, perm); err != nil {
		return uuid.Nil, err
	}
	return userID, nil
}

// liveTeam fails with notFound unless the team exists and is not archived.
func (m *Module) liveTeam(ctx context.Context, teamID uuid.UUID, notFound error) error {
	team, err := m.q.GetTeam(ctx, teamID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && team.ArchivedAt != nil) {
		return notFound
	}
	return err
}

func (m *Module) record(ctx context.Context, vaultID, actor uuid.UUID, action, targetType string, targetID *uuid.UUID, title *string) error {
	return m.activity.Record(ctx, vaultactivity.Event{
		VaultID: vaultID, Actor: actor, Action: action, TargetType: targetType, TargetID: targetID, TargetTitle: title,
	})
}

func requireIdentity(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, wire.ErrUnauthenticated
	}
	return id, nil
}

// uuidParam is Swift's uuidParameter: UUID(uuidString:) or 400 "invalid <name>".
func uuidParam(c *echo.Context, name string) (uuid.UUID, error) {
	id, ok := parseUUID(c.Param(name))
	if !ok {
		return uuid.Nil, wire.NewError(http.StatusBadRequest, "invalid "+name)
	}
	return id, nil
}

// parseUUID accepts what Foundation's UUID(uuidString:) accepts: the 36-character form.
func parseUUID(s string) (uuid.UUID, bool) {
	if len(s) != 36 {
		return uuid.Nil, false
	}
	id, err := ids.Parse(s)
	return id, err == nil
}

// validName is Swift's (2 ... 80).contains(name.count), counting characters.
func validName(name string) bool {
	n := swiftstr.Count(name)
	return n >= 2 && n <= 80
}

// newToken is UUID().uuidString + UUID().uuidString.
func newToken() string { return ids.Upper(uuid.New()) + ids.Upper(uuid.New()) }

// hashToken is the lowercase hex SHA-256 Swift stores in token_hash.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// caseInsensitiveLess approximates String.localizedCaseInsensitiveCompare ==
// .orderedAscending by comparing simple-lowercased names. It agrees with
// Foundation for letters, digits and spaces; ICU collation can order some
// punctuation and accented letters differently.
func caseInsensitiveLess(a, b string) bool {
	return strings.ToLower(a) < strings.ToLower(b)
}

func teamSummary(t db.Team, role string, archivedAt *time.Time) TeamSummary {
	return TeamSummary{Id: wire.UUID{UUID: t.ID}, Name: t.Name, Role: TeamSummaryRole(role), ArchivedAt: optTime(archivedAt)}
}

func vaultSummary(v db.Vault, teamID *uuid.UUID, personal bool, ms db.VaultMembership) VaultSummary {
	return VaultSummary{
		Id: wire.UUID{UUID: v.ID}, TeamID: optUUID(teamID), Name: v.Name, IsPersonal: personal,
		Role: VaultSummaryRole(ms.Role), CanUseAI: ms.CanUseAi, ArchivedAt: optTime(v.ArchivedAt),
	}
}

func vaultMember(ms db.VaultMembership, username, email string) VaultMember {
	return VaultMember{
		Id: wire.UUID{UUID: ms.ID}, UserID: wire.UUID{UUID: ms.UserID}, Username: username, Email: email,
		Role: VaultMemberRole(ms.Role), CanUseAI: ms.CanUseAi,
	}
}

func invitation(inv db.TeamInvitation) TeamInvitation {
	return TeamInvitation{
		Id: wire.UUID{UUID: inv.ID}, TeamID: wire.UUID{UUID: inv.TeamID}, Email: inv.Email,
		ExpiresAt: wire.Time{Time: inv.ExpiresAt},
	}
}

func optUUID(id *uuid.UUID) *wire.UUID {
	if id == nil {
		return nil
	}
	return &wire.UUID{UUID: *id}
}

func optTime(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}
