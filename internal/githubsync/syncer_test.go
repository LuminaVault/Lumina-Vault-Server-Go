package githubsync_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"lumina/internal/githubapp"
	"lumina/internal/githubapp/githubapptest"
	"lumina/internal/githubsync"
	"lumina/internal/vaultgit"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// MARK: environment

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// syncEnv is a database, a stub GitHub (REST and git), a committer over a
// temp vault root and a syncer hooked to its commits: the wiring of
// cmd/App/githubsync.go, with test clocks.
type syncEnv struct {
	t         *testing.T
	pool      *pgxpool.Pool
	store     *githubsync.Store
	gh        *githubapptest.Server
	client    *githubapp.Client
	root      string
	clock     *fakeClock
	committer *vaultgit.Committer
	syncer    *githubsync.Syncer
	// hook is who hears about commits; tests swap it to play a restart or a
	// second replica.
	hook    atomic.Pointer[func(uuid.UUID, string)]
	nextID  int64
	nextIDm sync.Mutex
	vaults  []uuid.UUID
}

type envOption func(*githubsync.SyncerOptions)

func withPull(p githubsync.PullFunc) envOption {
	return func(o *githubsync.SyncerOptions) { o.Pull = p }
}

func newSyncEnv(t *testing.T, opts ...envOption) *syncEnv {
	t.Helper()
	p := pool(t)
	gh := githubapptest.New(t)
	v := &syncEnv{
		t: t, pool: p, store: githubsync.NewStore(p), gh: gh, client: gh.Client(t), root: t.TempDir(),
		// Whole seconds, so times read back from Postgres compare equal.
		clock: &fakeClock{t: time.Now().UTC().Truncate(time.Second)}, nextID: 1000,
	}
	v.committer = v.newCommitter()
	v.syncer = v.newSyncer(v.committer, opts...)
	v.listen(v.syncer.OnCommit)
	return v
}

func (v *syncEnv) raw(vault uuid.UUID) string { return filepath.Join(v.root, vault.String(), "raw") }

func (v *syncEnv) listen(fn func(uuid.UUID, string)) {
	if fn == nil {
		v.hook.Store(nil)
		return
	}
	v.hook.Store(&fn)
}

// newCommitter is one replica's committer: the same vault root and database
// lock as every other.
func (v *syncEnv) newCommitter() *vaultgit.Committer {
	c := vaultgit.NewCommitter(vaultgit.Options{
		RawRoot: v.raw, Lock: vaultgit.PGLocker{Pool: v.pool},
		// Long enough that ten saves in a loop are one burst on a slow runner.
		Debounce: 250 * time.Millisecond, MaxWait: 3 * time.Second,
		OnCommit: func(tenant uuid.UUID, sha string) {
			if fn := v.hook.Load(); fn != nil {
				(*fn)(tenant, sha)
			}
		},
	})
	v.t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func (v *syncEnv) newSyncer(repos githubsync.Repos, opts ...envOption) *githubsync.Syncer {
	o := githubsync.SyncerOptions{
		Store: v.store, GitHub: v.client, Repos: repos, RawRoot: v.raw, GitBaseURL: v.gh.URL,
		Now: v.clock.now, LockedRetry: 50 * time.Millisecond, PushTimeout: 20 * time.Second,
	}
	for _, opt := range opts {
		opt(&o)
	}
	s := githubsync.NewSyncer(o)
	v.t.Cleanup(s.Stop)
	return s
}

type link struct {
	vault        uuid.UUID
	installation int64
	repo         int64
	name         string
	branch       string
}

// connect makes a user with a vault, a repository on the stub the App is
// installed on, and the connection row the E4.7 flow would have saved:
// active, with no remote_sha.
func (v *syncEnv) connect(change ...func(*githubsync.Connection)) link {
	v.t.Helper()
	v.nextIDm.Lock()
	v.nextID++
	id := v.nextID
	owner := user(v.t, v.pool)
	v.vaults = append(v.vaults, owner)
	v.nextIDm.Unlock()
	l := link{vault: owner, installation: id, repo: id + 100_000, name: "octocat/notes-" + strconv.FormatInt(id, 10), branch: "main"}
	v.gh.Lock()
	v.gh.Repos[l.repo] = githubapp.Repository{ID: l.repo, FullName: l.name, Private: true, DefaultBranch: "main"}
	v.gh.InstallationRepos[l.installation] = []int64{l.repo}
	v.gh.Unlock()
	c := githubsync.Connection{
		VaultID: l.vault, InstallationID: l.installation, AccountLogin: "octocat", AccountType: "User",
		RepoID: l.repo, RepoFullName: l.name, RepoPrivate: true, Branch: l.branch, State: githubsync.StateActive,
		ConnectedByUserID: &owner, ConnectedGitHubLogin: "octocat",
	}
	for _, f := range change {
		f(&c)
	}
	l.branch = c.Branch
	require.NoError(v.t, v.store.Insert(context.Background(), c))
	return l
}

func (v *syncEnv) write(vault uuid.UUID, path, body string) {
	v.t.Helper()
	full := filepath.Join(v.raw(vault), filepath.FromSlash(path))
	require.NoError(v.t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(v.t, os.WriteFile(full, []byte(body), 0o600))
}

// save is what a vault writer does: bytes on disk, then Touch.
func (v *syncEnv) save(vault uuid.UUID, path, body string) {
	v.t.Helper()
	v.write(vault, path, body)
	v.committer.Touch(vault, "upload", vault.String(), "web")
}

// commit writes a file and commits at once, without the debounce.
func (v *syncEnv) commit(vault uuid.UUID, path, body string) string {
	v.t.Helper()
	v.write(vault, path, body)
	return v.flush(vault)
}

func (v *syncEnv) flush(vault uuid.UUID) string {
	v.t.Helper()
	_, err := v.committer.Flush(context.Background(), vault, vaultgit.Message{Reason: "upload"})
	require.NoError(v.t, err)
	return v.head(vault)
}

func (v *syncEnv) head(vault uuid.UUID) string {
	v.t.Helper()
	repo, err := vaultgit.Open(v.raw(vault))
	if errors.Is(err, vaultgit.ErrNoRepo) {
		return ""
	}
	require.NoError(v.t, err)
	head, err := repo.Head()
	require.NoError(v.t, err)
	return head
}

func (v *syncEnv) row(vault uuid.UUID) *githubsync.Connection {
	v.t.Helper()
	c, err := v.store.Get(context.Background(), vault)
	require.NoError(v.t, err)
	require.NotNil(v.t, c)
	return c
}

func (v *syncEnv) tip(l link) string { return v.gh.Git.Tip(l.repo, l.branch) }

// idle waits until no sync is running or queued for the vault.
func (v *syncEnv) idle(vault uuid.UUID) {
	v.t.Helper()
	require.Eventually(v.t, func() bool { return !v.syncer.Syncing(vault) }, 20*time.Second, 5*time.Millisecond)
}

// synced waits until GitHub has the vault's HEAD and the row says so.
func (v *syncEnv) synced(l link) string {
	v.t.Helper()
	var head string
	require.Eventually(v.t, func() bool {
		head = v.head(l.vault)
		if head == "" || v.tip(l) != head {
			return false
		}
		c, err := v.store.Get(context.Background(), l.vault)
		return err == nil && c != nil && c.RemoteSHA != nil && *c.RemoteSHA == head
	}, 30*time.Second, 10*time.Millisecond)
	v.idle(l.vault)
	return head
}

func (v *syncEnv) pushes(l link) []githubapptest.GitPush {
	var out []githubapptest.GitPush
	for _, p := range v.gh.GitPushes() {
		if p.Repo == l.repo {
			out = append(out, p)
		}
	}
	return out
}

func (v *syncEnv) gitRequests() int {
	v.gh.Lock()
	defer v.gh.Unlock()
	return v.gh.Git.Requests + v.gh.Git.Unauthorized
}

func (v *syncEnv) git(change func(g *githubapptest.GitRemote)) {
	v.gh.Lock()
	defer v.gh.Unlock()
	change(v.gh.Git)
}

// sweep runs the sweep and waits for every vault it touched. (A sweep that
// finds a sync already running for a vault only asks for one more run.)
func (v *syncEnv) sweep() {
	v.t.Helper()
	_, err := v.syncer.Sweep(context.Background())
	require.NoError(v.t, err)
	v.nextIDm.Lock()
	vaults := append([]uuid.UUID(nil), v.vaults...)
	v.nextIDm.Unlock()
	for _, vault := range vaults {
		v.idle(vault)
	}
}

func (v *syncEnv) commits(vault uuid.UUID) int {
	v.t.Helper()
	repo, err := git.PlainOpen(v.raw(vault))
	require.NoError(v.t, err)
	return countCommits(v.t, repo, "")
}

func countCommits(t *testing.T, repo *git.Repository, from string) int {
	t.Helper()
	opts := &git.LogOptions{}
	if from != "" {
		opts.From = plumbing.NewHash(from)
	}
	iter, err := repo.Log(opts)
	require.NoError(t, err)
	n := 0
	require.NoError(t, iter.ForEach(func(*object.Commit) error { n++; return nil }))
	return n
}

// fsck reads every commit, tree and blob reachable from tip, and returns the
// files of tip. There is no git binary to ask; a missing or corrupt object
// fails the walk.
func fsck(t *testing.T, repo *git.Repository, tip string) map[string]string {
	t.Helper()
	require.True(t, plumbing.IsHash(tip), tip)
	iter, err := repo.Log(&git.LogOptions{From: plumbing.NewHash(tip)})
	require.NoError(t, err)
	files := map[string]string{}
	first := true
	require.NoError(t, iter.ForEach(func(c *object.Commit) error {
		tree, err := c.Tree()
		if err != nil {
			return fmt.Errorf("commit %s: %w", c.Hash, err)
		}
		isTip := first
		first = false
		return tree.Files().ForEach(func(f *object.File) error {
			rd, err := f.Reader()
			if err != nil {
				return fmt.Errorf("blob %s (%s): %w", f.Hash, f.Name, err)
			}
			defer func() { _ = rd.Close() }()
			body, err := io.ReadAll(rd)
			if err != nil {
				return fmt.Errorf("blob %s (%s): %w", f.Hash, f.Name, err)
			}
			if int64(len(body)) != f.Size {
				return fmt.Errorf("blob %s (%s): %d bytes, header says %d", f.Hash, f.Name, len(body), f.Size)
			}
			if isTip {
				files[f.Name] = string(body)
			}
			return nil
		})
	}))
	return files
}

func (v *syncEnv) remoteFiles(l link) map[string]string {
	v.t.Helper()
	return fsck(v.t, v.gh.Git.Repo(l.repo), v.tip(l))
}

func (v *syncEnv) localFiles(vault uuid.UUID) map[string]string {
	v.t.Helper()
	repo, err := git.PlainOpen(v.raw(vault))
	require.NoError(v.t, err)
	return fsck(v.t, repo, v.head(vault))
}

func str(s string) *string { return &s }

// MARK: D17, E4.8

// D17 done-when: a saved note is on GitHub after one commit debounce plus one
// push, with the same commit SHA. Ten saves in a burst are one commit and one
// push. A commit arriving during a push causes exactly one more push.
func TestSavedNoteReachesGitHub(t *testing.T) {
	v := newSyncEnv(t)
	l := v.connect()
	saved := v.clock.now()

	v.save(l.vault, "inbox/plan.md", "# Plan\n")
	head := v.synced(l)
	require.Equal(t, head, v.tip(l), "the same commit SHA")
	require.Equal(t, "# Plan\n", v.remoteFiles(l)["inbox/plan.md"])
	c := v.row(l.vault)
	require.Equal(t, githubsync.StateActive, c.State)
	require.Nil(t, c.ErrorCode)
	require.Zero(t, c.RetryCount)
	require.Nil(t, c.NextRetryAt)
	require.NotNil(t, c.LastPushAt)
	// The debounce is 5 s to 30 s in production and the push follows it at
	// once: no second debounce, no wait for the sweep. On the syncer's clock
	// the push took no time at all.
	require.Less(t, c.LastPushAt.Sub(saved), time.Minute)
	require.Len(t, v.pushes(l), 1)

	// The token was minted for this one repository, with contents and metadata only.
	mints := v.gh.Mints()
	require.Len(t, mints, 1)
	require.Equal(t, []int64{l.repo}, mints[0].RepositoryIDs)
	require.Equal(t, map[string]string{"contents": "write", "metadata": "read"}, mints[0].Permissions)
	cfg, err := os.ReadFile(filepath.Join(v.raw(l.vault), ".git", "config"))
	require.NoError(t, err)
	require.NotContains(t, string(cfg), mints[0].Token)

	// A burst.
	before := v.commits(l.vault)
	for i := range 10 {
		v.save(l.vault, fmt.Sprintf("burst/n%d.md", i), "x")
	}
	require.Eventually(t, func() bool { return v.commits(l.vault) == before+1 }, 10*time.Second, 10*time.Millisecond)
	v.synced(l)
	time.Sleep(300 * time.Millisecond) // nothing more is coming
	require.Equal(t, before+1, v.commits(l.vault), "ten saves, one commit")
	require.Len(t, v.pushes(l), 2, "one commit, one push")
	require.Len(t, v.remoteFiles(l), len(v.localFiles(l.vault)))

	// A push is in flight, held at the remote. Notifies pile up behind it and
	// a commit is made as soon as the push lets go of the vault.
	inFlight, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	v.git(func(g *githubapptest.GitRemote) {
		g.BeforeReceive = func(int64) {
			once.Do(func() {
				close(inFlight)
				<-release
			})
		}
	})
	v.commit(l.vault, "during/a.md", "first")
	<-inFlight
	require.True(t, v.syncer.Syncing(l.vault))
	for range 5 {
		v.syncer.Notify(l.vault)
	}
	committed := make(chan struct{})
	go func() {
		defer close(committed)
		v.commit(l.vault, "during/b.md", "second") // waits for the vault's lock
	}()
	select {
	case <-committed:
		t.Fatal("a commit ran while a push held the vault")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	<-committed
	v.synced(l)
	time.Sleep(300 * time.Millisecond)
	require.Len(t, v.pushes(l), 4, "the push in flight, then exactly one more")
	require.Equal(t, "second", v.remoteFiles(l)["during/b.md"])
	require.Zero(t, v.gh.GitForced())
}

// testPull is a stand-in for E4.9's pull, good enough to prove the push side
// of D6: it fetches the branch, writes the remote's files that the vault
// lacks, and makes a two-parent commit. It exists in this test file only.
func testPull(calls *atomic.Int32, inputs *[]githubsync.PullInput, merge bool) githubsync.PullFunc {
	return func(ctx context.Context, in githubsync.PullInput) error {
		calls.Add(1)
		if inputs != nil {
			*inputs = append(*inputs, in)
		}
		if !merge {
			return nil
		}
		root := in.Repo.Root()
		repo, err := git.PlainOpen(root)
		if err != nil {
			return err
		}
		url, err := in.Repo.RemoteURL()
		if err != nil {
			return err
		}
		branch := in.Connection.Branch
		tracking := plumbing.ReferenceName("refs/remotes/origin/" + branch)
		remote := git.NewRemote(repo.Storer, &config.RemoteConfig{Name: "origin", URLs: []string{url}})
		err = remote.FetchContext(ctx, &git.FetchOptions{
			RefSpecs: []config.RefSpec{config.RefSpec("+refs/heads/" + branch + ":" + tracking.String())},
			Auth:     &githttp.BasicAuth{Username: "x-access-token", Password: in.Token},
		})
		if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
			return fmt.Errorf("fetch: %w", err)
		}
		ref, err := repo.Reference(tracking, true)
		if err != nil {
			return err
		}
		theirs, err := repo.CommitObject(ref.Hash())
		if err != nil {
			return err
		}
		files, err := theirs.Files()
		if err != nil {
			return err
		}
		err = files.ForEach(func(f *object.File) error {
			target := filepath.Join(root, filepath.FromSlash(f.Name))
			if _, err := os.Stat(target); err == nil {
				return nil // local stays
			}
			body, err := f.Contents()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			return os.WriteFile(target, []byte(body), 0o600)
		})
		if err != nil {
			return err
		}
		wt, err := repo.Worktree()
		if err != nil {
			return err
		}
		if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
			return err
		}
		head, err := repo.Head()
		if err != nil {
			return err
		}
		sig := vaultgit.Author
		sig.When = time.Now()
		_, err = wt.Commit("pull: merge "+branch+" from GitHub (0 conflicts)\n\nLV-Reason: pull\n", &git.CommitOptions{
			Author: &sig, Committer: &sig, Parents: []plumbing.Hash{head.Hash(), theirs.Hash},
		})
		return err
	}
}

// D17: remote moved. The push is refused, nothing is forced. Without a pull
// (this PR ships none) that is a failure that keeps retrying; with one, the
// pull runs in the same lock and a two-parent commit is pushed.
func TestRemoteMoved(t *testing.T) {
	var pulls atomic.Int32
	var inputs []githubsync.PullInput
	v := newSyncEnv(t)
	withPuller := v.newSyncer(v.committer, withPull(testPull(&pulls, &inputs, true)))
	l := v.connect()
	v.commit(l.vault, "a.md", "ours")
	first := v.synced(l)

	theirs := v.gh.Git.Commit(l.repo, l.branch, "edit on github", map[string]*string{"from-github.md": str("theirs")})
	require.NotEqual(t, first, theirs)

	// No pull available: E4.8 as it ships.
	v.commit(l.vault, "b.md", "ours too")
	v.idle(l.vault)
	c := v.row(l.vault)
	require.Equal(t, githubsync.StateActive, c.State, "it keeps retrying")
	require.NotNil(t, c.ErrorCode)
	require.Equal(t, githubsync.CodeSyncFailed, *c.ErrorCode)
	require.Equal(t, 1, c.RetryCount)
	require.Equal(t, v.clock.now().Add(30*time.Second), c.NextRetryAt.UTC())
	require.Equal(t, first, *c.RemoteSHA)
	require.Equal(t, theirs, v.tip(l), "GitHub still has their commit")
	require.Zero(t, v.gh.GitForced())
	require.Zero(t, pulls.Load())

	// With a pull: fetch, merge, push, in one attempt.
	v.clock.advance(31 * time.Second)
	_, err := withPuller.Sweep(context.Background())
	require.NoError(t, err)
	head := v.head(l.vault)
	require.Equal(t, head, v.tip(l))
	require.EqualValues(t, 1, pulls.Load())
	require.Len(t, inputs, 1)
	require.NotEmpty(t, inputs[0].Token)
	require.Equal(t, l.repo, inputs[0].Connection.RepoID)
	remote := v.gh.Git.Repo(l.repo)
	merge, err := remote.CommitObject(plumbing.NewHash(head))
	require.NoError(t, err)
	require.Len(t, merge.ParentHashes, 2, "a two-parent commit")
	require.Contains(t, merge.ParentHashes, plumbing.NewHash(theirs))
	require.Zero(t, v.gh.GitForced(), "the stub never saw a force")
	files := v.remoteFiles(l)
	require.Equal(t, "theirs", files["from-github.md"])
	require.Equal(t, "ours", files["a.md"])
	require.Equal(t, "ours too", files["b.md"])
	c = v.row(l.vault)
	require.Nil(t, c.ErrorCode)
	require.Zero(t, c.RetryCount)
	require.Equal(t, head, *c.RemoteSHA)

	// D12: the merge commit is on GitHub and is HEAD. Being told about it
	// again (a notify, a sweep, GitHub's own push event later) does nothing.
	requests, pushes := v.gitRequests(), len(v.pushes(l))
	withPuller.Notify(l.vault)
	_, err = withPuller.Sweep(context.Background())
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !withPuller.Syncing(l.vault) }, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, requests, v.gitRequests(), "no fetch, no push")
	require.Len(t, v.pushes(l), pushes)
	require.EqualValues(t, 1, pulls.Load())
	repo, err := vaultgit.Open(v.raw(l.vault))
	require.NoError(t, err)
	for _, sha := range []string{head, theirs, first} {
		known, err := repo.Contains(sha)
		require.NoError(t, err)
		require.True(t, known, "the webhook intake ignores a commit that is HEAD or behind it")
	}
}

// D6: at most 3 rounds of pull and push in one attempt; after that it is a
// transient failure. And a first sync to a branch that already has commits
// (D11) hands the pull the owner's merge consent as the connect flow saved it.
func TestRemoteMovedRoundsAndMergeConsent(t *testing.T) {
	var pulls atomic.Int32
	var inputs []githubsync.PullInput
	v := newSyncEnv(t, withPull(testPull(&pulls, &inputs, false))) // a pull that changes nothing
	l := v.connect(func(c *githubsync.Connection) { c.MergeExisting = true })
	theirs := v.gh.Git.Commit(l.repo, l.branch, "their vault", map[string]*string{"README.md": str("hello")})

	v.commit(l.vault, "a.md", "ours")
	v.idle(l.vault)
	require.EqualValues(t, 3, pulls.Load(), "three rounds, then stop")
	require.Len(t, inputs, 3)
	require.True(t, inputs[0].Connection.MergeExisting)
	c := v.row(l.vault)
	require.Equal(t, githubsync.StateActive, c.State)
	require.Equal(t, githubsync.CodeSyncFailed, *c.ErrorCode)
	require.Nil(t, c.RemoteSHA)
	require.Equal(t, theirs, v.tip(l), "an unrelated history is never overwritten")
	require.Zero(t, v.gh.GitForced())

	// No consent recorded: the pull is told so.
	other := v.connect()
	v.gh.Git.Commit(other.repo, other.branch, "appeared after connect", map[string]*string{"README.md": str("hello")})
	v.commit(other.vault, "a.md", "ours")
	v.idle(other.vault)
	require.False(t, inputs[len(inputs)-1].Connection.MergeExisting)
	require.Zero(t, v.gh.GitForced())
}

// D17: the transient failure schedule (30 s, 2 min, 10 min, 30 min, 1 h),
// honoured by notifies and sweeps alike, and cleared by a success.
func TestTransientFailureSchedule(t *testing.T) {
	v := newSyncEnv(t)
	l := v.connect()
	v.git(func(g *githubapptest.GitRemote) { g.Status[l.repo] = http.StatusServiceUnavailable })

	v.commit(l.vault, "a.md", "one")
	v.idle(l.vault)
	want := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour, time.Hour, time.Hour}
	for i, wait := range want {
		c := v.row(l.vault)
		require.Equal(t, githubsync.StateActive, c.State)
		require.Equal(t, githubsync.CodeSyncFailed, *c.ErrorCode)
		require.Equal(t, v.clock.now(), c.ErrorAt.UTC())
		require.Equal(t, i+1, c.RetryCount)
		require.Equal(t, v.clock.now().Add(wait), c.NextRetryAt.UTC(), "failure %d", i+1)
		require.Nil(t, c.RemoteSHA)

		// Not yet: neither a new commit nor a sweep reaches GitHub early.
		v.clock.advance(wait - time.Second)
		requests := v.gitRequests()
		v.commit(l.vault, "a.md", fmt.Sprintf("edit %d", i))
		v.sweep()
		v.idle(l.vault)
		require.Equal(t, requests, v.gitRequests(), "inside the wait after failure %d", i+1)
		require.Equal(t, i+1, v.row(l.vault).RetryCount)

		v.clock.advance(time.Second)
		v.sweep()
		require.Greater(t, v.gitRequests(), requests)
	}

	v.git(func(g *githubapptest.GitRemote) { delete(g.Status, l.repo) })
	v.clock.advance(time.Hour)
	v.sweep()
	head := v.synced(l)
	c := v.row(l.vault)
	require.Nil(t, c.ErrorCode)
	require.Nil(t, c.ErrorAt)
	require.Nil(t, c.NextRetryAt)
	require.Zero(t, c.RetryCount)
	require.Equal(t, head, *c.RemoteSHA)
	require.Equal(t, v.clock.now(), c.LastPushAt.UTC())
	require.Equal(t, 30*time.Second, githubsync.Backoff(0))
	require.Equal(t, time.Hour, githubsync.Backoff(99))
}

// GitHub's rate limit schedules the retry for when GitHub said, and "Sync
// now" does not cut that wait short; an ordinary retry wait it does.
func TestRateLimitAndSyncNow(t *testing.T) {
	v := newSyncEnv(t)
	ctx := context.Background()
	limited := v.connect()
	reset := v.clock.now().Add(17 * time.Minute)
	var header atomic.Pointer[http.Header]
	set := func(h http.Header) { header.Store(&h) }
	set(http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(reset.Unix(), 10)}})
	mintPath := "/app/installations/" + strconv.FormatInt(limited.installation, 10) + "/access_tokens"
	v.gh.Lock()
	v.gh.Intercept = func(w http.ResponseWriter, r *http.Request) bool {
		h := header.Load()
		if r.URL.Path != mintPath || h == nil || *h == nil {
			return false
		}
		for k, vals := range *h {
			w.Header()[k] = vals
		}
		if w.Header().Get("Retry-After") != "" {
			w.WriteHeader(http.StatusTooManyRequests)
		} else {
			w.WriteHeader(http.StatusForbidden)
		}
		return true
	}
	v.gh.Unlock()

	v.commit(limited.vault, "a.md", "one")
	v.idle(limited.vault)
	c := v.row(limited.vault)
	require.Equal(t, githubsync.StateActive, c.State)
	require.Equal(t, githubsync.CodeRateLimited, *c.ErrorCode)
	require.Equal(t, reset, c.NextRetryAt.UTC(), "when GitHub said, not the 30 s of the schedule")
	require.Zero(t, v.gitRequests(), "no token, no push")

	require.NoError(t, v.syncer.SyncNow(ctx, limited.vault))
	v.idle(limited.vault)
	require.Equal(t, reset, v.row(limited.vault).NextRetryAt.UTC(), "Sync now does not hammer a rate limit")
	require.Empty(t, v.gh.Mints())

	// A secondary limit names a Retry-After.
	v.clock.advance(17 * time.Minute)
	set(http.Header{"Retry-After": {"120"}})
	v.sweep()
	c = v.row(limited.vault)
	require.Equal(t, githubsync.CodeRateLimited, *c.ErrorCode)
	require.Equal(t, v.clock.now().Add(2*time.Minute), c.NextRetryAt.UTC())
	require.Equal(t, 2, c.RetryCount)

	set(nil)
	v.clock.advance(2 * time.Minute)
	v.sweep()
	v.synced(limited)
	require.Nil(t, v.row(limited.vault).ErrorCode)

	// An ordinary failure: "Sync now" makes it due at once.
	plain := v.connect()
	v.git(func(g *githubapptest.GitRemote) { g.Status[plain.repo] = http.StatusBadGateway })
	v.commit(plain.vault, "a.md", "one")
	v.idle(plain.vault)
	require.Equal(t, githubsync.CodeSyncFailed, *v.row(plain.vault).ErrorCode)
	require.NotNil(t, v.row(plain.vault).NextRetryAt)
	v.git(func(g *githubapptest.GitRemote) { delete(g.Status, plain.repo) })
	require.NoError(t, v.syncer.SyncNow(ctx, plain.vault))
	v.synced(plain)
	require.Nil(t, v.row(plain.vault).ErrorCode)
}

// D6's table: failures that need a person stop retries and set
// needs_attention. What GitHub documents ambiguously is believed only when it
// repeats.
func TestFailuresThatNeedAPerson(t *testing.T) {
	v := newSyncEnv(t)
	ctx := context.Background()
	state := func(l link) (string, string, int) {
		c := v.row(l.vault)
		code := ""
		if c.ErrorCode != nil {
			code = *c.ErrorCode
		}
		return c.State, code, c.RetryCount
	}
	// retry lets the wait pass and sweeps.
	retry := func() {
		v.clock.advance(2 * time.Hour)
		v.sweep()
	}
	stopped := func(l link, code string) {
		t.Helper()
		s, got, _ := state(l)
		require.Equal(t, githubsync.StateNeedsAttention, s)
		require.Equal(t, code, got)
		require.Nil(t, v.row(l.vault).NextRetryAt)
		// Stopped means stopped: a commit and a sweep reach nobody.
		mints, requests := len(v.gh.Mints()), v.gitRequests()
		v.commit(l.vault, "later.md", time.Now().String())
		retry()
		v.idle(l.vault)
		require.Len(t, v.gh.Mints(), mints)
		require.Equal(t, requests, v.gitRequests())
	}

	// Branch protection refuses the App's push: final at once.
	protected := v.connect()
	v.git(func(g *githubapptest.GitRemote) { g.Reject[protected.repo] = "protected branch hook declined" })
	v.commit(protected.vault, "a.md", "one")
	v.idle(protected.vault)
	stopped(protected, githubsync.CodePushRejected)
	require.Empty(t, v.tip(protected))
	// The owner lifts the rule and asks for a sync.
	v.git(func(g *githubapptest.GitRemote) { delete(g.Reject, protected.repo) })
	require.NoError(t, v.syncer.SyncNow(ctx, protected.vault))
	v.synced(protected)
	s, code, _ := state(protected)
	require.Equal(t, githubsync.StateActive, s)
	require.Empty(t, code)

	// The App was uninstalled: a 404 minting the token, final at once, and
	// "Sync now" cannot bring it back (a new installation needs a reconnect).
	removed := v.connect()
	v.gh.Lock()
	delete(v.gh.InstallationRepos, removed.installation)
	v.gh.Unlock()
	v.commit(removed.vault, "a.md", "one")
	v.idle(removed.vault)
	stopped(removed, githubsync.CodeInstallationRemoved)
	require.NoError(t, v.syncer.SyncNow(ctx, removed.vault))
	v.idle(removed.vault)
	s, code, _ = state(removed)
	require.Equal(t, githubsync.StateNeedsAttention, s)
	require.Equal(t, githubsync.CodeInstallationRemoved, code)

	// The repository left the installation: a 422, which GitHub also answers
	// to a burst. It is a retry twice, final the third time running.
	gone := v.connect()
	v.gh.Lock()
	v.gh.InstallationRepos[gone.installation] = []int64{}
	v.gh.Unlock()
	v.commit(gone.vault, "a.md", "one")
	v.idle(gone.vault)
	for attempt := 1; attempt <= 2; attempt++ {
		s, code, n := state(gone)
		require.Equal(t, githubsync.StateActive, s, "attempt %d is not believed yet", attempt)
		require.Equal(t, githubsync.CodeRepoAccessRemoved, code)
		require.Equal(t, attempt, n)
		retry()
	}
	stopped(gone, githubsync.CodeRepoAccessRemoved)
	// Added back on GitHub: "Sync now" picks it up.
	v.gh.Lock()
	v.gh.InstallationRepos[gone.installation] = []int64{gone.repo}
	v.gh.Unlock()
	require.NoError(t, v.syncer.SyncNow(ctx, gone.vault))
	v.synced(gone)

	// One 422 that does not repeat is forgotten.
	blip := v.connect()
	v.gh.Lock()
	v.gh.InstallationRepos[blip.installation] = []int64{}
	v.gh.Unlock()
	v.commit(blip.vault, "a.md", "one")
	v.idle(blip.vault)
	v.gh.Lock()
	v.gh.InstallationRepos[blip.installation] = []int64{blip.repo}
	v.gh.Unlock()
	retry()
	v.synced(blip)
	s, code, n := state(blip)
	require.Equal(t, githubsync.StateActive, s)
	require.Empty(t, code)
	require.Zero(t, n)

	// A suspended installation: a 403 minting the token, three times running.
	suspended := v.connect()
	mintPath := "/app/installations/" + strconv.FormatInt(suspended.installation, 10) + "/access_tokens"
	v.gh.Lock()
	v.gh.Intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != mintPath {
			return false
		}
		w.WriteHeader(http.StatusForbidden)
		return true
	}
	v.gh.Unlock()
	v.commit(suspended.vault, "a.md", "one")
	v.idle(suspended.vault)
	retry()
	s, code, _ = state(suspended)
	require.Equal(t, githubsync.StateActive, s)
	require.Equal(t, githubsync.CodeInstallationSuspended, code)
	retry()
	stopped(suspended, githubsync.CodeInstallationSuspended)

	// The remote forbids the push itself (an archived repository answers
	// 403): push_rejected, once it has repeated.
	archived := v.connect()
	v.git(func(g *githubapptest.GitRemote) { g.Status[archived.repo] = http.StatusForbidden })
	v.commit(archived.vault, "a.md", "one")
	v.idle(archived.vault)
	retry()
	s, _, _ = state(archived)
	require.Equal(t, githubsync.StateActive, s)
	retry()
	stopped(archived, githubsync.CodePushRejected)

	// A token GitHub no longer accepts is dropped and minted again.
	stale := v.connect()
	v.commit(stale.vault, "a.md", "one")
	v.synced(stale)
	var token string
	for _, m := range v.gh.Mints() {
		if m.Installation == stale.installation {
			token = m.Token
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, v.gh.URL+"/installation/token", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req) // revoked behind the client's back
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	v.commit(stale.vault, "a.md", "two")
	v.idle(stale.vault)
	s, code, _ = state(stale)
	require.Equal(t, githubsync.StateActive, s)
	require.Equal(t, githubsync.CodeSyncFailed, code)
	retry()
	v.synced(stale)

	require.Zero(t, v.gh.GitForced())
}

// D17: a restart between a commit and its push loses the in-memory trigger;
// GitHubSyncSweep pushes it. The sweep also makes the first push of a row the
// connect flow saved, and leaves archived vaults and stopped connections alone.
func TestSweepPushesWhatATriggerMissed(t *testing.T) {
	v := newSyncEnv(t)
	l := v.connect()
	v.listen(nil) // the process that made the commit never told anyone
	head := v.commit(l.vault, "a.md", "one")
	time.Sleep(100 * time.Millisecond)
	require.Empty(t, v.tip(l))
	require.Nil(t, v.row(l.vault).RemoteSHA, "connected by E4.7: active, never pushed")

	restarted := v.newSyncer(v.committer)
	job := restarted.SweepJob()
	require.Equal(t, "GitHubSyncSweep", job.Name)
	require.NoError(t, job.Run(context.Background()))
	require.Equal(t, head, v.tip(l))
	require.Equal(t, head, *v.row(l.vault).RemoteSHA)

	// Nothing new: the next sweep asks GitHub nothing.
	requests, mints := v.gitRequests(), len(v.gh.Mints())
	require.NoError(t, job.Run(context.Background()))
	require.Equal(t, requests, v.gitRequests())
	require.Len(t, v.gh.Mints(), mints)

	// A vault with no history yet, an archived vault (D13) and a connection
	// that needs attention are skipped.
	empty := v.connect()
	archived := v.connect()
	v.commit(archived.vault, "a.md", "one")
	_, err := v.pool.Exec(context.Background(), `UPDATE vaults SET archived_at = now() WHERE id = $1`, archived.vault)
	require.NoError(t, err)
	stopped := v.connect()
	v.commit(stopped.vault, "a.md", "one")
	_, err = v.pool.Exec(context.Background(), `UPDATE github_sync_connections SET state = 'needs_attention', error_code = 'push_rejected' WHERE vault_id = $1`, stopped.vault)
	require.NoError(t, err)
	require.NoError(t, job.Run(context.Background()))
	restarted.Notify(archived.vault)
	require.Eventually(t, func() bool { return !restarted.Syncing(archived.vault) }, 5*time.Second, 5*time.Millisecond)
	for _, skipped := range []link{empty, archived, stopped} {
		require.Empty(t, v.tip(skipped))
		require.Nil(t, v.row(skipped.vault).RemoteSHA)
	}
	require.Equal(t, requests, v.gitRequests())

	// A vault that is not connected at all costs one read and nothing else.
	loner := user(t, v.pool)
	v.listen(restarted.OnCommit)
	v.commit(loner, "a.md", "one")
	require.Eventually(t, func() bool { return !restarted.Syncing(loner) }, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, requests, v.gitRequests())

	// After Stop a notify is ignored: a commit flushed at shutdown waits for
	// the next start's sweep.
	restarted.Stop()
	v.commit(l.vault, "a.md", "two")
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, head, v.tip(l))
	_, err = restarted.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, head, v.tip(l))
	v.sweep()
	require.Equal(t, v.head(l.vault), v.tip(l))
}

// D17: a repository renamed on GitHub is followed through
// GET /repositories/{id}; the stored name and the remote are updated. A
// repository that is gone is not a rename.
func TestRenameIsFollowedByID(t *testing.T) {
	v := newSyncEnv(t)
	l := v.connect()
	v.commit(l.vault, "a.md", "one")
	v.synced(l)

	renamed := "octocat/second-brain"
	v.gh.Lock()
	repo := v.gh.Repos[l.repo]
	repo.FullName = renamed
	v.gh.Repos[l.repo] = repo
	v.gh.Renamed[l.name] = l.repo
	v.gh.Unlock()

	head := v.commit(l.vault, "a.md", "two")
	v.synced(l)
	require.Equal(t, head, v.tip(l))
	c := v.row(l.vault)
	require.Equal(t, renamed, c.RepoFullName)
	require.Nil(t, c.ErrorCode)
	require.Zero(t, c.RetryCount, "a rename is not an error")
	repoLocal, err := vaultgit.Open(v.raw(l.vault))
	require.NoError(t, err)
	url, err := repoLocal.RemoteURL()
	require.NoError(t, err)
	require.Equal(t, v.gh.URL+"/"+renamed+".git", url)
	v.gh.Lock()
	followed := v.gh.Followed
	v.gh.Unlock()
	require.Zero(t, followed, "no redirect was followed")

	// The next push goes straight to the new name.
	requests := v.gitRequests()
	v.commit(l.vault, "a.md", "three")
	v.synced(l)
	require.Equal(t, requests+2, v.gitRequests(), "one advertisement, one push")

	// Deleted on GitHub: the git side and the lookup by id both answer 404.
	// Like the 422, it is believed when it repeats.
	deleted := v.connect()
	v.commit(deleted.vault, "a.md", "one")
	v.synced(deleted)
	v.gh.Lock()
	delete(v.gh.Repos, deleted.repo)
	v.gh.Unlock()
	v.commit(deleted.vault, "a.md", "two")
	v.idle(deleted.vault)
	c = v.row(deleted.vault)
	require.Equal(t, githubsync.StateActive, c.State)
	require.Equal(t, githubsync.CodeRepoAccessRemoved, *c.ErrorCode)
	for range 2 {
		v.clock.advance(2 * time.Hour)
		v.sweep()
	}
	c = v.row(deleted.vault)
	require.Equal(t, githubsync.StateNeedsAttention, c.State)
	require.Equal(t, githubsync.CodeRepoAccessRemoved, *c.ErrorCode)
}

// D8: a push never removes a held file from GitHub, and never sends a file
// the vault keeps out of history.
func TestHeldFilesSurviveAPush(t *testing.T) {
	v := newSyncEnv(t)
	l := v.connect()
	held := map[string]string{
		".obsidian/app.json": `{"theme":"obsidian"}`,
		"My Note.md":         "a name the vault cannot hold",
		"diagram.canvas":     `{"nodes":[]}`,
	}
	// What a pull leaves behind (E4.9): the entries are in the index and in
	// HEAD, the list names them, and raw/ does not have them.
	for path, body := range held {
		v.write(l.vault, path, body)
	}
	v.commit(l.vault, "note.md", "a note")
	require.NoError(t, v.committer.WithRepo(context.Background(), l.vault, func(r *vaultgit.Repo) error {
		paths := []string{}
		for p := range held {
			paths = append(paths, p)
		}
		return r.SetHeld(paths)
	}))
	for path := range held {
		require.NoError(t, os.Remove(filepath.Join(v.raw(l.vault), filepath.FromSlash(path))))
	}
	require.NoError(t, os.Remove(filepath.Join(v.raw(l.vault), ".obsidian")))
	// A file over the history limit saves normally and has no history.
	v.write(l.vault, "big.pdf", strings.Repeat("x", int(vaultgit.DefaultMaxFileSize)+1))

	for i := range 3 {
		before := v.head(l.vault)
		v.save(l.vault, "note.md", fmt.Sprintf("edit %d", i))
		require.Eventually(t, func() bool { return v.head(l.vault) != before }, 10*time.Second, 10*time.Millisecond)
		v.synced(l)
		files := v.remoteFiles(l)
		for path, body := range held {
			require.Equal(t, body, files[path], "%s is still on GitHub, byte-identical, after push %d", path, i+1)
		}
		require.Equal(t, fmt.Sprintf("edit %d", i), files["note.md"])
		require.NotContains(t, files, "big.pdf")
		require.NotContains(t, files, ".git/lv-held")
	}
	// A sweep commit (the committer's own, every 10 minutes) does not delete
	// them either.
	_, _, err := v.committer.Sweep(context.Background(), v.root)
	require.NoError(t, err)
	for path := range held {
		_, err := os.Stat(filepath.Join(v.raw(l.vault), filepath.FromSlash(path)))
		require.ErrorIs(t, err, os.ErrNotExist, "held bytes never reach raw/")
	}
	require.Equal(t, v.remoteFiles(l), v.localFiles(l.vault))
}

// D13: disconnect removes the origin remote and its refs; history, the held
// list and GitHub stay as they are, and nothing is pushed afterwards.
func TestDisconnectedRemovesTheRemote(t *testing.T) {
	v := newSyncEnv(t)
	ctx := context.Background()
	l := v.connect()
	v.commit(l.vault, "a.md", "one")
	head := v.synced(l)
	with := func(fn func(r *vaultgit.Repo)) {
		t.Helper()
		require.NoError(t, v.committer.WithRepo(ctx, l.vault, func(r *vaultgit.Repo) error { fn(r); return nil }))
	}
	with(func(r *vaultgit.Repo) {
		url, err := r.RemoteURL()
		require.NoError(t, err)
		require.Equal(t, v.gh.URL+"/"+l.name+".git", url)
		require.NoError(t, r.SetHeld([]string{"My Note.md"}))
	})

	// A new connection made before the removal ran keeps its remote.
	v.syncer.Disconnected(ctx, l.vault)
	with(func(r *vaultgit.Repo) {
		url, _ := r.RemoteURL()
		require.NotEmpty(t, url, "the row is still there: this was not a disconnect")
	})

	gone, err := v.store.Delete(ctx, l.vault)
	require.NoError(t, err)
	require.NotNil(t, gone)
	v.syncer.Disconnected(ctx, l.vault)
	with(func(r *vaultgit.Repo) {
		url, err := r.RemoteURL()
		require.NoError(t, err)
		require.Empty(t, url)
		refs, err := r.RemoteRefs()
		require.NoError(t, err)
		require.Empty(t, refs)
		heldPaths, err := r.Held()
		require.NoError(t, err)
		require.Equal(t, []string{"My Note.md"}, heldPaths, "the held list stays (D8)")
	})
	cfg, err := os.ReadFile(filepath.Join(v.raw(l.vault), ".git", "config"))
	require.NoError(t, err)
	require.NotContains(t, string(cfg), "remote")
	require.Equal(t, head, v.head(l.vault))
	require.Equal(t, head, v.tip(l), "the repository on GitHub is not touched")

	// Local commits go on; none is pushed.
	requests := v.gitRequests()
	v.commit(l.vault, "a.md", "two")
	v.sweep()
	v.idle(l.vault)
	require.Equal(t, requests, v.gitRequests())
	require.Equal(t, head, v.tip(l))

	// Reconnect to the same repository: the remote comes back with the first
	// push, which is a fast-forward of what GitHub has.
	owner := l.vault
	require.NoError(t, v.store.Insert(ctx, githubsync.Connection{
		VaultID: l.vault, InstallationID: l.installation, AccountLogin: "octocat", AccountType: "User",
		RepoID: l.repo, RepoFullName: l.name, RepoPrivate: true, Branch: l.branch, State: githubsync.StateActive,
		ConnectedByUserID: &owner, ConnectedGitHubLogin: "octocat",
	}))
	v.sweep()
	v.synced(l)
	require.Zero(t, v.gh.GitForced())

	// A disconnect while a push is in flight: the remote goes once the push
	// has let go of the vault, and the push's result is written to no row.
	inFlight, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	v.git(func(g *githubapptest.GitRemote) {
		g.BeforeReceive = func(int64) { once.Do(func() { close(inFlight); <-release }) }
	})
	v.commit(l.vault, "a.md", "three")
	<-inFlight
	_, err = v.store.Delete(ctx, l.vault)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() { defer close(done); v.syncer.Disconnected(ctx, l.vault) }()
	time.Sleep(100 * time.Millisecond)
	close(release)
	<-done
	require.Eventually(t, func() bool {
		repo, err := vaultgit.Open(v.raw(l.vault))
		if err != nil {
			return false
		}
		url, err := repo.RemoteURL()
		return err == nil && url == ""
	}, 10*time.Second, 20*time.Millisecond)
	c, err := v.store.Get(ctx, l.vault)
	require.NoError(t, err)
	require.Nil(t, c)
}

// D17: push and commit on one tenant never overlap. Two replicas (two
// committers and two syncers that share nothing but the database and the
// disk) take 50 parallel uploads while pushes and sweeps run; every commit
// reaches GitHub exactly once, nothing is forced, and both repositories are
// whole afterwards. Run with -race.
func TestTwoReplicasNeverOverlap(t *testing.T) {
	v := newSyncEnv(t)
	l := v.connect()
	other := v.newCommitter()
	second := v.newSyncer(other)
	v.listen(func(tenant uuid.UUID, sha string) {
		v.syncer.OnCommit(tenant, sha)
		second.OnCommit(tenant, sha) // the other replica hears of it too (its sweep, in life)
	})

	// One commit, both replicas told many times over: one push.
	head := v.commit(l.vault, "a.md", "one")
	for range 20 {
		v.syncer.Notify(l.vault)
		second.Notify(l.vault)
	}
	v.synced(l)
	require.Eventually(t, func() bool { return !second.Syncing(l.vault) }, 10*time.Second, 5*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	require.Len(t, v.pushes(l), 1, "two replicas, one commit, one push")
	require.Equal(t, head, v.tip(l))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for _, s := range []*githubsync.Syncer{v.syncer, second} {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = s.Sweep(context.Background())
					time.Sleep(15 * time.Millisecond)
				}
			}
		})
	}
	var uploads sync.WaitGroup
	for i := range 50 {
		uploads.Go(func() {
			committer := v.committer
			if i%2 == 1 {
				committer = other
			}
			for j := range 4 {
				v.write(l.vault, fmt.Sprintf("up/%02d.md", i), fmt.Sprintf("upload %d, edit %d", i, j))
				committer.Touch(l.vault, "upload", l.vault.String(), "web")
				time.Sleep(time.Duration(5+i%7) * time.Millisecond)
			}
		})
	}
	uploads.Wait()
	// Whatever a debounce is still holding.
	require.Eventually(t, func() bool {
		repo, err := vaultgit.Open(v.raw(l.vault))
		if err != nil {
			return false
		}
		dirty, err := repo.Dirty(vaultgit.DefaultMaxFileSize)
		return err == nil && !dirty
	}, 30*time.Second, 25*time.Millisecond)
	v.synced(l)
	close(stop)
	wg.Wait()
	require.Eventually(t, func() bool { return !second.Syncing(l.vault) }, 10*time.Second, 5*time.Millisecond)

	require.Zero(t, v.gh.GitForced())
	local, remote := v.localFiles(l.vault), v.remoteFiles(l)
	require.Equal(t, local, remote)
	for i := range 50 {
		require.Equal(t, fmt.Sprintf("upload %d, edit 3", i), remote[fmt.Sprintf("up/%02d.md", i)])
	}
	// Every push was a fast-forward from the one before: a straight line.
	pushes := v.pushes(l)
	for i := 1; i < len(pushes); i++ {
		require.Equal(t, pushes[i-1].New, pushes[i].Old)
	}
	require.LessOrEqual(t, len(pushes), v.commits(l.vault), "never more pushes than commits")
	require.Equal(t, v.commits(l.vault), countCommits(t, v.gh.Git.Repo(l.repo), v.tip(l)))
	c := v.row(l.vault)
	require.Nil(t, c.ErrorCode)
	require.Zero(t, c.RetryCount, "a vault locked by the other replica is not a failure")
}
