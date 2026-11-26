package vaultgit

import (
	"context"
	"errors"
	"fmt"
	"lumina/internal/githubapp"
	"lumina/internal/githubapp/githubapptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Sync spec D17 (E4.6, left to E4.8): OnCommit fires once per commit, the
// first commit of a new repository included, and never for an empty flush.
func TestOnCommitFiresOncePerCommit(t *testing.T) {
	root := t.TempDir()
	tenant := uuid.New()
	var (
		mu   sync.Mutex
		seen []string
	)
	c := NewCommitter(Options{
		RawRoot: func(uuid.UUID) string { return root },
		OnCommit: func(got uuid.UUID, sha string) {
			require.Equal(t, tenant, got)
			mu.Lock()
			seen = append(seen, sha)
			mu.Unlock()
		},
	})
	defer c.Close(context.Background())
	ctx := context.Background()
	calls := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}

	// A new vault: the flush makes the repository and its initial import,
	// and returns "" as it always did. The hook still hears about the commit.
	write(t, root, "a.md", "one")
	sha, err := c.Flush(ctx, tenant, Message{Reason: "upload"})
	require.NoError(t, err)
	require.Empty(t, sha)
	repo, err := Open(root)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, []string{head}, calls())

	// Nothing changed: no commit, no call.
	sha, err = c.Flush(ctx, tenant, Message{Reason: "sweep"})
	require.NoError(t, err)
	require.Empty(t, sha)
	require.Len(t, calls(), 1)

	write(t, root, "a.md", "two")
	sha, err = c.Flush(ctx, tenant, Message{Reason: "upload"})
	require.NoError(t, err)
	require.NotEmpty(t, sha)
	require.Equal(t, []string{head, sha}, calls())

	// The sweep goes through Flush, so it notifies too, and again not for a
	// clean tree.
	tenants := t.TempDir()
	rawOf := func(id uuid.UUID) string { return filepath.Join(tenants, id.String(), "raw") }
	var swept atomic.Int32
	sc := NewCommitter(Options{RawRoot: rawOf, OnCommit: func(uuid.UUID, string) { swept.Add(1) }})
	defer sc.Close(context.Background())
	write(t, rawOf(tenant), "n.md", "x")
	_, _, err = sc.Sweep(ctx, tenants)
	require.NoError(t, err)
	_, _, err = sc.Sweep(ctx, tenants)
	require.NoError(t, err)
	require.EqualValues(t, 1, swept.Load())
}

// The hook runs after the tenant's locks are released: it can take them.
func TestOnCommitRunsAfterTheLockIsReleased(t *testing.T) {
	root := t.TempDir()
	tenant := uuid.New()
	var c *Committer
	ran := false
	c = NewCommitter(Options{
		RawRoot: func(uuid.UUID) string { return root },
		Lock:    &fakeLocker{},
		OnCommit: func(tenant uuid.UUID, sha string) {
			err := c.WithRepo(context.Background(), tenant, func(r *Repo) error {
				head, err := r.Head()
				require.NoError(t, err)
				require.Equal(t, sha, head)
				ran = true
				return nil
			})
			require.NoError(t, err)
		},
	})
	defer c.Close(context.Background())
	write(t, root, "a.md", "one")
	done := make(chan error, 1)
	go func() {
		_, err := c.Flush(context.Background(), tenant, Message{Reason: "upload"})
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Flush deadlocked: OnCommit ran inside the tenant lock")
	}
	require.True(t, ran)
}

// fakeLocker is a cross-replica lock for tests: one holder per tenant.
type fakeLocker struct {
	mu   sync.Mutex
	held map[uuid.UUID]bool
}

func (l *fakeLocker) TryLock(_ context.Context, tenant uuid.UUID) (func(), bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		l.held = map[uuid.UUID]bool{}
	}
	if l.held[tenant] {
		return nil, false, nil
	}
	l.held[tenant] = true
	return func() {
		l.mu.Lock()
		delete(l.held, tenant)
		l.mu.Unlock()
	}, true, nil
}

// Sync spec D5, D17: WithRepo holds the lock Flush and gc take. Neither runs
// while it does, on this replica (the mutex) or on another (the advisory
// lock), and it does not run while they do.
func TestWithRepoExcludesFlushAndGC(t *testing.T) {
	root := t.TempDir()
	tenant := uuid.New()
	shared := &fakeLocker{}
	rawRoot := func(uuid.UUID) string { return root }
	a := NewCommitter(Options{RawRoot: rawRoot, Lock: shared})
	b := NewCommitter(Options{RawRoot: rawRoot, Lock: shared}) // "another replica"
	defer a.Close(context.Background())
	defer b.Close(context.Background())
	ctx := context.Background()

	require.ErrorIs(t, a.WithRepo(ctx, tenant, func(*Repo) error { return nil }), ErrNoRepo)
	write(t, root, "a.md", "one")
	_, err := a.Flush(ctx, tenant, Message{Reason: "upload"})
	require.NoError(t, err)

	inside, release := make(chan struct{}), make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- a.WithRepo(ctx, tenant, func(*Repo) error {
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside

	// Another replica: refused at once, for a commit, a gc and a WithRepo.
	write(t, root, "a.md", "two")
	_, err = b.Flush(ctx, tenant, Message{Reason: "upload"})
	require.ErrorIs(t, err, ErrLocked)
	require.ErrorIs(t, b.gcTenant(ctx, tenant), ErrLocked)
	require.ErrorIs(t, b.WithRepo(ctx, tenant, func(*Repo) error { return nil }), ErrLocked)

	// This replica: a commit and a gc wait for it.
	var flushed, collected atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		_, err := a.Flush(ctx, tenant, Message{Reason: "upload"})
		require.NoError(t, err)
		flushed.Store(true)
	})
	wg.Go(func() {
		require.NoError(t, a.gcTenant(ctx, tenant))
		collected.Store(true)
	})
	time.Sleep(150 * time.Millisecond)
	require.False(t, flushed.Load(), "Flush ran inside WithRepo")
	require.False(t, collected.Load(), "gc ran inside WithRepo")
	require.Equal(t, 1, commitCount(t, root))
	close(release)
	require.NoError(t, <-held)
	wg.Wait()
	require.Equal(t, 2, commitCount(t, root))

	// An error from fn comes back, and the lock is released after it.
	boom := errors.New("boom")
	require.ErrorIs(t, a.WithRepo(ctx, tenant, func(*Repo) error { return boom }), boom)
	require.NoError(t, b.WithRepo(ctx, tenant, func(*Repo) error { return nil }))
}

func tracked(t *testing.T, root, path string) bool {
	t.Helper()
	repo, err := git.PlainOpen(root)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	c, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	_, err = c.File(path)
	return err == nil
}

// Sync spec D8: a held path lives in git only. Missing from raw/, it is never
// committed as a deletion; with no list, behaviour is what it always was.
func TestHeldPathsAreNeverCommittedAsDeletions(t *testing.T) {
	root := t.TempDir()
	write(t, root, ".obsidian/app.json", `{"theme":"dark"}`)
	write(t, root, "My Note.md", "from obsidian")
	write(t, root, "note.md", "a note")
	repo, _, err := Init(root, DefaultMaxFileSize)
	require.NoError(t, err)

	held, err := repo.Held()
	require.NoError(t, err)
	require.Empty(t, held)
	require.NoError(t, repo.SetHeld([]string{"My Note.md", ".obsidian/app.json", "never/in/the/index.bin"}))
	held, err = repo.Held()
	require.NoError(t, err)
	require.Equal(t, []string{".obsidian/app.json", "My Note.md", "never/in/the/index.bin"}, held)
	// Git allows a newline in a path: such a path is listed like any other.
	require.NoError(t, repo.SetHeld([]string{"a\nb", "c"}))
	held, err = repo.Held()
	require.NoError(t, err)
	require.Equal(t, []string{"a\nb", "c"}, held)
	require.Error(t, repo.SetHeld([]string{"a\x00b"}))
	require.Error(t, repo.SetHeld([]string{""}))
	// A list from before (one path per line) still reads.
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "lv-held"), []byte("x y.md\n.obsidian/app.json\n"), 0o600))
	held, err = repo.Held()
	require.NoError(t, err)
	require.Equal(t, []string{".obsidian/app.json", "x y.md"}, held)
	require.NoError(t, repo.SetHeld([]string{"My Note.md", ".obsidian/app.json", "never/in/the/index.bin"}))

	require.NoError(t, os.RemoveAll(filepath.Join(root, ".obsidian")))
	require.NoError(t, os.Remove(filepath.Join(root, "My Note.md")))
	dirty, err := repo.Dirty(DefaultMaxFileSize)
	require.NoError(t, err)
	require.False(t, dirty, "held paths missing from raw/ are not a change")

	write(t, root, "note.md", "edited")
	_, changed, err := repo.Commit(Message{Reason: "upload"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, tracked(t, root, ".obsidian/app.json"))
	require.True(t, tracked(t, root, "My Note.md"))
	body, err := repo.Blob(mustHead(t, repo), ".obsidian/app.json")
	require.NoError(t, err)
	require.JSONEq(t, `{"theme":"dark"}`, string(body))

	// The list is not in history and not in the working tree's status.
	require.False(t, tracked(t, root, ".git/lv-held"))

	// An ordinary file still deletes.
	require.NoError(t, os.Remove(filepath.Join(root, "note.md")))
	_, changed, err = repo.Commit(Message{Reason: "delete"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, tracked(t, root, "note.md"))
	require.True(t, tracked(t, root, "My Note.md"))

	// Without the list, the old behaviour: tracked and missing is a removal.
	require.NoError(t, repo.SetHeld(nil))
	_, changed, err = repo.Commit(Message{Reason: "sweep"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, tracked(t, root, "My Note.md"))
}

func mustHead(t *testing.T, r *Repo) string {
	t.Helper()
	head, err := r.Head()
	require.NoError(t, err)
	require.NotEmpty(t, head)
	return head
}

// remote is a stub GitHub with one repository the App is installed on, and a
// token for it.
type remote struct {
	gh    *githubapptest.Server
	token string
	url   string
}

const (
	stubInstallation = int64(7)
	stubRepo         = int64(456)
)

func newRemote(t *testing.T) *remote {
	t.Helper()
	gh := githubapptest.New(t)
	gh.Repos[stubRepo] = githubapp.Repository{ID: stubRepo, FullName: "octocat/notes"}
	gh.InstallationRepos[stubInstallation] = []int64{stubRepo}
	token, err := gh.Client(t).InstallationToken(context.Background(), stubInstallation, stubRepo)
	require.NoError(t, err)
	return &remote{gh: gh, token: token, url: gh.URL + "/octocat/notes.git"}
}

func newRepo(t *testing.T) *Repo {
	t.Helper()
	root := t.TempDir()
	write(t, root, "a.md", "one")
	repo, _, err := Init(root, DefaultMaxFileSize)
	require.NoError(t, err)
	return repo
}

// Sync spec D13: the remote is origin with no credentials in it, and
// RemoveRemote takes it and its refs away again. History stays.
func TestSetRemoteAndRemoveRemote(t *testing.T) {
	rm := newRemote(t)
	repo := newRepo(t)
	ctx := context.Background()

	_, err := repo.Push(ctx, "main", rm.token)
	require.ErrorIs(t, err, ErrNoRemote)
	require.NoError(t, repo.RemoveRemote(), "removing a remote that does not exist is fine")

	require.Error(t, repo.SetRemote("https://x-access-token:secret@github.com/octocat/notes.git"))
	require.NoError(t, repo.SetRemote(rm.url))
	require.NoError(t, repo.SetRemote(rm.url))
	url, err := repo.RemoteURL()
	require.NoError(t, err)
	require.Equal(t, rm.url, url)

	head := mustHead(t, repo)
	sha, err := repo.Push(ctx, "main", rm.token)
	require.NoError(t, err)
	require.Equal(t, head, sha)
	require.Equal(t, head, rm.gh.Git.Tip(stubRepo, "main"))

	// The token was used for the call and written nowhere.
	cfg, err := os.ReadFile(filepath.Join(repo.Root(), ".git", "config"))
	require.NoError(t, err)
	require.Contains(t, string(cfg), rm.url)
	require.NotContains(t, string(cfg), rm.token)
	require.NotContains(t, string(cfg), "x-access-token")

	// A remote-tracking ref, as a fetch would leave.
	require.NoError(t, repo.repo.Storer.SetReference(plumbing.NewHashReference("refs/remotes/origin/main", plumbing.NewHash(head))))
	require.NoError(t, repo.repo.Storer.SetReference(plumbing.NewHashReference("refs/remotes/origin/drafts", plumbing.NewHash(head))))
	refs, err := repo.RemoteRefs()
	require.NoError(t, err)
	require.Len(t, refs, 2)

	require.NoError(t, repo.RemoveRemote())
	url, err = repo.RemoteURL()
	require.NoError(t, err)
	require.Empty(t, url)
	refs, err = repo.RemoteRefs()
	require.NoError(t, err)
	require.Empty(t, refs)
	cfg, err = os.ReadFile(filepath.Join(repo.Root(), ".git", "config"))
	require.NoError(t, err)
	require.NotContains(t, string(cfg), "octocat")
	raw, err := git.PlainOpen(repo.Root())
	require.NoError(t, err)
	_, err = raw.Remote(RemoteName)
	require.ErrorIs(t, err, git.ErrRemoteNotFound)
	require.Equal(t, head, mustHead(t, repo), "history stays")
	require.Equal(t, head, rm.gh.Git.Tip(stubRepo, "main"), "GitHub is not touched")
	_, err = repo.Push(ctx, "main", rm.token)
	require.ErrorIs(t, err, ErrNoRemote)
}

func commit(t *testing.T, repo *Repo, path, body string) string {
	t.Helper()
	write(t, repo.Root(), path, body)
	sha, changed, err := repo.Commit(Message{Reason: "upload"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)
	require.True(t, changed)
	return sha
}

// Sync spec D6: the push goes to the connected branch, creates it when it is
// new, is a no-op when GitHub already has the commit, and is never forced.
func TestPush(t *testing.T) {
	rm := newRemote(t)
	repo := newRepo(t)
	ctx := context.Background()
	require.NoError(t, repo.SetRemote(rm.url))

	_, err := repo.Push(ctx, "not a branch", rm.token)
	require.Error(t, err)

	first, err := repo.Push(ctx, "notes/2026", rm.token)
	require.NoError(t, err)
	require.Equal(t, first, rm.gh.Git.Tip(stubRepo, "notes/2026"))
	require.Empty(t, rm.gh.Git.Tip(stubRepo, "main"), "only the connected branch is written")

	again, err := repo.Push(ctx, "notes/2026", rm.token)
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.Len(t, rm.gh.GitPushes(), 1, "already up to date sends nothing")

	second := commit(t, repo, "b.md", "two")
	got, err := repo.Push(ctx, "notes/2026", rm.token)
	require.NoError(t, err)
	require.Equal(t, second, got)
	require.Equal(t, second, rm.gh.Git.Tip(stubRepo, "notes/2026"))

	// The remote moved: someone else committed there.
	theirs := "theirs"
	moved := rm.gh.Git.Commit(stubRepo, "notes/2026", "edit on github", map[string]*string{"c.md": &theirs})
	commit(t, repo, "d.md", "three")
	_, err = repo.Push(ctx, "notes/2026", rm.token)
	require.ErrorIs(t, err, ErrNonFastForward)
	require.Equal(t, moved, rm.gh.Git.Tip(stubRepo, "notes/2026"), "nothing was sent")
	require.Zero(t, rm.gh.GitForced())

	// An unrelated history on the remote (a first connect to a branch that
	// has commits) is the same case, not an overwrite.
	other := newRepo(t)
	require.NoError(t, other.SetRemote(rm.url))
	_, err = other.Push(ctx, "notes/2026", rm.token)
	require.ErrorIs(t, err, ErrNonFastForward)
	require.Equal(t, moved, rm.gh.Git.Tip(stubRepo, "notes/2026"))
	require.Zero(t, rm.gh.GitForced())
}

// go-git has no sentinel for a non-fast-forward push; pushError matches its
// text. This pins the text against the vendored version.
func TestNonFastForwardTextIsWhatGoGitSays(t *testing.T) {
	rm := newRemote(t)
	repo := newRepo(t)
	require.NoError(t, repo.SetRemote(rm.url))
	_, err := repo.Push(context.Background(), "main", rm.token)
	require.NoError(t, err)
	theirs := "theirs"
	rm.gh.Git.Commit(stubRepo, "main", "edit on github", map[string]*string{"c.md": &theirs})
	commit(t, repo, "d.md", "three")

	origin, err := repo.repo.Remote(RemoteName)
	require.NoError(t, err)
	err = origin.PushContext(context.Background(), &git.PushOptions{
		RemoteName: RemoteName,
		RefSpecs:   []config.RefSpec{"refs/heads/main:refs/heads/main"},
		Auth:       basicAuth(rm.token),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), nonFastForward)
	require.ErrorIs(t, pushError(err), ErrNonFastForward)
}

// Sync spec D6: what the remote answers decides what the worker does next.
func TestPushErrorsAreSorted(t *testing.T) {
	rm := newRemote(t)
	repo := newRepo(t)
	ctx := context.Background()
	require.NoError(t, repo.SetRemote(rm.url))
	set := func(change func(g *githubapptest.GitRemote)) {
		rm.gh.Lock()
		defer rm.gh.Unlock()
		rm.gh.Git.Reject, rm.gh.Git.Status, rm.gh.Git.BeforeReceive = map[int64]string{}, map[int64]int{}, nil
		change(rm.gh.Git)
	}

	// A token the remote does not know.
	_, err := repo.Push(ctx, "main", "ghs_notminted")
	require.ErrorIs(t, err, ErrRemoteUnauthorized)

	set(func(g *githubapptest.GitRemote) { g.Status[stubRepo] = 403 })
	_, err = repo.Push(ctx, "main", rm.token)
	require.ErrorIs(t, err, ErrRemoteForbidden)

	set(func(g *githubapptest.GitRemote) { g.Status[stubRepo] = 503 })
	_, err = repo.Push(ctx, "main", rm.token)
	require.Error(t, err)
	for _, sorted := range []error{ErrNonFastForward, ErrPushRejected, ErrRemoteNotFound, ErrRemoteUnauthorized, ErrRemoteForbidden} {
		require.NotErrorIs(t, err, sorted, "a 5xx is transient")
	}

	// Branch protection: the pack is taken, the update refused.
	set(func(g *githubapptest.GitRemote) { g.Reject[stubRepo] = "protected branch hook declined" })
	_, err = repo.Push(ctx, "main", rm.token)
	require.ErrorIs(t, err, ErrPushRejected)
	require.Empty(t, rm.gh.Git.Tip(stubRepo, "main"))

	// The branch moves between the advertisement and the update: the remote
	// refuses, and that is a moved remote, not a rejection.
	set(func(*githubapptest.GitRemote) {})
	_, err = repo.Push(ctx, "main", rm.token)
	require.NoError(t, err)
	commit(t, repo, "b.md", "two")
	theirs := "theirs"
	set(func(g *githubapptest.GitRemote) {
		g.BeforeReceive = func(int64) {
			rm.gh.Git.Commit(stubRepo, "main", "raced", map[string]*string{"c.md": &theirs})
		}
	})
	_, err = repo.Push(ctx, "main", rm.token)
	require.ErrorIs(t, err, ErrNonFastForward)
	require.Zero(t, rm.gh.GitForced())

	// A repository GitHub no longer knows by that name: a 404, or the 301 a
	// rename answers, which is not followed.
	set(func(*githubapptest.GitRemote) {})
	require.NoError(t, repo.SetRemote(rm.gh.URL+"/octocat/gone.git"))
	_, err = repo.Push(ctx, "main", rm.token)
	require.ErrorIs(t, err, ErrRemoteNotFound)

	rm.gh.Lock()
	rm.gh.Repos[stubRepo] = githubapp.Repository{ID: stubRepo, FullName: "octocat/renamed"}
	rm.gh.Renamed["octocat/notes"] = stubRepo
	rm.gh.Unlock()
	require.NoError(t, repo.SetRemote(rm.url))
	before := rm.gh.Git.Tip(stubRepo, "main")
	_, err = repo.Push(ctx, "main", rm.token)
	require.ErrorIs(t, err, ErrRemoteNotFound)
	require.Equal(t, before, rm.gh.Git.Tip(stubRepo, "main"), "the redirect was not followed")
}

// Sync spec D12: loop prevention asks whether a commit is HEAD or behind it.
func TestContains(t *testing.T) {
	repo := newRepo(t)
	first := mustHead(t, repo)
	second := commit(t, repo, "b.md", "two")
	for sha, want := range map[string]bool{
		first: true, second: true, "": false, "not-a-sha": false,
		"0123456789012345678901234567890123456789": false,
	} {
		got, err := repo.Contains(sha)
		require.NoError(t, err)
		require.Equal(t, want, got, fmt.Sprintf("%q", sha))
	}
	// A commit that exists but is not behind HEAD.
	other := newRepo(t)
	foreign := commit(t, other, "z.md", "elsewhere")
	got, err := repo.Contains(foreign)
	require.NoError(t, err)
	require.False(t, got)
}

// A commit that finds the tenant locked by another replica (a push to GitHub
// holds the lock for up to a minute) is tried again, not left to the sweep.
func TestCommitRetriesWhenAnotherReplicaHoldsTheTenant(t *testing.T) {
	root := t.TempDir()
	tenant := uuid.New()
	shared := &fakeLocker{}
	c := NewCommitter(Options{
		RawRoot: func(uuid.UUID) string { return root }, Lock: shared,
		Debounce: 30 * time.Millisecond, MaxWait: 100 * time.Millisecond,
	})
	defer c.Close(context.Background())
	write(t, root, "a.md", "one")
	_, err := c.Flush(context.Background(), tenant, Message{Reason: "upload"})
	require.NoError(t, err)

	unlock, ok, err := shared.TryLock(context.Background(), tenant) // the other replica
	require.NoError(t, err)
	require.True(t, ok)
	write(t, root, "a.md", "two")
	c.Touch(tenant, "upload", "U", "web")
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, 1, commitCount(t, root), "locked: no commit yet")
	unlock()
	require.Eventually(t, func() bool { return commitCount(t, root) == 2 }, 5*time.Second, 20*time.Millisecond)
	repo, err := Open(root)
	require.NoError(t, err)
	log, err := repo.Log("a.md", 1, "")
	require.NoError(t, err)
	require.Equal(t, "upload", log[0].Reason)
	require.Equal(t, "U", log[0].ActorID)
}
