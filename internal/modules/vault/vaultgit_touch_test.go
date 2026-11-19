package vault

import (
	"encoding/json"
	"lumina/internal/ids"
	"lumina/internal/vaultgit/vaultgittest"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// E4.2: each vault write route signals the history committer, with the actor,
// so a save becomes a commit within the debounce rather than at the next sweep.
func TestVaultWritesTouchHistory(t *testing.T) {
	v := setup(t)
	rec := vaultgittest.Record(t)
	u := v.user("trial")

	path := v.upload(u, "a.md", "hello")
	r := v.move(u, path, "b.md")
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	v.deleteFile(u, "b.md")
	items := v.trash(u)
	require.Len(t, items, 1)
	r = v.restore(u, items[0].Id.String())
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	var dto fileDTO
	require.NoError(t, json.Unmarshal(r.body, &dto))
	require.Equal(t, "b.md", dto.Path)

	require.Equal(t, []string{"upload", "move", "delete", "restore"}, rec.Reasons(u))
	touches := rec.For(u)
	require.Equal(t, ids.Upper(u), touches[0].Actor, "upload names the actor")
	require.Equal(t, ids.Upper(u), touches[2].Actor, "delete names the actor")
	require.Equal(t, ids.Upper(u), touches[3].Actor, "restore names the actor")

	other := v.user("trial")
	require.Empty(t, rec.For(other), "touches are per tenant")
}
