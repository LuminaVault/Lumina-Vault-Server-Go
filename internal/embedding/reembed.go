package embedding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/jobs"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReembedTarget is EmbeddingReembedService.Target: one table whose text
// vectors the service rewrites. Every field is a constant, never input, so the
// SQL built from them is safe.
type ReembedTarget struct {
	// Name is the key in embedding_reembed_progress.
	Name              string
	Table             string
	IDColumn          string
	IDType            string
	TextColumn        string
	ClearsGraphLayout bool
}

var (
	TargetMemories = ReembedTarget{
		Name: "memories", Table: "memories", IDColumn: "id", IDType: "uuid",
		TextColumn: "content", ClearsGraphLayout: true,
	}
	TargetMemoryChunks = ReembedTarget{
		Name: "memory_chunks", Table: "memory_chunks", IDColumn: "chunk_id", IDType: "text",
		TextColumn: "text",
	}
	TargetMemoriesArchive = ReembedTarget{
		Name: "memories_archive", Table: "memories_archive", IDColumn: "id", IDType: "uuid",
		TextColumn: "content",
	}
)

// AllReembedTargets is Target.all; HealableTargets is Target.healable.
func AllReembedTargets() []ReembedTarget {
	return []ReembedTarget{TargetMemories, TargetMemoryChunks, TargetMemoriesArchive}
}

func HealableTargets() []ReembedTarget {
	return []ReembedTarget{TargetMemories, TargetMemoryChunks}
}

// ReembedConfig is EmbeddingReembedService.Configuration.
type ReembedConfig struct {
	BatchSize                  int
	Pause                      time.Duration
	FailedBatchesBeforeBackoff int
	BackoffBase                time.Duration
	MaxBackoff                 time.Duration
	HealInterval               time.Duration
	HealMaxBackoff             time.Duration
}

// DefaultReembedConfig is Configuration().
func DefaultReembedConfig() ReembedConfig {
	return ReembedConfig{
		BatchSize: 50, Pause: 2 * time.Second, FailedBatchesBeforeBackoff: 3,
		BackoffBase: 30 * time.Second, MaxBackoff: 900 * time.Second,
		HealInterval: 600 * time.Second, HealMaxBackoff: 3600 * time.Second,
	}
}

// HealResultKind is the case of Swift's HealResult.
type HealResultKind int

const (
	HealCaughtUp HealResultKind = iota
	HealProviderUnavailable
	HealRefused
)

// HealResult is EmbeddingReembedService.HealResult.
type HealResult struct {
	Kind   HealResultKind
	Healed int
	Failed int
}

// BatchResultKind is the case of Swift's BatchResult.
type BatchResultKind int

const (
	BatchProgressed BatchResultKind = iota
	BatchProviderFailed
	BatchCompleted
	BatchAlreadyComplete
)

// BatchResult is EmbeddingReembedService.BatchResult.
type BatchResult struct {
	Kind       BatchResultKind
	Reembedded int
	Failed     int
	Skipped    int
}

// ReembedProgress is one embedding_reembed_progress row.
type ReembedProgress struct {
	LastID      *string
	RowsDone    int64
	RowsFailed  int64
	CompletedAt *time.Time
}

// ReembedService is EmbeddingReembedService. It does two things, in order:
//
//   - the one-shot re-embed (EMBEDDING_REEMBED_ENABLED, off by default): every
//     stored text vector in every target is rewritten through the active
//     provider, the cursor persisted per batch so a restart resumes, and
//     completed_at set once a target is done. It refuses under the deterministic
//     stub, which would only rewrite hash noise.
//   - the heal pass (EMBEDDING_HEAL_ENABLED, on by default with a real provider):
//     rows a write path saved with embedding NULL because the provider failed are
//     embedded. A provider that cannot serve any row ends the pass after one call,
//     and the next pass waits the heal interval doubling up to its cap.
//
// Single replica, like Swift: a second replica would redo batches.
type ReembedService struct {
	Pool        *pgxpool.Pool
	Embeddings  Service
	ActiveKind  Kind
	Targets     []ReembedTarget
	HealTargets []ReembedTarget
	ReembedOn   bool
	HealOn      bool
	Config      ReembedConfig
	Logger      *slog.Logger
	// Sleep is the pause between batches. Tests pass one that returns at once.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now is the job's clock for the heal backoff.
	Now func() time.Time

	mu                       sync.Mutex
	consecutiveFailedBatches int
	consecutiveFailedHeals   int
	healCursors              map[string]string
	reembedDone              bool
	stopped                  bool
	nextHealAt               time.Time
}

// NewReembedService fills Swift's defaults and clamps the configuration as its
// init does.
func NewReembedService(pool *pgxpool.Pool, embeddings Service, kind Kind, reembedOn, healOn bool, cfg ReembedConfig, logger *slog.Logger) *ReembedService {
	cfg.BatchSize = max(1, cfg.BatchSize)
	cfg.FailedBatchesBeforeBackoff = max(1, cfg.FailedBatchesBeforeBackoff)
	cfg.HealMaxBackoff = max(cfg.HealInterval, cfg.HealMaxBackoff)
	if logger == nil {
		logger = slog.Default()
	}
	return &ReembedService{
		Pool: pool, Embeddings: embeddings, ActiveKind: kind,
		Targets: AllReembedTargets(), HealTargets: HealableTargets(),
		ReembedOn: reembedOn, HealOn: healOn, Config: cfg, Logger: logger,
		Sleep: sleepCtx, Now: time.Now,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Job runs Swift's run(): the re-embed to completion (inside the first run, with
// its own pauses and backoff), then one heal pass per run. The runner wakes it
// every heal interval; a pass backing off skips runs until its delay is up, so
// the waits are Swift's rounded up to a whole interval. A pass the provider
// refused returns an error so last-success means the heal is keeping up.
func (s *ReembedService) Job() jobs.Job {
	return jobs.Job{Name: "EmbeddingReembedService", Schedule: jobs.Every(s.Config.HealInterval, 0), Run: s.runOnce}
}

func (s *ReembedService) runOnce(ctx context.Context) error {
	s.mu.Lock()
	stopped, reembedDone, nextHeal := s.stopped, s.reembedDone, s.nextHealAt
	s.mu.Unlock()
	if stopped {
		return jobs.ErrSkipped
	}
	if s.ReembedOn && !reembedDone {
		ran, err := s.RunToCompletion(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.stop()
			s.Logger.Error(fmt.Sprintf("embedding.reembed stopped: %v", err))
			return err
		}
		s.mu.Lock()
		s.reembedDone = true
		s.mu.Unlock()
		if !ran {
			s.stop()
			return nil
		}
	}
	if !s.HealOn {
		s.stop()
		return jobs.ErrSkipped
	}
	if s.Now().Before(nextHeal) {
		return jobs.ErrSkipped
	}
	result, err := s.HealMissing(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.Logger.Error(fmt.Sprintf("embedding.heal pass failed: %v", err))
		result = HealResult{Kind: HealProviderUnavailable}
	}
	if result.Kind == HealRefused {
		s.stop()
		return nil
	}
	delay := s.DelayAfterHeal(result)
	s.mu.Lock()
	s.nextHealAt = s.Now().Add(delay)
	s.mu.Unlock()
	if result.Kind == HealProviderUnavailable {
		return fmt.Errorf("embedding provider unavailable; healed %d, next probe in %s", result.Healed, delay)
	}
	return nil
}

func (s *ReembedService) stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
}

// RunToCompletion walks every target to completion. It returns false, having done
// nothing, while the deterministic stub is the active provider.
func (s *ReembedService) RunToCompletion(ctx context.Context) (bool, error) {
	if s.ActiveKind == KindDeterministic {
		s.Logger.Warn("embedding.reembed refused: active embedding provider is deterministic, so re-embedding would only rewrite hash noise. Set EMBEDDING_PROVIDER to a real provider (e.g. openrouter) first.")
		return false, nil
	}
	names := make([]string, 0, len(s.Targets))
	for _, t := range s.Targets {
		names = append(names, t.Name)
	}
	s.Logger.Info(fmt.Sprintf("embedding.reembed started provider=%s targets=%s batch=%d", s.ActiveKind, strings.Join(names, ","), s.Config.BatchSize))
	for _, target := range s.Targets {
		if err := s.runTarget(ctx, target); err != nil {
			return false, err
		}
	}
	s.Logger.Info("embedding.reembed all targets complete")
	return true, nil
}

func (s *ReembedService) runTarget(ctx context.Context, target ReembedTarget) error {
	started := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := s.RunBatch(ctx, target)
		if err != nil {
			return err
		}
		switch result.Kind {
		case BatchAlreadyComplete:
			s.Logger.Debug("embedding.reembed target=" + target.Name + " already complete")
			return nil
		case BatchCompleted:
			p, err := s.Progress(ctx, target)
			if err != nil {
				return err
			}
			var done, failed int64
			if p != nil {
				done, failed = p.RowsDone, p.RowsFailed
			}
			s.Logger.Info(fmt.Sprintf("embedding.reembed complete target=%s rows_done=%d rows_failed=%d elapsed=%s", target.Name, done, failed, time.Since(started)))
			return nil
		case BatchProgressed:
			err = s.Sleep(ctx, s.Config.Pause)
		case BatchProviderFailed:
			err = s.Sleep(ctx, s.DelayAfterFailedBatch())
		}
		if err != nil {
			return err
		}
	}
}

// RunBatch re-embeds one batch of target after its persisted cursor.
func (s *ReembedService) RunBatch(ctx context.Context, target ReembedTarget) (BatchResult, error) {
	if s.ActiveKind == KindDeterministic {
		return BatchResult{Kind: BatchAlreadyComplete}, nil
	}
	current, err := s.Progress(ctx, target)
	if err != nil {
		return BatchResult{}, err
	}
	if current != nil && current.CompletedAt != nil {
		return BatchResult{Kind: BatchAlreadyComplete}, nil
	}
	var cursor *string
	if current != nil {
		cursor = current.LastID
	}
	rows, err := s.fetchBatch(ctx, target, cursor, false)
	if err != nil {
		return BatchResult{}, err
	}
	if len(rows) == 0 {
		if err := s.markComplete(ctx, target); err != nil {
			return BatchResult{}, err
		}
		return BatchResult{Kind: BatchCompleted}, nil
	}
	var reembedded, failed, skipped int
	for _, row := range rows {
		if strings.TrimSpace(row.text) == "" {
			skipped++
			continue
		}
		wrote, err := s.embedAndWrite(ctx, target, row, false)
		if err != nil {
			if ctx.Err() != nil {
				return BatchResult{}, ctx.Err()
			}
			failed++
			s.Logger.Warn(fmt.Sprintf("embedding.reembed row failed target=%s id=%s tenant=%s: %v", target.Name, row.id, row.tenant, err))
			continue
		}
		if wrote {
			reembedded++
		} else {
			skipped++
		}
	}
	last := rows[len(rows)-1].id
	s.mu.Lock()
	if reembedded == 0 && skipped == 0 && failed > 0 {
		s.consecutiveFailedBatches++
		n := s.consecutiveFailedBatches
		s.mu.Unlock()
		after := "start"
		if cursor != nil {
			after = *cursor
		}
		s.Logger.Warn(fmt.Sprintf("embedding.reembed batch failed target=%s after=%s failed=%d consecutive=%d; cursor not advanced", target.Name, after, failed, n))
		return BatchResult{Kind: BatchProviderFailed, Failed: failed}, nil
	}
	s.consecutiveFailedBatches = 0
	s.mu.Unlock()
	if err := s.advance(ctx, target, last, reembedded, failed); err != nil {
		return BatchResult{}, err
	}
	s.Logger.Debug(fmt.Sprintf("embedding.reembed batch target=%s rows=%d reembedded=%d failed=%d skipped=%d last=%s", target.Name, len(rows), reembedded, failed, skipped, last))
	return BatchResult{Kind: BatchProgressed, Reembedded: reembedded, Failed: failed, Skipped: skipped}, nil
}

// Progress reads target's progress row, nil when there is none.
func (s *ReembedService) Progress(ctx context.Context, target ReembedTarget) (*ReembedProgress, error) {
	var p ReembedProgress
	err := s.Pool.QueryRow(ctx, `
SELECT last_id, rows_done, rows_failed, completed_at
FROM embedding_reembed_progress
WHERE target = $1`, target.Name).Scan(&p.LastID, &p.RowsDone, &p.RowsFailed, &p.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// DelayAfterFailedBatch is the pause until the threshold, then backoffBase
// doubling per further failed batch, capped at maxBackoff.
func (s *ReembedService) DelayAfterFailedBatch() time.Duration {
	s.mu.Lock()
	n := s.consecutiveFailedBatches
	s.mu.Unlock()
	threshold := s.Config.FailedBatchesBeforeBackoff
	if n < threshold {
		return s.Config.Pause
	}
	exponent := min(n-threshold, 16)
	capped := min(s.Config.BackoffBase*time.Duration(1<<exponent), s.Config.MaxBackoff)
	s.Logger.Warn(fmt.Sprintf("embedding.reembed backing off %s after %d failed batches", capped, n))
	return capped
}

// HealMissing is one heal pass over every heal target.
func (s *ReembedService) HealMissing(ctx context.Context) (HealResult, error) {
	if s.ActiveKind == KindDeterministic {
		s.Logger.Warn("embedding.heal refused: active embedding provider is deterministic")
		return HealResult{Kind: HealRefused}, nil
	}
	var healed, failed int
	for _, target := range s.HealTargets {
		for {
			if err := ctx.Err(); err != nil {
				return HealResult{}, err
			}
			s.mu.Lock()
			cursor, ok := s.healCursors[target.Name]
			s.mu.Unlock()
			var after *string
			if ok {
				after = &cursor
			}
			rows, err := s.fetchBatch(ctx, target, after, true)
			if err != nil {
				return HealResult{}, err
			}
			for _, row := range rows {
				wrote, err := s.embedAndWrite(ctx, target, row, true)
				s.setHealCursor(target.Name, &row.id)
				if err == nil {
					if wrote {
						healed++
					}
					continue
				}
				if ctx.Err() != nil {
					return HealResult{}, ctx.Err()
				}
				var providerErr *Error
				if errors.As(err, &providerErr) && !providerErr.IsProviderUnavailable() {
					failed++
					s.Logger.Warn(fmt.Sprintf("embedding.heal row failed target=%s id=%s tenant=%s: %v", target.Name, row.id, row.tenant, err))
					continue
				}
				s.Logger.Warn(fmt.Sprintf("embedding.heal provider unavailable target=%s tenant=%s healed=%d: %v", target.Name, row.tenant, healed, err))
				return HealResult{Kind: HealProviderUnavailable, Healed: healed}, nil
			}
			if len(rows) < s.Config.BatchSize {
				// End of the table: the next pass starts from the top.
				s.setHealCursor(target.Name, nil)
				break
			}
			if err := s.Sleep(ctx, s.Config.Pause); err != nil {
				return HealResult{}, err
			}
		}
	}
	if healed > 0 || failed > 0 {
		s.Logger.Info(fmt.Sprintf("embedding.heal pass complete healed=%d failed=%d", healed, failed))
	}
	return HealResult{Kind: HealCaughtUp, Healed: healed, Failed: failed}, nil
}

func (s *ReembedService) setHealCursor(target string, id *string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == nil {
		delete(s.healCursors, target)
		return
	}
	if s.healCursors == nil {
		s.healCursors = map[string]string{}
	}
	s.healCursors[target] = *id
}

// DelayAfterHeal is the wait before the next heal pass: the interval once caught
// up, doubling per refused pass up to healMaxBackoff.
func (s *ReembedService) DelayAfterHeal(result HealResult) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if result.Kind != HealProviderUnavailable {
		s.consecutiveFailedHeals = 0
		return s.Config.HealInterval
	}
	s.consecutiveFailedHeals++
	exponent := min(s.consecutiveFailedHeals-1, 16)
	delay := min(s.Config.HealInterval*time.Duration(1<<exponent), s.Config.HealMaxBackoff)
	s.Logger.Warn(fmt.Sprintf("embedding.heal next probe in %s after %d refused passes", delay, s.consecutiveFailedHeals))
	return delay
}

type reembedRow struct {
	id     string
	tenant uuid.UUID
	text   string
}

func (s *ReembedService) fetchBatch(ctx context.Context, target ReembedTarget, cursor *string, missing bool) ([]reembedRow, error) {
	filter := "embedding IS NOT NULL"
	if missing {
		filter = "embedding IS NULL AND btrim(" + target.TextColumn + ") <> ''"
	}
	var (
		rows pgx.Rows
		err  error
	)
	if cursor != nil {
		rows, err = s.Pool.Query(ctx, fmt.Sprintf(`
SELECT %[1]s::text AS id, tenant_id, %[2]s AS text
FROM %[3]s
WHERE %[4]s
  AND %[1]s > $1::%[5]s
ORDER BY %[1]s
LIMIT $2`, target.IDColumn, target.TextColumn, target.Table, filter, target.IDType), *cursor, s.Config.BatchSize)
	} else {
		rows, err = s.Pool.Query(ctx, fmt.Sprintf(`
SELECT %[1]s::text AS id, tenant_id, %[2]s AS text
FROM %[3]s
WHERE %[4]s
ORDER BY %[1]s
LIMIT $1`, target.IDColumn, target.TextColumn, target.Table, filter), s.Config.BatchSize)
	}
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (reembedRow, error) {
		var row reembedRow
		err := r.Scan(&row.id, &row.tenant, &row.text)
		return row, err
	})
}

func (s *ReembedService) embedAndWrite(ctx context.Context, target ReembedTarget, row reembedRow, onlyIfMissing bool) (bool, error) {
	vector, err := s.Embeddings.Embed(ctx, row.text, row.tenant)
	if err != nil {
		return false, err
	}
	if len(vector) != Dimensions {
		return false, &Error{Kind: FailureDimMismatch, Expected: Dimensions, Got: len(vector)}
	}
	missingGuard := ""
	if onlyIfMissing {
		missingGuard = "AND embedding IS NULL"
	}
	// The text predicate keeps a row edited after it was read on the vector
	// its writer stored.
	var id string
	err = s.Pool.QueryRow(ctx, fmt.Sprintf(`
UPDATE %[1]s
SET embedding = $1::vector
WHERE %[2]s = $2::%[3]s
  AND tenant_id = $3
  AND %[4]s = $4
  %[5]s
RETURNING %[2]s::text`, target.Table, target.IDColumn, target.IDType, target.TextColumn, missingGuard),
		FormatVector(vector), row.id, row.tenant, row.text).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *ReembedService) advance(ctx context.Context, target ReembedTarget, lastID string, done, failed int) error {
	_, err := s.Pool.Exec(ctx, `
INSERT INTO embedding_reembed_progress (target, last_id, rows_done, rows_failed, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (target) DO UPDATE
SET last_id = EXCLUDED.last_id,
    rows_done = embedding_reembed_progress.rows_done + EXCLUDED.rows_done,
    rows_failed = embedding_reembed_progress.rows_failed + EXCLUDED.rows_failed,
    updated_at = now()`, target.Name, lastID, int64(done), int64(failed))
	return err
}

func (s *ReembedService) markComplete(ctx context.Context, target ReembedTarget) error {
	if _, err := s.Pool.Exec(ctx, `
INSERT INTO embedding_reembed_progress (target, completed_at, updated_at)
VALUES ($1, now(), now())
ON CONFLICT (target) DO UPDATE
SET completed_at = now(), updated_at = now()`, target.Name); err != nil {
		return err
	}
	if target.ClearsGraphLayout {
		_, err := s.Pool.Exec(ctx, `
UPDATE memories
SET graph_x = NULL, graph_y = NULL, graph_z = NULL, graph_layout_at = NULL
WHERE graph_x IS NOT NULL`)
		return err
	}
	return nil
}
