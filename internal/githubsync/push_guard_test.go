package githubsync_test

import (
	"context"
	"crypto/rand"
	"io"
	"lumina/internal/githubapp"
	"lumina/internal/githubapp/githubapptest"
	"lumina/internal/githubsync"
	"lumina/internal/vaultgit"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// MARK: helpers for a push in flight

func withPushTimeout(d time.Duration) envOption {
	return func(_ *syncEnv, o *githubsync.SyncerOptions) { o.PushTimeout = d }
}

func withMaxRepoBytes(n int64) envOption {
	return func(_ *syncEnv, o *githubsync.SyncerOptions) { o.MaxRepoBytes = n }
}

// gate stands in front of the stub's receive-pack for one repository name: it
// counts the pushes that arrive as requests and, while shut, keeps each one
// waiting, as a remote that has stopped answering does.
type gate struct {
	posts   atomic.Int32
	mu      sync.Mutex
	shut    chan struct{} // nil: open
	arrived chan struct{}
}

func (v *syncEnv) gate(name string) *gate {
	g := &gate{arrived: make(chan struct{}, 64)}
	v.gh.Lock()
	v.gh.Intercept = func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/"+name+".git/git-receive-pack" {
			return false
		}
		g.posts.Add(1)
		g.mu.Lock()
		shut := g.shut
		g.mu.Unlock()
		g.arrived <- struct{}{}
		if shut != nil {
			select {
			case <-shut:
			case <-r.Context().Done():
			}
		}
		return false
	}
	v.gh.Unlock()
	// A request still waiting would keep the stub's server from closing.
	v.t.Cleanup(g.open)
	return g
}

func (g *gate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shut == nil {
		g.shut = make(chan struct{})
	}
}

func (g *gate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shut != nil {
		close(g.shut)
		g.shut = nil
	}
}

// wait blocks until a push has arrived at the gate.
func (g *gate) wait(t *testing.T) {
	t.Helper()
	select {
	case <-g.arrived:
	case <-time.After(20 * time.Second):
		t.Fatal("no push arrived")
	}
}

// noTokenInLogs: whatever the workers logged, no installation token the stub
// ever minted is in it, nor the user it travels with.
func (v *syncEnv) noTokenInLogs() {
	v.t.Helper()
	logs := v.logs.String()
	mints := v.gh.Mints()
	require.NotEmpty(v.t, mints)
	for _, m := range mints {
		require.NotContains(v.t, logs, m.Token)
	}
	require.NotContains(v.t, logs, "x-access-token")
	require.NotContains(v.t, logs, "Authorization")
}

// MARK: item 1, the large first push

// A remote that stops answering costs the push its deadline and no more: the
// failure is a transient one, and the next attempt goes through.
func TestPushTimeoutExpires(t *testing.T) {
	v := newSyncEnv(t, withPushTimeout(300*time.Millisecond))
	l := v.connect()
	g := v.gate(l.name)
	g.close()

	v.commit(l.vault, "a.md", "one")
	g.wait(t)
	began := time.Now()
	v.idle(l.vault)
	require.Less(t, time.Since(began), 10*time.Second, "the deadline ended it, not the remote")
	c := v.row(l.vault)
	require.Equal(t, githubsync.StateActive, c.State)
	require.Equal(t, githubsync.CodeSyncFailed, *c.ErrorCode)
	require.Equal(t, 1, c.RetryCount, "counted once: by the reservation, not again by the failure")
	require.Equal(t, v.clock.now().Add(30*time.Second), c.NextRetryAt.UTC())
	require.Nil(t, c.ReservedAt, "a recorded failure, no longer a reservation")
	require.Empty(t, v.tip(l))
	require.Contains(t, v.logs.String(), "kind=timeout")

	g.open()
	v.clock.advance(30 * time.Second)
	v.sweep()
	head := v.synced(l)
	c = v.row(l.vault)
	require.Nil(t, c.ErrorCode)
	require.Nil(t, c.NextRetryAt)
	require.Zero(t, c.RetryCount)
	require.Equal(t, head, *c.RemoteSHA)
	v.noTokenInLogs()
}

// While a push is in flight the row already carries its retry time, with no
// error: a process killed there (out of memory on a large pack) comes back to
// a row that is not due, and a second process does not start the same push.
// Stop ends the push at once, and a stop, unlike a kill, takes the time back.
func TestStopDuringAPushInFlight(t *testing.T) {
	v := newSyncEnv(t)
	l := v.connect()
	g := v.gate(l.name)
	g.close()

	head := v.commit(l.vault, "a.md", "one")
	g.wait(t)
	c := v.row(l.vault)
	require.Equal(t, githubsync.StateActive, c.State)
	require.Nil(t, c.ErrorCode, "nothing has failed")
	require.NotNil(t, c.ReservedAt)
	require.Equal(t, v.clock.now().Add(30*time.Second), c.NextRetryAt.UTC())
	require.Equal(t, 1, c.RetryCount)

	// What a restart after a kill would find: the sweep passes the vault by,
	// and a commit's nudge only looks at the row.
	second := v.newSyncer(v.committer)
	looked, err := second.Sweep(context.Background())
	require.NoError(t, err)
	require.Zero(t, looked)
	second.Notify(l.vault)
	require.Never(t, func() bool { return g.posts.Load() > 1 }, 400*time.Millisecond, 20*time.Millisecond)
	second.Stop()

	stopped := make(chan struct{})
	go func() {
		v.syncer.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop waited for a push that was going nowhere")
	}
	c = v.row(l.vault)
	require.Nil(t, c.ErrorCode, "a stop is not a failure")
	require.Nil(t, c.NextRetryAt, "and the next start pushes at once")
	require.Nil(t, c.ReservedAt)
	require.Zero(t, c.RetryCount)
	require.Empty(t, v.tip(l))

	g.open()
	third := v.newSyncer(v.committer)
	looked, err = third.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, looked)
	require.Equal(t, head, v.tip(l))
	require.Equal(t, head, *v.row(l.vault).RemoteSHA)
	v.noTokenInLogs()
}

// A push that kills the process leaves its reservation behind, and the next
// one reserves a longer wait before it starts: 30 s, then 2 min, and on up
// D6's schedule, instead of a crash every time the pod is back.
func TestAKilledPushWaitsLongerEachTime(t *testing.T) {
	v := newSyncEnv(t)
	ctx := context.Background()
	l := v.connect()
	v.listen(nil)
	v.commit(l.vault, "a.md", "one")
	g := v.gate(l.name)

	// What the killed attempt left: its reservation, and nothing else.
	c := v.row(l.vault)
	ok, err := v.store.Reserve(ctx, c.Key(), v.clock.now(), v.clock.now().Add(githubsync.Backoff(0)), true)
	require.NoError(t, err)
	require.True(t, ok)

	v.sweep()
	require.Zero(t, g.posts.Load(), "not due")
	v.clock.advance(30 * time.Second)
	g.close()
	swept := make(chan error, 1)
	go func() {
		_, err := v.syncer.Sweep(ctx)
		swept <- err
	}()
	g.wait(t)
	c = v.row(l.vault)
	require.Nil(t, c.ErrorCode)
	require.NotNil(t, c.ReservedAt)
	require.Equal(t, 2, c.RetryCount)
	require.Equal(t, v.clock.now().Add(2*time.Minute), c.NextRetryAt.UTC(), "the second wait of the schedule")

	g.open()
	require.NoError(t, <-swept)
	v.synced(l)
	c = v.row(l.vault)
	require.Nil(t, c.NextRetryAt)
	require.Nil(t, c.ReservedAt)
	require.Zero(t, c.RetryCount)
}

// go-git holds a push's pack in memory, so a pack over GITHUB_SYNC_MAX_REPO_MB
// is never built whole and never sent: the connection stops with
// repo_too_large. A vault under the limit is not touched by it.
func TestLargePackIsRefused(t *testing.T) {
	v := newSyncEnv(t, withMaxRepoBytes(64<<10))
	small := v.connect()
	v.commit(small.vault, "a.md", "one")
	v.synced(small)

	big := v.connect()
	g := v.gate(big.name)
	noise := make([]byte, 256<<10) // does not compress
	_, err := rand.Read(noise)
	require.NoError(t, err)
	v.commit(big.vault, "scan.pdf", string(noise))
	v.idle(big.vault)
	c := v.row(big.vault)
	require.Equal(t, githubsync.StateNeedsAttention, c.State)
	require.Equal(t, githubsync.CodeRepoTooLarge, *c.ErrorCode)
	require.Nil(t, c.NextRetryAt)
	require.Nil(t, c.ReservedAt)
	require.Zero(t, g.posts.Load(), "nothing was sent")
	require.Empty(t, v.tip(big))

	// Stopped: neither a commit nor a sweep builds the pack again.
	mints := len(v.gh.Mints())
	v.commit(big.vault, "later.md", "two")
	v.clock.advance(2 * time.Hour)
	v.sweep()
	v.idle(big.vault)
	require.Len(t, v.gh.Mints(), mints)
	require.Zero(t, g.posts.Load())
}

// MARK: item 7, a connection that drops, and a reconnect under a push

// The connection is cut while the pack is on its way. That is a transient
// failure; the retry sends the whole pack again and the remote is intact.
func TestConnectionDroppedMidPack(t *testing.T) {
	v := newSyncEnv(t)
	l := v.connect()
	var drops atomic.Int32
	v.gh.Lock()
	v.gh.Intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/"+l.name+".git/git-receive-pack" || drops.Add(1) > 1 {
			return false
		}
		_, _ = io.CopyN(io.Discard, r.Body, 32)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return true
	}
	v.gh.Unlock()

	v.commit(l.vault, "a.md", strings.Repeat("a line of a note\n", 4096))
	v.idle(l.vault)
	require.EqualValues(t, 1, drops.Load())
	c := v.row(l.vault)
	require.Equal(t, githubsync.StateActive, c.State)
	require.Equal(t, githubsync.CodeSyncFailed, *c.ErrorCode)
	require.Equal(t, 1, c.RetryCount)
	require.Empty(t, v.tip(l))
	require.Contains(t, v.logs.String(), "kind=transport")

	v.clock.advance(30 * time.Second)
	v.sweep()
	v.synced(l)
	require.Equal(t, v.localFiles(l.vault), v.remoteFiles(l))
	require.Nil(t, v.row(l.vault).ErrorCode)
	require.Zero(t, v.gh.GitForced())
	v.noTokenInLogs()
}

// The owner disconnects and connects another repository while a push to the
// first one still holds the vault's lock. Nothing the old attempt writes
// lands on the new connection's row, and the new connection pushes to its
// own repository.
func TestReconnectWhileAPushHoldsTheLock(t *testing.T) {
	v := newSyncEnv(t)
	ctx := context.Background()
	old := v.connect()
	g := v.gate(old.name)
	g.close()
	head := v.commit(old.vault, "a.md", "one")
	g.wait(t)

	gone, err := v.store.Delete(ctx, old.vault)
	require.NoError(t, err)
	require.NotNil(t, gone)
	v.syncer.Disconnected(ctx, old.vault) // returns while the push still runs

	next := link{vault: old.vault, installation: old.installation, repo: old.repo + 1_000_000, name: old.name + "-next", branch: "main"}
	v.gh.Lock()
	v.gh.Repos[next.repo] = githubapp.Repository{ID: next.repo, FullName: next.name, Private: true, DefaultBranch: "main"}
	v.gh.InstallationRepos[old.installation] = []int64{old.repo, next.repo}
	v.gh.Unlock()
	require.NoError(t, v.store.Insert(ctx, githubsync.Connection{
		VaultID: next.vault, InstallationID: next.installation, AccountLogin: "octocat", AccountType: "User",
		RepoID: next.repo, RepoFullName: next.name, RepoPrivate: true, Branch: next.branch, State: githubsync.StateActive,
		ConnectedByUserID: &next.vault, ConnectedGitHubLogin: "octocat",
	}))

	g.open()
	v.idle(old.vault)
	c := v.row(next.vault)
	require.Equal(t, next.repo, c.RepoID)
	require.Nil(t, c.RemoteSHA, "the old push is not this connection's")
	require.Nil(t, c.LastPushAt)
	require.Nil(t, c.ErrorCode)
	require.Nil(t, c.NextRetryAt)
	require.Nil(t, c.ReservedAt)
	require.Zero(t, c.RetryCount)

	v.syncer.Notify(next.vault)
	require.Equal(t, head, v.synced(next))
	require.NoError(t, v.committer.WithRepo(ctx, next.vault, func(r *vaultgit.Repo) error {
		url, err := r.RemoteURL()
		require.NoError(t, err)
		require.Equal(t, v.gh.URL+"/"+next.name+".git", url, "the late clean-up left the new remote alone")
		return nil
	}))
	require.Zero(t, v.gh.GitForced())
}

// MARK: item 3, a renamed repository whose old name is taken

// The repository was renamed and its old name now belongs to something the
// token may not touch: the remote answers 403 (or 401), not 404. The name is
// checked by id before that answer is believed.
func TestRefusalOnARenamedRepository(t *testing.T) {
	v := newSyncEnv(t)
	rename := func(l link, to string) {
		v.gh.Lock()
		repo := v.gh.Repos[l.repo]
		repo.FullName = to
		v.gh.Repos[l.repo] = repo
		v.gh.Unlock()
	}
	check := func(l link, to string) {
		t.Helper()
		head := v.commit(l.vault, "a.md", "two")
		v.synced(l)
		require.Equal(t, head, v.tip(l))
		c := v.row(l.vault)
		require.Equal(t, to, c.RepoFullName)
		require.Equal(t, githubsync.StateActive, c.State)
		require.Nil(t, c.ErrorCode)
		require.Zero(t, c.RetryCount, "a rename is not an error")
		require.Zero(t, c.ConfirmCount)
	}

	// 403: the old name answers "forbidden" to this token.
	forbidden := v.connect()
	v.commit(forbidden.vault, "a.md", "one")
	v.synced(forbidden)
	rename(forbidden, "octocat/forbidden-moved")
	v.gh.Lock()
	v.gh.Intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/"+forbidden.name+".git/") {
			return false
		}
		w.WriteHeader(http.StatusForbidden)
		return true
	}
	v.gh.Unlock()
	check(forbidden, "octocat/forbidden-moved")

	// 401: another repository took the old name, and the token is not for it.
	reused := v.connect()
	v.commit(reused.vault, "a.md", "one")
	v.synced(reused)
	rename(reused, "octocat/reused-moved")
	v.gh.Lock()
	v.gh.Repos[reused.repo+1_000_000] = githubapp.Repository{ID: reused.repo + 1_000_000, FullName: reused.name, Private: true}
	v.gh.Unlock()
	check(reused, "octocat/reused-moved")
	require.Empty(t, v.gh.Git.Tip(reused.repo+1_000_000, "main"), "nothing went to the name's new owner")
	v.noTokenInLogs()
}

// MARK: items 2, 4 and 5

// What the remote said when it refused is in the log, for all three classes;
// the token is not.
func TestRefusalsAreLoggedWithoutTheToken(t *testing.T) {
	v := newSyncEnv(t)

	protected := v.connect()
	v.git(func(g *githubapptest.GitRemote) { g.Reject[protected.repo] = "protected branch hook declined" })
	v.commit(protected.vault, "a.md", "one")
	v.idle(protected.vault)
	require.Equal(t, githubsync.StateNeedsAttention, v.row(protected.vault).State)
	require.Contains(t, v.logs.String(), "kind=rejected")
	require.Contains(t, v.logs.String(), "protected branch hook declined")

	archived := v.connect()
	v.git(func(g *githubapptest.GitRemote) { g.Status[archived.repo] = http.StatusForbidden })
	v.commit(archived.vault, "a.md", "one")
	v.idle(archived.vault)
	require.Contains(t, v.logs.String(), "kind=forbidden")
	require.Contains(t, v.logs.String(), "authorization failed")

	down := v.connect()
	v.git(func(g *githubapptest.GitRemote) { g.Status[down.repo] = http.StatusServiceUnavailable })
	v.commit(down.vault, "a.md", "one")
	v.idle(down.vault)
	require.Contains(t, v.logs.String(), "kind=transport")
	require.Contains(t, v.logs.String(), "503")

	v.noTokenInLogs()
}

// An ambiguous answer stops retries only as the answer of three attempts
// running. Two of them around some other failure are not three.
func TestConfirmationCountsConsecutiveAnswersOnly(t *testing.T) {
	v := newSyncEnv(t)
	l := v.connect()
	answer := func(status int) {
		v.git(func(g *githubapptest.GitRemote) { g.Status[l.repo] = status })
	}
	retry := func() {
		v.clock.advance(2 * time.Hour)
		v.sweep()
		v.idle(l.vault)
	}
	state := func(code string, confirms, retries int) {
		t.Helper()
		c := v.row(l.vault)
		require.Equal(t, githubsync.StateActive, c.State)
		require.Equal(t, code, *c.ErrorCode)
		require.Equal(t, confirms, c.ConfirmCount)
		require.Equal(t, retries, c.RetryCount)
	}

	answer(http.StatusForbidden)
	v.commit(l.vault, "a.md", "one")
	v.idle(l.vault)
	state(githubsync.CodePushRejected, 1, 1)
	retry()
	state(githubsync.CodePushRejected, 2, 2)

	answer(http.StatusServiceUnavailable) // something else: the run is broken
	retry()
	state(githubsync.CodeSyncFailed, 0, 3)

	answer(http.StatusForbidden)
	retry()
	state(githubsync.CodePushRejected, 1, 4)
	retry()
	state(githubsync.CodePushRejected, 2, 5)
	retry()
	c := v.row(l.vault)
	require.Equal(t, githubsync.StateNeedsAttention, c.State, "the third in a row")
	require.Equal(t, githubsync.CodePushRejected, *c.ErrorCode)

	// "Sync now" starts the count again.
	require.NoError(t, v.syncer.SyncNow(context.Background(), l.vault))
	v.idle(l.vault)
	state(githubsync.CodePushRejected, 1, 1)
}

// last_push_at is when something was last sent. A push that finds GitHub
// already has the commit records the tip and leaves the time alone.
func TestLastPushAtStaysWhenNothingWasSent(t *testing.T) {
	v := newSyncEnv(t)
	ctx := context.Background()
	l := v.connect()
	v.commit(l.vault, "a.md", "one")
	head := v.synced(l)
	pushedAt := *v.row(l.vault).LastPushAt
	require.Len(t, v.pushes(l), 1)

	// The row lost the tip (as when the push landed and recording it failed).
	_, err := v.pool.Exec(ctx, `UPDATE github_sync_connections SET remote_sha = NULL WHERE vault_id = $1`, l.vault)
	require.NoError(t, err)
	v.clock.advance(time.Hour)
	v.syncer.Notify(l.vault)
	v.synced(l)
	c := v.row(l.vault)
	require.Equal(t, head, *c.RemoteSHA)
	require.Equal(t, pushedAt, *c.LastPushAt, "nothing was sent")
	require.Len(t, v.pushes(l), 1)
	require.Nil(t, c.NextRetryAt)
	require.Zero(t, c.RetryCount)

	v.commit(l.vault, "a.md", "two")
	v.synced(l)
	require.Equal(t, v.clock.now(), v.row(l.vault).LastPushAt.UTC())
}
