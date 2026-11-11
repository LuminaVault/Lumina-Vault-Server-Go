package brain_test

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func nodeIDs(g graphOut) (ids []string) {
	for _, n := range g.Nodes {
		ids = append(ids, n.ID)
	}
	return ids
}

func TestFocusDepth(t *testing.T) {
	v := setup(t)
	// Seed chain: m1 -source-> n1 -link-> n2.
	g := getGraph(t, v, "?focus="+m1+"&depth=1")
	require.ElementsMatch(t, []string{m1, n1}, nodeIDs(g))
	require.Len(t, g.Edges, 1)
	for _, n := range g.Nodes {
		require.Equal(t, 1, n.Weight, "weight is recomputed for the kept subgraph")
	}

	require.ElementsMatch(t, []string{m1, n1}, nodeIDs(getGraph(t, v, "?focus="+m1)), "depth defaults to 1")
	require.ElementsMatch(t, []string{m1, n1, n2}, nodeIDs(getGraph(t, v, "?focus="+m1+"&depth=2")))
	// depth=9 is clamped to 3 rather than rejected; depth=abc is the default 1.
	require.Equal(t, getGraph(t, v, "?focus="+m1+"&depth=3"), getGraph(t, v, "?focus="+m1+"&depth=9"))
	require.ElementsMatch(t, []string{m1, n1}, nodeIDs(getGraph(t, v, "?focus="+m1+"&depth=abc")))

	rec := v.do(alice, http.MethodGet, "/v1/brain/graph?focus=00000000-0000-4000-8000-0000000009ff", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "node not found")
	require.Empty(t, rec.Header().Get("ETag"))

	for _, q := range []string{"?focus=", "?focus=" + strings.Repeat("x", 301)} {
		rec = v.do(alice, http.MethodGet, "/v1/brain/graph"+q, "")
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "invalid focus")
		require.Empty(t, rec.Header().Get("ETag"))
	}
}

func TestETagRoundTrip(t *testing.T) {
	v := setup(t)
	const path = "/v1/brain/graph"
	first := v.do(alice, http.MethodGet, path, "")
	require.Equal(t, http.StatusOK, first.Code)
	tag := first.Header().Get("ETag")
	require.Regexp(t, `^"[0-9a-f]{64}"$`, tag)
	require.Equal(t, "private, no-cache", first.Header().Get("Cache-Control"))

	for _, inm := range []string{tag, "W/" + tag, `"other", ` + tag} {
		rec := v.do(alice, http.MethodGet, path, "", "If-None-Match", inm)
		require.Equal(t, http.StatusNotModified, rec.Code, inm)
		require.Empty(t, rec.Body.Bytes())
		require.Equal(t, tag, rec.Header().Get("ETag"))
		require.Equal(t, "private, no-cache", rec.Header().Get("Cache-Control"))
	}
	for _, inm := range []string{`"other"`, "*", ""} {
		require.Equal(t, http.StatusOK, v.do(alice, http.MethodGet, path, "", "If-None-Match", inm).Code, inm)
	}
	// A focused view is a different response and has its own tag.
	require.NotEqual(t, tag, v.do(alice, http.MethodGet, path+"?focus="+m1, "").Header().Get("ETag"))

	exec(t, v, `INSERT INTO memories (id, tenant_id, content, created_at, updated_at) VALUES ($1, $2, 'fresh', now(), now())`,
		"00000000-0000-4000-8000-0000000002a9", alice)
	rec := v.do(alice, http.MethodGet, path, "", "If-None-Match", tag)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotEqual(t, tag, rec.Header().Get("ETag"))
}

// A matching tag never bypasses the access check: a tag alice was given while she could read the
// team vault is worth nothing once her membership is gone.
func TestNotModifiedStillChecksVaultAccess(t *testing.T) {
	v := setup(t)
	path := "/v1/brain/graph?vaults=" + teamVault.String()
	first := v.do(alice, http.MethodGet, path, "")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	tag := first.Header().Get("ETag")
	require.NotEmpty(t, tag)
	require.Equal(t, http.StatusNotModified, v.do(alice, http.MethodGet, path, "", "If-None-Match", tag).Code)

	exec(t, v, `DELETE FROM vault_memberships WHERE vault_id = $1 AND user_id = $2`, teamVault, alice)
	rec := v.do(alice, http.MethodGet, path, "", "If-None-Match", tag)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Empty(t, rec.Header().Get("ETag"))
	require.NotContains(t, rec.Body.String(), "nodes")
}

func TestGraphIsCompressed(t *testing.T) {
	v := setup(t)
	// The gzip middleware has a 512-byte floor; add rows until the body clears it.
	for i := range 20 {
		exec(t, v, `INSERT INTO memories (id, tenant_id, content, created_at, updated_at) VALUES ($1, $2, $3, now(), now())`,
			fmt.Sprintf("00000000-0000-4000-8000-00000000%04x", 0x3000+i), alice, fmt.Sprintf("bulk memory number %d", i))
	}
	rec := v.do(alice, http.MethodGet, "/v1/brain/graph", "", "Accept-Encoding", "gzip")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
	zr, err := gzip.NewReader(rec.Body)
	require.NoError(t, err)
	raw, err := io.ReadAll(zr)
	require.NoError(t, err)
	require.Greater(t, len(raw), 512)
	require.True(t, json.Valid(raw))
	require.NotEmpty(t, rec.Header().Get("ETag"))
}

// Node ids carry uuids in UPPERCASE like every other endpoint, and focus accepts either case.
func TestFocusAcceptsEitherCase(t *testing.T) {
	v := setup(t)
	upper, lower := strings.ToUpper(m1), strings.ToLower(m1)
	want := getGraph(t, v, "?focus="+upper)
	require.Contains(t, nodeIDs(want), upper)
	require.Equal(t, want, getGraph(t, v, "?focus="+lower))

	space := "5ace0000-0000-4000-8000-0000000000ab"
	exec(t, v, `INSERT INTO spaces (id, tenant_id, name, slug) VALUES ($1, $2, 'Research', 'research')`, space, alice)
	exec(t, v, `UPDATE vault_files SET space_id = $1 WHERE id = $2`, space, n1)
	exec(t, v, `UPDATE memories SET space_id = $1 WHERE id = $2`, space, m1)
	g := getGraph(t, v, "?links=space&focus=space:"+strings.ToLower(space))
	require.ElementsMatch(t, []string{"space:" + strings.ToUpper(space), strings.ToUpper(n1), upper}, nodeIDs(g))
	require.Equal(t, g, getGraph(t, v, "?links=space&focus=space:"+strings.ToUpper(space)))
}
