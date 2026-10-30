// Package vaultactivity records vault activity rows: the DB half of
// LuminaVaultServer Sources/App/Teams/VaultActivityPublisher.swift
// (VaultActivityRecorder.record).
//
// Swift also pushes each event to in-process SSE subscribers of
// /v1/vaults/{id}/activity (Teams, wave 3). A row written here is not pushed
// to clients connected to the Swift server; they see it on their next fetch.
// coupling.md "Vault activity → SSE" is the flip blocker this leaves open.
package vaultactivity

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Recorder struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func New(pool *pgxpool.Pool) *Recorder { return &Recorder{pool: pool, now: time.Now} }

// Event is one activity row. Action and TargetType are Swift's strings
// ("created", "space", …).
type Event struct {
	VaultID     uuid.UUID
	Actor       uuid.UUID
	Action      string
	TargetType  string
	TargetID    *uuid.UUID
	TargetTitle *string
}

// Record inserts the row with the actor's username as actor_name, as Swift
// does from the authenticated User.
func (r *Recorder) Record(ctx context.Context, e Event) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO vault_activity_events
		  (id, vault_id, actor_user_id, actor_name, action, target_type, target_id, target_title, created_at)
		SELECT $1, $2, u.id, u.username, $4, $5, $6, $7, $8 FROM users u WHERE u.id = $3`,
		uuid.New(), e.VaultID, e.Actor, e.Action, e.TargetType, e.TargetID, e.TargetTitle, r.now())
	return err
}
