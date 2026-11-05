package vaultfiles

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteAtomicCreatesAndReplacesLeavingNoAuxFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.md")

	require.NoError(t, WriteAtomic(target, []byte("one")))
	require.NoError(t, WriteAtomic(target, []byte("two")))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "two", string(got))
	require.Equal(t, []string{"a.md"}, dirNames(t, dir))
}

func TestWriteAtomicNeedsTheParent(t *testing.T) {
	require.Error(t, WriteAtomic(filepath.Join(t.TempDir(), "missing", "a.md"), []byte("x")))
}

func TestWriteAtomicRefusesADirectoryAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.md")
	require.NoError(t, os.Mkdir(target, 0o755))

	require.Error(t, WriteAtomic(target, []byte("x")))
	require.Equal(t, []string{"a.md"}, dirNames(t, dir))
}

func TestReplaceFileCreatesParentsAndOverwrites(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "inbox", "deep", "a.md")

	require.NoError(t, ReplaceFile(target, []byte("one")))
	require.NoError(t, ReplaceFile(target, []byte("two")))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "two", string(got))
	require.Equal(t, []string{"a.md"}, dirNames(t, filepath.Dir(target)))
}

// FileManager.removeItem is recursive, so an upload onto a directory's name
// takes the directory with it.
func TestReplaceFileRemovesADirectoryAtTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.md")
	require.NoError(t, os.Mkdir(target, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(target, "inside.md"), []byte("x"), 0o644))

	require.NoError(t, ReplaceFile(target, []byte("file")))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "file", string(got))
}

// A dangling symlink doesn't "exist", so it isn't removed, and moveItem refuses
// it. The temp file stays behind, with Swift's name.
func TestReplaceFileFailsOnADanglingSymlinkAndLeavesTheTempFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.md")
	require.NoError(t, os.Symlink(filepath.Join(dir, "nowhere"), target))

	require.Error(t, ReplaceFile(target, []byte("x")))

	var tmp []string
	for _, name := range dirNames(t, dir) {
		if name != "a.md" {
			tmp = append(tmp, name)
		}
	}
	require.Len(t, tmp, 1)
	require.Regexp(t, regexp.MustCompile(`^a\.md\.tmp-[0-9A-F]{8}$`), tmp[0])
}
