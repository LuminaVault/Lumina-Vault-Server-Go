package workers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/embedding"
	"lumina/internal/ids"
	"lumina/internal/jobs"
	"lumina/internal/keepjudge"
	"lumina/internal/memorystore"
	"lumina/internal/swiftstr"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MemoryIndexTick is MemoryIndexWorker.tickInterval.
const MemoryIndexTick = 2 * time.Second

// memoryIndexMaxChars is the prefix(12000) Swift stores and embeds.
const memoryIndexMaxChars = 12000

// sourceKinds is MemorySourceKindDTO's raw values; anything else is .chat.
var sourceKinds = map[string]bool{
	memorystore.SourceManual: true, memorystore.SourceChat: true, memorystore.SourceQuery: true,
	memorystore.SourceKnowledgeCompile: true, memorystore.SourceSkill: true, memorystore.SourceVault: true,
	memorystore.SourceImport: true, memorystore.SourceLink: true, memorystore.SourceLegacy: true,
}

// KeepJudge is MemoryKeepJudge.
type KeepJudge interface {
	Judge(ctx context.Context, content string, tenant uuid.UUID) (keepjudge.Decision, error)
}

// MemoryIndex is MemoryIndexWorker (Workers/MemoryIndexWorker.swift): the durable
// write-behind that turns a final model answer queued in memory_index_jobs
// (memorystore.EnqueueOutput) into a memory with an embedding. One job is claimed
// per tick with FOR UPDATE SKIP LOCKED, so two processes are safe.
type MemoryIndex struct {
	Pool       *pgxpool.Pool
	Embeddings embedding.Service
	Memories   *memorystore.Repository
	// Judge, when set, stores a reply only if it's durable
	// (MEMORY_KEEP_GATE_ENABLED). Nil keeps every reply.
	Judge KeepJudge
	Log   *slog.Logger
}

type memoryIndexJob struct {
	id                    uuid.UUID
	tenant                uuid.UUID
	sourceKind            string
	sourceID              string
	conversationMessageID *uuid.UUID
	content               string
	provider              *string
	model                 *string
	attempts              int
}

// Job runs ticks back to back while there's work and sleeps MemoryIndexTick when
// the queue is empty, as Swift's run loop.
func (w *MemoryIndex) Job() jobs.Job {
	return jobs.Job{
		Name:     "MemoryIndexWorker",
		Schedule: jobs.Every(MemoryIndexTick, 0),
		Run: func(ctx context.Context) error {
			for ctx.Err() == nil {
				n, err := w.Tick(ctx)
				if err != nil {
					w.Log.Warn(fmt.Sprintf("memory.index.worker tick failed: %v", err))
					return err
				}
				if n == 0 {
					return nil
				}
			}
			return nil
		},
	}
}

// Tick claims and handles one job. It returns 0 when none was due.
func (w *MemoryIndex) Tick(ctx context.Context) (int, error) {
	var j memoryIndexJob
	err := w.Pool.QueryRow(ctx, `
		WITH candidate AS (
			SELECT id FROM memory_index_jobs
			WHERE status IN ('pending', 'retry') AND next_attempt_at <= NOW()
			ORDER BY created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE memory_index_jobs j
		SET status = 'processing', attempts = attempts + 1, updated_at = NOW()
		FROM candidate
		WHERE j.id = candidate.id
		RETURNING j.id, j.tenant_id, j.source_kind, j.source_id,
		          j.conversation_message_id, j.content, j.provider, j.model, j.attempts`).
		Scan(&j.id, &j.tenant, &j.sourceKind, &j.sourceID, &j.conversationMessageID,
			&j.content, &j.provider, &j.model, &j.attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	decision := w.decide(ctx, j)
	if !decision.Keep {
		if _, err := w.Pool.Exec(ctx, `
			UPDATE memory_index_jobs
			SET status = 'skipped', last_error = NULL, updated_at = NOW()
			WHERE id = $1`, j.id); err != nil {
			return 1, w.retry(ctx, j, err)
		}
		w.Log.Info(fmt.Sprintf("memory.keep.skipped job=%s tenant=%s", ids.Upper(j.id), ids.Upper(j.tenant)))
		return 1, nil
	}
	if err := w.index(ctx, j, decision); err != nil {
		return 1, w.retry(ctx, j, err)
	}
	return 1, nil
}

// decide is the judge's verdict, failing open: an error keeps the reply.
func (w *MemoryIndex) decide(ctx context.Context, j memoryIndexJob) keepjudge.Decision {
	if w.Judge == nil {
		return keepjudge.KeepUnclassified
	}
	d, err := w.Judge.Judge(ctx, j.content, j.tenant)
	if err != nil {
		w.Log.Warn(fmt.Sprintf("memory.keep.judge.failed job=%s: %v", ids.Upper(j.id), err))
		return keepjudge.KeepUnclassified
	}
	return d
}

func (w *MemoryIndex) index(ctx context.Context, j memoryIndexJob, d keepjudge.Decision) error {
	var existing uuid.UUID
	err := w.Pool.QueryRow(ctx, `
		SELECT id FROM memories
		WHERE tenant_id = $1 AND origin_kind = $2 AND origin_source_id = $3
		LIMIT 1`, j.tenant, j.sourceKind, j.sourceID).Scan(&existing)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		content := swiftstr.Prefix(j.content, memoryIndexMaxChars)
		vector, err := w.Embeddings.Embed(ctx, content, j.tenant)
		if err != nil {
			return err
		}
		source := j.sourceKind
		if !sourceKinds[source] {
			source = memorystore.SourceChat
		}
		ref := j.sourceID
		contribution := memorystore.ModelContribution(memorystore.OperationCreate, source, j.provider, j.model, &ref)
		var kind *string
		if d.Kind != "" {
			kind = &d.Kind
		}
		if _, err := w.Memories.Create(ctx, memorystore.CreateParams{
			Tenant:                      j.tenant,
			Content:                     content,
			Embedding:                   vector,
			Tags:                        []string{"model-output"},
			ReviewState:                 memorystore.ReviewAuto,
			Contribution:                &contribution,
			OriginConversationMessageID: j.conversationMessageID,
			Kind:                        kind,
		}); err != nil {
			return err
		}
	case err != nil:
		return err
	}
	_, err = w.Pool.Exec(ctx, `
		UPDATE memory_index_jobs
		SET status = 'completed', last_error = NULL, updated_at = NOW()
		WHERE id = $1`, j.id)
	return err
}

// retry backs off 2^attempts seconds, at most an hour, as Swift. A failed retry
// write fails the tick.
func (w *MemoryIndex) retry(ctx context.Context, j memoryIndexJob, cause error) error {
	delay := min(3600, 1<<min(j.attempts, 10))
	if _, err := w.Pool.Exec(ctx, `
		UPDATE memory_index_jobs
		SET status = 'retry', last_error = $2,
		    next_attempt_at = NOW() + ($3 * INTERVAL '1 second'),
		    updated_at = NOW()
		WHERE id = $1`, j.id, swiftstr.Prefix(fmt.Sprint(cause), 500), delay); err != nil {
		return err
	}
	w.Log.Warn("memory output indexing deferred", "job_id", ids.Upper(j.id), "attempt", j.attempts)
	return nil
}
