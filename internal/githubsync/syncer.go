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
	sweepBatch    = 1000

	defaultPushTimeout = 60 * time.Second
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

// PullInput is what a pull gets. It runs inside the vault's lock.
type PullInput struct {
	Repo       *vaultgit.Repo
	Connection Connection
	Token      string
}

// PullFunc brings the remote branch into the local history (D10) so that the
// next push is a fast-forward. E4.9 provides it.
type PullFunc func(ctx context.Context, in PullInput) error

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
	// Pull runs when the remote moved (D6). Nil until E4.9: a push that is
	// not a fast-forward is then a transient failure, and nothing is forced.
	Pull PullFunc
	// PushTimeout bounds one push (default 60 s).
	PushTimeout time.Duration
	// Concurrency bounds pushes in flight in this process (default 4). Each
	// holds a database connection for the vault's advisory lock.
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

type slot struct{ again bool }

func NewSyncer(opts SyncerOptions) *Syncer {
	if opts.PushTimeout <= 0 {
		opts.PushTimeout = defaultPushTimeout
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultConcurrency
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
func (s *Syncer) Notify(vault uuid.UUID) { s.start(vault, false) }

// OnCommit is vaultgit.Options.OnCommit.
func (s *Syncer) OnCommit(tenant uuid.UUID, _ string) { s.Notify(tenant) }

func (s *Syncer) start(vault uuid.UUID, wait bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if sl := s.slots[vault]; sl != nil {
		sl.again = true
		s.mu.Unlock()
		return
	}
	sl := &slot{}
	s.slots[vault] = sl
	s.wg.Add(1)
	s.mu.Unlock()
	run := func() {
		defer s.wg.Done()
		for {
			s.attempt(s.ctx, vault)
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
// and queues a sync.
func (s *Syncer) SyncNow(ctx context.Context, vault uuid.UUID) error {
	if _, err := s.opts.Store.Requeue(ctx, vault, retryableByOwner, CodeRateLimited, s.opts.Now()); err != nil {
		return err
	}
	s.Notify(vault)
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

// wake queues a sync for vault after d, replacing an earlier wake.
func (s *Syncer) wake(vault uuid.UUID, d time.Duration) {
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
	s.wakes[vault] = time.AfterFunc(max(d, 0), func() { s.Notify(vault) })
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
	vaults, err := s.opts.Store.Due(ctx, s.opts.Now(), sweepBatch)
	if err != nil {
		return 0, err
	}
	work := make(chan uuid.UUID)
	var wg sync.WaitGroup
	for range cap(s.sem) {
		wg.Go(func() {
			for v := range work {
				s.start(v, true)
			}
		})
	}
	for _, v := range vaults {
		if ctx.Err() != nil {
			break
		}
		work <- v
		looked++
	}
	close(work)
	wg.Wait()
	return looked, ctx.Err()
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

func (s *Syncer) attempt(ctx context.Context, vault uuid.UUID) {
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
		s.wake(vault, conn.NextRetryAt.Sub(now))
		return
	}
	if conn.ErrorCode == nil && s.peekUpToDate(conn) {
		return
	}
	if archived, err := s.opts.Store.VaultArchived(ctx, vault); err != nil || archived {
		return
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return
	}
	var out outcome
	err = s.opts.Repos.WithRepo(ctx, vault, func(repo *vaultgit.Repo) error {
		out = s.sync(ctx, repo, vault)
		return nil
	})
	switch {
	case errors.Is(err, vaultgit.ErrNoRepo):
		return // no history yet; its first commit will notify
	case errors.Is(err, vaultgit.ErrLocked):
		s.wake(vault, s.opts.LockedRetry)
		return
	case err != nil:
		s.log.WarnContext(ctx, "github sync: taking the vault lock failed", "vault", vault.String(), "error", err)
		return
	}
	s.record(ctx, out)
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

// sync runs under the vault's lock: it reads the row again, and pushes when
// HEAD is not what GitHub has.
func (s *Syncer) sync(ctx context.Context, repo *vaultgit.Repo, vault uuid.UUID) outcome {
	conn, err := s.opts.Store.Get(ctx, vault)
	if err != nil {
		s.log.WarnContext(ctx, "github sync: reading the connection failed", "vault", vault.String(), "error", err)
		return outcome{}
	}
	if conn == nil || conn.State != StateActive {
		return outcome{}
	}
	out := outcome{conn: conn}
	head, err := repo.Head()
	if err != nil {
		return out.retry("local_head")
	}
	if head == "" {
		return outcome{}
	}
	if conn.RemoteSHA != nil && *conn.RemoteSHA == head {
		if conn.ErrorCode != nil { // an earlier failure, since overtaken
			if _, err := s.opts.Store.MarkPushed(ctx, conn.Key(), head, s.opts.Now(), false); err != nil {
				s.log.WarnContext(ctx, "github sync: clearing the error failed", "vault", vault.String(), "error", err)
			}
		}
		return outcome{}
	}
	renamed := false
	for range maxRounds {
		token, err := s.opts.GitHub.InstallationToken(ctx, conn.InstallationID, conn.RepoID)
		if err != nil {
			return out.mint(err, s.opts.Now())
		}
		if err := repo.SetRemote(s.remoteURL(conn.RepoFullName)); err != nil {
			return out.retry("set_remote")
		}
		pctx, cancel := context.WithTimeout(ctx, s.opts.PushTimeout)
		sha, err := repo.Push(pctx, conn.Branch, token)
		cancel()
		switch {
		case err == nil:
			if _, err := s.opts.Store.MarkPushed(ctx, conn.Key(), sha, s.opts.Now(), true); err != nil {
				// The push happened; the next look finds the remote at HEAD
				// (the push answers "already up to date") and records it.
				s.log.WarnContext(ctx, "github sync: recording the push failed", "vault", vault.String(), "error", err)
				return outcome{}
			}
			out.result = resultPushed
			return out
		case errors.Is(err, vaultgit.ErrNonFastForward):
			if s.opts.Pull == nil {
				return out.retry("remote_moved_no_pull")
			}
			if err := s.opts.Pull(ctx, PullInput{Repo: repo, Connection: *conn, Token: token}); err != nil {
				s.log.WarnContext(ctx, "github sync: pull failed", "vault", vault.String(), "error", err)
				return out.retry("pull")
			}
		case errors.Is(err, vaultgit.ErrRemoteNotFound):
			if renamed {
				return out.retry("remote_not_found")
			}
			name, o := s.currentName(ctx, out, token)
			if name == "" {
				return o
			}
			conn.RepoFullName, renamed = name, true
		case errors.Is(err, vaultgit.ErrRemoteUnauthorized):
			// The cached token is no good; the next attempt mints another.
			_ = s.opts.GitHub.RevokeInstallationToken(ctx, conn.InstallationID, conn.RepoID)
			return out.retry("push_unauthorized")
		case errors.Is(err, vaultgit.ErrRemoteForbidden):
			return out.confirm(CodePushRejected, "push_forbidden")
		case errors.Is(err, vaultgit.ErrPushRejected):
			return out.final(CodePushRejected, "push_rejected")
		default:
			s.log.WarnContext(ctx, "github sync: push failed", "vault", vault.String(), "kind", pushKind(pctx, err))
			return out.retry("push")
		}
	}
	return out.retry("rounds")
}

func pushKind(ctx context.Context, err error) string {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "transport"
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

// record writes what an attempt found to the row it read.
func (s *Syncer) record(ctx context.Context, out outcome) {
	if out.conn == nil || out.result == resultNone {
		return
	}
	// A stop must not turn into a recorded failure, and the write below must
	// not be lost to the cancellation that caused it.
	if s.ctx.Err() != nil {
		return
	}
	conn, now := out.conn, s.opts.Now()
	vault := conn.VaultID
	if out.result == resultPushed {
		s.dropWake(vault)
		s.log.InfoContext(ctx, "github sync: pushed", "vault", vault.String(), "repo", conn.RepoID)
		return
	}
	if out.result == resultConfirm {
		repeated := conn.ErrorCode != nil && *conn.ErrorCode == out.code && conn.RetryCount >= confirmAfter-1
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
	next := now.Add(Backoff(conn.RetryCount))
	if out.code == CodeRateLimited && out.retryAt.After(now) {
		next = out.retryAt
	}
	if _, err := s.opts.Store.MarkRetry(ctx, conn.Key(), out.code, now, next); err != nil {
		s.log.WarnContext(ctx, "github sync: recording the failure failed", "vault", vault.String(), "error", err)
		return
	}
	s.wake(vault, next.Sub(now))
	s.log.WarnContext(ctx, "github sync: failed, will retry", "vault", vault.String(), "code", out.code, "cause", out.cause,
		"attempt", conn.RetryCount+1, "retry_in", next.Sub(now).String())
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
