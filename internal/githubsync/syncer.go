package githubsync

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/githubapp"
	"lumina/internal/jobs"
	"lumina/internal/vaultgit"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Connection error codes (D6's table, D14). The first two are retried and
// leave the state active; the rest need a person and stop retries.
const (
	// CodeSyncFailed is any transient failure: network, a 5xx, a timeout, a
	// remote that moved while no pull is available.
	CodeSyncFailed = "sync_failed"
	// CodeRateLimited is GitHub's rate limit. It is retried when GitHub says,
	// and "Sync now" does not cut that wait short.
	CodeRateLimited           = "github_rate_limited"
	CodeInstallationRemoved   = "installation_removed"
	CodeInstallationSuspended = "installation_suspended"
	CodeRepoAccessRemoved     = "repo_access_removed"
	CodePushRejected          = "push_rejected"
	CodeRepoTooLarge          = "repo_too_large"
	// CodeRemoteNotEmpty: the branch has files and a history the vault shares
	// nothing with, and the owner did not agree to merge them (D11). It is
	// the code the connect route refuses such a branch with; here the files
	// turned up between that check and the first sync. Only a reconnect,
	// which asks the question, clears it.
	CodeRemoteNotEmpty = "remote_not_empty"
)

// retryableByOwner are the stopped states "Sync now" may try again: the cause
// may have been fixed on GitHub without the connection changing. An
// uninstalled App is not among them: installing it again makes a new
// installation, which only a reconnect can learn.
var retryableByOwner = []string{CodePushRejected, CodeRepoTooLarge, CodeInstallationSuspended, CodeRepoAccessRemoved}

// backoff is D6's schedule for transient failures; after it, hourly.
var backoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

const (
	backoffCap = time.Hour
	// maxRounds bounds push, pull, push again within one attempt (D6).
	maxRounds = 3
	// confirmAfter: an answer GitHub documents ambiguously (a 422 or 403
	// minting a token, a 403 on the push) stops retries only when it is the
	// answer this many attempts in a row. See mintOutcome.
	confirmAfter       = 3
	defaultLockedRetry = 5 * time.Second
	// maxWake: a retry further away than this is left to the sweep.
	maxWake = 10 * time.Minute
	// SweepInterval is how often GitHubSyncSweep runs (D6).
	SweepInterval = 5 * time.Minute

	// DefaultPollInterval is GITHUB_SYNC_POLL_INTERVAL's default (D9).
	DefaultPollInterval = 15 * time.Minute
	// requeueAfter: a delivery still queued this long after it arrived is
	// queued again by the sweep (D9).
	requeueAfter = time.Minute
	// deliveryRetention is how long GitHubSyncPurge keeps a delivery row.
	deliveryRetention = 7 * 24 * time.Hour

	defaultPushTimeout = 60 * time.Second
	// pushRate is the slowest upload a push is given time for: its deadline
	// is PushTimeout plus the repository's size at this rate.
	pushRate           = 1 << 20 // bytes per second
	defaultLockTimeout = 5 * time.Minute
	defaultConcurrency = 4
	disconnectWait     = 2 * time.Second
	disconnectTries    = 60
)

// GitHub is what the syncer needs from githubapp.Client.
type GitHub interface {
	InstallationToken(ctx context.Context, installation, repo int64) (string, error)
	RevokeInstallationToken(ctx context.Context, installation, repo int64) error
	RepositoryByID(ctx context.Context, token string, id int64) (githubapp.Repository, error)
}

// Repos is (*vaultgit.Committer).WithRepo: the only way sync touches a
// repository (D5).
type Repos interface {
	WithRepo(ctx context.Context, tenant uuid.UUID, fn func(*vaultgit.Repo) error) error
}

// PullInput is what a pull's locked phases get: the repository, inside the
// vault's lock.
type PullInput struct {
	Repo       *vaultgit.Repo
	Connection Connection
	Token      string
}

// PullFunc is a Puller that does all its work in one call inside the vault's
// lock and leaves nothing to apply. It is for tests of the syncer's rounds;
// the real one is Pull (pull.go).
type PullFunc func(ctx context.Context, in PullInput) error

func (f PullFunc) Plan(ctx context.Context, in PullInput) (*Plan, error) { return nil, f(ctx, in) }
func (PullFunc) Apply(context.Context, *Plan) error                      { return nil }
func (PullFunc) Finish(context.Context, PullInput, *Plan) error          { return nil }

// SyncerOptions configure a Syncer. Store, GitHub, Repos, RawRoot and
// GitBaseURL are required.
type SyncerOptions struct {
	Store  *Store
	GitHub GitHub
	Repos  Repos
	// RawRoot is vaultpaths.Paths.Raw, for a look at HEAD without the lock.
	RawRoot func(vault uuid.UUID) string
	// GitBaseURL is https://github.com.
	GitBaseURL string
	// Pull brings the remote branch into the vault (D10): when a push finds
	// the remote moved (D6), and when a webhook, the poll or "Sync now" asks.
	// Nil: a push that is not a fast-forward is a transient failure, nothing
	// is ever pulled, and nothing is forced.
	Pull Puller
	// MaxRepoBytes is GITHUB_SYNC_MAX_REPO_MB (D11), checked before every
	// fetch against the size GitHub reports, and against the pack of every
	// push before it is sent: go-git holds that pack in memory. Zero: no check.
	MaxRepoBytes int64
	// PollInterval is how often PollJob asks every remote for its tip (D9).
	PollInterval time.Duration
	// LockTimeout bounds one locked phase of a pull beyond its fetch: the
	// vault's lock, a pinned database connection and a worker slot are held
	// that long at most (default 5 min).
	LockTimeout time.Duration
	// PushTimeout is GITHUB_SYNC_PUSH_TIMEOUT: what one push may take (default
	// 60 s), plus a second for every MiB of objects the repository holds, up
	// to MaxRepoBytes: a first push of a large vault gets the time it needs.
	// It also bounds the question "where is the remote's tip", unscaled.
	PushTimeout time.Duration
	// Concurrency bounds pushes in flight in this process (default 4). Each
	// pins one database connection for the vault's advisory lock for as long
	// as it runs, and needs a second for its row reads and writes: it must
	// stay well under the pool's size, or pushes starve the API, and at the
	// pool's size they wait on each other for ever.
	Concurrency int
	// LockedRetry is how soon a vault whose lock another replica holds is
	// looked at again (default 5 s). That is not a failure and is not recorded.
	LockedRetry time.Duration
	Now         func() time.Time
	Log         *slog.Logger
}

// Syncer pushes vault commits to GitHub (D6). Per vault at most one sync runs
// and at most one more waits, in this process. Across replicas the vault's
// Postgres advisory lock (WithRepo) is what excludes two pushes, and the
// row, re-read under that lock, is what says whether there is anything left
// to do; retry state lives in the row, never in memory.
type Syncer struct {
	opts SyncerOptions
	log  *slog.Logger
	sem  chan struct{}

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	slots  map[uuid.UUID]*slot
	wakes  map[uuid.UUID]*time.Timer
	closed bool
	wg     sync.WaitGroup
}

// slot is one vault's sync in this process. pull says the run that is
// waiting must look at the remote even when nothing local changed.
type slot struct{ again, pull bool }

func NewSyncer(opts SyncerOptions) *Syncer {
	if opts.PushTimeout <= 0 {
		opts.PushTimeout = defaultPushTimeout
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultConcurrency
	}
	if opts.LockTimeout <= 0 {
		opts.LockTimeout = defaultLockTimeout
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.LockedRetry <= 0 {
		opts.LockedRetry = defaultLockedRetry
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Syncer{
		opts: opts, log: log, sem: make(chan struct{}, opts.Concurrency), ctx: ctx, cancel: cancel,
		slots: map[uuid.UUID]*slot{}, wakes: map[uuid.UUID]*time.Timer{},
	}
}

// Notify says vault has a new commit, or is otherwise worth a look. It never
// blocks: the sync runs on its own goroutine. While one runs for the vault,
// any number of notifies make exactly one more run after it. After Stop it
// does nothing.
func (s *Syncer) Notify(vault uuid.UUID) { s.start(vault, false, false) }

// NotifyPull is Notify for a vault whose remote may have moved (a webhook,
// the poll, "Sync now"): the sync looks at GitHub first, and pulls what it
// finds, even when the vault has nothing to push.
func (s *Syncer) NotifyPull(vault uuid.UUID) { s.start(vault, false, true) }

// OnCommit is vaultgit.Options.OnCommit.
func (s *Syncer) OnCommit(tenant uuid.UUID, _ string) { s.Notify(tenant) }

func (s *Syncer) start(vault uuid.UUID, wait, pull bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if sl := s.slots[vault]; sl != nil {
		sl.again = true
		sl.pull = sl.pull || pull
		s.mu.Unlock()
		return
	}
	sl := &slot{pull: pull}
	s.slots[vault] = sl
	s.wg.Add(1)
	s.mu.Unlock()
	run := func() {
		defer s.wg.Done()
		for {
			s.mu.Lock()
			pull := sl.pull
			sl.pull = false
			s.mu.Unlock()
			s.attempt(s.ctx, vault, pull)
			s.mu.Lock()
			if !sl.again || s.closed {
				delete(s.slots, vault)
				s.mu.Unlock()
				return
			}
			sl.again = false
			s.mu.Unlock()
		}
	}
	if wait {
		run()
		return
	}
	go run()
}

// Syncing reports a sync running or waiting for vault in this process.
func (s *Syncer) Syncing(vault uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slots[vault] != nil
}

// SyncNow is "Sync now" (D14): it makes a waiting or stopped connection due
// and queues a pull, then a push.
func (s *Syncer) SyncNow(ctx context.Context, vault uuid.UUID) error {
	if _, err := s.opts.Store.Requeue(ctx, vault, retryableByOwner, CodeRateLimited, s.opts.Now()); err != nil {
		return err
	}
	s.NotifyPull(vault)
	return nil
}

// Stop ends running syncs and waits for them. Notifies after it are ignored,
// so a commit made during shutdown is pushed by the next start's sweep.
func (s *Syncer) Stop() {
	s.mu.Lock()
	s.closed = true
	for _, t := range s.wakes {
		t.Stop()
	}
	s.wakes = map[uuid.UUID]*time.Timer{}
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
}

// wake queues a sync for vault after d, replacing an earlier wake. pull says
// the sync it queues must look at the remote, as the one it stands in for
// was going to.
func (s *Syncer) wake(vault uuid.UUID, d time.Duration, pull bool) {
	if d > maxWake {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if t := s.wakes[vault]; t != nil {
		t.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(max(d, 0), func() {
		// The entry goes when its timer fires, unless a later wake took its
		// place. (t is set before the lock is let go, which this waits for.)
		s.mu.Lock()
		if s.wakes[vault] == t {
			delete(s.wakes, vault)
		}
		s.mu.Unlock()
		s.start(vault, false, pull)
	})
	s.wakes[vault] = t
}

func (s *Syncer) dropWake(vault uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.wakes[vault]; t != nil {
		t.Stop()
		delete(s.wakes, vault)
	}
}

// Sweep looks at every due connection and pushes those whose local HEAD is
// not what GitHub has (D6: durability). It is what pushes a commit whose
// in-memory trigger was lost to a restart, and what retries after a wait no
// timer survived. It is safe on every replica at once.
func (s *Syncer) Sweep(ctx context.Context) (looked int, err error) {
	now := s.opts.Now()
	vaults, err := s.opts.Store.Due(ctx, now)
	if err != nil {
		return 0, err
	}
	// D9: a delivery still queued a minute after it arrived lost its
	// in-memory nudge (a restart, another replica's crash). Its vault is
	// looked at with a pull.
	stale := map[uuid.UUID]bool{}
	if s.opts.Pull != nil {
		waiting, err := s.opts.Store.StaleDeliveries(ctx, now.Add(-requeueAfter), now)
		if err != nil {
			return 0, err
		}
		for _, v := range waiting {
			stale[v] = true
		}
	}
	return s.each(ctx, vaults, func(v uuid.UUID) bool { return stale[v] })
}

// each runs a sync for every vault, a few at a time, and waits for them.
func (s *Syncer) each(ctx context.Context, vaults []uuid.UUID, pull func(uuid.UUID) bool) (looked int, err error) {
	work := make(chan uuid.UUID)
	var wg sync.WaitGroup
	for range cap(s.sem) {
		wg.Go(func() {
			for v := range work {
				s.start(v, true, pull(v))
			}
		})
	}
send:
	for _, v := range vaults {
		// A worker may be inside a long sync: the caller's cancellation is
		// not kept waiting for it.
		select {
		case work <- v:
			looked++
		case <-ctx.Done():
			break send
		}
	}
	close(work)
	wg.Wait()
	return looked, ctx.Err()
}

// Poll asks every due connection's remote for its tip and pulls where it is
// not what the vault has (D9). GitHub does not redeliver a failed webhook, so
// this is what catches a missed one, and it is the whole pull path where
// GitHub cannot reach the server (staging). It is safe on every replica at
// once: the question is one small request, and the pull itself is serialized
// by the vault's lock.
func (s *Syncer) Poll(ctx context.Context) (looked int, err error) {
	if s.opts.Pull == nil {
		return 0, nil
	}
	vaults, err := s.opts.Store.Due(ctx, s.opts.Now())
	if err != nil {
		return 0, err
	}
	return s.each(ctx, vaults, func(uuid.UUID) bool { return true })
}

// PollJob is GitHubSyncPoll, every GITHUB_SYNC_POLL_INTERVAL.
func (s *Syncer) PollJob() jobs.Job {
	return jobs.Job{
		Name:     "GitHubSyncPoll",
		Schedule: jobs.Every(s.opts.PollInterval, min(s.opts.PollInterval, time.Minute)),
		Run: func(ctx context.Context) error {
			looked, err := s.Poll(ctx)
			s.log.DebugContext(ctx, "github sync poll", "connections", looked)
			return err
		},
	}
}

// PurgeJob is GitHubSyncPurge: once a day it deletes webhook delivery rows
// older than 7 days (D9).
func (s *Syncer) PurgeJob() jobs.Job {
	return jobs.Job{
		Name:     "GitHubSyncPurge",
		Schedule: jobs.DailyUTC(4),
		Run: func(ctx context.Context) error {
			n, err := s.opts.Store.PurgeDeliveries(ctx, s.opts.Now().Add(-deliveryRetention))
			s.log.InfoContext(ctx, "github sync purge", "deliveries", n)
			return err
		},
	}
}

// SweepJob is GitHubSyncSweep, every 5 minutes.
func (s *Syncer) SweepJob() jobs.Job {
	return jobs.Job{
		Name:     "GitHubSyncSweep",
		Schedule: jobs.Every(SweepInterval, time.Minute),
		Run: func(ctx context.Context) error {
			looked, err := s.Sweep(ctx)
			s.log.InfoContext(ctx, "github sync sweep", "connections", looked)
			return err
		},
	}
}

// Disconnected removes the origin remote and its refs after the connection's
// row was deleted (D13). It waits for a push in flight, which holds the
// vault's lock, but keeps the caller only a moment: past that the removal
// finishes in the background. A connection made in the meantime keeps its
// remote. The held list stays (D8).
func (s *Syncer) Disconnected(ctx context.Context, vault uuid.UUID) {
	s.dropWake(vault)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer s.wg.Done()
		defer close(done)
		s.removeRemote(vault)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(disconnectWait):
	}
}

func (s *Syncer) removeRemote(vault uuid.UUID) {
	for range disconnectTries {
		err := s.opts.Repos.WithRepo(s.ctx, vault, func(repo *vaultgit.Repo) error {
			conn, err := s.opts.Store.Get(s.ctx, vault)
			if err != nil || conn != nil {
				return err
			}
			return repo.RemoveRemote()
		})
		switch {
		case err == nil, errors.Is(err, vaultgit.ErrNoRepo):
			return
		case errors.Is(err, vaultgit.ErrLocked):
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(s.opts.LockedRetry):
			}
		default:
			s.log.Warn("github sync: removing the remote failed", "vault", vault.String(), "error", err)
			return
		}
	}
	s.log.Warn("github sync: removing the remote gave up: vault stayed locked", "vault", vault.String())
}

// MARK: one attempt

type result int

const (
	resultNone    result = iota // nothing to do, or nothing to record
	resultPushed                // the remote has HEAD
	resultRetry                 // transient: back off and try again
	resultConfirm               // ambiguous: final only when it repeats
	resultFinal                 // a person has to act
)

// outcome is what one attempt found, under the lock, for the row it read.
type outcome struct {
	result  result
	conn    *Connection
	code    string
	cause   string    // for the log only; never stored
	retryAt time.Time // when GitHub said to come back, if it did
}

// hold is the retry time an attempt wrote to the row before it pushed or
// fetched (Store.Reserve), so that a process killed in the middle does not
// try again the moment it is back.
type hold struct {
	made bool // the row carries it
	kept bool // the attempt's end replaced it: nothing to take back
	key  Key
	next time.Time
	base int // retry_count before the attempt was counted in it
}

// reserve writes the hold, or moves it on when the attempt already has one.
// False: the row is no longer this connection, or cannot be written; nothing
// may be sent.
func (s *Syncer) reserve(ctx context.Context, conn *Connection, h *hold) bool {
	if h.made && h.key != conn.Key() {
		*h = hold{}
	}
	first := !h.made
	if first {
		h.base = conn.RetryCount
	}
	// Postgres keeps microseconds; Release compares what it reads back.
	now := s.opts.Now()
	next := now.Add(Backoff(h.base)).Truncate(time.Microsecond)
	ok, err := s.opts.Store.Reserve(ctx, conn.Key(), now, next, first)
	if err != nil {
		s.log.WarnContext(ctx, "github sync: reserving the retry failed", "vault", conn.VaultID.String(), "error", err)
		return false
	}
	if !ok {
		return false
	}
	h.made, h.key, h.next = true, conn.Key(), next
	return true
}

// release takes the hold back when the attempt ended with nothing recorded.
func (s *Syncer) release(ctx context.Context, h *hold) {
	if !h.made || h.kept {
		return
	}
	// A shutdown is the usual reason to be here; the write must outlive it.
	if _, err := s.opts.Store.Release(context.WithoutCancel(ctx), h.key, h.next); err != nil {
		s.log.WarnContext(ctx, "github sync: releasing the retry failed", "vault", h.key.Vault.String(), "error", err)
	}
}

// attempt is one look at a vault. pull says the remote may have moved.
//
// A sync is a push, and a pull when one is asked for or the push finds the
// remote ahead. A pull's plan and finish run inside the vault's lock, like
// the push; its apply runs between them with no lock, no worker slot and no
// pinned connection held, however long it takes (D10).
func (s *Syncer) attempt(ctx context.Context, vault uuid.UUID, pull bool) {
	started := s.opts.Now()
	conn, err := s.opts.Store.Get(ctx, vault)
	if err != nil {
		s.log.WarnContext(ctx, "github sync: reading the connection failed", "vault", vault.String(), "error", err)
		return
	}
	if conn == nil || conn.State != StateActive {
		return
	}
	now := s.opts.Now()
	if conn.NextRetryAt != nil && now.Before(*conn.NextRetryAt) {
		wait := conn.NextRetryAt.Sub(now)
		if conn.ReservedAt != nil {
			// Another replica's attempt is in flight, or died there. Either
			// way the time is not this look's to wait out whole: the attempt
			// ends in seconds and takes it back, and one that died keeps it.
			// Looking again is one row read.
			wait = min(wait, s.opts.LockedRetry)
		}
		s.wake(vault, wait, pull)
		return
	}
	// A connection whose last sync failed is looked at whole: the failure
	// may have been a pull's, and a push-only look would call it mended.
	pull = (pull || conn.ErrorCode != nil) && s.opts.Pull != nil
	if archived, err := s.opts.Store.VaultArchived(ctx, vault); err != nil || archived {
		return
	}
	if pull {
		// One small request, outside every lock: most looks end here.
		moved, out := s.remoteMoved(ctx, conn)
		if out.result != resultNone {
			s.record(ctx, out, true, &hold{})
			return
		}
		if !moved {
			s.deliveriesDone(ctx, conn, started)
			pull = false
		}
	}
	if !pull && conn.ErrorCode == nil && s.peekUpToDate(conn) {
		return
	}
	asked := pull
	var (
		out outcome
		h   hold
	)
	defer s.release(ctx, &h)
	for round := 0; ; round++ {
		var plan *Plan
		err := s.locked(ctx, vault, asked, func(repo *vaultgit.Repo) {
			out, plan = s.sync(ctx, repo, vault, pull, &h)
		})
		if err != nil {
			return
		}
		if plan == nil {
			break
		}
		if round == maxRounds {
			out = outcome{conn: out.conn}.retry("rounds")
			break
		}
		if err := s.opts.Pull.Apply(ctx, plan); err != nil {
			s.log.WarnContext(ctx, "github sync: applying a pull failed", "vault", vault.String(), "error", err)
			out = outcome{conn: out.conn}.retry("pull_apply")
			break
		}
		err = s.locked(ctx, vault, true, func(repo *vaultgit.Repo) {
			fctx, cancel := context.WithTimeout(ctx, s.opts.LockTimeout)
			defer cancel()
			if err := s.opts.Pull.Finish(fctx, PullInput{Repo: repo, Connection: *out.conn}, plan); err != nil {
				s.log.WarnContext(ctx, "github sync: finishing a pull failed", "vault", vault.String(), "error", err)
				out = outcome{conn: out.conn}.retry("pull_finish")
				plan = nil
			}
		})
		if err != nil {
			return
		}
		if plan == nil {
			break
		}
		// The remote tip is merged. The next round pushes what the merge
		// made, or finds nothing left to do.
		pull = false
	}
	if asked && (out.result == resultNone || out.result == resultPushed) && out.conn != nil {
		s.deliveriesDone(ctx, out.conn, started)
	}
	s.record(ctx, out, asked, &h)
}

// locked runs fn inside the vault's lock, holding a worker slot. An error
// means fn did not run: the attempt is over, and what there is to say about
// it has been said. pull is whether the look that comes back after another
// replica let go of the lock must be a pull.
func (s *Syncer) locked(ctx context.Context, vault uuid.UUID, pull bool, fn func(*vaultgit.Repo)) error {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	err := s.opts.Repos.WithRepo(ctx, vault, func(repo *vaultgit.Repo) error {
		fn(repo)
		return nil
	})
	switch {
	case errors.Is(err, vaultgit.ErrNoRepo): // no history yet; its first commit will notify
	case errors.Is(err, vaultgit.ErrLocked):
		s.wake(vault, s.opts.LockedRetry, pull)
	case err != nil:
		s.log.WarnContext(ctx, "github sync: taking the vault lock failed", "vault", vault.String(), "error", err)
	}
	return err
}

// remoteMoved asks GitHub for the branch's tip and reports whether a pull has
// anything to bring: the tip is neither what the row records nor a commit the
// vault's history already has (D12: by ancestry, never by who pushed). When
// the question itself fails for a reason a pull would fail for too, the pull
// is left to find out, under the lock, where a rename is followed.
func (s *Syncer) remoteMoved(ctx context.Context, conn *Connection) (bool, outcome) {
	out := outcome{conn: conn}
	token, err := s.opts.GitHub.InstallationToken(ctx, conn.InstallationID, conn.RepoID)
	if err != nil {
		return false, out.mint(err, s.opts.Now())
	}
	lctx, cancel := context.WithTimeout(ctx, s.opts.PushTimeout)
	defer cancel()
	tip, err := vaultgit.LsRemote(lctx, s.remoteURL(conn.RepoFullName), conn.Branch, token)
	if err != nil {
		return true, outcome{}
	}
	if tip == "" || (conn.RemoteSHA != nil && *conn.RemoteSHA == tip) {
		return false, outcome{}
	}
	return !s.has(conn.VaultID, tip), outcome{}
}

// has reports whether the vault's history contains the commit, read without
// the lock: a stale no costs one fetch that finds nothing.
func (s *Syncer) has(vault uuid.UUID, sha string) bool {
	repo, err := vaultgit.Open(s.opts.RawRoot(vault))
	if err != nil {
		return false
	}
	ok, err := repo.Contains(sha)
	return err == nil && ok
}

// deliveriesDone closes the push deliveries that asked for the pull which
// just ended well (D9).
func (s *Syncer) deliveriesDone(ctx context.Context, conn *Connection, started time.Time) {
	ref := "refs/heads/" + conn.Branch
	if err := s.opts.Store.DeliveriesDone(context.WithoutCancel(ctx), conn.RepoID, ref, started, s.opts.Now()); err != nil {
		s.log.WarnContext(ctx, "github sync: closing deliveries failed", "vault", conn.VaultID.String(), "error", err)
	}
}

// peekUpToDate reads HEAD without the lock. It only ever saves work: a stale
// answer is corrected by the next notify or sweep, and the decision to push
// is made again under the lock.
func (s *Syncer) peekUpToDate(conn *Connection) bool {
	repo, err := vaultgit.Open(s.opts.RawRoot(conn.VaultID))
	if errors.Is(err, vaultgit.ErrNoRepo) {
		return true
	}
	if err != nil {
		return false
	}
	head, err := repo.Head()
	if err != nil {
		return false
	}
	return head == "" || (conn.RemoteSHA != nil && *conn.RemoteSHA == head)
}

func (s *Syncer) remoteURL(fullName string) string {
	return strings.TrimRight(s.opts.GitBaseURL, "/") + "/" + fullName + ".git"
}

// sync runs under the vault's lock: it reads the row again, plans a pull
// when one is asked for, and pushes when HEAD is not what GitHub has. A
// returned plan is a pull to apply and finish before the next look; the
// outcome beside it only names the connection.
func (s *Syncer) sync(ctx context.Context, repo *vaultgit.Repo, vault uuid.UUID, pull bool, h *hold) (outcome, *Plan) {
	conn, err := s.opts.Store.Get(ctx, vault)
	if err != nil {
		s.log.WarnContext(ctx, "github sync: reading the connection failed", "vault", vault.String(), "error", err)
		return outcome{}, nil
	}
	if conn == nil || conn.State != StateActive {
		return outcome{}, nil
	}
	out := outcome{conn: conn}
	if pull {
		// The fetch holds its pack in memory as the push does: the retry
		// time is on the row before it starts.
		if !s.reserve(ctx, conn, h) {
			return outcome{}, nil
		}
		token, err := s.opts.GitHub.InstallationToken(ctx, conn.InstallationID, conn.RepoID)
		if err != nil {
			return out.mint(err, s.opts.Now()), nil
		}
		if o, plan := s.plan(ctx, repo, out, token); plan != nil || o.result != resultNone {
			return o, plan
		}
	}
	head, err := repo.Head()
	if err != nil {
		return out.retry("local_head"), nil
	}
	if head == "" {
		return outcome{conn: conn}, nil
	}
	if conn.RemoteSHA != nil && *conn.RemoteSHA == head {
		if conn.ErrorCode != nil { // an earlier failure, since overtaken
			if _, err := s.opts.Store.MarkPushed(ctx, conn.Key(), head, s.opts.Now(), false); err != nil {
				s.log.WarnContext(ctx, "github sync: clearing the error failed", "vault", vault.String(), "error", err)
			}
		}
		return outcome{conn: conn}, nil
	}
	if !s.reserve(ctx, conn, h) {
		return outcome{}, nil
	}
	return s.push(ctx, repo, out, vault)
}

// plan is a pull's first phase, inside the lock (D10). It returns a plan to
// apply, or an outcome to record, or neither: there is nothing to pull.
func (s *Syncer) plan(ctx context.Context, repo *vaultgit.Repo, out outcome, token string) (outcome, *Plan) {
	conn := out.conn
	ctx, cancel := context.WithTimeout(ctx, s.opts.LockTimeout)
	defer cancel()
	if s.opts.MaxRepoBytes > 0 {
		// D11: the size limit holds on every fetch. The same answer carries
		// the repository's current name, so a rename is learned here too.
		info, err := s.opts.GitHub.RepositoryByID(ctx, token, conn.RepoID)
		switch {
		case errors.Is(err, githubapp.ErrNotFound):
			return out.confirm(CodeRepoAccessRemoved, "repository_gone"), nil
		case errors.Is(err, githubapp.ErrRateLimited):
			var se *githubapp.StatusError
			if errors.As(err, &se) {
				out.retryAt = se.RetryAt(s.opts.Now())
			}
			out.code = CodeRateLimited
			return out.retry("rate_limited"), nil
		case err != nil:
			return out.retry("repository_lookup"), nil
		case info.Size*1024 > s.opts.MaxRepoBytes:
			return out.final(CodeRepoTooLarge, "repository_size"), nil
		}
		if info.FullName != "" && info.FullName != conn.RepoFullName {
			if _, err := s.opts.Store.SetRepoFullName(ctx, conn.Key(), info.FullName, s.opts.Now()); err != nil {
				return out.retry("store"), nil
			}
			conn.RepoFullName = info.FullName
		}
	}
	renamed := false
	for {
		if err := repo.SetRemote(s.remoteURL(conn.RepoFullName)); err != nil {
			return out.retry("set_remote"), nil
		}
		plan, err := s.opts.Pull.Plan(ctx, PullInput{Repo: repo, Connection: *conn, Token: token})
		switch {
		case err == nil:
			return outcome{conn: conn}, plan
		case errors.Is(err, ErrRepoTooLarge):
			return out.final(CodeRepoTooLarge, "remote_files"), nil
		case errors.Is(err, ErrMergeConsent):
			return out.final(CodeRemoteNotEmpty, "no_merge_consent"), nil
		case errors.Is(err, vaultgit.ErrRemoteNotFound) && !renamed:
			name, o := s.currentName(ctx, out, token)
			if name == "" {
				return o, nil
			}
			conn.RepoFullName, renamed = name, true
		case errors.Is(err, vaultgit.ErrRemoteUnauthorized):
			_ = s.opts.GitHub.RevokeInstallationToken(ctx, conn.InstallationID, conn.RepoID)
			return out.retry("fetch_unauthorized"), nil
		case errors.Is(err, vaultgit.ErrRemoteForbidden):
			return out.confirm(CodeRepoAccessRemoved, "fetch_forbidden"), nil
		default:
			s.log.WarnContext(ctx, "github sync: planning a pull failed", "vault", conn.VaultID.String(), "error", err)
			return out.retry("pull"), nil
		}
	}
}

// push sends HEAD to GitHub, following a rename, and plans a pull when the
// remote has moved (D6).
func (s *Syncer) push(ctx context.Context, repo *vaultgit.Repo, out outcome, vault uuid.UUID) (outcome, *Plan) {
	conn := out.conn
	timeout := s.pushTimeout(repo)
	renamed := false
	for range maxRounds {
		token, err := s.opts.GitHub.InstallationToken(ctx, conn.InstallationID, conn.RepoID)
		if err != nil {
			return out.mint(err, s.opts.Now()), nil
		}
		if err := repo.SetRemote(s.remoteURL(conn.RepoFullName)); err != nil {
			return out.retry("set_remote"), nil
		}
		pctx, cancel := context.WithTimeout(ctx, timeout)
		res, err := repo.PushWith(pctx, conn.Branch, token, vaultgit.PushOptions{MaxPackBytes: s.opts.MaxRepoBytes})
		timedOut := pctx.Err() != nil
		cancel()
		if err != nil && !renamed && (errors.Is(err, vaultgit.ErrRemoteUnauthorized) || errors.Is(err, vaultgit.ErrRemoteForbidden)) {
			// The repository was renamed and its old name taken by another:
			// the remote knows the name and refuses the token. The answer
			// is believed only once the name has been checked by id.
			if name := s.movedTo(ctx, conn, token); name != "" {
				conn.RepoFullName, renamed = name, true
				continue
			}
		}
		switch {
		case err == nil:
			// Sent is false when GitHub already had the commit: recorded as
			// in sync, and last_push_at stays.
			if _, err := s.opts.Store.MarkPushed(ctx, conn.Key(), res.SHA, s.opts.Now(), res.Sent); err != nil {
				// The push happened; the next look finds the remote at HEAD
				// (the push answers "already up to date") and records it.
				s.log.WarnContext(ctx, "github sync: recording the push failed", "vault", vault.String(), "error", err)
				return outcome{}, nil
			}
			out.result = resultPushed
			return out, nil
		case errors.Is(err, vaultgit.ErrNonFastForward):
			if s.opts.Pull == nil {
				return out.retry("remote_moved_no_pull"), nil
			}
			if o, plan := s.plan(ctx, repo, out, token); plan != nil || o.result != resultNone {
				return o, plan
			}
		case errors.Is(err, vaultgit.ErrRemoteNotFound):
			if renamed {
				return out.retry("remote_not_found"), nil
			}
			name, o := s.currentName(ctx, out, token)
			if name == "" {
				return o, nil
			}
			conn.RepoFullName, renamed = name, true
		case errors.Is(err, vaultgit.ErrPackTooLarge):
			// Nothing was sent. go-git would have held the whole pack in
			// memory; the owner's limit on a repository is the limit on it.
			return out.final(CodeRepoTooLarge, "pack_size"), nil
		case errors.Is(err, vaultgit.ErrRemoteUnauthorized):
			// The cached token is no good; the next attempt mints another.
			_ = s.opts.GitHub.RevokeInstallationToken(ctx, conn.InstallationID, conn.RepoID)
			s.pushFailed(ctx, vault, "unauthorized", err)
			return out.retry("push_unauthorized"), nil
		case errors.Is(err, vaultgit.ErrRemoteForbidden):
			s.pushFailed(ctx, vault, "forbidden", err)
			return out.confirm(CodePushRejected, "push_forbidden"), nil
		case errors.Is(err, vaultgit.ErrPushRejected):
			s.pushFailed(ctx, vault, "rejected", err)
			return out.final(CodePushRejected, "push_rejected"), nil
		default:
			kind := "transport"
			if timedOut || errors.Is(err, context.DeadlineExceeded) {
				kind = "timeout"
			}
			s.pushFailed(ctx, vault, kind, err)
			return out.retry("push"), nil
		}
	}
	return out.retry("rounds"), nil
}

// pushFailed logs what the remote, or the transport, said. The text is
// go-git's error: the remote's own words for a refused update, a status for
// a refused request, the URL and the cause for a failed one. The token is in
// none of them: it travels in a header and is never part of the URL
// (vaultgit.SetRemote refuses one that holds credentials). Only the class is
// stored (record).
func (s *Syncer) pushFailed(ctx context.Context, vault uuid.UUID, kind string, err error) {
	s.log.WarnContext(ctx, "github sync: push failed", "vault", vault.String(), "kind", kind, "error", err.Error())
}

// pushTimeout is one push's deadline: PushTimeout, and a second more for
// every MiB of objects the repository holds, which is the most a first push
// sends. Past MaxRepoBytes the push is refused, so the size counts up to it.
func (s *Syncer) pushTimeout(repo *vaultgit.Repo) time.Duration {
	size, err := repo.ObjectsSize()
	if err != nil {
		return s.opts.PushTimeout
	}
	if s.opts.MaxRepoBytes > 0 {
		size = min(size, s.opts.MaxRepoBytes)
	}
	return s.opts.PushTimeout + time.Duration(size/pushRate)*time.Second
}

// movedTo asks for the repository by id after the remote refused the token
// under the stored name, and returns its name when that is another one, now
// stored. "" is every other answer: the refusal stands as it was given.
func (s *Syncer) movedTo(ctx context.Context, conn *Connection, token string) string {
	info, err := s.opts.GitHub.RepositoryByID(ctx, token, conn.RepoID)
	if err != nil || info.FullName == "" || info.FullName == conn.RepoFullName {
		return ""
	}
	if _, err := s.opts.Store.SetRepoFullName(ctx, conn.Key(), info.FullName, s.opts.Now()); err != nil {
		return ""
	}
	s.log.InfoContext(ctx, "github sync: repository renamed on GitHub", "vault", conn.VaultID.String(), "repo", conn.RepoID)
	return info.FullName
}

// currentName follows a rename (D6): the remote did not know the stored name,
// so the repository is asked for by id. It returns the new name, stored, or
// "" and what to record instead.
func (s *Syncer) currentName(ctx context.Context, out outcome, token string) (string, outcome) {
	conn := out.conn
	info, err := s.opts.GitHub.RepositoryByID(ctx, token, conn.RepoID)
	var se *githubapp.StatusError
	switch {
	case errors.Is(err, githubapp.ErrRateLimited):
		if errors.As(err, &se) {
			out.retryAt = se.RetryAt(s.opts.Now())
		}
		out.code = CodeRateLimited
		return "", out.retry("rate_limited")
	case errors.Is(err, githubapp.ErrNotFound):
		// Deleted, or gone from the installation, after the token was minted.
		return "", out.confirm(CodeRepoAccessRemoved, "repository_gone")
	case err != nil:
		return "", out.retry("repository_lookup")
	case info.FullName == "" || info.FullName == conn.RepoFullName:
		// GitHub's API knows it by this name and its git side does not.
		return "", out.retry("remote_not_found")
	}
	if _, err := s.opts.Store.SetRepoFullName(ctx, conn.Key(), info.FullName, s.opts.Now()); err != nil {
		return "", out.retry("store")
	}
	s.log.InfoContext(ctx, "github sync: repository renamed on GitHub", "vault", conn.VaultID.String(), "repo", conn.RepoID)
	return info.FullName, out
}

func (o outcome) retry(cause string) outcome {
	o.result, o.cause = resultRetry, cause
	if o.code == "" {
		o.code = CodeSyncFailed
	}
	return o
}

func (o outcome) confirm(code, cause string) outcome {
	o.result, o.code, o.cause = resultConfirm, code, cause
	return o
}

func (o outcome) final(code, cause string) outcome {
	o.result, o.code, o.cause = resultFinal, code, cause
	return o
}

// mint sorts a failed token request (D6's table).
//
// GitHub documents the endpoint's answers as 401, 403 "Forbidden", 404
// "Resource not found" and 422 "Validation failed, or the endpoint has been
// spammed" (docs.github.com, REST "Create an installation access token for
// an app", read 2026-11-25). So a 404 is final at once, as D6 says: the
// installation is gone. A 422 is what a repository outside the installation
// gets, and also what a burst of requests gets; a 403 is what a suspended
// installation gets, and is not documented as only that. Neither is believed
// the first time: each stays a retry, under its own code, until it has been
// the answer confirmAfter times running.
func (o outcome) mint(err error, now time.Time) outcome {
	var se *githubapp.StatusError
	switch {
	case errors.Is(err, githubapp.ErrRateLimited):
		if errors.As(err, &se) {
			o.retryAt = se.RetryAt(now)
		}
		o.code = CodeRateLimited
		return o.retry("rate_limited")
	case errors.Is(err, githubapp.ErrRepoAccessRemoved):
		return o.confirm(CodeRepoAccessRemoved, "token_422")
	case errors.Is(err, githubapp.ErrNotFound):
		return o.final(CodeInstallationRemoved, "token_404")
	case errors.Is(err, githubapp.ErrForbidden):
		return o.confirm(CodeInstallationSuspended, "token_403")
	}
	return o.retry("token")
}

// record writes what an attempt found to the row it read. h is the hold the
// attempt put on that row, if it did.
func (s *Syncer) record(ctx context.Context, out outcome, pull bool, h *hold) {
	if out.conn == nil || out.result == resultNone {
		return
	}
	// A stop must not turn into a recorded failure, and the write below must
	// not be lost to the cancellation that caused it.
	if s.ctx.Err() != nil {
		return
	}
	// Whatever is written below replaces the hold; when the write fails, the
	// hold is the only wait there is, and stays.
	h.kept = true
	conn, now := out.conn, s.opts.Now()
	vault := conn.VaultID
	if out.result == resultPushed {
		s.dropWake(vault)
		s.log.InfoContext(ctx, "github sync: pushed", "vault", vault.String(), "repo", conn.RepoID)
		return
	}
	confirming := out.result == resultConfirm
	if confirming {
		// Final only as the answer of confirmAfter attempts running: any
		// other failure, or a success, in between starts the count again.
		repeated := conn.ErrorCode != nil && *conn.ErrorCode == out.code && conn.ConfirmCount >= confirmAfter-1
		if repeated {
			out.result = resultFinal
		} else {
			out.result = resultRetry
		}
	}
	if out.result == resultFinal {
		if _, err := s.opts.Store.MarkNeedsAttention(ctx, conn.Key(), out.code, now); err != nil {
			s.log.WarnContext(ctx, "github sync: recording the failure failed", "vault", vault.String(), "error", err)
			return
		}
		s.dropWake(vault)
		s.log.WarnContext(ctx, "github sync: needs attention", "vault", vault.String(), "code", out.code, "cause", out.cause)
		return
	}
	// The hold counted this attempt in retry_count already; the wait is the
	// one for the failures before it.
	counted := h.made && h.key == conn.Key()
	failures := conn.RetryCount
	if counted {
		failures = h.base
	}
	next := now.Add(Backoff(failures))
	if out.code == CodeRateLimited && out.retryAt.After(now) {
		next = out.retryAt
	}
	if _, err := s.opts.Store.MarkRetry(ctx, conn.Key(), out.code, now, next, counted, confirming); err != nil {
		s.log.WarnContext(ctx, "github sync: recording the failure failed", "vault", vault.String(), "error", err)
		return
	}
	s.wake(vault, next.Sub(now), pull)
	s.log.WarnContext(ctx, "github sync: failed, will retry", "vault", vault.String(), "code", out.code, "cause", out.cause,
		"attempt", failures+1, "retry_in", next.Sub(now).String())
}

// Backoff is the wait after the failure that follows failures earlier ones:
// 30 s, 2 min, 10 min, 30 min, then hourly (D6).
func Backoff(failures int) time.Duration {
	if failures < 0 {
		failures = 0
	}
	if failures < len(backoff) {
		return backoff[failures]
	}
	return backoffCap
}
