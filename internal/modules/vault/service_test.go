package vault

import (
	"context"
	"lumina/internal/vaultpaths"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Files is what a caller with no HTTP request uses. These tests build one the
// way the chat wiring does and check it against what the routes show.

func errStatus(t *testing.T, err error) int {
	t.Helper()
	var coded interface{ StatusCode() int }
	require.ErrorAs(t, err, &coded)
	return coded.StatusCode()
}

func TestFilesMove(t *testing.T) {
	v := setup(t)
	files := NewFiles(v.pool, vaultpaths.Paths{Root: v.root})
	ctx := context.Background()

	t.Run("renames the file and its row", func(t *testing.T) {
		u := v.user("trial")
		old := v.upload(u, "old.md", "x")
		row, err := files.Move(ctx, u, old, "kept/new.md")
		require.NoError(t, err)
		require.Equal(t, "kept/new.md", row.Path)
		list := v.list(u, "")
		require.NotNil(t, findFile(list.Files, "kept/new.md"))
		require.Nil(t, findFile(list.Files, old))
	})

	t.Run("refuses an existing destination", func(t *testing.T) {
		u := v.user("trial")
		a, b := v.upload(u, "a.md", "a"), v.upload(u, "b.md", "b")
		_, err := files.Move(ctx, u, a, b)
		require.Equal(t, http.StatusConflict, errStatus(t, err))
		require.NotNil(t, findFile(v.list(u, "").Files, a), "the source is left where it was")
	})

	t.Run("refuses the same path, traversal and a missing file", func(t *testing.T) {
		u := v.user("trial")
		a := v.upload(u, "a.md", "a")
		_, err := files.Move(ctx, u, a, a)
		require.Equal(t, http.StatusBadRequest, errStatus(t, err))
		_, err = files.Move(ctx, u, a, "../../escape.md")
		require.Equal(t, http.StatusBadRequest, errStatus(t, err))
		_, err = files.Move(ctx, u, "inbox/nope.md", "inbox/x.md")
		require.Equal(t, http.StatusNotFound, errStatus(t, err))
	})

	t.Run("cannot reach another vault's file", func(t *testing.T) {
		alice, bob := v.user("trial"), v.user("trial")
		path := v.upload(alice, "mine.md", "x")
		_, err := files.Move(ctx, bob, path, "inbox/stolen.md")
		require.Equal(t, http.StatusNotFound, errStatus(t, err))
		require.NotNil(t, findFile(v.list(alice, "").Files, path))
	})
}

func TestFilesDelete(t *testing.T) {
	v := setup(t)
	files := NewFiles(v.pool, vaultpaths.Paths{Root: v.root})
	ctx := context.Background()

	t.Run("removes the row and keeps the file, renamed", func(t *testing.T) {
		u := v.user("trial")
		path := v.upload(u, "gone.md", "x")
		require.NoError(t, files.Delete(ctx, u, u, path))
		require.Nil(t, findFile(v.list(u, "").Files, path))

		entries, err := os.ReadDir(v.module.paths.Raw(u))
		require.NoError(t, err)
		deleted := 0
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "_deleted_") {
				deleted++
			}
		}
		require.Equal(t, 1, deleted)
		// Recorded for restore, against the user who deleted it.
		trash, err := v.module.store.ListTrash(ctx, u)
		require.NoError(t, err)
		require.Len(t, trash, 1)
		require.Equal(t, path, trash[0].OriginalPath)
		require.Equal(t, &u, trash[0].DeletedByUserID)

		require.Equal(t, http.StatusNotFound, errStatus(t, files.Delete(ctx, u, u, path)), "a second delete finds nothing")
	})

	t.Run("refuses traversal and another vault's file", func(t *testing.T) {
		alice, bob := v.user("trial"), v.user("trial")
		path := v.upload(alice, "mine.md", "x")
		require.Equal(t, http.StatusBadRequest, errStatus(t, files.Delete(ctx, alice, alice, "../../etc/passwd")))
		require.Equal(t, http.StatusNotFound, errStatus(t, files.Delete(ctx, bob, bob, path)))
		require.NotNil(t, findFile(v.list(alice, "").Files, path))
	})

	t.Run("an unknown vault has nothing to delete", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound, errStatus(t, files.Delete(ctx, uuid.New(), uuid.New(), "inbox/a.md")))
	})
}
