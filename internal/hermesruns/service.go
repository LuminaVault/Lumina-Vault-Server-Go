package hermesruns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/hermesresolve"
	"lumina/internal/ids"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ServiceError is HermesRunsServiceError.
type ServiceError string

const (
	ErrRunNotFound          ServiceError = "hermes_run_not_found"
	ErrConversationNotFound ServiceError = "conversation_not_found"
	ErrTooManyActiveRuns    ServiceError = "hermes_runs_limit"
	ErrApprovalNotPending   ServiceError = "hermes_approval_not_pending"
	ErrRunNotActive         ServiceError = "hermes_run_not_active"
	ErrEmptyPrompt          ServiceError = "prompt_required"
)

func (e ServiceError) Error() string { return string(e) }

// StableCode is stableCode.
func (e ServiceError) StableCode() string { return string(e) }

// HTTPStatus is HermesRunsController.status(for: HermesRunsServiceError).
func (e ServiceError) HTTPStatus() int {
	switch e {
	case ErrRunNotFound, ErrConversationNotFound:
		return http.StatusNotFound
	case ErrTooManyActiveRuns:
		return http.StatusTooManyRequests
	case ErrApprovalNotPending, ErrRunNotActive:
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// Limits is HermesRunsService.Limits.
type Limits struct {
	MaxWatchersPerTenant  int
	MaxWatchersPerProcess int
	// ReattachWindow matches the Hermes run store TTL.
	ReattachWindow     time.Duration
	CapabilityCacheTTL time.Duration
	Watcher            WatcherConfig
}

// DefaultLimits is Swift's: 8 per tenant, 32 per process, 300 s, 1 h.
func DefaultLimits() Limits {
	return Limits{
		MaxWatchersPerTenant: 8, MaxWatchersPerProcess: 32,
		ReattachWindow: 300 * time.Second, CapabilityCacheTTL: time.Hour,
		Watcher: DefaultWatcherConfig(),
	}
}

// ResolveFunc resolves a tenant's Hermes endpoint.
type ResolveFunc func(ctx context.Context, tenant uuid.UUID) (hermesresolve.Resolution, error)

// ClientFactory builds a client for a resolution and session key.
type ClientFactory func(res hermesresolve.Resolution, sessionKey string) *Client

// StartRequest is HermesRunStartRequest.
type StartRequest struct {
	Prompt         string
	SessionID      *string
	Model          *string
	ConversationID *uuid.UUID
}

type watcherHandle struct {
	tenant uuid.UUID
	cancel context.CancelFunc
	done   chan struct{}
}

type cachedCapability struct {
	supportsRuns bool
	checkedAt    time.Time
}

// Service is HermesRunsService: starts runs, owns one watcher goroutine per
// active run (bounded per tenant and per process) and serves the persisted view.
type Service struct {
	store      *Store
	notifier   Notifier
	resolve    ResolveFunc
	makeClient ClientFactory
	limits     Limits
	log        *slog.Logger

	mu           sync.Mutex
	watchers     map[uuid.UUID]*watcherHandle
	capabilities map[string]cachedCapability
}

// Options builds a Service.
type Options struct {
	Pool       *pgxpool.Pool
	Bus        *Bus
	Notifier   Notifier
	Resolve    ResolveFunc
	MakeClient ClientFactory
	// Limits zero means DefaultLimits.
	Limits Limits
	Log    *slog.Logger
	Now    func() time.Time
}

// NewService builds the service.
func NewService(opt Options) *Service {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Limits == (Limits{}) {
		opt.Limits = DefaultLimits()
	}
	if opt.Bus == nil {
		opt.Bus = NewBus()
	}
	if opt.Notifier == nil {
		opt.Notifier = APNSNotifier{}
	}
	return &Service{
		store:    &Store{Pool: opt.Pool, Bus: opt.Bus, Log: opt.Log, Now: opt.Now},
		notifier: opt.Notifier, resolve: opt.Resolve, makeClient: opt.MakeClient,
		limits: opt.Limits, log: opt.Log,
		watchers: map[uuid.UUID]*watcherHandle{}, capabilities: map[string]cachedCapability{},
	}
}

// Store is the service's store.
func (s *Service) Store() *Store { return s.store }

// Bus is the service's event bus.
func (s *Service) Bus() *Bus { return s.store.Bus }

// StopAll cancels every watcher and waits for each to exit, so none writes
// after the pool closes. It returns how many were running.
func (s *Service) StopAll() int {
	s.mu.Lock()
	active := s.watchers
	s.watchers = map[uuid.UUID]*watcherHandle{}
	s.mu.Unlock()
	for _, h := range active {
		h.cancel()
	}
	for _, h := range active {
		<-h.done
	}
	return len(active)
}

// Reattach re-attaches watchers to runs that were active when the process last
// stopped, marking runs older than the window lost. Rows already watched are
// skipped, so it is safe to call more than once.
func (s *Service) Reattach(ctx context.Context) {
	rows, err := s.store.ActiveRuns(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "hermes runs re-attach scan failed: "+redact(err.Error()))
		return
	}
	now := s.store.Now()
	attached, lost := 0, 0
	for _, run := range rows {
		if s.IsWatching(run.ID) {
			continue
		}
		if now.Sub(run.StartedAt) > s.limits.ReattachWindow {
			s.markLost(ctx, run, fmt.Sprintf("watcher lost across restart; run older than %d s", int(s.limits.ReattachWindow.Seconds())))
			lost++
			continue
		}
		res, err := s.resolve(ctx, run.Tenant)
		if err != nil {
			s.markLost(ctx, run, "watcher could not re-attach: "+redact(err.Error()))
			lost++
			continue
		}
		s.spawnWatcher(run, s.makeClient(res, ""), ModePoll)
		attached++
	}
	if attached > 0 || lost > 0 {
		s.log.InfoContext(ctx, "hermes runs re-attach", "attached", attached, "lost", lost)
	}
}

// Start is start(tenantID:request:sessionKey:).
func (s *Service) Start(ctx context.Context, tenant uuid.UUID, req StartRequest, sessionKey string) (DTO, error) {
	prompt := trimWhitespacesAndNewlines(req.Prompt)
	if prompt == "" {
		return DTO{}, ErrEmptyPrompt
	}
	if req.ConversationID != nil {
		var found bool
		if err := s.store.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conversations WHERE tenant_id = $1 AND id = $2)`,
			tenant, *req.ConversationID).Scan(&found); err != nil {
			return DTO{}, err
		}
		if !found {
			return DTO{}, ErrConversationNotFound
		}
	}
	if err := s.enforceLimits(tenant); err != nil {
		return DTO{}, err
	}
	res, err := s.resolve(ctx, tenant)
	if err != nil {
		return DTO{}, err
	}
	client := s.makeClient(res, sessionKey)
	if err := s.requireRunsSupport(ctx, client); err != nil {
		return DTO{}, err
	}
	// Limits again: the capability probe took time.
	if err := s.enforceLimits(tenant); err != nil {
		return DTO{}, err
	}
	hermesRunID, err := client.Start(ctx, prompt, req.SessionID, req.Model)
	if err != nil {
		return DTO{}, err
	}
	run := &Run{
		ID: uuid.New(), Tenant: tenant, HermesRunID: hermesRunID, Status: Running, Prompt: prompt,
		SessionID: req.SessionID, Model: req.Model, ConversationID: req.ConversationID, StartedAt: s.store.Now(),
	}
	if err := s.store.Insert(ctx, run); err != nil {
		return DTO{}, err
	}
	if req.ConversationID != nil {
		if _, err := s.store.Pool.Exec(ctx, `INSERT INTO conversation_messages (id, conversation_id, role, content, source_memory_ids, tool_call_count, created_at)
			VALUES ($1, $2, 'system', $3, '{}', 0, $4)`, uuid.New(), *req.ConversationID, ConversationMarker(run.ID, prompt), s.store.Now()); err != nil {
			return DTO{}, err
		}
	}
	s.spawnWatcher(run, client, ModeStream)
	s.store.Publish(run, 0, "run.accepted")
	return run.DTO(), nil
}

// ConversationMarker is the system turn linking a conversation to its run.
func ConversationMarker(runID uuid.UUID, prompt string) string {
	return "Hermes run started: " + prompt + "\nlumina://hermes/runs/" + ids.Upper(runID)
}

// Approve forwards the choice to Hermes and clears the pending approval.
func (s *Service) Approve(ctx context.Context, tenant, runID uuid.UUID, choice Choice) (DTO, error) {
	run, err := s.store.Find(ctx, tenant, runID)
	if err != nil {
		return DTO{}, err
	}
	if run == nil {
		return DTO{}, ErrRunNotFound
	}
	if run.PendingApproval == nil || run.Status.IsTerminal() {
		return DTO{}, ErrApprovalNotPending
	}
	res, err := s.resolve(ctx, tenant)
	if err != nil {
		return DTO{}, err
	}
	if err := s.makeClient(res, "").Approve(ctx, run.HermesRunID, choice); err != nil {
		if isClientErr(err, ErrApprovalNotPendingUpstream) {
			// Hermes already moved on (answered elsewhere): mirror it.
			if err := s.store.ClearPendingApproval(ctx, runID); err != nil {
				return DTO{}, err
			}
			return DTO{}, ErrApprovalNotPending
		}
		return DTO{}, err
	}
	if err := s.store.ClearPendingApproval(ctx, runID); err != nil {
		return DTO{}, err
	}
	updated, err := s.store.Find(ctx, tenant, runID)
	if err != nil {
		return DTO{}, err
	}
	if updated == nil {
		return DTO{}, ErrRunNotFound
	}
	s.store.Publish(updated, updated.LastSeq, "approval.sent")
	return updated.DTO(), nil
}

// Stop asks Hermes to cancel. The terminal state lands through the watcher, so
// the returned DTO may still read running.
func (s *Service) Stop(ctx context.Context, tenant, runID uuid.UUID) (DTO, error) {
	run, err := s.store.Find(ctx, tenant, runID)
	if err != nil {
		return DTO{}, err
	}
	if run == nil {
		return DTO{}, ErrRunNotFound
	}
	if run.Status.IsTerminal() {
		return run.DTO(), nil
	}
	res, err := s.resolve(ctx, tenant)
	if err != nil {
		return DTO{}, err
	}
	if err := s.makeClient(res, "").Stop(ctx, run.HermesRunID); err != nil {
		if !isClientErr(err, ErrRunExpired) {
			return DTO{}, err
		}
		// Hermes forgot it: nothing to wait for.
		s.cancelWatcher(runID)
		finished, err := s.store.Finish(ctx, runID, Stopped, nil)
		if err != nil {
			return DTO{}, err
		}
		if finished == nil {
			return DTO{}, ErrRunNotFound
		}
		s.notifier.RunFinished(ctx, tenant, finished.DTO())
		return finished.DTO(), nil
	}
	current, err := s.store.Find(ctx, tenant, runID)
	if err != nil {
		return DTO{}, err
	}
	if current == nil {
		return DTO{}, ErrRunNotFound
	}
	return current.DTO(), nil
}

// List is the tenant's runs newest first, the limit clamped to 1...50.
func (s *Service) List(ctx context.Context, tenant uuid.UUID, limit int, conversation *uuid.UUID) ([]DTO, error) {
	limit = max(1, min(limit, 50))
	rows, err := s.store.List(ctx, tenant, limit, conversation)
	if err != nil {
		return nil, err
	}
	out := make([]DTO, len(rows))
	for i, r := range rows {
		out[i] = r.DTO()
	}
	return out, nil
}

// Get is get(tenantID:runID:).
func (s *Service) Get(ctx context.Context, tenant, runID uuid.UUID) (DTO, error) {
	run, err := s.store.Find(ctx, tenant, runID)
	if err != nil {
		return DTO{}, err
	}
	if run == nil {
		return DTO{}, ErrRunNotFound
	}
	return run.DTO(), nil
}

// Events is the persisted events with seq > afterSeq, oldest first.
func (s *Service) Events(ctx context.Context, tenant, runID uuid.UUID, afterSeq, limit int) ([]EventDTO, error) {
	run, err := s.store.Find(ctx, tenant, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrRunNotFound
	}
	rows, err := s.store.Events(ctx, runID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	out := make([]EventDTO, len(rows))
	for i, r := range rows {
		out[i] = r.DTO()
	}
	return out, nil
}

// ActiveWatcherCount is activeWatcherCount.
func (s *Service) ActiveWatcherCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.watchers)
}

// IsWatching is isWatching(runID:).
func (s *Service) IsWatching(runID uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watchers[runID] != nil
}

// AwaitWatcher blocks until the watcher for runID has exited (a test hook).
func (s *Service) AwaitWatcher(runID uuid.UUID) {
	s.mu.Lock()
	h := s.watchers[runID]
	s.mu.Unlock()
	if h != nil {
		<-h.done
	}
}

func (s *Service) enforceLimits(tenant uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	perTenant := 0
	for _, h := range s.watchers {
		if h.tenant == tenant {
			perTenant++
		}
	}
	if len(s.watchers) >= s.limits.MaxWatchersPerProcess || perTenant >= s.limits.MaxWatchersPerTenant {
		return ErrTooManyActiveRuns
	}
	return nil
}

func (s *Service) requireRunsSupport(ctx context.Context, client *Client) error {
	key := client.URLKey()
	now := s.store.Now()
	s.mu.Lock()
	cached, ok := s.capabilities[key]
	s.mu.Unlock()
	if ok && now.Sub(cached.checkedAt) < s.limits.CapabilityCacheTTL {
		if !cached.supportsRuns {
			return &ClientError{Kind: ErrUnsupported}
		}
		return nil
	}
	caps, err := client.Capabilities(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.capabilities[key] = cachedCapability{supportsRuns: caps.SupportsRuns(), checkedAt: now}
	s.mu.Unlock()
	if !caps.SupportsRuns() {
		return &ClientError{Kind: ErrUnsupported}
	}
	return nil
}

func (s *Service) spawnWatcher(run *Run, client *Client, mode Mode) {
	w := NewWatcher(run, mode, client, s.store, s.notifier, s.limits.Watcher, s.log)
	ctx, cancel := context.WithCancel(context.Background()) // outlives the request; StopAll cancels
	h := &watcherHandle{tenant: run.Tenant, cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.watchers[run.ID] = h
	s.mu.Unlock()
	go func() {
		defer close(h.done)
		defer cancel()
		w.Run(ctx)
		s.mu.Lock()
		if s.watchers[run.ID] == h {
			delete(s.watchers, run.ID)
		}
		s.mu.Unlock()
	}()
}

func (s *Service) cancelWatcher(runID uuid.UUID) {
	s.mu.Lock()
	h := s.watchers[runID]
	delete(s.watchers, runID)
	s.mu.Unlock()
	if h != nil {
		h.cancel()
	}
}

func (s *Service) markLost(ctx context.Context, run *Run, reason string) {
	finished, err := s.store.Finish(ctx, run.ID, Lost, &reason)
	if err != nil {
		s.log.ErrorContext(ctx, "hermes run mark-lost failed", "run", run.ID, "error", redact(err.Error()))
		return
	}
	if finished != nil {
		s.notifier.RunFinished(ctx, run.Tenant, finished.DTO())
	}
}

// IsServiceError reports whether err is a HermesRunsServiceError.
func IsServiceError(err error) (ServiceError, bool) {
	var se ServiceError
	ok := errors.As(err, &se)
	return se, ok
}
