package vaultfiles_test

import (
	"lumina/internal/vaultfiles"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var deletedAt = time.Unix(1800000000, 900000000)

func TestTrashPath(t *testing.T) {
	require.Equal(t, "/v/raw/_deleted_1800000000_a.md", vaultfiles.TrashPath("/v/raw", "a.md", deletedAt))
	require.Equal(t, "/v/raw/_deleted_1800000000_inbox_deep_a.md", vaultfiles.TrashPath("/v/raw", "inbox/deep/a.md", deletedAt))
}

func softDelete(t *testing.T, raw, relative string) error {
	t.Helper()
	target, err := vaultfiles.ResolveInside(raw, relative)
	require.NoError(t, err)
	return vaultfiles.SoftDeleteFile(raw, target, relative, deletedAt)
}

func TestSoftDeleteRenames(t *testing.T) {
	raw := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(raw, "inbox"), 0o755))
	write(t, filepath.Join(raw, "inbox", "a.md"), "body")

	require.NoError(t, softDelete(t, raw, "inbox/a.md"))
	require.NoFileExists(t, filepath.Join(raw, "inbox", "a.md"))
	body, err := os.ReadFile(filepath.Join(raw, "_deleted_1800000000_inbox_a.md"))
	require.NoError(t, err)
	require.Equal(t, "body", string(body))
	require.DirExists(t, filepath.Join(raw, "inbox"), "the folder stays")
}

func TestSoftDeleteMissingFileIsNoop(t *testing.T) {
	require.NoError(t, softDelete(t, t.TempDir(), "gone.md"))
}

// moveItem doesn't overwrite: the second delete in one second fails and its file
// stays, where os.Rename would have destroyed the first trash file.
func TestSoftDeleteCollisionKeepsBothFiles(t *testing.T) {
	raw := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(raw, "a"), 0o755))
	write(t, filepath.Join(raw, "a", "b.md"), "first")
	write(t, filepath.Join(raw, "a_b.md"), "second")

	require.NoError(t, softDelete(t, raw, "a/b.md"))
	require.ErrorIs(t, softDelete(t, raw, "a_b.md"), os.ErrExist)

	body, err := os.ReadFile(filepath.Join(raw, "_deleted_1800000000_a_b.md"))
	require.NoError(t, err)
	require.Equal(t, "first", string(body))
	require.FileExists(t, filepath.Join(raw, "a_b.md"))
}

func TestSoftDeleteMovesTheLinkNotItsTarget(t *testing.T) {
	root := t.TempDir()
	raw := filepath.Join(root, "raw")
	require.NoError(t, os.MkdirAll(raw, 0o755))
	outside := filepath.Join(root, "outside.md")
	write(t, outside, "x")
	require.NoError(t, os.Symlink(outside, filepath.Join(raw, "note.md")))

	require.NoError(t, softDelete(t, raw, "note.md"))
	require.FileExists(t, outside)
	info, err := os.Lstat(filepath.Join(raw, "_deleted_1800000000_note.md"))
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink)
}

// fileExists follows links, so a dangling one isn't deleted.
func TestSoftDeleteLeavesDanglingLink(t *testing.T) {
	raw := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(raw, "nowhere"), filepath.Join(raw, "note.md")))
	require.NoError(t, softDelete(t, raw, "note.md"))
	_, err := os.Lstat(filepath.Join(raw, "note.md"))
	require.NoError(t, err)
}

func TestSoftDeleteNameTooLongFails(t *testing.T) {
	raw := t.TempDir()
	dir := "d"
	for range 30 {
		dir += "/segment"
	}
	require.NoError(t, os.MkdirAll(filepath.Join(raw, dir), 0o755))
	relative := dir + "/a.md"
	write(t, filepath.Join(raw, relative), "x")
	require.Error(t, softDelete(t, raw, relative))
	require.FileExists(t, filepath.Join(raw, relative))
}
