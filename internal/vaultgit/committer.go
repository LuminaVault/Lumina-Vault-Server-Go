package vaultgit

import (
	"context"
	"errors"
	"fmt"
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
	// OnCommit is called after a Flush that made a commit (the first commit of
	// a new repository included), once both locks are released. It is never
	// called for a flush that recorded nothing. It must not block: GitHub sync
	// only queues a push from it (sync spec D5, D6). Nil by default.
	OnCommit func(tenant uuid.UUID, sha string)
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
	_, err := c.Flush(context.Background(), tenant, b.message())
	switch {
	case errors.Is(err, ErrLocked):
		// Another replica holds the tenant: a commit, a gc, or a push to
		// GitHub, which can take a minute. Try again after another debounce
		// rather than leave the change to the sweep.
		c.requeue(tenant, b)
	case err != nil:
		c.opts.Logger.Warn("vault git commit failed", "tenant", strings.ToUpper(tenant.String()), "error", err)
	}
}

// requeue puts a burst whose commit found the tenant locked back in line.
// Touches that arrived meanwhile already made a new burst, which will commit
// the same tree; then there is nothing to add.
func (c *Committer) requeue(tenant uuid.UUID, b *burst) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.pending[tenant] != nil {
		return
	}
	b.first = c.opts.Now()
	b.timer = time.AfterFunc(c.opts.Debounce, func() { c.fire(tenant) })
	c.pending[tenant] = b
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
	sha, made, err := c.flush(ctx, tenant, msg)
	// After flush returned, so after its deferred unlocks: the hook may take
	// the tenant's lock itself.
	if made != "" && c.opts.OnCommit != nil {
		c.opts.OnCommit(tenant, made)
	}
	return sha, err
}

// flush is Flush under the tenant's locks. made is the commit OnCommit is
// told about: sha, or the initial import when this call created the
// repository and had nothing more to record.
func (c *Committer) flush(ctx context.Context, tenant uuid.UUID, msg Message) (sha, made string, err error) {
	unlock, err := c.lock(ctx, tenant)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	if c.opts.Space != nil {
		if err := c.opts.Space(0); err != nil {
			c.opts.Logger.Warn("vault git commit skipped: low disk", "tenant", strings.ToUpper(tenant.String()))
			return "", "", nil
		}
	}
	limit := c.opts.MaxFileSize(tenant)
	repo, created, err := Init(c.opts.RawRoot(tenant), limit)
	if err != nil {
		return "", "", err
	}
	sha, _, err = repo.Commit(msg, limit, c.opts.Now())
	if err != nil {
		return "", "", err
	}
	made = sha
	if made == "" && created {
		if made, err = repo.Head(); err != nil {
			return "", "", err
		}
	}
	return sha, made, nil
}

// lock takes the tenant's in-process mutex and, when configured, its Postgres
// advisory lock. ErrLocked means another replica holds the latter.
func (c *Committer) lock(ctx context.Context, tenant uuid.UUID) (unlock func(), err error) {
	unlock, _, err = c.lockChecked(ctx, tenant)
	return unlock, err
}

// lockChecked is lock, plus a way to ask later whether the lock is still
// held (nil when the Locker cannot say, or there is none).
func (c *Committer) lockChecked(ctx context.Context, tenant uuid.UUID) (unlock func(), alive func(context.Context) error, err error) {
	l := c.tenantLock(tenant)
	l.Lock()
	if c.opts.Lock == nil {
		return l.Unlock, nil, nil
	}
	var (
		release func()
		ok      bool
	)
	if checked, can := c.opts.Lock.(CheckedLocker); can {
		release, alive, ok, err = checked.TryLockChecked(ctx, tenant)
	} else {
		release, ok, err = c.opts.Lock.TryLock(ctx, tenant)
	}
	if err != nil {
		l.Unlock()
		return nil, nil, err
	}
	if !ok {
		l.Unlock()
		return nil, nil, ErrLocked
	}
	return func() {
		release()
		l.Unlock()
	}, alive, nil
}

// WithRepo runs fn on tenant's repository under the same in-process mutex and
// Postgres advisory lock Flush and GC use, so a push, a pull, a commit and a
// gc never overlap on one tenant (spec D3; sync spec D5). It is the only way
// GitHub sync touches a repository. It is not re-entrant: fn must not call
// Flush or WithRepo for the same tenant. A tenant with no repository yet is
// ErrNoRepo; another replica holding the lock is ErrLocked.
func (c *Committer) WithRepo(ctx context.Context, tenant uuid.UUID, fn func(*Repo) error) error {
	unlock, alive, err := c.lockChecked(ctx, tenant)
	if err != nil {
		return err
	}
	defer unlock()
	repo, err := Open(c.opts.RawRoot(tenant))
	if err != nil {
		return err
	}
	repo.alive = alive
	return fn(repo)
}

// ErrLockLost means the tenant's lock went away while it was thought held:
// its database session died. Whatever was about to be written must not be.
var ErrLockLost = errors.New("vaultgit: the vault lock was lost")

// StillLocked reports, inside WithRepo, whether the tenant's cross-replica
// lock is still held. A session advisory lock dies silently with its
// connection, and another replica may then be working on the same
// repository: a long operation asks before it writes. Outside WithRepo, or
// with no cross-replica lock, it is nil.
func (r *Repo) StillLocked(ctx context.Context) error {
	if r.alive == nil {
		return nil
	}
	if err := r.alive(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrLockLost, err)
	}
	return nil
}

// MaxFileSize is tenant's history limit, as Flush reads it.
func (c *Committer) MaxFileSize(tenant uuid.UUID) int64 { return c.opts.MaxFileSize(tenant) }

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

// CheckedLocker is a Locker that can say later whether a lock it gave is
// still held.
type CheckedLocker interface {
	Locker
	TryLockChecked(ctx context.Context, tenant uuid.UUID) (unlock func(), alive func(context.Context) error, ok bool, err error)
}

// TryLock takes pg_try_advisory_lock on a dedicated connection, held until unlock.
func (l PGLocker) TryLock(ctx context.Context, tenant uuid.UUID) (func(), bool, error) {
	unlock, _, ok, err := l.TryLockChecked(ctx, tenant)
	return unlock, ok, err
}

// TryLockChecked is TryLock, and alive asks the lock's own session whether it
// still holds the lock. The lock belongs to that session: when the connection
// dies the lock is gone without a word, and alive then fails.
func (l PGLocker) TryLockChecked(ctx context.Context, tenant uuid.UUID) (func(), func(context.Context) error, bool, error) {
	conn, err := l.Pool.Acquire(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	key := lockKey(tenant)
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&ok); err != nil {
		conn.Release()
		return nil, nil, false, err
	}
	if !ok {
		conn.Release()
		return nil, nil, false, nil
	}
	alive := func(ctx context.Context) error {
		var held bool
		// The key's two halves, as pg_locks shows a bigint advisory lock.
		err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory'
			AND pid = pg_backend_pid() AND classid = ($1::bigint >> 32)::int::oid AND objid = ($1::bigint & 4294967295)::oid AND objsubid = 1)`, key).Scan(&held)
		if err != nil {
			return err
		}
		if !held {
			return errors.New("the session no longer holds the advisory lock")
		}
		return nil
	}
	return func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key)
		conn.Release()
	}, alive, true, nil
}
