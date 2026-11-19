package vaultgit

import (
	"context"
	"errors"
	"hash/fnv"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Debounce defaults (spec D1): commit 5 s after the last touch, or at most 30 s
// after the first.
const (
	DefaultDebounce = 5 * time.Second
	DefaultMaxWait  = 30 * time.Second
)

// Locker serializes commits on one tenant across API replicas (spec D3). Lock
// returns ok=false when another replica holds it.
type Locker interface {
	TryLock(ctx context.Context, tenant uuid.UUID) (unlock func(), ok bool, err error)
}

// SpaceCheck is diskguard.Guard.Check: a non-nil error skips the commit.
type SpaceCheck func(incomingBytes int64) error

// Options configure a Committer. RawRoot is required; the rest have defaults.
type Options struct {
	// RawRoot is vaultpaths.Paths.Raw: the tenant's working tree.
	RawRoot func(tenant uuid.UUID) string
	// MaxFileSize is the per-tenant history limit (owner answer Q2). Nil means DefaultMaxFileSize.
	MaxFileSize func(tenant uuid.UUID) int64
	Debounce    time.Duration
	MaxWait     time.Duration
	Lock        Locker     // nil: in-process serialization only
	Space       SpaceCheck // nil: no disk check
	Logger      *slog.Logger
	Now         func() time.Time
}

// Committer turns touches into commits, one tenant at a time.
type Committer struct {
	opts Options

	mu      sync.Mutex
	pending map[uuid.UUID]*burst
	tenant  map[uuid.UUID]*sync.Mutex
	closed  bool
	wg      sync.WaitGroup
}

type burst struct {
	first   time.Time
	timer   *time.Timer
	reason  string
	reasons map[string]struct{}
	actor   string
	source  string
}

// NewCommitter fills in defaults.
func NewCommitter(opts Options) *Committer {
	if opts.Debounce <= 0 {
		opts.Debounce = DefaultDebounce
	}
	if opts.MaxWait <= 0 {
		opts.MaxWait = DefaultMaxWait
	}
	if opts.MaxFileSize == nil {
		opts.MaxFileSize = func(uuid.UUID) int64 { return DefaultMaxFileSize }
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Committer{opts: opts, pending: map[uuid.UUID]*burst{}, tenant: map[uuid.UUID]*sync.Mutex{}}
}

// Touch records that tenant's vault changed. It never blocks on git: the commit
// runs after the debounce, on its own goroutine.
func (c *Committer) Touch(tenant uuid.UUID, reason, actor, source string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	now := c.opts.Now()
	b := c.pending[tenant]
	if b == nil {
		b = &burst{first: now, reasons: map[string]struct{}{}}
		c.pending[tenant] = b
		b.timer = time.AfterFunc(c.opts.Debounce, func() { c.fire(tenant) })
	} else {
		wait := c.opts.Debounce
		if deadline := b.first.Add(c.opts.MaxWait); now.Add(wait).After(deadline) {
			wait = max(0, deadline.Sub(now))
		}
		b.timer.Reset(wait)
	}
	if reason != "" {
		b.reasons[reason] = struct{}{}
		b.reason = reason
	}
	if actor != "" {
		b.actor = actor
	}
	if source != "" {
		b.source = source
	}
}

func (c *Committer) fire(tenant uuid.UUID) {
	c.mu.Lock()
	b := c.pending[tenant]
	delete(c.pending, tenant)
	if b == nil || c.closed {
		c.mu.Unlock()
		return
	}
	c.wg.Add(1)
	c.mu.Unlock()
	defer c.wg.Done()
	if _, err := c.Flush(context.Background(), tenant, b.message()); err != nil {
		c.opts.Logger.Warn("vault git commit failed", "tenant", strings.ToUpper(tenant.String()), "error", err)
	}
}

// message is one reason, or "mixed" when the burst saw several.
func (b *burst) message() Message {
	reason := b.reason
	if len(b.reasons) > 1 {
		reason = "mixed"
	}
	if reason == "" {
		reason = "sweep"
	}
	return Message{Reason: reason, Actor: b.actor, Source: b.source}
}

func (c *Committer) tenantLock(tenant uuid.UUID) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.tenant[tenant]
	if l == nil {
		l = &sync.Mutex{}
		c.tenant[tenant] = l
	}
	return l
}

// ErrLocked means another replica is committing for this tenant; the sweep retries.
var ErrLocked = errors.New("vaultgit: tenant locked by another replica")

// Flush commits tenant's pending changes now, creating the repository on first
// use (spec D5). It returns the new commit, or "" when nothing changed or the
// disk check skipped it.
func (c *Committer) Flush(ctx context.Context, tenant uuid.UUID, msg Message) (string, error) {
	l := c.tenantLock(tenant)
	l.Lock()
	defer l.Unlock()
	if c.opts.Lock != nil {
		unlock, ok, err := c.opts.Lock.TryLock(ctx, tenant)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", ErrLocked
		}
		defer unlock()
	}
	if c.opts.Space != nil {
		if err := c.opts.Space(0); err != nil {
			c.opts.Logger.Warn("vault git commit skipped: low disk", "tenant", strings.ToUpper(tenant.String()))
			return "", nil
		}
	}
	limit := c.opts.MaxFileSize(tenant)
	repo, _, err := Init(c.opts.RawRoot(tenant), limit)
	if err != nil {
		return "", err
	}
	sha, _, err := repo.Commit(msg, limit, c.opts.Now())
	return sha, err
}

// Close stops new touches, commits every pending burst now and waits for
// in-flight commits, so a shutdown loses nothing it was told about.
func (c *Committer) Close(ctx context.Context) {
	c.mu.Lock()
	c.closed = true
	bursts := c.pending
	c.pending = map[uuid.UUID]*burst{}
	c.mu.Unlock()
	for tenant, b := range bursts {
		b.timer.Stop()
		if _, err := c.Flush(ctx, tenant, b.message()); err != nil {
			c.opts.Logger.Warn("vault git commit on shutdown failed", "tenant", strings.ToUpper(tenant.String()), "error", err)
		}
	}
	c.wg.Wait()
}

// Toucher receives the package-level Touch. *Committer is the real one; tests
// install a recorder through vaultgittest.
type Toucher interface {
	Touch(tenant uuid.UUID, reason, actor, source string)
}

// target wraps the Toucher so atomic.Pointer can hold an interface.
type target struct{ t Toucher }

// active is what Touch reports to. Nil (tests, tools, the flag off) makes Touch
// a no-op.
var active atomic.Pointer[target]

// Start makes c the target of the package-level Touch. Pass nil to stop.
func Start(c *Committer) {
	if c == nil {
		active.Store(nil)
		return
	}
	active.Store(&target{t: c})
}

// StartToucher is Start for any Toucher. Pass nil to stop.
func StartToucher(t Toucher) {
	if t == nil {
		active.Store(nil)
		return
	}
	active.Store(&target{t: t})
}

// Touch is the hook every vault writer calls after a write (spec D1). It is
// cheap and never fails; without a started Committer it does nothing.
func Touch(tenant uuid.UUID, reason, actor, source string) {
	if a := active.Load(); a != nil {
		a.t.Touch(tenant, reason, actor, source)
	}
}

// PGLocker is the Postgres session advisory lock keyed by tenant (spec D3).
type PGLocker struct{ Pool *pgxpool.Pool }

// lockKey namespaces the key so it can't collide with other advisory locks.
func lockKey(tenant uuid.UUID) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("vaultgit:"))
	_, _ = h.Write(tenant[:])
	return int64(h.Sum64()) //nolint:gosec // a hash; wrapping is fine
}

// TryLock takes pg_try_advisory_lock on a dedicated connection, held until unlock.
func (l PGLocker) TryLock(ctx context.Context, tenant uuid.UUID) (func(), bool, error) {
	conn, err := l.Pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	key := lockKey(tenant)
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key)
		conn.Release()
	}, true, nil
}
