package memorystore

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bumpTimeout bounds one queued UPDATE. The queue's work outlives the request that
// asked for it, so it cannot use the request's context.
const bumpTimeout = 30 * time.Second

// BumpQueryHits is MemoryRepository.bumpQueryHits: increments query_hit_count and
// stamps last_accessed_at for every id, in one statement.
func BumpQueryHits(ctx context.Context, pool *pgxpool.Pool, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := pool.Exec(ctx, `
		UPDATE memories
		SET query_hit_count = query_hit_count + 1,
		    last_accessed_at = NOW()
		WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return fmt.Errorf("memorystore: bump query hits: %w", err)
	}
	return nil
}

// HitCountQueue is MemoryHitCountUpdateQueue: it coalesces hit-count bumps so search
// never blocks on the UPDATE. An id enqueued twice before a batch is taken is bumped
// once. Across the two servers it is harmless, since the increments are relative.
// Failed bumps are dropped.
type HitCountQueue struct {
	pool *pgxpool.Pool

	mu      sync.Mutex
	pending []uuid.UUID
	worker  chan struct{} // closed when the current worker has finished; nil if idle
}

// NewHitCountQueue builds a queue. Run it for the life of the process.
func NewHitCountQueue(pool *pgxpool.Pool) *HitCountQueue {
	return &HitCountQueue{pool: pool}
}

// Enqueue is enqueue(ids:). It starts a worker if none is running and never blocks
// on the database.
func (q *HitCountQueue) Enqueue(ids []uuid.UUID) {
	if len(ids) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, ids...)
	if q.worker == nil {
		done := make(chan struct{})
		q.worker = done
		go q.process(done)
	}
}

// Drain waits until nothing is pending or in flight, or ctx ends.
func (q *HitCountQueue) Drain(ctx context.Context) {
	for {
		q.mu.Lock()
		worker := q.worker
		q.mu.Unlock()
		if worker == nil {
			return
		}
		select {
		case <-worker:
		case <-ctx.Done():
			return
		}
	}
}

// Run is the Service half: it holds until ctx ends, then drains, so a graceful
// shutdown finishes what it started.
func (q *HitCountQueue) Run(ctx context.Context) error {
	<-ctx.Done()
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bumpTimeout)
	defer cancel()
	q.Drain(drainCtx)
	return nil
}

func (q *HitCountQueue) process(done chan struct{}) {
	defer close(done)
	for {
		ids := q.takeBatch()
		if len(ids) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), bumpTimeout)
		_ = BumpQueryHits(ctx, q.pool, ids)
		cancel()
	}
}

// takeBatch empties pending, de-duplicated. Empty pending retires the worker under
// the same lock, so an Enqueue cannot slip between the check and the exit.
func (q *HitCountQueue) takeBatch() []uuid.UUID {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		q.worker = nil
		return nil
	}
	seen := make(map[uuid.UUID]struct{}, len(q.pending))
	ids := make([]uuid.UUID, 0, len(q.pending))
	for _, id := range q.pending {
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	q.pending = nil
	return ids
}
