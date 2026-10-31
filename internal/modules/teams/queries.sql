-- Teams (Swift: Sources/App/Teams/TeamController.swift). Each query is the one
-- Fluent issues for the controller's model calls, in the same shape: lists
-- without ORDER BY stay without it, so both servers return heap order.

-- name: ExpiredArchivedTeams :many
SELECT id FROM teams WHERE archived_at < $1;

-- name: DeleteTeam :exec
DELETE FROM teams WHERE id = $1;

-- name: GetTeam :one
SELECT * FROM teams WHERE id = $1 LIMIT 1;

-- name: CreateTeam :exec
INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at)
VALUES ($1, $2, $3, $3, $4, $4);

-- name: UpdateTeamOwner :exec
UPDATE teams SET owner_user_id = $2, billing_sponsor_user_id = $2, updated_at = $3 WHERE id = $1;

-- name: SetTeamArchived :exec
UPDATE teams SET archived_at = $2, updated_at = $3 WHERE id = $1;

-- name: TeamMembershipsByUser :many
SELECT * FROM team_memberships WHERE user_id = $1;

-- name: GetTeamMembership :one
SELECT * FROM team_memberships WHERE team_id = $1 AND user_id = $2 LIMIT 1;

-- name: CreateTeamMembership :exec
INSERT INTO team_memberships (id, team_id, user_id, role, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $5);

-- name: UpdateTeamMembershipRole :exec
UPDATE team_memberships SET role = $2, updated_at = $3 WHERE id = $1;

-- name: DeleteTeamMembership :exec
DELETE FROM team_memberships WHERE id = $1;

-- name: GetVault :one
SELECT * FROM vaults WHERE id = $1 LIMIT 1;

-- name: VaultsByTeam :many
SELECT * FROM vaults WHERE team_id = $1;

-- name: CreateVault :exec
INSERT INTO vaults (id, team_id, name, created_at, updated_at) VALUES ($1, $2, $3, $4, $4);

-- name: SetVaultArchived :exec
UPDATE vaults SET archived_at = $2, updated_at = $3 WHERE id = $1;

-- name: VaultMembershipsByUser :many
SELECT * FROM vault_memberships WHERE user_id = $1;

-- name: VaultMembershipsByVault :many
SELECT * FROM vault_memberships WHERE vault_id = $1;

-- name: GetVaultMembership :one
SELECT * FROM vault_memberships WHERE vault_id = $1 AND user_id = $2 LIMIT 1;

-- name: CreateVaultMembership :exec
INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7);

-- name: UpdateVaultMembership :exec
UPDATE vault_memberships SET role = $2, can_use_ai = $3, updated_at = $4 WHERE id = $1;

-- name: DeleteVaultMembership :exec
DELETE FROM vault_memberships WHERE id = $1;

-- name: DeleteUserVaultMemberships :exec
DELETE FROM vault_memberships WHERE vault_id = $1 AND user_id = $2;

-- name: GetUser :one
SELECT id, email, username, is_verified FROM users WHERE id = $1 LIMIT 1;

-- name: PendingInvitationExists :one
SELECT EXISTS (
  SELECT 1 FROM team_invitations
  WHERE team_id = $1 AND email = $2 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > $3
);

-- name: CreateInvitation :exec
INSERT INTO team_invitations (id, team_id, email, token_hash, vault_grants, invited_by_user_id, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: DeleteInvitation :exec
DELETE FROM team_invitations WHERE id = $1;

-- name: PendingInvitations :many
SELECT * FROM team_invitations
WHERE team_id = $1 AND accepted_at IS NULL AND revoked_at IS NULL
ORDER BY expires_at DESC;

-- name: GetInvitation :one
SELECT * FROM team_invitations WHERE id = $1 LIMIT 1;

-- name: InvitationByTokenHash :one
SELECT * FROM team_invitations WHERE token_hash = $1 LIMIT 1;

-- name: SetInvitationToken :exec
UPDATE team_invitations SET token_hash = $2, expires_at = $3 WHERE id = $1;

-- name: RevokeInvitation :exec
UPDATE team_invitations SET revoked_at = $2 WHERE id = $1;

-- name: AcceptInvitation :exec
UPDATE team_invitations SET accepted_at = $2 WHERE id = $1;

-- name: RecentActivity :many
SELECT * FROM vault_activity_events WHERE vault_id = $1 ORDER BY created_at DESC LIMIT 100;
