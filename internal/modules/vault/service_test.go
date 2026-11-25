package vault

import (
	"context"
	"lumina/internal/vaultpaths"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Files is what a caller with no HTTP request uses. These tests build one the
// way the chat wiring does and check it against what the routes show.

func errStatus(t *testing.T, err error) int {
	t.Helper()
	var coded interface{ StatusCode() int }
	if !assert.ErrorAs(t, err, &coded) {
		return 0
	}
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

// A caller that planned a move or a delete from a known version (GitHub
// sync's pull) names it; a file edited since is left alone.
func TestFilesMoveAndDeleteIfMatch(t *testing.T) {
	v := setup(t)
	files := v.module.Files()
	ctx := context.Background()
	u := v.user("trial")

	t.Run("move", func(t *testing.T) {
		path := v.upload(u, "m.md", "now")
		_, err := files.MoveIfMatch(ctx, u, path, "kept/m.md", shaHex("planned"))
		require.ErrorIs(t, err, ErrPreconditionFailed)
		require.NotNil(t, findFile(v.list(u, "").Files, path), "a stale move leaves the file where it was")
		row, err := files.MoveIfMatch(ctx, u, path, "kept/m.md", shaHex("now"))
		require.NoError(t, err)
		require.Equal(t, "kept/m.md", row.Path)
	})

	t.Run("delete", func(t *testing.T) {
		path := v.upload(u, "d.md", "now")
		require.ErrorIs(t, files.DeleteIfMatch(ctx, u, u, path, shaHex("planned")), ErrPreconditionFailed)
		require.NotNil(t, findFile(v.list(u, "").Files, path))
		require.NoError(t, files.DeleteIfMatch(ctx, u, u, path, shaHex("now")))
		require.Nil(t, findFile(v.list(u, "").Files, path))
	})

	t.Run("the routes still ignore If-Match", func(t *testing.T) {
		path := v.upload(u, "r.md", "now")
		stale := map[string]string{"If-Match": shaHex("planned")}
		r := v.do(u, http.MethodDelete, "/v1/vault/files/"+path, stale, nil)
		require.Equal(t, http.StatusNoContent, r.code, string(r.body))
	})
}

// The routes and Module.Files() share one set of path locks. Saves through a
// route, Files.Write and Files.DeleteIfMatch all start from one version of one
// path: exactly one of them may win, every round.
func TestRoutesAndFilesExcludeEachOther(t *testing.T) {
	v := setup(t)
	propsOn(t)
	files := v.module.Files()
	ctx := context.Background()
	u := v.user("trial")
	for round := range 6 {
		name := "race-" + string(rune('a'+round)) + ".md"
		path := v.upload(u, name, "base")
		base := shaHex("base")
		const each = 3
		// A loser sees 412, or 404 once the delete has won.
		lost := []int{http.StatusOK, http.StatusPreconditionFailed, http.StatusNotFound}
		won := make([]bool, 4*each)
		var wg sync.WaitGroup
		run := func(i int, f func() (ok bool)) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				won[i] = f()
			}()
		}
		for i := range each {
			run(i, func() bool { // the upload route
				c := v.save(u, name, "route-"+string(rune('a'+i)), base).code
				assert.Contains(t, lost, c)
				return c == http.StatusOK
			})
			run(each+i, func() bool { // a route that takes the module's lock itself
				c := v.patchProps(u, path, map[string]any{"set": map[string]any{"n": i}}, base).code
				assert.Contains(t, lost, c)
				return c == http.StatusOK
			})
			run(2*each+i, func() bool {
				_, err := files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: path, Data: []byte("files-" + string(rune('a'+i))), IfMatch: base})
				if err != nil {
					assert.Contains(t, lost, errStatus(t, err))
				}
				return err == nil
			})
			run(3*each+i, func() bool {
				err := files.DeleteIfMatch(ctx, u, u, path, base)
				if err != nil {
					assert.Contains(t, lost, errStatus(t, err))
				}
				return err == nil
			})
		}
		wg.Wait()
		n := 0
		for _, w := range won {
			if w {
				n++
			}
		}
		require.Equal(t, 1, n, "round %d", round)
	}
}

func TestPathLocksTwoPaths(t *testing.T) {
	var l pathLocks
	tenant := uuid.New()
	l.lock2(tenant, "a.md", "a.md")() // one stripe, taken once
	var wg sync.WaitGroup
	for i := range 200 {
		a, b := "p"+strconv.Itoa(i%7), "q"+strconv.Itoa(i%5)
		if i%2 == 1 {
			a, b = b, a
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.lock2(tenant, a, b)()
		}()
	}
	wg.Wait() // a deadlock is the test timing out
}
