package hermesmirror

import (
	"context"
	"log/slog"
	"lumina/internal/ids"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Worker is HermesMirrorRefreshWorker: keeps every linked Hermes fresh in the
// background. Each pass walks user_hermes_config rows with a dashboard URL or a
// gateway base URL, keyset-paginated by id, and for each tenant syncs skills and
// jobs, collects finished job runs and artifacts, and continues a capped vault
// import, with bounded concurrency, a per-tenant budget and failure isolation.
//
// Enqueue is the connect-time nudge (HermesMirrorNudging): the next run of the
// job refreshes those tenants first.
type Worker struct {
	Service *Service
	// PageSize, MaxConcurrent and PerTenantBudget default to Swift's 100, 4 and 60 s.
	PageSize        int
	MaxConcurrent   int
	PerTenantBudget time.Duration
	Log             *slog.Logger

	mu      sync.Mutex
	pending []uuid.UUID
}

// Swift's defaults.
const (
	WorkerTickInterval    = 900 * time.Second
	WorkerMaxJitter       = 30 * time.Second
	defaultWorkerPageSize = 100
	defaultMaxConcurrent  = 4
	defaultTenantBudget   = 60 * time.Second
)

func (w *Worker) pageSize() int {
	if w.PageSize <= 0 {
		return defaultWorkerPageSize
	}
	return w.PageSize
}

func (w *Worker) maxConcurrent() int {
	if w.MaxConcurrent <= 0 {
		return defaultMaxConcurrent
	}
	return w.MaxConcurrent
}

func (w *Worker) budget() time.Duration {
	if w.PerTenantBudget == 0 {
		return defaultTenantBudget
	}
	return w.PerTenantBudget
}

func (w *Worker) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default().With("label", "lv.hermes-mirror.worker")
}

// Enqueue is enqueue(tenantID:): records the tenant once.
func (w *Worker) Enqueue(tenant uuid.UUID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, id := range w.pending {
		if id == tenant {
			return
		}
	}
	w.pending = append(w.pending, tenant)
}

// PendingTenantIDs is pendingTenantIDs().
func (w *Worker) PendingTenantIDs() []uuid.UUID {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]uuid.UUID(nil), w.pending...)
}

func (w *Worker) takePending() []uuid.UUID {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.pending
	w.pending = nil
	return out
}

// Run is one turn of Swift's run loop: the nudged tenants first, then a tick.
// A tick error is logged, never returned, as in Swift.
func (w *Worker) Run(ctx context.Context) error {
	for _, tenant := range w.takePending() {
		_ = Refresh(ctx, tenant, w.Service, w.budget(), w.log())
	}
	summary, err := w.Tick(ctx)
	if err != nil {
		w.log().WarnContext(ctx, "hermes.mirror.worker tick error: "+Describe(err))
		return nil
	}
	if summary.Processed > 0 {
		w.log().InfoContext(ctx, "hermes.mirror.worker tick", "processed", summary.Processed, "failed", summary.Failed, "timedOut", summary.TimedOut)
	}
	return nil
}

// TickSummary is HermesMirrorRefreshWorker.TickSummary.
type TickSummary struct {
	Processed int
	Failed    int
	TimedOut  int
}

// Tick is tick(): one pass over every configured tenant.
func (w *Worker) Tick(ctx context.Context) (TickSummary, error) {
	var summary TickSummary
	var cursor *uuid.UUID
	for {
		page, err := w.ConfiguredTenants(ctx, cursor)
		if err != nil {
			return summary, err
		}
		if len(page) == 0 {
			break
		}
		last := page[len(page)-1].RowID
		cursor = &last
		for start := 0; start < len(page); start += w.maxConcurrent() {
			batch := page[start:min(start+w.maxConcurrent(), len(page))]
			outcomes := make([]Outcome, len(batch))
			var wg sync.WaitGroup
			for i, t := range batch {
				wg.Go(func() {
					outcomes[i] = Refresh(ctx, t.TenantID, w.Service, w.budget(), w.log())
				})
			}
			wg.Wait()
			for _, o := range outcomes {
				summary.Processed++
				switch o {
				case OutcomeFailed:
					summary.Failed++
				case OutcomeTimedOut:
					summary.TimedOut++
				case OutcomeOK:
				}
			}
		}
		if len(page) < w.pageSize() {
			break
		}
	}
	return summary, nil
}

// ConfiguredTenant is a user_hermes_config row id and its tenant.
type ConfiguredTenant struct {
	RowID    uuid.UUID
	TenantID uuid.UUID
}

// ConfiguredTenants is configuredTenants(after:): a keyset page of tenants
// linked to a Hermes of their own (a dashboard URL or a gateway base URL),
// ordered by row id.
func (w *Worker) ConfiguredTenants(ctx context.Context, after *uuid.UUID) ([]ConfiguredTenant, error) {
	rows, err := w.Service.Pool.Query(ctx, `SELECT id, tenant_id FROM user_hermes_config
		WHERE ((cron_dashboard_url IS NOT NULL AND cron_dashboard_url <> '') OR base_url <> '')
		AND ($1::uuid IS NULL OR id > $1)
		ORDER BY id LIMIT $2`, after, w.pageSize())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConfiguredTenant
	for rows.Next() {
		var c ConfiguredTenant
		if err := rows.Scan(&c.RowID, &c.TenantID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Outcome is HermesMirrorRefreshWorker.Outcome.
type Outcome string

// Outcomes.
const (
	OutcomeOK       Outcome = "ok"
	OutcomeFailed   Outcome = "failed"
	OutcomeTimedOut Outcome = "timedOut"
)

// HasPendingVaultImport is hasPendingVaultImport(tenantID:).
func (s *Service) HasPendingVaultImport(ctx context.Context, tenant uuid.UUID) (bool, error) {
	st, err := s.loadState(ctx, tenant)
	if err != nil {
		return false, err
	}
	return st != nil && st.VaultCursor != nil, nil
}

// Refresh is refresh(tenantID:service:budget:logger:): skills and jobs sync,
// collect finished job runs, collect artifacts, then continue a pending vault
// import, inside one time budget. Errors are logged, never returned. When the
// budget runs out first the work is canceled and awaited, as Swift's task group
// does.
func Refresh(ctx context.Context, tenant uuid.UUID, service *Service, budget time.Duration, log *slog.Logger) Outcome {
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan Outcome, 1)
	go func() { done <- refreshOnce(work, tenant, service, log) }()
	timer := time.NewTimer(max(budget, 0))
	defer timer.Stop()
	select {
	case o := <-done:
		return o
	case <-timer.C:
		cancel()
		<-done
		log.WarnContext(ctx, "hermes.mirror.worker tenant refresh exceeded budget", "tenant", ids.Upper(tenant), "budget", budget.String())
		return OutcomeTimedOut
	}
}

func refreshOnce(ctx context.Context, tenant uuid.UUID, service *Service, log *slog.Logger) Outcome {
	fail := func(err error) Outcome {
		log.WarnContext(ctx, "hermes.mirror.worker tenant refresh failed", "tenant", ids.Upper(tenant), "error", Describe(err))
		return OutcomeFailed
	}
	status, err := service.Sync(ctx, tenant, map[string]bool{ScopeSkills: true, ScopeJobs: true})
	if err != nil {
		return fail(err)
	}
	collected, err := service.CollectAllJobRuns(ctx, tenant)
	if err != nil {
		return fail(err)
	}
	if collected.Inserted > 0 || collected.Failed > 0 {
		log.InfoContext(ctx, "hermes.mirror.worker collect", "tenant", ids.Upper(tenant),
			"jobs", collected.Jobs, "runs", collected.Inserted, "files", collected.FilesWritten, "failed", collected.Failed)
	}
	artifacts, err := service.CollectArtifacts(ctx, tenant)
	if err != nil {
		return fail(err)
	}
	if artifacts > 0 {
		log.InfoContext(ctx, "hermes.mirror.worker artifacts", "tenant", ids.Upper(tenant), "inserted", artifacts)
	}
	pending, err := service.HasPendingVaultImport(ctx, tenant)
	if err != nil {
		return fail(err)
	}
	if pending {
		if _, err := service.ImportVault(ctx, tenant, nil); err != nil {
			return fail(err)
		}
	}
	if status.LastStatus == "failed" {
		return OutcomeFailed
	}
	return OutcomeOK
}

// Jitter is jitter(upTo:): a random delay in [0, maximum], in whole milliseconds.
func Jitter(maximum time.Duration) time.Duration {
	ms := maximum.Milliseconds()
	if ms <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(ms+1)) * time.Millisecond //nolint:gosec // jitter, not security
}
