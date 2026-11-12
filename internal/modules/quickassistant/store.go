// Package quickassistant is the Go half of Quick Assistant: actions the
// assistant proposes wait here for the user's Confirm.
package quickassistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"lumina/internal/modules/quickassistant/db"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PendingAction is a proposed action waiting for Confirm. VaultID is the vault
// it was proposed against; confirm acts on it, not on the request's vault.
type PendingAction struct {
	ID, TenantID, VaultID, ConversationID uuid.UUID
	Tool                                  string
	Args                                  map[string]any
	Summary                               string
	ExpiresAt                             time.Time
}

// ClaimedAction is what a claimed row held, read in the statement that claimed
// it. The row itself has already been cleared.
type ClaimedAction struct {
	Tool           string
	VaultID        uuid.UUID
	ConversationID uuid.UUID
	Args           map[string]any
}

// Store is the pending-action table. Every transition out of pending is one
// conditional statement, so two simultaneous taps cannot both win, and it also
// clears args and summary.
type Store struct {
	q    *db.Queries
	pool *pgxpool.Pool
}

// NewStore returns a Store over pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{q: db.New(pool), pool: pool}
}

// Insert stores a new pending action.
func (s *Store) Insert(ctx context.Context, a PendingAction) error {
	args := a.Args
	if args == nil {
		args = map[string]any{}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("pending action args: %w", err)
	}
	return s.q.InsertPendingAction(ctx, db.InsertPendingActionParams{
		ID: a.ID, TenantID: a.TenantID, VaultID: a.VaultID, ConversationID: a.ConversationID,
		Tool: a.Tool, Args: raw, Summary: a.Summary, ExpiresAt: a.ExpiresAt,
	})
}

// Claim moves a live pending action to confirmed, once. Nil, nil when there is
// no such action for this tenant, it is no longer pending, or it expired.
func (s *Store) Claim(ctx context.Context, id, tenant uuid.UUID) (*ClaimedAction, error) {
	row, err := s.q.ClaimPendingAction(ctx, db.ClaimPendingActionParams{ID: id, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	args := map[string]any{}
	if err := json.Unmarshal(row.ClaimedArgs, &args); err != nil || args == nil {
		args = map[string]any{} // unreadable args behave as none, as Swift's `?? [:]`
	}
	return &ClaimedAction{Tool: row.Tool, VaultID: row.VaultID, ConversationID: row.ConversationID, Args: args}, nil
}

// Cancel moves a pending action to cancelled and returns its conversation. Nil
// when it was not pending (or is not this tenant's).
func (s *Store) Cancel(ctx context.Context, id, tenant uuid.UUID) (conversation *uuid.UUID, err error) {
	c, err := s.q.CancelPendingAction(ctx, db.CancelPendingActionParams{ID: id, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// MarkFailed moves a confirmed action whose execution did not happen or did not
// work to failed. Any other status is left alone.
func (s *Store) MarkFailed(ctx context.Context, id uuid.UUID) error {
	return s.q.MarkPendingActionFailed(ctx, id)
}

// DeleteLongExpired deletes this tenant's actions that expired over a day ago,
// whatever their status. Run on each proposal, so no sweeper is needed.
func (s *Store) DeleteLongExpired(ctx context.Context, tenant uuid.UUID) error {
	return s.q.DeleteLongExpiredPendingActions(ctx, tenant)
}

// Status is the action's status; false when it does not exist for this tenant.
func (s *Store) Status(ctx context.Context, id, tenant uuid.UUID) (string, bool, error) {
	st, err := s.q.PendingActionStatus(ctx, db.PendingActionStatusParams{ID: id, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return st, true, nil
}

// Ready reports whether the pending-action table exists. An error means the
// database could not say, which is not the same as a missing table.
func Ready(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.assistant_pending_actions') IS NOT NULL`).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

// ActiveSpaceName is the name of the space c names, if it is a space in vault.
// The id is client input, so a space from anywhere else, or any failure,
// reads as no space (Swift's try?).
func ActiveSpaceName(ctx context.Context, pool *pgxpool.Pool, vault uuid.UUID, c Context) *string {
	if c.ActiveSpaceID == nil {
		return nil
	}
	name, err := db.New(pool).SpaceName(ctx, db.SpaceNameParams{VaultID: vault, ID: c.ActiveSpaceID.UUID})
	if err != nil {
		return nil
	}
	return &name
}
