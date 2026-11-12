package hermesruns

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"lumina/internal/wire"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// referenceDateUnix is 2001-01-01T00:00:00Z, Foundation's reference date. Fluent
// writes a jsonb Date with JSONEncoder's default strategy: a Double of seconds
// since then.
const referenceDateUnix = 978307200

// refDate is a time that round-trips through jsonb as Fluent writes a Date.
type refDate struct{ time.Time }

func (d refDate) MarshalJSON() ([]byte, error) {
	secs := float64(d.Unix()-referenceDateUnix) + float64(d.Nanosecond())/1e9
	return wire.Float64(secs).MarshalJSON()
}

func (d *refDate) UnmarshalJSON(b []byte) error {
	var secs float64
	if err := json.Unmarshal(b, &secs); err != nil {
		return err
	}
	whole := math.Floor(secs)
	d.Time = time.Unix(int64(whole)+referenceDateUnix, int64(math.Round((secs-whole)*1e9))).UTC()
	return nil
}

// PendingApproval is HermesRunPendingApprovalDTO.
type PendingApproval struct {
	Command     *string
	Choices     []Choice
	RequestedAt time.Time
	Extra       map[string]any
}

type pendingStored struct {
	Command     *string        `json:"command,omitempty"`
	Choices     []Choice       `json:"choices"`
	RequestedAt refDate        `json:"requestedAt"`
	Extra       map[string]any `json:"extra,omitempty"`
}

// PendingApprovalDTO is the wire form of PendingApproval.
type PendingApprovalDTO struct {
	Command     *string        `json:"command,omitempty"`
	Choices     []Choice       `json:"choices"`
	RequestedAt wire.Time      `json:"requestedAt"`
	Extra       map[string]any `json:"extra,omitempty"`
}

// Run is a hermes_runs row (HermesRun).
type Run struct {
	ID              uuid.UUID
	Tenant          uuid.UUID
	HermesRunID     string
	Status          Status
	Prompt          string
	SessionID       *string
	Model           *string
	ConversationID  *uuid.UUID
	StartedAt       time.Time
	FinishedAt      *time.Time
	LastEvent       *string
	LastSeq         int
	PendingApproval *PendingApproval
	Summary         *string
	Error           *string
}

// DTO is HermesRunDTO, keys in its declaration order, nil optionals omitted.
type DTO struct {
	ID              wire.UUID           `json:"id"`
	HermesRunID     string              `json:"hermesRunID"`
	Status          Status              `json:"status"`
	Prompt          string              `json:"prompt"`
	SessionID       *string             `json:"sessionID,omitempty"`
	Model           *string             `json:"model,omitempty"`
	ConversationID  *wire.UUID          `json:"conversationID,omitempty"`
	StartedAt       wire.Time           `json:"startedAt"`
	FinishedAt      *wire.Time          `json:"finishedAt,omitempty"`
	LastEvent       *string             `json:"lastEvent,omitempty"`
	LastSeq         int                 `json:"lastSeq"`
	PendingApproval *PendingApprovalDTO `json:"pendingApproval,omitempty"`
	Summary         *string             `json:"summary,omitempty"`
	Error           *string             `json:"error,omitempty"`
}

// DTO is HermesRun.toDTO.
func (r *Run) DTO() DTO {
	d := DTO{
		ID: wire.UUID{UUID: r.ID}, HermesRunID: r.HermesRunID, Status: r.Status, Prompt: r.Prompt,
		SessionID: r.SessionID, Model: r.Model, StartedAt: wire.Time{Time: r.StartedAt},
		LastEvent: r.LastEvent, LastSeq: r.LastSeq, Summary: r.Summary, Error: r.Error,
	}
	if r.ConversationID != nil {
		d.ConversationID = &wire.UUID{UUID: *r.ConversationID}
	}
	if r.FinishedAt != nil {
		d.FinishedAt = &wire.Time{Time: *r.FinishedAt}
	}
	if p := r.PendingApproval; p != nil {
		d.PendingApproval = &PendingApprovalDTO{Command: p.Command, Choices: p.Choices, RequestedAt: wire.Time{Time: p.RequestedAt}, Extra: FoundationValue(p.Extra).(map[string]any)}
		if p.Extra == nil {
			d.PendingApproval.Extra = nil
		}
	}
	return d
}

// EventRow is a hermes_run_events row (HermesRunEventRow).
type EventRow struct {
	RunID   uuid.UUID
	Seq     int
	Event   string
	Payload map[string]any
	At      time.Time
}

// EventDTO is HermesRunEventDTO.
type EventDTO struct {
	RunID   wire.UUID `json:"runID"`
	Seq     int       `json:"seq"`
	Event   string    `json:"event"`
	Payload any       `json:"payload"`
	At      wire.Time `json:"at"`
}

// DTO is HermesRunEventRow.toDTO.
func (e EventRow) DTO() EventDTO {
	return EventDTO{RunID: wire.UUID{UUID: e.RunID}, Seq: e.Seq, Event: e.Event, Payload: FoundationValue(e.Payload), At: wire.Time{Time: e.At}}
}

// scalarPayloadKey is what a non-object payload is stored under.
const scalarPayloadKey = "value"

func objectPayload(payload any) map[string]any {
	if m, ok := payload.(map[string]any); ok {
		return m
	}
	return map[string]any{scalarPayloadKey: payload}
}

// FoundationValue rewrites every number in a decoded JSON value as wire.Float64,
// so it prints as AnyJSONValue.number(Double) does through JSONEncoder.
func FoundationValue(v any) any {
	switch t := v.(type) {
	case float64:
		return wire.Float64(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = FoundationValue(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = FoundationValue(e)
		}
		return out
	}
	return v
}

// Published is HermesRunEventPayload: what the in-process bus carries.
type Published struct {
	RunID  uuid.UUID
	Tenant uuid.UUID
	Seq    int
	Event  string
	Status Status
}

// Bus is the hermes_run_event slice of Swift's EventBus: in process only, and
// lossy under backpressure. Readers re-read the database on every wake.
type Bus struct {
	mu   sync.Mutex
	subs map[chan Published]struct{}
}

// busCapacity is EventBus.bufferCapacity: each subscriber buffers this many.
const busCapacity = 64

// NewBus returns an empty bus.
func NewBus() *Bus { return &Bus{subs: map[chan Published]struct{}{}} }

// Subscribe returns a channel of events and the function that ends it.
func (b *Bus) Subscribe() (<-chan Published, func()) {
	ch := make(chan Published, busCapacity)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Publish never blocks. A full subscriber drops its oldest queued event to make
// room, as Swift's AsyncStream .bufferingNewest(64) does.
func (b *Bus) Publish(p Published) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- p:
			continue
		default:
		}
		// Publishers hold mu, so only the reader can race us, and it only frees room.
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- p:
		default:
		}
	}
}

// SubscriberCount is EventBus.subscriberCount(for: .hermesRunEvent).
func (b *Bus) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Store is HermesRunStore.
type Store struct {
	Pool *pgxpool.Pool
	Bus  *Bus
	Log  *slog.Logger
	Now  func() time.Time
}

const runColumns = `id, tenant_id, hermes_run_id, status, prompt, session_id, model, conversation_id,
	started_at, finished_at, last_event, last_seq, pending_approval, summary, error`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	var status string
	var pending []byte
	if err := row.Scan(&r.ID, &r.Tenant, &r.HermesRunID, &status, &r.Prompt, &r.SessionID, &r.Model, &r.ConversationID,
		&r.StartedAt, &r.FinishedAt, &r.LastEvent, &r.LastSeq, &pending, &r.Summary, &r.Error); err != nil {
		return nil, err
	}
	r.Status = parseStatus(status)
	r.StartedAt = r.StartedAt.UTC()
	if r.FinishedAt != nil {
		t := r.FinishedAt.UTC()
		r.FinishedAt = &t
	}
	if len(pending) > 0 && string(pending) != "null" {
		var p pendingStored
		if err := json.Unmarshal(pending, &p); err != nil {
			return nil, err
		}
		r.PendingApproval = &PendingApproval{Command: p.Command, Choices: p.Choices, RequestedAt: p.RequestedAt.Time, Extra: p.Extra}
	}
	return &r, nil
}

func encodePending(p *PendingApproval) ([]byte, error) {
	if p == nil {
		return nil, nil
	}
	return json.Marshal(pendingStored{Command: p.Command, Choices: p.Choices, RequestedAt: refDate{p.RequestedAt}, Extra: p.Extra})
}

func one(r *Run, err error) (*Run, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// Find is find(tenantID:runID:); nil when the tenant has no such run.
func (s *Store) Find(ctx context.Context, tenant, runID uuid.UUID) (*Run, error) {
	return one(scanRun(s.Pool.QueryRow(ctx, `SELECT `+runColumns+` FROM hermes_runs WHERE tenant_id = $1 AND id = $2 LIMIT 1`, tenant, runID)))
}

// FindByID is find(runID:).
func (s *Store) FindByID(ctx context.Context, runID uuid.UUID) (*Run, error) {
	return one(scanRun(s.Pool.QueryRow(ctx, `SELECT `+runColumns+` FROM hermes_runs WHERE id = $1`, runID)))
}

// List is the tenant's newest runs, optionally narrowed to one conversation.
func (s *Store) List(ctx context.Context, tenant uuid.UUID, limit int, conversation *uuid.UUID) ([]*Run, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+runColumns+` FROM hermes_runs
		WHERE tenant_id = $1 AND ($2::uuid IS NULL OR conversation_id = $2)
		ORDER BY started_at DESC LIMIT $3`, tenant, conversation, limit)
	if err != nil {
		return nil, err
	}
	return collectRuns(rows)
}

// ActiveRuns is the re-attach set: non-terminal rows, oldest first.
func (s *Store) ActiveRuns(ctx context.Context) ([]*Run, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+runColumns+` FROM hermes_runs
		WHERE status IN ('queued', 'running', 'waiting_for_approval') ORDER BY started_at ASC`)
	if err != nil {
		return nil, err
	}
	return collectRuns(rows)
}

func collectRuns(rows pgx.Rows) ([]*Run, error) {
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Insert saves a new run.
func (s *Store) Insert(ctx context.Context, r *Run) error {
	pending, err := encodePending(r.PendingApproval)
	if err != nil {
		return err
	}
	now := s.Now()
	_, err = s.Pool.Exec(ctx, `INSERT INTO hermes_runs (`+runColumns+`, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $16)`,
		r.ID, r.Tenant, r.HermesRunID, string(r.Status), r.Prompt, r.SessionID, r.Model, r.ConversationID,
		r.StartedAt, r.FinishedAt, r.LastEvent, r.LastSeq, pending, r.Summary, r.Error, now)
	return err
}

// Events is the persisted events with seq > afterSeq, oldest first.
func (s *Store) Events(ctx context.Context, runID uuid.UUID, afterSeq, limit int) ([]EventRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT run_id, seq, event, payload, at FROM hermes_run_events
		WHERE run_id = $1 AND seq > $2 ORDER BY seq ASC LIMIT $3`, runID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		var payload []byte
		if err := rows.Scan(&e.RunID, &e.Seq, &e.Event, &payload, &e.At); err != nil {
			return nil, err
		}
		e.At = e.At.UTC()
		v, ok := decodeJSON(payload)
		if !ok {
			return nil, errors.New("hermes_run_events: payload is not JSON")
		}
		e.Payload = objectPayload(v)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Mutation is what a watcher's `mutate` closure sets on the run row. Only set
// columns are written, as Fluent writes only the fields a save changed.
type Mutation struct {
	Status          *Status
	SetPending      bool
	PendingApproval *PendingApproval
	Summary         **string
	Error           *string
	FinishedAt      *time.Time
}

// Append is HermesRunStore.append: one event, the run row bumped to its seq in
// the same step. It returns nil when the run row is gone.
func (s *Store) Append(ctx context.Context, runID uuid.UUID, name string, payload any, at time.Time, mutate func(*Run) Mutation) (*Run, error) {
	run, err := s.FindByID(ctx, runID)
	if err != nil || run == nil {
		return nil, err
	}
	seq := run.LastSeq + 1
	body, err := json.Marshal(objectPayload(payload))
	if err != nil {
		return nil, err
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO hermes_run_events (id, run_id, seq, event, payload, at) VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New(), runID, seq, name, body, at); err != nil {
		return nil, err
	}
	run.LastSeq = seq
	run.LastEvent = &name
	m := Mutation{}
	if mutate != nil {
		m = mutate(run)
	}
	if m.Status != nil {
		run.Status = *m.Status
	}
	if m.SetPending {
		run.PendingApproval = m.PendingApproval
	}
	if m.Summary != nil {
		run.Summary = *m.Summary
	}
	if m.Error != nil {
		run.Error = m.Error
	}
	if m.FinishedAt != nil {
		run.FinishedAt = m.FinishedAt
	}
	pending, err := encodePending(run.PendingApproval)
	if err != nil {
		return nil, err
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE hermes_runs SET
			last_seq = $2, last_event = $3,
			status = CASE WHEN $4 THEN $5 ELSE status END,
			pending_approval = CASE WHEN $6 THEN $7::jsonb ELSE pending_approval END,
			summary = CASE WHEN $8 THEN $9 ELSE summary END,
			error = CASE WHEN $10 THEN $11 ELSE error END,
			finished_at = CASE WHEN $12 THEN $13 ELSE finished_at END,
			updated_at = $14
		WHERE id = $1`,
		runID, seq, name,
		m.Status != nil, string(run.Status),
		m.SetPending, pending,
		m.Summary != nil, run.Summary,
		m.Error != nil, run.Error,
		m.FinishedAt != nil, run.FinishedAt,
		s.Now()); err != nil {
		return nil, err
	}
	s.Publish(run, seq, name)
	return run, nil
}

// ClearPendingApproval writes only pending_approval and status, so it can't
// race the watcher's row saves.
func (s *Store) ClearPendingApproval(ctx context.Context, runID uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `UPDATE hermes_runs SET pending_approval = NULL, status = 'running', updated_at = $2 WHERE id = $1`, runID, s.Now())
	return err
}

// Finish is a terminal transition with no Hermes event behind it: a synthetic
// watcher.<status> event so SSE consumers see the end.
func (s *Store) Finish(ctx context.Context, runID uuid.UUID, status Status, errText *string) (*Run, error) {
	finishedAt := s.Now()
	payload := map[string]any{"status": string(status), "error": nil}
	if errText != nil {
		payload["error"] = *errText
	}
	return s.Append(ctx, runID, "watcher."+string(status), payload, finishedAt, func(*Run) Mutation {
		m := Mutation{Status: &status, SetPending: true, FinishedAt: &finishedAt}
		if errText != nil {
			m.Error = errText
		}
		return m
	})
}

// Publish is HermesRunStore.publish.
func (s *Store) Publish(r *Run, seq int, event string) {
	if s.Bus == nil {
		return
	}
	s.Bus.Publish(Published{RunID: r.ID, Tenant: r.Tenant, Seq: seq, Event: event, Status: r.Status})
}
