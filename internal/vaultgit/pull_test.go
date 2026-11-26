package vaultgit

import (
	"context"
	"errors"
	"lumina/internal/githubapp/githubapptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func file(body string) *githubapptest.Entry { return &githubapptest.Entry{Data: []byte(body)} }

// snapshot is every file under root outside .git, with its bytes.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	}))
	return out
}

const submoduleCommit = "1234567890abcdef1234567890abcdef12345678"

// Sync spec D10: a fetch brings one branch and nothing else; LsRemote asks
// without fetching; an empty repository and a missing branch are "".
func TestFetchAndLsRemote(t *testing.T) {
	rm := newRemote(t)
	repo := newRepo(t)
	ctx := context.Background()
	_, err := repo.Fetch(ctx, "main", rm.token)
	require.ErrorIs(t, err, ErrNoRemote)
	require.NoError(t, repo.SetRemote(rm.url))

	tip, err := repo.Fetch(ctx, "main", rm.token)
	require.NoError(t, err)
	require.Empty(t, tip, "an empty repository")
	tip, err = LsRemote(ctx, rm.url, "main", rm.token)
	require.NoError(t, err)
	require.Empty(t, tip)

	theirs := rm.gh.Git.CommitEntries(stubRepo, "main", "theirs", map[string]*githubapptest.Entry{"b.md": file("two")})
	rm.gh.Git.CommitEntries(stubRepo, "other", "another branch", map[string]*githubapptest.Entry{"c.md": file("three")})
	tip, err = LsRemote(ctx, rm.url, "main", rm.token)
	require.NoError(t, err)
	require.Equal(t, theirs, tip)
	tip, err = LsRemote(ctx, rm.url, "none", rm.token)
	require.NoError(t, err)
	require.Empty(t, tip)

	before := snapshot(t, repo.Root())
	tip, err = repo.Fetch(ctx, "main", rm.token)
	require.NoError(t, err)
	require.Equal(t, theirs, tip)
	refs, err := repo.RemoteRefs()
	require.NoError(t, err)
	require.Equal(t, []string{"refs/remotes/origin/main"}, refs, "only the connected branch")
	require.Equal(t, before, snapshot(t, repo.Root()), "a fetch writes no file")
	tip, err = repo.Fetch(ctx, "main", rm.token)
	require.NoError(t, err)
	require.Equal(t, theirs, tip, "already up to date")
	tip, err = repo.Fetch(ctx, "none", rm.token)
	require.NoError(t, err)
	require.Empty(t, tip)

	body, err := repo.Blob(theirs, "b.md")
	require.NoError(t, err)
	require.Equal(t, "two", string(body))
	base, err := repo.MergeBase(mustHead(t, repo), theirs)
	require.NoError(t, err)
	require.Empty(t, base, "unrelated histories")
	ok, err := repo.IsAncestor(theirs, mustHead(t, repo))
	require.NoError(t, err)
	require.False(t, ok)

	// Errors are sorted like a push's: a bad token, and a renamed repository,
	// whose redirect is not followed.
	_, err = repo.Fetch(ctx, "main", "not-a-token")
	require.ErrorIs(t, err, ErrRemoteUnauthorized)
	_, err = LsRemote(ctx, rm.url, "main", "not-a-token")
	require.ErrorIs(t, err, ErrRemoteUnauthorized)
	rm.gh.Lock()
	r := rm.gh.Repos[stubRepo]
	r.FullName = "octocat/renamed"
	rm.gh.Repos[stubRepo] = r
	rm.gh.Renamed["octocat/notes"] = stubRepo
	rm.gh.Unlock()
	_, err = repo.Fetch(ctx, "main", rm.token)
	require.ErrorIs(t, err, ErrRemoteNotFound)
	_, err = LsRemote(ctx, rm.url, "main", rm.token)
	require.ErrorIs(t, err, ErrRemoteNotFound)
	require.Zero(t, rm.gh.Followed)
}

// The E4.8 review: tracking refs describe the repository origin pointed at.
// When origin is pointed somewhere else they go, so a reconnect that raced a
// disconnect's clean-up cannot read another repository's branch as its own.
func TestSetRemoteDropsAnotherRepositorysRefs(t *testing.T) {
	rm := newRemote(t)
	repo := newRepo(t)
	require.NoError(t, repo.SetRemote(rm.url))
	rm.gh.Git.CommitEntries(stubRepo, "main", "theirs", map[string]*githubapptest.Entry{"b.md": file("two")})
	_, err := repo.Fetch(context.Background(), "main", rm.token)
	require.NoError(t, err)
	refs, err := repo.RemoteRefs()
	require.NoError(t, err)
	require.Len(t, refs, 1)

	require.NoError(t, repo.SetRemote(rm.url))
	refs, err = repo.RemoteRefs()
	require.NoError(t, err)
	require.Len(t, refs, 1, "the same URL keeps them")

	require.NoError(t, repo.SetRemote(rm.gh.URL+"/octocat/another.git"))
	refs, err = repo.RemoteRefs()
	require.NoError(t, err)
	require.Empty(t, refs)
}

// Sync spec D10 step 9, D8: moving the ref never touches a file, and what
// the vault cannot hold (a symlink, a submodule link, a name with a space)
// stays in the index and in every later commit.
func TestFastForwardAndMergeCommitTouchNoFile(t *testing.T) {
	rm := newRemote(t)
	repo := newRepo(t)
	root := repo.Root()
	ctx := context.Background()
	require.NoError(t, repo.SetRemote(rm.url))
	_, err := repo.Push(ctx, "main", rm.token)
	require.NoError(t, err)
	local := mustHead(t, repo)

	held := map[string]*githubapptest.Entry{
		"My Note.md":         file("spaced"),
		".obsidian/app.json": file("{}"),
		"link.md":            {Mode: "120000", Data: []byte("a.md")},
		"vendor/lib":         {Mode: "160000", Data: []byte(submoduleCommit)},
		"new\nline.md":       file("odd"),
	}
	theirs := rm.gh.Git.CommitEntries(stubRepo, "main", "theirs", held)
	tip, err := repo.Fetch(ctx, "main", rm.token)
	require.NoError(t, err)
	require.Equal(t, theirs, tip)

	tree, err := repo.Tree(theirs)
	require.NoError(t, err)
	require.Equal(t, "120000", tree["link.md"].Mode)
	require.Equal(t, "160000", tree["vendor/lib"].Mode)
	require.Equal(t, submoduleCommit, tree["vendor/lib"].ID)
	require.Equal(t, "100644", tree["My Note.md"].Mode)
	require.EqualValues(t, 6, tree["My Note.md"].Size)
	empty, err := repo.Tree("")
	require.NoError(t, err)
	require.Empty(t, empty)

	var paths []string
	for p := range held {
		paths = append(paths, p)
	}
	require.NoError(t, repo.SetHeld(paths))
	before := snapshot(t, root)
	require.NoError(t, repo.FastForward(theirs))
	require.Equal(t, theirs, mustHead(t, repo), "HEAD is the remote commit itself")
	require.Equal(t, before, snapshot(t, root), "no file was touched")
	dirty, err := repo.Dirty(DefaultMaxFileSize)
	require.NoError(t, err)
	require.False(t, dirty, "nothing to commit: no new commit follows a fast-forward")
	require.Error(t, repo.FastForward(local), "backwards is not a fast-forward")

	// A local save, then a commit: every held entry is still there.
	write(t, root, "a.md", "edited")
	sha, changed, err := repo.Commit(Message{Reason: "upload"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)
	require.True(t, changed)
	_, err = repo.Push(ctx, "main", rm.token)
	require.NoError(t, err)
	got := rm.gh.Git.Entries(stubRepo, "main")
	for p, e := range held {
		mode := e.Mode
		if mode == "" {
			mode = "100644"
		}
		require.Equal(t, githubapptest.Entry{Mode: mode, Data: e.Data}, got[p], p)
	}
	require.Equal(t, "edited", string(got["a.md"].Data))

	// Diverge: the remote edits, the vault edits, and a file turns up at a
	// held path. The merge has both parents, keeps the remote's held entry
	// (the file at its path is not taken), and still touches no file.
	theirs2 := rm.gh.Git.CommitEntries(stubRepo, "main", "theirs again", map[string]*githubapptest.Entry{
		"My Note.md": file("spaced, edited"), "c.md": file("three"),
	})
	write(t, root, "d.md", "local")
	write(t, root, "My Note.md", "shadow")
	ours, changed, err := repo.Commit(Message{Reason: "upload"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)
	require.True(t, changed)
	require.NotEqual(t, sha, ours)
	_, err = repo.Fetch(ctx, "main", rm.token)
	require.NoError(t, err)
	base, err := repo.MergeBase(ours, theirs2)
	require.NoError(t, err)
	require.Equal(t, sha, base)
	write(t, root, "c.md", "three") // what the apply phase would have written
	before = snapshot(t, root)
	merge, err := repo.MergeCommit(theirs2, nil, Message{Reason: "pull", Summary: "pull: merge main from GitHub (0 conflicts)"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)
	require.Equal(t, before, snapshot(t, root))
	require.Equal(t, merge, mustHead(t, repo))
	_, err = repo.Push(ctx, "main", rm.token)
	require.NoError(t, err)
	require.Equal(t, []string{ours, theirs2}, rm.gh.Git.Parents(stubRepo, merge))
	got = rm.gh.Git.Entries(stubRepo, "main")
	require.Equal(t, "spaced, edited", string(got["My Note.md"].Data), "the remote's entry, not the file that appeared at its path")
	require.Equal(t, "local", string(got["d.md"].Data))
	require.Equal(t, "three", string(got["c.md"].Data))
	require.Equal(t, "120000", got["link.md"].Mode)
	require.Equal(t, "160000", got["vendor/lib"].Mode)
	require.Zero(t, rm.gh.GitForced())

	// carry: an entry only this side has, which the vault cannot hold, is
	// kept through a merge with a history that never had it.
	other := rm.gh.Git.CommitEntries(stubRepo, "main", "unrelated", map[string]*githubapptest.Entry{"z.md": file("z")}, "")
	_, err = repo.Fetch(ctx, "main", rm.token)
	require.NoError(t, err)
	require.NoError(t, repo.SetHeld(paths))
	_, err = repo.MergeCommit(other, []string{"link.md", "vendor/lib"}, Message{Reason: "pull", Summary: "pull"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)
	tree, err = repo.Tree(mustHead(t, repo))
	require.NoError(t, err)
	require.Equal(t, "120000", tree["link.md"].Mode)
	require.Equal(t, "160000", tree["vendor/lib"].Mode)
}

// Sync spec D10: a version made on GitHub is in a note's history with reason
// "github" and no actor, restorable byte for byte; a merge commit is listed
// only for a path whose bytes differ from both parents.
func TestLogThroughAMerge(t *testing.T) {
	rm := newRemote(t)
	repo := newRepo(t)
	root := repo.Root()
	ctx := context.Background()
	require.NoError(t, repo.SetRemote(rm.url))
	_, err := repo.Push(ctx, "main", rm.token)
	require.NoError(t, err)

	remoteBody := "edited on GitHub\r\n\x00 bytes kept\n"
	theirs := rm.gh.Git.CommitEntries(stubRepo, "main", "Update a.md", map[string]*githubapptest.Entry{"a.md": file(remoteBody)})
	write(t, root, "b.md", "local")
	_, _, err = repo.Commit(Message{Reason: "upload", Actor: "ACTOR"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)
	_, err = repo.Fetch(ctx, "main", rm.token)
	require.NoError(t, err)
	write(t, root, "a.md", remoteBody)
	merge, err := repo.MergeCommit(theirs, nil, Message{Reason: "pull", Summary: "pull: merge main from GitHub (0 conflicts)"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)

	versions, err := repo.Log("a.md", 0, "")
	require.NoError(t, err)
	require.Len(t, versions, 2, "the GitHub edit and the first import, not the merge")
	require.Equal(t, theirs, versions[0].SHA)
	require.Equal(t, ReasonGitHub, versions[0].Reason)
	require.Empty(t, versions[0].ActorID)
	require.Equal(t, "Update a.md", versions[0].Message)
	require.Equal(t, "init", versions[1].Reason)
	body, err := repo.Blob(versions[0].SHA, "a.md")
	require.NoError(t, err)
	require.Equal(t, remoteBody, string(body))

	versions, err = repo.Log("b.md", 0, "")
	require.NoError(t, err)
	require.Len(t, versions, 1)
	require.Equal(t, "upload", versions[0].Reason)
	require.Equal(t, "ACTOR", versions[0].ActorID)
	for _, v := range versions {
		require.NotEqual(t, merge, v.SHA)
	}
}

// Sync spec D8: the remote's .gitignore is taken as it is and gains the
// managed block when it lacks the patterns.
func TestTakeIgnore(t *testing.T) {
	repo := newRepo(t)
	read := func() string {
		b, err := os.ReadFile(filepath.Join(repo.Root(), ".gitignore"))
		require.NoError(t, err)
		return string(b)
	}
	changed, err := repo.TakeIgnore([]byte("node_modules/\n*.log\n"))
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, strings.HasPrefix(read(), "node_modules/\n*.log\n# LuminaVault (managed)\n"))
	require.True(t, strings.HasSuffix(read(), "# end\n"))
	changed, err = repo.TakeIgnore([]byte(read()))
	require.NoError(t, err)
	require.False(t, changed, "our own file coming back changes nothing")
	changed, err = repo.TakeIgnore([]byte(GitIgnore))
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, GitIgnore, read())
}

type dyingLocker struct{ dead bool }

func (l *dyingLocker) TryLock(context.Context, uuid.UUID) (func(), bool, error) {
	return func() {}, true, nil
}

func (l *dyingLocker) TryLockChecked(context.Context, uuid.UUID) (func(), func(context.Context) error, bool, error) {
	return func() {}, func(context.Context) error {
		if l.dead {
			return errors.New("connection reset")
		}
		return nil
	}, true, nil
}

// The E4.8 review: a session advisory lock dies with its connection, without
// a word. A long operation asks before it writes.
func TestStillLocked(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "one")
	_, _, err := Init(root, DefaultMaxFileSize)
	require.NoError(t, err)
	lock := &dyingLocker{}
	c := NewCommitter(Options{RawRoot: func(uuid.UUID) string { return root }, Lock: lock})
	defer c.Close(context.Background())
	ctx := context.Background()
	require.NoError(t, c.WithRepo(ctx, uuid.New(), func(r *Repo) error {
		require.NoError(t, r.StillLocked(ctx))
		lock.dead = true
		require.ErrorIs(t, r.StillLocked(ctx), ErrLockLost)
		return nil
	}))
	plain, err := Open(root)
	require.NoError(t, err)
	require.NoError(t, plain.StillLocked(ctx), "outside WithRepo there is nothing to ask")
}
