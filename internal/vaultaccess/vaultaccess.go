// Package vaultaccess resolves which vault a request acts on and what the
// caller may do there: the Go port of LuminaVaultServer
// Sources/App/Teams/VaultAccessService.swift. Requests name a team vault with
// the X-Vault-ID header; without it they act on the caller's personal vault,
// whose id is the user id.
package vaultaccess

import (
	"context"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/ids"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Header names a team vault. Absent means the caller's personal vault.
const Header = "X-Vault-ID"

type Permission int

const (
	Read Permission = iota
	Write
	Admin
	// AI needs can_use_ai and a billing sponsor entitled to chat. It does not
	// imply Write.
	AI
)

var (
	errBadHeader     = wire.NewError(http.StatusBadRequest, "invalid X-Vault-ID")
	errVaultNotFound = wire.NewError(http.StatusNotFound, "vault not found")
	errTeamNotFound  = wire.NewError(http.StatusNotFound, "team vault not found")
	errDenied        = wire.NewError(http.StatusForbidden, "vault access denied")
	errWrite         = wire.NewError(http.StatusForbidden, "vault write permission required")
	errAdmin         = wire.NewError(http.StatusForbidden, "vault admin permission required")
	errAI            = wire.NewError(http.StatusForbidden, "vault AI access required")
	errAIPayment     = wire.NewError(http.StatusPaymentRequired, "team owner subscription does not include AI access")
)

// Access is Swift's ResolvedVaultAccess.
type Access struct {
	VaultID              uuid.UUID
	TeamID               *uuid.UUID
	BillingSponsorUserID uuid.UUID
	Role                 string
	CanUseAI             bool
	IsPersonal           bool
}

func (a Access) CanWrite() bool { return a.Role == "editor" || a.Role == "admin" }
func (a Access) CanAdmin() bool { return a.Role == "admin" }

type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Resolve reads X-Vault-ID (or falls back to the caller's personal vault) and
// checks perm, with Swift's status codes and messages. As in Swift, a
// malformed header is a 400 before the caller's identity is required, so
// routes using it sit behind auth.Identify, not auth.Middleware.
func (s *Service) Resolve(c *echo.Context, perm Permission) (Access, error) {
	var requested *uuid.UUID
	if raw, ok := c.Request().Header[http.CanonicalHeaderKey(Header)]; ok && len(raw) > 0 {
		id, err := ids.Parse(raw[0])
		if err != nil || len(raw[0]) != 36 {
			return Access{}, errBadHeader
		}
		requested = &id
	}
	userID, ok := auth.Identity(c)
	if !ok {
		return Access{}, wire.ErrUnauthenticated
	}
	if requested == nil {
		requested = &userID
	}
	return s.ResolveVault(c.Request().Context(), userID, *requested, perm)
}

// ResolveVault checks userID's access to vaultID.
func (s *Service) ResolveVault(ctx context.Context, userID, vaultID uuid.UUID, perm Permission) (Access, error) {
	var (
		teamID     *uuid.UUID
		owner      *uuid.UUID
		archivedAt *time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT team_id, personal_owner_user_id, archived_at FROM vaults WHERE id = $1`, vaultID,
	).Scan(&teamID, &owner, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && archivedAt != nil) {
		return Access{}, errVaultNotFound
	}
	if err != nil {
		return Access{}, err
	}

	var access Access
	if owner != nil && *owner == userID {
		access = Access{VaultID: vaultID, BillingSponsorUserID: userID, Role: "admin", CanUseAI: true, IsPersonal: true}
	} else {
		var role string
		var canUseAI bool
		err := s.pool.QueryRow(ctx,
			`SELECT role, can_use_ai FROM vault_memberships WHERE vault_id = $1 AND user_id = $2 LIMIT 1`, vaultID, userID,
		).Scan(&role, &canUseAI)
		if errors.Is(err, pgx.ErrNoRows) {
			return Access{}, errDenied
		}
		if err != nil {
			return Access{}, err
		}
		if teamID == nil {
			return Access{}, errTeamNotFound
		}
		var sponsor uuid.UUID
		var teamArchived *time.Time
		err = s.pool.QueryRow(ctx,
			`SELECT billing_sponsor_user_id, archived_at FROM teams WHERE id = $1`, *teamID,
		).Scan(&sponsor, &teamArchived)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && teamArchived != nil) {
			return Access{}, errTeamNotFound
		}
		if err != nil {
			return Access{}, err
		}
		access = Access{VaultID: vaultID, TeamID: teamID, BillingSponsorUserID: sponsor, Role: role, CanUseAI: canUseAI}
	}

	switch {
	case perm == Write && !access.CanWrite():
		return Access{}, errWrite
	case perm == Admin && !access.CanAdmin():
		return Access{}, errAdmin
	case perm == AI && !access.CanUseAI:
		return Access{}, errAI
	case perm == AI:
		entitled, err := s.chatEntitled(ctx, access.BillingSponsorUserID)
		if err != nil {
			return Access{}, err
		}
		if !entitled {
			return Access{}, errAIPayment
		}
	}
	return access, nil
}

// chatEntitled is User.entitled(for: .chat): the effective tier is not
// archived. A pro or ultimate tier_override lifts any tier; an unknown tier
// falls back to lapsed, which is entitled (Billing/EntitlementChecker.swift).
// A missing sponsor row is not entitled.
func (s *Service) chatEntitled(ctx context.Context, userID uuid.UUID) (bool, error) {
	var tier, override string
	err := s.pool.QueryRow(ctx, `SELECT tier, tier_override FROM users WHERE id = $1`, userID).Scan(&tier, &override)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return tier != "archived" || override == "pro" || override == "ultimate", nil
}
