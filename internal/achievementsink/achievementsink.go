// Package achievementsink is the Go half of Swift's AchievementsWorker inbox
// (Sources/App/Achievements/AchievementsWorker.swift). Handlers call Enqueue,
// which never touches the DB or waits. Run drains the inbox: it records each
// event and pushes every unlock through APNS.
//
// Progress lands in Postgres, so a Go producer and a Swift producer feed the same
// counters. Increments aren't transactional in either server (see
// achievements.Recorder); a lost increment is accepted there and here.
package achievementsink

import (
	"context"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/modules/achievements"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	defaultBufferSize = 256
	// shutdownBudget bounds the final drain, which Swift doesn't.
	shutdownBudget = 10 * time.Second
)

// Recorder is achievements.Recorder.
type Recorder interface {
	Record(ctx context.Context, tenant uuid.UUID, e achievements.Event) ([]achievements.Sub, error)
}

// Pusher is apns.Service.
type Pusher interface {
	NotifyAchievement(ctx context.Context, tenant uuid.UUID, key, label string) error
}

// Options configures the sink.
type Options struct {
	// BufferSize is the inbox size; Swift's is 256.
	BufferSize int
	// OnUnlock runs for each new unlock, where Swift publishes EventBus
	// achievement_unlocked. Its one real subscriber is MeTodayCache, so pass
	// me.Module.InvalidateToday's caller here if me's cache still needs it
	// (flip-blockers.md). It must not block.
	OnUnlock func(tenant uuid.UUID, sub achievements.Sub)
}

type job struct {
	tenant uuid.UUID
	event  achievements.Event
}

// Sink is the bounded inbox. A nil *Sink ignores everything.
type Sink struct {
	recorder Recorder
	pusher   Pusher
	onUnlock func(uuid.UUID, achievements.Sub)
	size     int

	mu      sync.Mutex
	queue   []job
	stopped bool
	wake    chan struct{}
}

// New builds the sink. pusher may be nil, which records without pushing.
func New(recorder Recorder, pusher Pusher, opts Options) *Sink {
	size := opts.BufferSize
	if size <= 0 {
		size = defaultBufferSize
	}
	return &Sink{recorder: recorder, pusher: pusher, onUnlock: opts.OnUnlock, size: size, wake: make(chan struct{}, 1)}
}

// Enqueue never blocks, starts no goroutine and touches no database. When the
// inbox is full the oldest job is dropped, as AsyncStream's .bufferingNewest
// does; once the sink has stopped, the event is dropped.
func (s *Sink) Enqueue(tenant uuid.UUID, event achievements.Event) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		slog.Debug("achievements worker stopped; dropped event", "event", string(event))
		return
	}
	dropped := len(s.queue) >= s.size
	if dropped {
		s.queue = s.queue[1:]
	}
	s.queue = append(s.queue, job{tenant, event})
	s.mu.Unlock()
	if dropped {
		slog.Warn("achievements inbox full; dropped oldest event", "incoming", string(event))
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run drains the inbox until ctx is cancelled, then stops accepting and finishes
// what is queued under its own deadline. Cancel ctx after the HTTP server has
// stopped and before the database pool closes.
func (s *Sink) Run(ctx context.Context) {
	if s == nil {
		return
	}
	for {
		s.drain(ctx)
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.stopped = true
			s.mu.Unlock()
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownBudget)
			defer cancel()
			s.drain(final)
			return
		case <-s.wake:
		}
	}
}

func (s *Sink) drain(ctx context.Context) {
	for ctx.Err() == nil {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.mu.Unlock()
			return
		}
		next := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		s.recordAndPush(ctx, next)
	}
}

// recordAndPush never fails: errors are logged, and one failed push doesn't stop
// the next.
func (s *Sink) recordAndPush(ctx context.Context, j job) {
	unlocks, err := s.recorder.Record(ctx, j.tenant, j.event)
	if err != nil {
		slog.WarnContext(ctx, "achievements record failed", "event", string(j.event), "tenant", ids.Upper(j.tenant), "error", err)
		return
	}
	for _, sub := range unlocks {
		slog.InfoContext(ctx, "achievements.unlocked", "tenant", ids.Upper(j.tenant), "key", sub.Key)
		if s.onUnlock != nil {
			s.onUnlock(j.tenant, sub)
		}
	}
	if s.pusher == nil {
		return
	}
	for _, sub := range unlocks {
		if err := s.pusher.NotifyAchievement(ctx, j.tenant, sub.Key, sub.Label); err != nil {
			slog.WarnContext(ctx, "achievements push failed", "key", sub.Key, "error", err)
		}
	}
}
