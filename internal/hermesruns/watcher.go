package hermesruns

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Mode is HermesRunWatcher.Mode.
type Mode int

const (
	// ModeStream consumes GET /v1/runs/{id}/events (a fresh run).
	ModeStream Mode = iota
	// ModePoll polls GET /v1/runs/{id}: a re-attach, or a stream that dropped.
	// Hermes tears its event queue down when a subscriber leaves, so a second
	// /events call 404s.
	ModePoll
)

// WatcherConfig is HermesRunWatcher.Config.
type WatcherConfig struct {
	PollInterval time.Duration
	// MaxRunLifetime is the hard stop for a run Hermes never finishes.
	MaxRunLifetime time.Duration
}

// DefaultWatcherConfig is Swift's: 2 s polls, 4 h lifetime.
func DefaultWatcherConfig() WatcherConfig {
	return WatcherConfig{PollInterval: 2 * time.Second, MaxRunLifetime: 4 * time.Hour}
}

// Watcher is HermesRunWatcher: follows one Hermes run to its end. It runs on
// one goroutine, so its fields need no lock.
type Watcher struct {
	runID, tenant      uuid.UUID
	hermesRunID        string
	startedAt          time.Time
	client             *Client
	store              *Store
	notifier           Notifier
	config             WatcherConfig
	log                *slog.Logger
	mode               Mode
	status             Status
	hasPendingApproval bool
}

// NewWatcher builds a watcher for run.
func NewWatcher(run *Run, mode Mode, client *Client, store *Store, notifier Notifier, config WatcherConfig, log *slog.Logger) *Watcher {
	return &Watcher{
		runID: run.ID, tenant: run.Tenant, hermesRunID: run.HermesRunID, startedAt: run.StartedAt,
		client: client, store: store, notifier: notifier, config: config, log: log, mode: mode,
		status: run.Status, hasPendingApproval: run.PendingApproval != nil,
	}
}

// Status is the watcher's view of the run's status.
func (w *Watcher) Status() Status { return w.status }

// Run follows the run until it is terminal or ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	if w.mode == ModeStream {
		err := w.consumeStream(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			w.log.WarnContext(ctx, "hermes run stream ended early; falling back to polling", "run", w.runID, "error", redact(err.Error()))
		}
	}
	if !w.status.IsTerminal() && ctx.Err() == nil {
		w.poll(ctx)
	}
}

var errTerminal = errors.New("terminal")

func (w *Watcher) consumeStream(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	err := w.client.Events(ctx, w.hermesRunID, func(f Frame) {
		if ctx.Err() != nil {
			return
		}
		w.apply(ctx, f)
		if w.status.IsTerminal() {
			cancel()
		}
	})
	if w.status.IsTerminal() {
		return nil
	}
	if errors.Is(err, errTerminal) {
		return nil
	}
	return err
}

func (w *Watcher) poll(ctx context.Context) {
	for ctx.Err() == nil && !w.status.IsTerminal() {
		snap, err := w.client.Status(ctx, w.hermesRunID)
		switch {
		case err == nil:
			if mapped, ok := snap.Mapped(); ok && mapped != w.status {
				w.apply(ctx, Synthesise(mapped, snap, time.Now()))
			}
		case isClientErr(err, ErrRunExpired):
			msg := "hermes no longer knows this run"
			w.finish(ctx, Lost, &msg)
			return
		case ctx.Err() != nil:
			return
		default:
			w.log.DebugContext(ctx, "hermes run poll failed", "run", w.runID, "error", redact(err.Error()))
		}
		if w.status.IsTerminal() {
			return
		}
		if w.store.Now().Sub(w.startedAt) > w.config.MaxRunLifetime {
			msg := "run exceeded the maximum lifetime"
			w.finish(ctx, Lost, &msg)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.config.PollInterval):
		}
	}
}

func (w *Watcher) apply(ctx context.Context, f Frame) {
	previous := w.status
	next := Transition(previous, f.Event)
	approvalWasPending := w.hasPendingApproval
	at := f.At
	// Persist even when the watcher is being canceled: Swift's apply is not
	// interrupted by cancellation once it started.
	wctx := context.WithoutCancel(ctx)
	run, err := w.store.Append(wctx, w.runID, f.Name, f.Payload, at, func(*Run) Mutation {
		m := Mutation{Status: &next}
		switch f.Event.Kind {
		case KindApprovalRequest:
			var extra map[string]any
			if len(f.Event.Extra) > 0 {
				extra = f.Event.Extra
			}
			m.SetPending, m.PendingApproval = true, &PendingApproval{Command: f.Event.Command, Choices: f.Event.Choices, RequestedAt: at, Extra: extra}
		case KindRunCompleted:
			summary := f.Event.Summary
			m.Summary, m.SetPending, m.FinishedAt = &summary, true, &at
		case KindRunFailed, KindError:
			msg := f.Event.Text
			m.Error, m.SetPending, m.FinishedAt = &msg, true, &at
		case KindRunCancelled:
			m.SetPending, m.FinishedAt = true, &at
		case KindApprovalResponded, KindToolStarted, KindToolCompleted, KindToolFailed:
			if next == Running {
				m.SetPending = true
			}
		}
		return m
	})
	if err != nil {
		w.log.ErrorContext(ctx, "hermes run event persist failed", "run", w.runID, "event", f.Name, "error", redact(err.Error()))
		return
	}
	if run == nil {
		// Row deleted under us (account deletion): nothing left to track.
		w.status = Lost
		return
	}
	w.status = next
	w.hasPendingApproval = run.PendingApproval != nil
	dto := run.DTO()
	if f.Event.Kind == KindApprovalRequest && !approvalWasPending {
		w.notifier.ApprovalRequested(wctx, w.tenant, dto)
	}
	if next.IsTerminal() && !previous.IsTerminal() {
		CommitConversationTurn(wctx, w.store, run, w.log)
		w.notifier.RunFinished(wctx, w.tenant, dto)
	}
}

func (w *Watcher) finish(ctx context.Context, terminal Status, errText *string) {
	previous := w.status
	wctx := context.WithoutCancel(ctx)
	run, err := w.store.Finish(wctx, w.runID, terminal, errText)
	if err != nil {
		w.log.ErrorContext(ctx, "hermes run finish failed", "run", w.runID, "status", string(terminal), "error", redact(err.Error()))
		w.status = terminal
		return
	}
	w.status = terminal
	if run == nil {
		return
	}
	w.hasPendingApproval = false
	if !previous.IsTerminal() {
		w.notifier.RunFinished(wctx, w.tenant, run.DTO())
	}
}

// CommitConversationTurn writes a completed run's answer into its conversation
// as an assistant turn. Idempotent on hermes_run_id (a re-attached watcher
// replays the terminal edge) and never fails the caller.
func CommitConversationTurn(ctx context.Context, store *Store, run *Run, log *slog.Logger) {
	if run.ConversationID == nil || run.Status != Completed {
		return
	}
	summary := ""
	if run.Summary != nil {
		summary = trimWhitespacesAndNewlines(*run.Summary)
	}
	if summary == "" {
		return
	}
	err := func() error {
		var exists bool
		if err := store.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conversation_messages WHERE hermes_run_id = $1)`, run.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		events, err := store.Events(ctx, run.ID, 0, 1000)
		if err != nil {
			return err
		}
		tools := 0
		for _, e := range events {
			if _, ok := ToolName(e.Event, e.Payload); ok {
				tools++
			}
		}
		if _, err := store.Pool.Exec(ctx, `INSERT INTO conversation_messages
			(id, conversation_id, role, content, source_memory_ids, tool_call_count, hermes_run_id, created_at)
			VALUES ($1, $2, 'assistant', $3, '{}', $4, $5, $6)`,
			uuid.New(), *run.ConversationID, summary, tools, run.ID, store.Now()); err != nil {
			return err
		}
		log.InfoContext(ctx, "hermes run answer committed to conversation", "run", run.ID, "conversation", *run.ConversationID, "tools", tools)
		return nil
	}()
	if err != nil {
		log.ErrorContext(ctx, "hermes run conversation commit failed", "run", run.ID, "error", redact(err.Error()))
	}
}
