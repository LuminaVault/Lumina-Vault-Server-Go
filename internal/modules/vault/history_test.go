package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"lumina/internal/vaultgit"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// commit snapshots user's vault the way the committer would.
func (v env) commit(user uuid.UUID, reason string) string {
	v.t.Helper()
	raw := v.module.paths.Raw(user)
	repo, _, err := vaultgit.Init(raw, vaultgit.DefaultMaxFileSize)
	require.NoError(v.t, err)
	_, _, err = repo.Commit(vaultgit.Message{Reason: reason, Actor: user.String()}, vaultgit.DefaultMaxFileSize, time.Now())
	require.NoError(v.t, err)
	// The first call's Init already committed what was there.
	sha, err := repo.Head()
	require.NoError(v.t, err)
	return sha
}

func (v env) history(user uuid.UUID, path, extra string) VaultHistoryResponse {
	v.t.Helper()
	r := v.do(user, http.MethodGet, "/v1/vault/history?path="+url.QueryEscape(path)+extra, nil, nil)
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var out VaultHistoryResponse
	require.NoError(v.t, json.Unmarshal(r.body, &out))
	return out
}

func TestHistoryIsEmptyWithoutARepository(t *testing.T) {
	v := setup(t)
	u := v.user("free")
	path := v.upload(u, "a.md", "one")
	require.Empty(t, v.history(u, path, "").Versions)

	r := v.do(u, http.MethodGet, "/v1/vault/versions/"+stringsRepeat("a", 40)+"?path="+url.QueryEscape(path), nil, nil)
	require.Equal(t, http.StatusNotFound, r.code)
}

func stringsRepeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}

func TestHistoryListsVersionsAndReadsOne(t *testing.T) {
	v := setup(t)
	u := v.user("free")
	path := v.upload(u, "n.md", "first")
	v1 := v.commit(u, "upload")
	v.upload(u, "n.md", "second, longer")
	v2 := v.commit(u, "upload")

	h := v.history(u, path, "")
	require.Len(t, h.Versions, 2)
	require.Equal(t, v2, h.Versions[0].Sha)
	require.Equal(t, v1, h.Versions[1].Sha)
	require.Equal(t, int64(len("second, longer")), h.Versions[0].SizeBytes)
	require.Equal(t, "upload", h.Versions[0].Reason)
	require.NotNil(t, h.Versions[0].ActorId)
	require.Equal(t, u, h.Versions[0].ActorId.UUID)

	page := v.history(u, path, "&limit=1&before="+v2)
	require.Len(t, page.Versions, 1)
	require.Equal(t, v1, page.Versions[0].Sha)

	r := v.do(u, http.MethodGet, "/v1/vault/versions/"+v1+"?path="+url.QueryEscape(path), nil, nil)
	require.Equal(t, http.StatusOK, r.code)
	require.Equal(t, "first", string(r.body))
	require.Equal(t, "text/markdown", r.headers.Get("Content-Type"))

	r = v.do(u, http.MethodGet, "/v1/vault/versions/"+v1+"?path=never.md", nil, nil)
	require.Equal(t, http.StatusNotFound, r.code)
	r = v.do(u, http.MethodGet, "/v1/vault/history", nil, nil)
	require.Equal(t, http.StatusBadRequest, r.code)
	r = v.do(u, http.MethodGet, "/v1/vault/history?path=a.md&limit=0", nil, nil)
	require.Equal(t, http.StatusBadRequest, r.code)
}

func TestRestoreVersionIsByteExactAndANewVersion(t *testing.T) {
	v := setup(t)
	u := v.user("free")
	path := v.upload(u, "n.md", "keep me\n")
	v1 := v.commit(u, "upload")
	v.upload(u, "n.md", "oops, overwritten")
	v.commit(u, "upload")

	r := v.do(u, http.MethodPost, "/v1/vault/versions/"+v1+"/restore?path="+url.QueryEscape(path), nil, nil)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	var dto struct {
		Path   string `json:"path"`
		Sha256 string `json:"sha256"`
	}
	require.NoError(t, json.Unmarshal(r.body, &dto))
	require.Equal(t, path, dto.Path)
	sum := sha256.Sum256([]byte("keep me\n"))
	require.Equal(t, hex.EncodeToString(sum[:]), dto.Sha256)

	got, err := os.ReadFile(filepath.Join(v.module.paths.Raw(u), filepath.FromSlash(path)))
	require.NoError(t, err)
	require.Equal(t, "keep me\n", string(got))

	v.commit(u, "restore")
	require.Len(t, v.history(u, path, "").Versions, 3, "restore adds a version, never rewrites")
}

func TestRestoreRecreatesADeletedFile(t *testing.T) {
	v := setup(t)
	u := v.user("free")
	path := v.upload(u, "gone.md", "was here")
	v1 := v.commit(u, "upload")
	v.deleteFile(u, path)
	v.commit(u, "delete")
	h := v.history(u, path, "")
	require.True(t, h.Versions[0].Deleted)

	r := v.do(u, http.MethodPost, "/v1/vault/versions/"+v1+"/restore?path="+url.QueryEscape(path), nil, nil)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	require.Contains(t, pathsOf(v.list(u, "")), path)
}

func TestRestoreHonorsIfMatch(t *testing.T) {
	v := setup(t)
	u := v.user("free")
	path := v.upload(u, "n.md", "v1")
	v1 := v.commit(u, "upload")
	v.upload(u, "n.md", "v2")

	r := v.do(u, http.MethodPost, "/v1/vault/versions/"+v1+"/restore?path="+url.QueryEscape(path),
		map[string]string{"If-Match": `"` + stringsRepeat("0", 64) + `"`}, nil)
	require.Equal(t, http.StatusPreconditionFailed, r.code)
	got, err := os.ReadFile(filepath.Join(v.module.paths.Raw(u), filepath.FromSlash(path)))
	require.NoError(t, err)
	require.Equal(t, "v2", string(got), "a stale restore writes nothing")
}

func TestHistoryIsPerTenant(t *testing.T) {
	v := setup(t)
	owner, other := v.user("free"), v.user("free")
	path := v.upload(owner, "secret.md", "mine")
	sha := v.commit(owner, "upload")

	require.Empty(t, v.history(other, path, "").Versions)
	r := v.do(other, http.MethodGet, "/v1/vault/versions/"+sha+"?path="+url.QueryEscape(path), nil, nil)
	require.Equal(t, http.StatusNotFound, r.code)
	r = v.do(other, http.MethodPost, "/v1/vault/versions/"+sha+"/restore?path="+url.QueryEscape(path), nil, nil)
	require.Equal(t, http.StatusNotFound, r.code)
	r = v.do(uuid.Nil, http.MethodGet, "/v1/vault/history?path=a.md", nil, nil)
	require.Equal(t, http.StatusUnauthorized, r.code)
}

func pathsOf(l fileListResponse) []string {
	out := make([]string, 0, len(l.Files))
	for _, f := range l.Files {
		out = append(out, f.Path)
	}
	return out
}
