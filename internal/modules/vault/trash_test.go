package vault

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func (v env) trash(user uuid.UUID) []VaultTrashItem {
	v.t.Helper()
	r := v.do(user, http.MethodGet, trashPath, nil, nil)
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var out VaultTrashListResponse
	require.NoError(v.t, json.Unmarshal(r.body, &out))
	return out.Items
}

func (v env) deleteFile(user uuid.UUID, path string) {
	v.t.Helper()
	r := v.do(user, http.MethodDelete, "/v1/vault/files/"+path, nil, nil)
	require.Equal(v.t, http.StatusNoContent, r.code, string(r.body))
}

func (v env) restore(user uuid.UUID, id string) response {
	v.t.Helper()
	return v.do(user, http.MethodPost, trashPath+"/"+id+"/restore", nil, nil)
}

// Backlog E3.2: list, restore and delete forever.
func TestTrashRoutes(t *testing.T) {
	v := setup(t)

	t.Run("restore puts the file and row back", func(t *testing.T) {
		u := v.user("trial")
		path := v.upload(u, "a.md", "hello")
		v.deleteFile(u, path)
		require.Nil(t, findFile(v.list(u, "").Files, path))

		items := v.trash(u)
		require.Len(t, items, 1)
		require.Equal(t, path, items[0].Path)
		require.Equal(t, int64(5), items[0].SizeBytes)

		r := v.restore(u, items[0].Id.String())
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var dto fileDTO
		require.NoError(t, json.Unmarshal(r.body, &dto))
		require.Equal(t, path, dto.Path)
		require.NotNil(t, findFile(v.list(u, "").Files, path))
		require.Empty(t, v.trash(u))
		body := v.do(u, http.MethodGet, "/v1/vault/files/"+path, nil, nil)
		require.Equal(t, http.StatusOK, body.code)
		require.Equal(t, "hello", string(body.body))
	})

	t.Run("a taken path restores beside it", func(t *testing.T) {
		u := v.user("trial")
		path := v.upload(u, "b.md", "old")
		v.deleteFile(u, path)
		v.upload(u, "b.md", "new")

		r := v.restore(u, v.trash(u)[0].Id.String())
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var dto fileDTO
		require.NoError(t, json.Unmarshal(r.body, &dto))
		require.Equal(t, "inbox/b-restored.md", dto.Path)
		require.Equal(t, "new", string(v.do(u, http.MethodGet, "/v1/vault/files/"+path, nil, nil).body))
		require.Equal(t, "old", string(v.do(u, http.MethodGet, "/v1/vault/files/inbox/b-restored.md", nil, nil).body))
	})

	t.Run("delete forever removes the file", func(t *testing.T) {
		u := v.user("trial")
		v.deleteFile(u, v.upload(u, "c.md", "x"))
		item := v.trash(u)[0]
		entries, err := os.ReadDir(v.module.paths.Raw(u))
		require.NoError(t, err)
		var before int
		for _, e := range entries {
			if len(e.Name()) > 9 && e.Name()[:9] == "_deleted_" {
				before++
			}
		}
		require.Equal(t, 1, before)

		r := v.do(u, http.MethodDelete, trashPath+"/"+item.Id.String(), nil, nil)
		require.Equal(t, http.StatusNoContent, r.code, string(r.body))
		require.Empty(t, v.trash(u))
		entries, err = os.ReadDir(v.module.paths.Raw(u))
		require.NoError(t, err)
		for _, e := range entries {
			require.NotContains(t, e.Name(), "_deleted_")
		}
		require.Equal(t, http.StatusNotFound, v.do(u, http.MethodDelete, trashPath+"/"+item.Id.String(), nil, nil).code)
	})

	t.Run("only your own", func(t *testing.T) {
		u, other := v.user("trial"), v.user("trial")
		v.deleteFile(u, v.upload(u, "d.md", "x"))
		id := v.trash(u)[0].Id.String()
		require.Empty(t, v.trash(other))
		require.Equal(t, http.StatusNotFound, v.restore(other, id).code)
		require.Equal(t, http.StatusNotFound, v.do(other, http.MethodDelete, trashPath+"/"+id, nil, nil).code)
		require.Len(t, v.trash(u), 1)
	})

	t.Run("a file gone from disk is 410 and drops the entry", func(t *testing.T) {
		u := v.user("trial")
		v.deleteFile(u, v.upload(u, "e.md", "x"))
		entry, err := v.module.store.ListTrash(t.Context(), u)
		require.NoError(t, err)
		require.NoError(t, os.Remove(filepath.Join(v.module.paths.Raw(u), entry[0].TrashName)))

		require.Equal(t, http.StatusGone, v.restore(u, entry[0].ID.String()).code)
		require.Empty(t, v.trash(u))
	})

	t.Run("bad id", func(t *testing.T) {
		u := v.user("trial")
		require.Equal(t, http.StatusBadRequest, v.restore(u, "nope").code)
		require.Equal(t, http.StatusNotFound, v.restore(u, uuid.NewString()).code)
	})
}
