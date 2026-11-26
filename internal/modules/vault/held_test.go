package vault

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// E4.9 review item 1: a save or a move whose path a held entry of the
// connected GitHub repository is in the way of is a 409 path_held, and
// writes nothing. A vault with no held list (every vault that never synced)
// is not looked at twice.
func TestHeldPathRefusesASaveAndAMove(t *testing.T) {
	v := setup(t)
	u, other := v.user("trial"), v.user("trial")
	raw := v.module.paths.Raw(u)
	dir, theirs := v.space(u, "dir"), v.space(other, "dir")
	// An upload is filed under its Space's slug, or under inbox.
	put := func(user uuid.UUID, space *uuid.UUID, name string) response {
		t.Helper()
		target := "/v1/vault/files?path=" + url.QueryEscape(name)
		if space != nil {
			target += "&space_id=" + space.String()
		}
		return v.do(user, http.MethodPost, target, map[string]string{"Content-Type": "text/markdown"}, []byte("x"))
	}
	saved := func(r response, path string) {
		t.Helper()
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var out VaultUploadResponse
		require.NoError(t, json.Unmarshal(r.body, &out))
		require.Equal(t, path, out.Path)
	}

	// No list: nothing is refused.
	saved(put(u, &dir, "free.md"), "dir/free.md")
	saved(put(u, nil, "a.md"), "inbox/a.md")

	// The list a pull leaves: GitHub has a link named dir, and a dot-file
	// inside what would be the note inbox/b.md.
	require.NoError(t, os.MkdirAll(filepath.Join(raw, ".git"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(raw, ".git", "lv-held"), []byte("dir\ninbox/b.md/.cache\n"), 0o600))

	refused := func(r response, held string) {
		t.Helper()
		require.Equal(t, http.StatusConflict, r.code, string(r.body))
		var out struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(r.body, &out))
		require.Equal(t, "path_held", out.Error.Code)
		require.Contains(t, out.Error.Message, `"`+held+`"`)
	}
	refused(put(u, &dir, "new.md"), "dir")
	refused(put(u, &dir, "free.md"), "dir") // a replace is a save too
	refused(put(u, nil, "b.md"), "inbox/b.md/.cache")
	refused(v.move(u, "inbox/a.md", "dir/a.md"), "dir")
	refused(v.move(u, "inbox/a.md", "inbox/b.md"), "inbox/b.md/.cache")
	require.NoFileExists(t, filepath.Join(raw, "dir", "new.md"))
	require.NoFileExists(t, filepath.Join(raw, "inbox", "b.md"))
	require.FileExists(t, filepath.Join(raw, "inbox", "a.md"), "a refused move moved nothing")
	var rows int
	require.NoError(t, v.pool.QueryRow(t.Context(), `SELECT count(*) FROM vault_files WHERE tenant_id = $1`, u).Scan(&rows))
	require.Equal(t, 2, rows, "no row was made")

	// Names beside the held ones are free, and so is every other vault.
	saved(put(u, nil, "c.md"), "inbox/c.md")
	require.Equal(t, http.StatusOK, v.move(u, "inbox/a.md", "inbox/b.mdx.md").code)
	saved(put(other, &theirs, "new.md"), "dir/new.md")

	// A list that cannot be read refuses nothing: saves do not depend on
	// sync being well.
	require.NoError(t, os.Remove(filepath.Join(raw, ".git", "lv-held")))
	require.NoError(t, os.Mkdir(filepath.Join(raw, ".git", "lv-held"), 0o750))
	saved(put(u, &dir, "after.md"), "dir/after.md")
}
