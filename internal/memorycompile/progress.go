package memorycompile

import (
	"context"
	"encoding/json"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/wire"

	"github.com/google/uuid"
)

// Event is KBCompileProgressEvent on the wire: {"type":...,"payload":...}.
type Event struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

type runPayload struct {
	RunID wire.UUID `json:"runId"`
}

// Response is KBCompileResponse.
type Response struct {
	MemoriesIngested int         `json:"memoriesIngested"`
	MemoriesUpdated  *int        `json:"memoriesUpdated,omitempty"`
	DurationMs       *int        `json:"durationMs,omitempty"`
	RunID            wire.UUID   `json:"runId"`
	PendingMemoryIDs []wire.UUID `json:"pendingMemoryIds"`
}

// StartedEvent is .started.
func StartedEvent(run uuid.UUID, totalFiles int) Event {
	return Event{"started", struct {
		RunID      wire.UUID `json:"runId"`
		TotalFiles int       `json:"totalFiles"`
	}{wire.UUID{UUID: run}, totalFiles}}
}

// PreparingEvent is .preparing.
func PreparingEvent(run uuid.UUID) Event {
	return Event{"preparing", runPayload{wire.UUID{UUID: run}}}
}

// ThinkingEvent is .thinking.
func ThinkingEvent(run uuid.UUID, iteration int) Event {
	return Event{"thinking", struct {
		RunID     wire.UUID `json:"runId"`
		Iteration int       `json:"iteration"`
	}{wire.UUID{UUID: run}, iteration}}
}

type savedMemory struct {
	ID          wire.UUID  `json:"id"`
	Content     string     `json:"content"`
	Tags        []string   `json:"tags"`
	CreatedAt   *wire.Time `json:"createdAt,omitempty"`
	ReviewState string     `json:"reviewState"`
	Kind        *string    `json:"kind,omitempty"`
}

// MemorySavedEvent is .memorySaved, the frame iOS's review sheet fills from.
func MemorySavedEvent(run uuid.UUID, m *memorystore.Memory, kind *string) Event {
	dto := savedMemory{ID: wire.UUID{UUID: m.ID}, Content: m.Content, Tags: m.Tags, ReviewState: m.ReviewState, Kind: kind}
	if dto.Tags == nil {
		dto.Tags = []string{}
	}
	if m.CreatedAt != nil {
		dto.CreatedAt = &wire.Time{Time: *m.CreatedAt}
	}
	return Event{"memorySaved", struct {
		RunID  wire.UUID   `json:"runId"`
		Memory savedMemory `json:"memory"`
	}{wire.UUID{UUID: run}, dto}}
}

// CompletedEvent is .completed.
func CompletedEvent(run uuid.UUID, response Response) Event {
	return Event{"completed", struct {
		RunID    wire.UUID `json:"runId"`
		Response Response  `json:"response"`
	}{wire.UUID{UUID: run}, response}}
}

// ErrorEvent is .error.
func ErrorEvent(run uuid.UUID, message string) Event {
	return Event{"error", struct {
		RunID   wire.UUID `json:"runId"`
		Message string    `json:"message"`
	}{wire.UUID{UUID: run}, message}}
}

// Publisher is MemoryCompileProgressPublisher.
type Publisher interface {
	Publish(ctx context.Context, event Event, tenant uuid.UUID)
}

// NoopPublisher drops every event. It's what cmd/App wires until /v1/ws moves to Go:
// sockets are on Swift, so nothing here could reach one. That's why the kb-compile
// module flips with /v1/ws.
type NoopPublisher struct{}

// Publish does nothing.
func (NoopPublisher) Publish(context.Context, Event, uuid.UUID) {}

// BroadcastPublisher is WebSocketMemoryCompileProgressPublisher. Broadcast is
// ConnectionManager.broadcast, which arrives with /v1/ws. Encoding failures are logged
// and swallowed: a sick socket path must never break a compile.
type BroadcastPublisher struct {
	Broadcast func(ctx context.Context, tenantID, message string)
}

// Publish encodes the event and hands it to Broadcast.
func (p BroadcastPublisher) Publish(ctx context.Context, event Event, tenant uuid.UUID) {
	data, err := json.Marshal(event)
	if err != nil {
		slog.WarnContext(ctx, "kb-compile progress publish failed", "tenant_id", ids.Upper(tenant), "error", err)
		return
	}
	p.Broadcast(ctx, ids.Upper(tenant), string(data))
}
