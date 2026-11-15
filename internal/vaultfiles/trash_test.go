package vaultfiles_test

import (
	"context"
	"lumina/internal/vaultfiles"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var (
	deletedAt = time.Unix(1800000000, 900000000)
	trashID   = uuid.MustParse("0A0B0C0D-0000-4000-8000-0000000000EE")
)

const trashA = "_deleted_1800000000_0a0b0c0d0000400080000000000000ee_a.md"

func TestTrashName(t *testing.T) {
	require.Equal(t, trashA, vaultfiles.TrashName(trashID, "a.md", deletedAt))
	require.Equal(t, trashA, vaultfiles.TrashName(trashID, "inbox/deep/a.md", deletedAt), "only the file name is kept")
}

func TestTrashNameCutsALongNameOnARuneBoundary(t *testing.T) {
	name := vaultfiles.TrashName(trashID, "dir/"+strings.Repeat("é", 200)+".md", deletedAt)
	require.True(t, utf8.ValidString(name))
	require.LessOrEqual(t, len(name), 255)
	require.True(t, strings.HasPrefix(name, "_deleted_1800000000_0a0b0c0d0000400080000000000000ee_é"))
}

func moveToTrash(t *testing.T, raw, relative string, id uuid.UUID) (bool, error) {
	t.Helper()
	target, err := vaultfiles.ResolveInside(raw, relative)
	require.NoError(t, err)
	return vaultfiles.MoveFileToTrash(raw, target, vaultfiles.TrashName(id, relative, deletedAt))
}

func TestMoveFileToTrashRenames(t *testing.T) {
	raw := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(raw, "inbox"), 0o755))
	write(t, filepath.Join(raw, "inbox", "a.md"), "body")

	moved, err := moveToTrash(t, raw, "inbox/a.md", trashID)
	require.NoError(t, err)
	require.True(t, moved)
	require.NoFileExists(t, filepath.Join(raw, "inbox", "a.md"))
	body, err := os.ReadFile(filepath.Join(raw, trashA))
	require.NoError(t, err)
	require.Equal(t, "body", string(body))
	require.DirExists(t, filepath.Join(raw, "inbox"), "the folder stays")
}

func TestMoveFileToTrashMissingFileIsNoop(t *testing.T) {
	moved, err := moveToTrash(t, t.TempDir(), "gone.md", trashID)
	require.NoError(t, err)
	require.False(t, moved)
}

// The old flattened name made "a/b.md" and "a_b.md" collide when deleted in
// the same second. Distinct trash ids keep both.
func TestMoveFileToTrashNoCollision(t *testing.T) {
	raw := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(raw, "a"), 0o755))
	write(t, filepath.Join(raw, "a", "b.md"), "first")
	write(t, filepath.Join(raw, "a_b.md"), "second")

	for _, p := range []string{"a/b.md", "a_b.md"} {
		moved, err := moveToTrash(t, raw, p, uuid.New())
		require.NoError(t, err)
		require.True(t, moved)
	}
	entries, err := os.ReadDir(raw)
	require.NoError(t, err)
	var trashed int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "_deleted_") {
			trashed++
		}
	}
	require.Equal(t, 2, trashed)
}

// A deep path used to make a name longer than the filesystem allows.
func TestMoveFileToTrashDeepPath(t *testing.T) {
	raw := t.TempDir()
	dir := "d"
	for range 30 {
		dir += "/segment"
	}
	require.NoError(t, os.MkdirAll(filepath.Join(raw, dir), 0o755))
	relative := dir + "/a.md"
	write(t, filepath.Join(raw, relative), "x")
	moved, err := moveToTrash(t, raw, relative, trashID)
	require.NoError(t, err)
	require.True(t, moved)
	require.FileExists(t, filepath.Join(raw, trashA))
}

func TestMoveFileToTrashMovesTheLinkNotItsTarget(t *testing.T) {
	root := t.TempDir()
	raw := filepath.Join(root, "raw")
	require.NoError(t, os.MkdirAll(raw, 0o755))
	outside := filepath.Join(root, "outside.md")
	write(t, outside, "x")
	require.NoError(t, os.Symlink(outside, filepath.Join(raw, "note.md")))

	_, err := moveToTrash(t, raw, "note.md", trashID)
	require.NoError(t, err)
	require.FileExists(t, outside)
	info, err := os.Lstat(filepath.Join(raw, "_deleted_1800000000_0a0b0c0d0000400080000000000000ee_note.md"))
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink)
}

// fileExists follows links, so a dangling one isn't moved.
func TestMoveFileToTrashLeavesDanglingLink(t *testing.T) {
	raw := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(raw, "nowhere"), filepath.Join(raw, "note.md")))
	moved, err := moveToTrash(t, raw, "note.md", trashID)
	require.NoError(t, err)
	require.False(t, moved)
	_, err = os.Lstat(filepath.Join(raw, "note.md"))
	require.NoError(t, err)
}

func TestTrashRecordsTheRow(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	space := f.space(t, alice, "work")
	id := f.upsert(t, vaultfiles.UpsertParams{
		Path: "work/plan.md", SpaceID: &space, SizeBytes: 4, SHA256: "s1",
		Metadata: &vaultfiles.Metadata{Title: ptr("Plan")},
	})
	raw := f.paths.Raw(alice)
	require.NoError(t, os.MkdirAll(filepath.Join(raw, "work"), 0o755))
	write(t, filepath.Join(raw, "work", "plan.md"), "body")
	row := f.row(t, alice, id)
	target, err := vaultfiles.ResolveInside(raw, row.Path)
	require.NoError(t, err)

	entry, err := f.store.Trash(ctx, raw, target, row, &alice, deletedAt)
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.NoFileExists(t, target)
	body, err := os.ReadFile(filepath.Join(raw, entry.TrashName))
	require.NoError(t, err)
	require.Equal(t, "body", string(body))

	list, err := f.store.ListTrash(ctx, alice)
	require.NoError(t, err)
	require.Len(t, list, 1)
	got := list[0]
	require.Equal(t, entry.ID, got.ID)
	require.Equal(t, id, got.VaultFileID)
	require.Equal(t, "work/plan.md", got.OriginalPath)
	require.Equal(t, entry.TrashName, got.TrashName)
	require.Equal(t, &space, got.SpaceID)
	require.Equal(t, "text/markdown", got.ContentType)
	require.Equal(t, int64(4), got.SizeBytes)
	require.Equal(t, "s1", got.SHA256)
	require.NotNil(t, got.Metadata)
	require.Equal(t, "Plan", *got.Metadata.Title)
	require.Equal(t, &alice, got.DeletedByUserID)
	require.True(t, got.DeletedAt.Equal(deletedAt.Truncate(time.Microsecond)))

	others, err := f.store.ListTrash(ctx, bob)
	require.NoError(t, err)
	require.Empty(t, others, "trash is tenant-scoped")
}

func TestTrashMissingFileRecordsNothing(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id := f.upsert(t, vaultfiles.UpsertParams{Path: "ghost.md", SizeBytes: 1, SHA256: "s"})
	row := f.row(t, alice, id)
	raw := f.paths.Raw(alice)
	target, err := vaultfiles.ResolveInside(raw, row.Path)
	require.NoError(t, err)

	entry, err := f.store.Trash(ctx, raw, target, row, &alice, deletedAt)
	require.NoError(t, err)
	require.Nil(t, entry)
	list, err := f.store.ListTrash(ctx, alice)
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestRestoredPath(t *testing.T) {
	require.Equal(t, "inbox/a.md", vaultfiles.RestoredPath("inbox/a.md", 0))
	require.Equal(t, "inbox/a-restored.md", vaultfiles.RestoredPath("inbox/a.md", 1))
	require.Equal(t, "inbox/a-restored-3.md", vaultfiles.RestoredPath("inbox/a.md", 3))
	require.Equal(t, "notes-restored", vaultfiles.RestoredPath("notes", 1))
	require.Equal(t, "d.x/.env-restored", vaultfiles.RestoredPath("d.x/.env", 1), "a leading dot is not an extension")
}
