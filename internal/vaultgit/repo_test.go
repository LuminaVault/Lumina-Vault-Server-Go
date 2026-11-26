package vaultgit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func write(t *testing.T, root, p, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(p))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(body), 0o600))
}

var t0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func TestInitCommitsExistingFilesOnMain(t *testing.T) {
	root := t.TempDir()
	write(t, root, "notes/a.md", "a")
	write(t, root, "_deleted_1_x.md", "trash")
	write(t, root, ".DS_Store", "junk")

	r, created, err := Init(root, DefaultMaxFileSize)
	require.NoError(t, err)
	require.True(t, created)

	head, err := r.repo.Head()
	require.NoError(t, err)
	require.Equal(t, "refs/heads/main", head.Name().String())

	b, err := r.Blob(head.Hash().String(), "notes/a.md")
	require.NoError(t, err)
	require.Equal(t, "a", string(b))
	_, err = r.Blob(head.Hash().String(), "_deleted_1_x.md")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = r.Blob(head.Hash().String(), ".DS_Store")
	require.ErrorIs(t, err, ErrNotFound)

	headSHA, err := r.Head()
	require.NoError(t, err)
	require.Equal(t, head.Hash().String(), headSHA)

	again, created, err := Init(root, DefaultMaxFileSize)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, root, again.Root())
}

// Sync spec D8: the managed patterns are added to a .gitignore that lacks
// them, in a marked block, and every line that was there stays. A file from
// before sync, which is the patterns and nothing else, is left byte for byte,
// so no existing vault gets a commit out of this.
func TestInitKeepsAUsersIgnoreLines(t *testing.T) {
	root := t.TempDir()
	_, _, err := Init(root, DefaultMaxFileSize)
	require.NoError(t, err)
	read := func() string {
		b, err := os.ReadFile(filepath.Join(root, ".gitignore"))
		require.NoError(t, err)
		return string(b)
	}
	require.Equal(t, GitIgnore, read(), "a new vault")
	_, _, err = Init(root, DefaultMaxFileSize)
	require.NoError(t, err)
	require.Equal(t, GitIgnore, read(), "left alone")

	write(t, root, ".gitignore", "nothing\n*.log")
	_, _, err = Init(root, DefaultMaxFileSize)
	require.NoError(t, err)
	want := "nothing\n*.log\n# LuminaVault (managed)\n" + GitIgnore + "# end\n"
	require.Equal(t, want, read())
	_, _, err = Init(root, DefaultMaxFileSize)
	require.NoError(t, err)
	require.Equal(t, want, read(), "the block is added once")

	require.Equal(t, GitIgnore, WithManagedIgnore(""))
	require.Equal(t, GitIgnore, WithManagedIgnore(" \n"))
	mine := "*.log\r\n.lv-*\r\n*.tmp\r\n.DS_Store\r\n_deleted_*\r\n"
	require.Equal(t, mine, WithManagedIgnore(mine), "all four are there, in any order and with any line ending")
}

func TestOpenWithoutRepo(t *testing.T) {
	_, err := Open(t.TempDir())
	require.ErrorIs(t, err, ErrNoRepo)
}

func TestCommitRecordsChangesAndNothingWhenClean(t *testing.T) {
	root := t.TempDir()
	r, _, err := Init(root, DefaultMaxFileSize)
	require.NoError(t, err)

	_, changed, err := r.Commit(Message{Reason: "sweep"}, DefaultMaxFileSize, t0)
	require.NoError(t, err)
	require.False(t, changed, "clean tree commits nothing")

	write(t, root, "a.md", "one")
	write(t, root, "b.md", "two")
	sha, changed, err := r.Commit(Message{Reason: "upload", Actor: "U1", Source: "web"}, DefaultMaxFileSize, t0)
	require.NoError(t, err)
	require.True(t, changed)

	c, err := r.repo.CommitObject(mustHash(t, sha))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(c.Message, "upload: a.md (+1 more)\n"))
	require.Contains(t, c.Message, "LV-Actor: U1")
	require.Contains(t, c.Message, "LV-Source: web")
	require.Equal(t, "LuminaVault", c.Author.Name)

	dirty, err := r.Dirty(DefaultMaxFileSize)
	require.NoError(t, err)
	require.False(t, dirty)
}

func TestDeleteAndRestoreHistory(t *testing.T) {
	root := t.TempDir()
	r, _, err := Init(root, DefaultMaxFileSize)
	require.NoError(t, err)

	write(t, root, "n.md", "v1")
	v1, _, err := r.Commit(Message{Reason: "upload", Actor: "A"}, DefaultMaxFileSize, t0)
	require.NoError(t, err)
	write(t, root, "n.md", "v2 longer")
	v2, _, err := r.Commit(Message{Reason: "upload", Actor: "B"}, DefaultMaxFileSize, t0.Add(time.Minute))
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, "n.md")))
	v3, changed, err := r.Commit(Message{Reason: "delete"}, DefaultMaxFileSize, t0.Add(2*time.Minute))
	require.NoError(t, err)
	require.True(t, changed)

	versions, err := r.Log("n.md", 0, "")
	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, v3, versions[0].SHA)
	require.True(t, versions[0].Deleted)
	require.Equal(t, v2, versions[1].SHA)
	require.Equal(t, int64(len("v2 longer")), versions[1].SizeBytes)
	require.Equal(t, "B", versions[1].ActorID)
	require.Equal(t, "upload", versions[1].Reason)
	require.Equal(t, v1, versions[2].SHA)

	page, err := r.Log("n.md", 1, v3)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, v2, page[0].SHA)

	b, err := r.Blob(v1, "n.md")
	require.NoError(t, err)
	require.Equal(t, "v1", string(b))
	_, err = r.Blob(v3, "n.md")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = r.Blob("not-a-sha", "n.md")
	require.ErrorIs(t, err, ErrNotFound)

	none, err := r.Log("never.md", 0, "")
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestLargeFilesStayOutOfHistory(t *testing.T) {
	root := t.TempDir()
	r, _, err := Init(root, 10)
	require.NoError(t, err)

	write(t, root, "small.md", "tiny")
	write(t, root, "big.pdf", strings.Repeat("x", 11))
	sha, changed, err := r.Commit(Message{Reason: "upload"}, 10, t0)
	require.NoError(t, err)
	require.True(t, changed)
	_, err = r.Blob(sha, "big.pdf")
	require.ErrorIs(t, err, ErrNotFound)

	dirty, err := r.Dirty(10)
	require.NoError(t, err)
	require.False(t, dirty, "an oversized file alone is not a pending change")

	// A tracked file that grows past the limit keeps its last version.
	write(t, root, "small.md", strings.Repeat("y", 20))
	_, changed, err = r.Commit(Message{Reason: "upload"}, 10, t0)
	require.NoError(t, err)
	require.False(t, changed)
	b, err := r.Blob(sha, "small.md")
	require.NoError(t, err)
	require.Equal(t, "tiny", string(b))
}

func TestBodyListsAtMostFiftyPaths(t *testing.T) {
	paths := make([]string, 60)
	for i := range paths {
		paths[i] = "p" + string(rune('a'+i%26)) + ".md"
	}
	msg := Message{Reason: "import"}.render(paths)
	require.Contains(t, msg, "import: pa.md (+59 more)")
	require.Contains(t, msg, "… and 10 more")
	require.Equal(t, 1, bytes.Count([]byte(msg), []byte("LV-Reason: import")))
}
