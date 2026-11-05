package ingestion

import (
	"context"
	"lumina/internal/modules/ingestion/db"
	"lumina/internal/sse"
	"lumina/internal/wire"

	"github.com/google/uuid"
)

// eventSource is what IngestionSSEResponse polls. Each call re-reads the batch, so
// a batch deleted mid-stream fails the poll, and the stream drops as in Swift.
type eventSource struct {
	m      *Module
	tenant uuid.UUID
	batch  uuid.UUID
}

// Events is MultimodalIngestionService.events. A row with a type the enum doesn't
// know is dropped, and doesn't advance the cursor unless a later one does.
func (s eventSource) Events(ctx context.Context, after int64) ([]sse.Event, error) {
	if _, err := s.m.requireBatch(ctx, s.tenant, s.batch); err != nil {
		return nil, err
	}
	rows, err := s.m.q.ListEvents(ctx, db.ListEventsParams{TenantID: s.tenant, BatchID: s.batch, ID: after})
	if err != nil {
		return nil, err
	}
	out := make([]sse.Event, 0, len(rows))
	for _, r := range rows {
		if !eventType(r.Type) {
			continue
		}
		var state *string
		if r.State != nil {
			if known, ok := itemState(*r.State); ok {
				state = &known
			}
		}
		out = append(out, sse.Event{ID: r.ID, Data: eventDTO{
			ID: r.ID, BatchID: wire.UUID{UUID: r.BatchID}, ItemID: wireUUID(r.ItemID), Type: r.Type,
			State: state, UploadedBytes: r.UploadedBytes, CreatedAt: wire.Time{Time: r.CreatedAt},
		}})
	}
	return out, nil
}

// Terminal is MultimodalIngestionService.isTerminal.
func (s eventSource) Terminal(ctx context.Context) (bool, error) {
	b, err := s.m.requireBatch(ctx, s.tenant, s.batch)
	if err != nil {
		return false, err
	}
	return b.State == "completed" || b.State == "attention", nil
}
