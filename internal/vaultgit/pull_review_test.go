package vaultgit

import (
	"context"
	"crypto/rand"
	"lumina/internal/githubapp/githubapptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// Owner decision 2026-11-26 (sync spec D8): only the managed patterns decide
// what a commit leaves out. A user's own lines stay in the file byte for
// byte, hide no vault file, and cannot bring the trash into history, wherever
// in the file they stand.
func TestOnlyTheManagedIgnoreDecides(t *testing.T) {
	root := t.TempDir()
	repo, _, err := Init(root, DefaultMaxFileSize)
	require.NoError(t, err)
	// The user's lines before the block, and more of them after it: the
	// last match wins in git, so the ones after would undo the block.
	ignore := "drafts/\nsecret.md\n*.md\n!_deleted_*\n" + "# LuminaVault (managed)\n" + GitIgnore + "# end\n" + "!*.tmp\n!.DS_Store\n"
	write(t, root, ".gitignore", ignore)
	write(t, root, "sub/.gitignore", "*\n")
	for p, body := range map[string]string{
		"drafts/a.md": "a draft", "secret.md": "a secret", "plain.md": "a note", "sub/b.md": "in a folder that ignores all",
		"_deleted_1790000000_0123456789abcdef0123456789abcdef_gone.md":    "trash",
		"notes/_deleted_1790000000_0123456789abcdef0123456789abcdef_x.md": "trash in a folder",
		"upload.tmp": "half a write", ".DS_Store": "litter", "notes/.lv-state": "ours",
	} {
		write(t, root, p, body)
	}
	_, changed, err := repo.Commit(Message{Reason: "upload"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)
	require.True(t, changed)
	for _, p := range []string{"drafts/a.md", "secret.md", "plain.md", "sub/b.md", ".gitignore"} {
		require.True(t, tracked(t, root, p), "%s is a vault file: a user's rule does not hide it", p)
	}
	for _, p := range []string{
		"_deleted_1790000000_0123456789abcdef0123456789abcdef_gone.md",
		"notes/_deleted_1790000000_0123456789abcdef0123456789abcdef_x.md",
		"upload.tmp", ".DS_Store", "notes/.lv-state",
	} {
		require.False(t, tracked(t, root, p), "%s is kept out by the managed patterns, whatever the user's lines say", p)
	}
	b, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	require.NoError(t, err)
	require.Equal(t, ignore, string(b), "the file is the user's: not a byte of it changed")
	committed, err := repo.Blob(mustHead(t, repo), ".gitignore")
	require.NoError(t, err)
	require.Equal(t, ignore, string(committed))

	// A later edit of a file a user's rule names is a change like any other,
	// and so is its deletion.
	write(t, root, "secret.md", "edited")
	require.NoError(t, os.Remove(filepath.Join(root, "drafts", "a.md")))
	dirty, err := repo.Dirty(DefaultMaxFileSize)
	require.NoError(t, err)
	require.True(t, dirty)
	_, _, err = repo.Commit(Message{Reason: "upload"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)
	body, err := repo.Blob(mustHead(t, repo), "secret.md")
	require.NoError(t, err)
	require.Equal(t, "edited", string(body))
	require.False(t, tracked(t, root, "drafts/a.md"))
	dirty, err = repo.Dirty(DefaultMaxFileSize)
	require.NoError(t, err)
	require.False(t, dirty, "the trash and the litter are still nothing to commit")
}

// E4.9 review item 6: the held list is one path per line, the format a
// release from before the NUL separator reads, unless a path has a newline.
func TestHeldListStaysReadableByAnOlderRelease(t *testing.T) {
	repo := newRepo(t)
	file := filepath.Join(repo.Root(), ".git", "lv-held")
	// What the release before read: every line that is not empty.
	older := func() []string {
		b, err := os.ReadFile(file)
		require.NoError(t, err)
		var out []string
		for line := range strings.SplitSeq(string(b), "\n") {
			if line != "" {
				out = append(out, line)
			}
		}
		return out
	}
	require.NoError(t, repo.SetHeld([]string{"link.md", ".obsidian/app.json", "My Note.md"}))
	b, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, ".obsidian/app.json\nMy Note.md\nlink.md\n", string(b))
	require.Equal(t, []string{".obsidian/app.json", "My Note.md", "link.md"}, older())
	held, err := repo.Held()
	require.NoError(t, err)
	require.Equal(t, older(), held)

	// Only a path that has a newline needs the other separator.
	require.NoError(t, repo.SetHeld([]string{"new\nline.md", "link.md"}))
	b, err = os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, "link.md\x00new\nline.md\x00", string(b))
	held, err = repo.Held()
	require.NoError(t, err)
	require.Equal(t, []string{"link.md", "new\nline.md"}, held)
	// And the list goes back to lines when that path is gone.
	require.NoError(t, repo.SetHeld([]string{"link.md"}))
	require.Equal(t, []string{"link.md"}, older())
}

// heldLink leaves the repository as a pull does when the remote has a link
// named dir and a dot-file inside what would be the note b.md: both are in
// the index, on the held list, and not in raw/.
func heldLink(t *testing.T) (*Repo, *remote) {
	t.Helper()
	rm := newRemote(t)
	repo := newRepo(t)
	ctx := context.Background()
	require.NoError(t, repo.SetRemote(rm.url))
	_, err := repo.Push(ctx, "main", rm.token)
	require.NoError(t, err)
	theirs := rm.gh.Git.CommitEntries(stubRepo, "main", "theirs", map[string]*githubapptest.Entry{
		"dir":         {Mode: "120000", Data: []byte("a.md")},
		"b.md/.cache": file("{}"),
	})
	_, err = repo.Fetch(ctx, "main", rm.token)
	require.NoError(t, err)
	require.NoError(t, repo.SetHeld([]string{"dir", "b.md/.cache"}))
	require.NoError(t, repo.FastForward(theirs))
	return repo, rm
}

// E4.9 review item 1: a file saved inside a held path, or at a folder of
// one, cannot be committed beside the held entry (git has no file and folder
// of one name). It is left out of every commit, the entry GitHub has stays,
// and later commits go on working.
func TestAFileInTheWayOfAHeldPathIsLeftOut(t *testing.T) {
	repo, rm := heldLink(t)
	root := repo.Root()
	ctx := context.Background()

	for path, want := range map[string]string{
		"dir/new.md": "dir", "dir/deep/er.md": "dir", "b.md": "b.md/.cache",
		"dir": "", "b.md/.cache": "", "b.md/other.md": "", "directory/x.md": "", "a.md": "", "b.mdx": "",
	} {
		got, err := HeldAt(root, path)
		require.NoError(t, err)
		require.Equal(t, want, got, path)
	}
	none, err := HeldAt(t.TempDir(), "dir/new.md")
	require.NoError(t, err)
	require.Empty(t, none, "no repository, no list: nothing is held")

	// Both ways round: a file inside the link's name, and a file where the
	// remote has a folder.
	write(t, root, "dir/new.md", "inside a held path")
	write(t, root, "b.md", "at a folder of a held path")
	write(t, root, "later.md", "an ordinary save")
	for i := range 3 {
		write(t, root, "later.md", strings.Repeat("again ", i+1))
		_, changed, err := repo.Commit(Message{Reason: "upload"}, DefaultMaxFileSize, time.Now())
		require.NoError(t, err, "commit %d", i)
		require.True(t, changed)
	}
	dirty, err := repo.Dirty(DefaultMaxFileSize)
	require.NoError(t, err)
	require.False(t, dirty)
	_, err = repo.Push(ctx, "main", rm.token)
	require.NoError(t, err)
	got := rm.gh.Git.Entries(stubRepo, "main")
	require.Equal(t, "120000", got["dir"].Mode, "the link GitHub has is still there")
	require.Contains(t, got, "b.md/.cache")
	require.NotContains(t, got, "dir/new.md")
	require.NotContains(t, got, "b.md")
	require.Contains(t, got, "later.md")
	// The files themselves are untouched: they stay in the vault.
	require.FileExists(t, filepath.Join(root, "dir", "new.md"))
	require.FileExists(t, filepath.Join(root, "b.md"))
}

// E4.9 review item 1: a repository that already has a file and a folder
// under one name (a release before the guard staged one, and may have
// committed it) does not stop the vault's commits for ever: it is mended,
// with the held entry kept.
func TestAFileAndAFolderOfOneNameIsMended(t *testing.T) {
	for _, committed := range []bool{false, true} {
		repo, rm := heldLink(t)
		root := repo.Root()
		ctx := context.Background()
		write(t, root, "dir/new.md", "inside a held path")

		// What the release before did: it staged the file beside the entry.
		idx, err := repo.repo.Storer.Index()
		require.NoError(t, err)
		blob := repo.repo.Storer.NewEncodedObject()
		blob.SetType(plumbing.BlobObject)
		w, err := blob.Writer()
		require.NoError(t, err)
		_, err = w.Write([]byte("inside a held path"))
		require.NoError(t, err)
		require.NoError(t, w.Close())
		hash, err := repo.repo.Storer.SetEncodedObject(blob)
		require.NoError(t, err)
		idx.Entries = append(idx.Entries, &index.Entry{Name: "dir/new.md", Hash: hash, Mode: filemode.Regular})
		require.NoError(t, repo.repo.Storer.SetIndex(idx))
		wt, err := repo.repo.Worktree()
		require.NoError(t, err)
		if committed {
			// And, when its commit went through, HEAD's tree has the two.
			sig := Author
			sig.When = time.Now()
			_, err = wt.Commit("upload: dir/new.md\n", &git.CommitOptions{Author: &sig, Committer: &sig})
			require.NoError(t, err)
		}
		if _, err = wt.Status(); committed {
			require.Error(t, err, "this is the state that stopped every commit")
		}

		for i := range 2 {
			write(t, root, "later.md", strings.Repeat("again ", i+1))
			_, changed, err := repo.Commit(Message{Reason: "upload"}, DefaultMaxFileSize, time.Now())
			require.NoError(t, err, "commit %d (committed: %v)", i, committed)
			require.True(t, changed)
		}
		tree, err := repo.Tree(mustHead(t, repo))
		require.NoError(t, err)
		require.Equal(t, "120000", tree["dir"].Mode)
		require.NotContains(t, tree, "dir/new.md")
		require.Contains(t, tree, "later.md")
		require.Contains(t, tree, "b.md/.cache")
		require.FileExists(t, filepath.Join(root, "dir", "new.md"))
		dirty, err := repo.Dirty(DefaultMaxFileSize)
		require.NoError(t, err)
		require.False(t, dirty)
		_, err = repo.Push(ctx, "main", rm.token)
		require.NoError(t, err)
		got := rm.gh.Git.Entries(stubRepo, "main")
		require.Equal(t, "120000", got["dir"].Mode)
		require.NotContains(t, got, "dir/new.md")
	}
}

// E4.9 review item 7: a fetch that reads more than its limit ends there,
// and a walk of a tree ends when its caller has seen enough.
func TestFetchAndTreeWalkAreBounded(t *testing.T) {
	rm := newRemote(t)
	repo := newRepo(t)
	ctx := context.Background()
	require.NoError(t, repo.SetRemote(rm.url))
	_, err := repo.Push(ctx, "main", rm.token)
	require.NoError(t, err)
	noise := make([]byte, 512<<10) // does not compress
	_, err = rand.Read(noise)
	require.NoError(t, err)
	theirs := rm.gh.Git.CommitEntries(stubRepo, "main", "big", map[string]*githubapptest.Entry{
		"big.bin": {Data: noise}, "one.md": file("1"), "two.md": file("2"), "three/four.md": file("4"),
	})

	_, err = repo.FetchWith(ctx, "main", rm.token, FetchOptions{MaxBytes: 64 << 10})
	require.ErrorIs(t, err, ErrPackTooLarge)
	has, err := repo.Contains(theirs)
	require.NoError(t, err)
	require.False(t, has, "nothing of a refused fetch is kept")
	tip, err := repo.FetchWith(ctx, "main", rm.token, FetchOptions{MaxBytes: 4 << 20})
	require.NoError(t, err)
	require.Equal(t, theirs, tip, "a limit the fetch fits changes nothing")

	seen := 0
	stop := errorString("enough")
	err = repo.TreeEach(theirs, func(TreeEntry) error {
		if seen++; seen == 2 {
			return stop
		}
		return nil
	})
	require.ErrorIs(t, err, stop)
	require.Equal(t, 2, seen, "the walk stopped where it was told to")
	all, err := repo.Tree(theirs)
	require.NoError(t, err)
	require.Len(t, all, 6)
}

type errorString string

func (e errorString) Error() string { return string(e) }

// forge puts on the stub's branch a commit that claims to be the vault's
// own: its committer e-mail and an LV-Actor trailer. Anyone who can push to
// the repository can make one.
func forge(t *testing.T, rm *remote, path, body, actor string) string {
	t.Helper()
	honest := rm.gh.Git.CommitEntries(stubRepo, "main", "Edit", map[string]*githubapptest.Entry{path: file(body)})
	bare := rm.gh.Git.Repo(stubRepo)
	c, err := bare.CommitObject(plumbing.NewHash(honest))
	require.NoError(t, err)
	sig := Author
	sig.When = c.Committer.When
	forged := &object.Commit{
		Author: sig, Committer: sig, TreeHash: c.TreeHash, ParentHashes: c.ParentHashes,
		Message: Message{Reason: "upload", Actor: actor}.render([]string{path}),
	}
	obj := bare.Storer.NewEncodedObject()
	require.NoError(t, forged.Encode(obj))
	hash, err := bare.Storer.SetEncodedObject(obj)
	require.NoError(t, err)
	require.NoError(t, bare.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), hash)))
	return hash.String()
}

// E4.9 review item 13: who made a change is believed only for commits this
// server made. A commit pulled from GitHub that uses the vault's committer
// e-mail and names an actor is shown as a GitHub change with no actor,
// whether it came in by a fast-forward or by a merge.
func TestActorIsBelievedOnlyOnTheServersOwnCommits(t *testing.T) {
	rm := newRemote(t)
	repo := newRepo(t)
	root := repo.Root()
	ctx := context.Background()
	require.NoError(t, repo.SetRemote(rm.url))
	write(t, root, "a.md", "mine")
	_, _, err := repo.Commit(Message{Reason: "upload", Actor: "ALICE"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)
	_, err = repo.Push(ctx, "main", rm.token)
	require.NoError(t, err)

	// By fast-forward.
	first := forge(t, rm, "a.md", "forged once", "MALLORY")
	_, err = repo.Fetch(ctx, "main", rm.token)
	require.NoError(t, err)
	require.NoError(t, repo.MarkForeign(first, mustHead(t, repo)))
	require.NoError(t, repo.MarkForeign(first, mustHead(t, repo)), "recording twice records once")
	write(t, root, "a.md", "forged once")
	require.NoError(t, repo.FastForward(first))

	// By merge, with a local commit of the server's own beside it.
	second := forge(t, rm, "a.md", "forged twice", "MALLORY")
	write(t, root, "b.md", "local")
	own, _, err := repo.Commit(Message{Reason: "upload", Actor: "BOB"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)
	_, err = repo.Fetch(ctx, "main", rm.token)
	require.NoError(t, err)
	require.NoError(t, repo.MarkForeign(second, own))
	write(t, root, "a.md", "forged twice")
	_, err = repo.MergeCommit(second, nil, Message{Reason: "pull", Summary: "pull"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)

	versions, err := repo.Log("a.md", 0, "")
	require.NoError(t, err)
	byID := map[string]Version{}
	for _, v := range versions {
		byID[v.SHA] = v
	}
	for _, sha := range []string{first, second} {
		require.Contains(t, byID, sha)
		require.Equal(t, ReasonGitHub, byID[sha].Reason, "a pulled commit is GitHub's, whatever e-mail it carries")
		require.Empty(t, byID[sha].ActorID, "and names nobody")
	}
	mine := 0
	for _, v := range versions {
		if v.ActorID == "ALICE" {
			mine++
			require.Equal(t, "upload", v.Reason)
		}
		require.NotEqual(t, "MALLORY", v.ActorID)
	}
	require.Equal(t, 1, mine, "the server's own commit, pushed and fetched back, keeps its actor")
	versions, err = repo.Log("b.md", 0, "")
	require.NoError(t, err)
	require.Len(t, versions, 1)
	require.Equal(t, "BOB", versions[0].ActorID)

	b, err := os.ReadFile(filepath.Join(root, ".git", "lv-foreign"))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{first, second}, strings.Fields(string(b)), "each pulled commit once, and none of the server's")
}

// E4.9 review item 14: a history with a merge in it is paged with `before`
// without losing or repeating a version, whichever commit a page ends on.
func TestLogPagesAcrossAMerge(t *testing.T) {
	rm := newRemote(t)
	repo := newRepo(t)
	root := repo.Root()
	ctx := context.Background()
	require.NoError(t, repo.SetRemote(rm.url))
	_, err := repo.Push(ctx, "main", rm.token)
	require.NoError(t, err)
	at := time.Now().Add(-time.Hour)
	local := func(body string) {
		at = at.Add(time.Minute)
		write(t, root, "a.md", body)
		_, _, err := repo.Commit(Message{Reason: "upload", Actor: "ALICE"}, DefaultMaxFileSize, at)
		require.NoError(t, err)
	}
	// Three edits on GitHub and three in the vault, from one base; then the
	// merge, which keeps a version neither side had (so it is a version
	// too); then two more edits.
	var theirs string
	for _, body := range []string{"r1", "r2", "r3"} {
		theirs = rm.gh.Git.CommitEntries(stubRepo, "main", "Edit "+body, map[string]*githubapptest.Entry{"a.md": file(body)})
	}
	for _, body := range []string{"l1", "l2", "l3"} {
		local(body)
	}
	_, err = repo.Fetch(ctx, "main", rm.token)
	require.NoError(t, err)
	write(t, root, "a.md", "merged by hand")
	at = at.Add(time.Minute)
	merge, err := repo.MergeCommit(theirs, nil, Message{Reason: "pull", Summary: "pull: merge"}, DefaultMaxFileSize, at)
	require.NoError(t, err)
	local("l4")
	local("l5")

	full, err := repo.Log("a.md", 100, "")
	require.NoError(t, err)
	require.Len(t, full, 10, "the first import, three and three, the merge, two more")
	ids := func(vs []Version) []string {
		out := make([]string, 0, len(vs))
		for _, v := range vs {
			out = append(out, v.SHA)
		}
		return out
	}
	require.Contains(t, ids(full), merge)
	require.Contains(t, ids(full), theirs)
	seen := map[string]bool{}
	for _, id := range ids(full) {
		require.False(t, seen[id], "a version is listed once")
		seen[id] = true
	}
	for _, size := range []int{1, 2, 3, 4, 7} {
		var paged []string
		before := ""
		for {
			page, err := repo.Log("a.md", size, before)
			require.NoError(t, err)
			require.LessOrEqual(t, len(page), size)
			if len(page) == 0 {
				break
			}
			paged = append(paged, ids(page)...)
			before = page[len(page)-1].SHA
			require.LessOrEqual(t, len(paged), len(full), "pages of %d never end", size)
		}
		require.Equal(t, ids(full), paged, "pages of %d", size)
	}
	// Every version is a place a page can start after, the merge and the
	// commits of either side of it included.
	for i, v := range full {
		rest, err := repo.Log("a.md", 100, v.SHA)
		require.NoError(t, err)
		require.Equal(t, ids(full[i+1:]), ids(rest), "after version %d", i)
	}
	// A cursor that is no version of the path lists nothing, as before.
	none, err := repo.Log("a.md", 100, strings.Repeat("0", 40))
	require.NoError(t, err)
	require.Empty(t, none)
}
