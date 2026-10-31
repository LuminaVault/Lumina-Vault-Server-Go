// Package vaultactivity records vault activity rows and fans them out to the
// live SSE subscribers of /v1/vaults/{id}/activity/stream: the Go port of
// LuminaVaultServer Sources/App/Teams/VaultActivityPublisher.swift
// (VaultActivityRecorder.record and the VaultActivityPublisher actor).
//
// Like Swift's publisher, the Hub is in-process only. An event recorded by the
// Swift server never reaches a stream served by Go, and the other way round;
// clients see the other server's writes on their next GET .../activity.
// Closing that gap needs a cross-process bridge (Postgres LISTEN/NOTIFY on
// vault_activity_events, in both servers) before Spaces, Kanban or Teams flips
// (wave-3-plan.md, coupling.md "Vault activity → SSE").
package vaultactivity

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/wire"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Activity is Swift's ActivityResponse, the body of GET .../activity rows
// and of each SSE frame. Fields are in Swift's encoding order; nil optionals
// are omitted as Swift's JSONEncoder omits them.
type Activity struct {
	ID          wire.UUID  `json:"id"`
	VaultID     wire.UUID  `json:"vaultID"`
	ActorUserID *wire.UUID `json:"actorUserID,omitempty"`
	ActorName   string     `json:"actorName"`
	Action      string     `json:"action"`
	TargetType  string     `json:"targetType"`
	TargetID    *wire.UUID `json:"targetID,omitempty"`
	TargetTitle *string    `json:"targetTitle,omitempty"`
	CreatedAt   wire.Time  `json:"createdAt"`
}

// Default is the process's hub. Swift builds one VaultActivityPublisher in
// App+build.swift and hands it to every recorder and to the stream route;
// every Recorder made with New publishes here, so writes from Spaces and
// Kanban reach Teams' stream without wiring.
var Default = NewHub()

// subscriberBuffer bounds each stream's queue. Swift's AsyncStream is
// unbounded; a client this far behind loses events (logged) rather than
// growing memory without limit.
const subscriberBuffer = 256

// Hub fans events out to subscribers by vault id.
type Hub struct {
	mu   sync.Mutex
	subs map[uuid.UUID]map[chan Activity]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[uuid.UUID]map[chan Activity]struct{}{}} }

// Subscribe returns the vault's event channel and a cancel func that must be
// called when the stream ends.
func (h *Hub) Subscribe(vaultID uuid.UUID) (<-chan Activity, func()) {
	ch := make(chan Activity, subscriberBuffer)
	h.mu.Lock()
	if h.subs[vaultID] == nil {
		h.subs[vaultID] = map[chan Activity]struct{}{}
	}
	h.subs[vaultID][ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs[vaultID], ch)
			if len(h.subs[vaultID]) == 0 {
				delete(h.subs, vaultID)
			}
			h.mu.Unlock()
		})
	}
}

// Publish delivers a to every subscriber of its vault without blocking.
func (h *Hub) Publish(a Activity) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[a.VaultID.UUID] {
		select {
		case ch <- a:
		default:
			slog.Warn("vault activity subscriber is full; event dropped", "vault", a.VaultID.String(), "event", a.ID.String())
		}
	}
}

// Subscribers reports how many streams follow vaultID (tests).
func (h *Hub) Subscribers(vaultID uuid.UUID) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[vaultID])
}

type Recorder struct {
	pool *pgxpool.Pool
	hub  *Hub
	now  func() time.Time
}

// New records rows and publishes them on Default.
func New(pool *pgxpool.Pool) *Recorder { return NewWithHub(pool, Default) }

// NewWithHub publishes on hub instead of Default (tests).
func NewWithHub(pool *pgxpool.Pool, hub *Hub) *Recorder {
	return &Recorder{pool: pool, hub: hub, now: time.Now}
}

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
// does from the authenticated User, then publishes it to the vault's live
// streams. A missing actor row inserts and publishes nothing.
func (r *Recorder) Record(ctx context.Context, e Event) error {
	var (
		id        uuid.UUID
		actorID   *uuid.UUID
		actorName string
		createdAt time.Time
	)
	err := r.pool.QueryRow(ctx, `
		INSERT INTO vault_activity_events
		  (id, vault_id, actor_user_id, actor_name, action, target_type, target_id, target_title, created_at)
		SELECT $1, $2, u.id, u.username, $4, $5, $6, $7, $8 FROM users u WHERE u.id = $3
		RETURNING id, actor_user_id, actor_name, created_at`,
		uuid.New(), e.VaultID, e.Actor, e.Action, e.TargetType, e.TargetID, e.TargetTitle, r.now(),
	).Scan(&id, &actorID, &actorName, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	r.hub.Publish(Activity{
		ID:          wire.UUID{UUID: id},
		VaultID:     wire.UUID{UUID: e.VaultID},
		ActorUserID: optUUID(actorID),
		ActorName:   actorName,
		Action:      e.Action,
		TargetType:  e.TargetType,
		TargetID:    optUUID(e.TargetID),
		TargetTitle: e.TargetTitle,
		CreatedAt:   wire.Time{Time: createdAt},
	})
	return nil
}

func optUUID(id *uuid.UUID) *wire.UUID {
	if id == nil {
		return nil
	}
	return &wire.UUID{UUID: *id}
}
