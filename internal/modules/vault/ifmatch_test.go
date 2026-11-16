package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func shaHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (v env) save(user uuid.UUID, path, body, ifMatch string) response {
	v.t.Helper()
	h := map[string]string{"Content-Type": "text/markdown"}
	if ifMatch != "" {
		h["If-Match"] = ifMatch
	}
	return v.do(user, http.MethodPost, "/v1/vault/files?path="+url.QueryEscape(path), h, []byte(body))
}

func TestParseIfMatch(t *testing.T) {
	tags, wildcard := parseIfMatch(`"ABC", def , W/"weak", *`)
	require.Equal(t, []string{"abc", "def"}, tags)
	require.True(t, wildcard)

	tags, wildcard = parseIfMatch(`W/"only-weak"`)
	require.Empty(t, tags)
	require.False(t, wildcard)
}

func TestUploadIfMatch(t *testing.T) {
	v := setup(t)

	t.Run("matching sha replaces the file", func(t *testing.T) {
		u := v.user("trial")
		path := v.upload(u, "n.md", "v1")
		r := v.save(u, "n.md", "v2", `"`+shaHex("v1")+`"`)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		got, err := os.ReadFile(filepath.Join(v.module.paths.Raw(u), path))
		require.NoError(t, err)
		require.Equal(t, "v2", string(got))
	})

	t.Run("bare sha is accepted", func(t *testing.T) {
		u := v.user("trial")
		v.upload(u, "n.md", "v1")
		require.Equal(t, http.StatusOK, v.save(u, "n.md", "v2", shaHex("v1")).code)
	})

	t.Run("stale sha gets 412 and writes nothing", func(t *testing.T) {
		u := v.user("trial")
		path := v.upload(u, "n.md", "v1")
		require.Equal(t, http.StatusOK, v.save(u, "n.md", "v2", shaHex("v1")).code)
		r := v.save(u, "n.md", "stale edit", shaHex("v1"))
		require.Equal(t, http.StatusPreconditionFailed, r.code, string(r.body))
		got, err := os.ReadFile(filepath.Join(v.module.paths.Raw(u), path))
		require.NoError(t, err)
		require.Equal(t, "v2", string(got))
	})

	t.Run("missing file fails, even with *", func(t *testing.T) {
		u := v.user("trial")
		require.Equal(t, http.StatusPreconditionFailed, v.save(u, "gone.md", "x", shaHex("x")).code)
		require.Equal(t, http.StatusPreconditionFailed, v.save(u, "gone.md", "x", "*").code)
	})

	t.Run("* passes when the file exists", func(t *testing.T) {
		u := v.user("trial")
		v.upload(u, "n.md", "v1")
		require.Equal(t, http.StatusOK, v.save(u, "n.md", "v2", "*").code)
	})

	t.Run("without If-Match the save overwrites as before", func(t *testing.T) {
		u := v.user("trial")
		v.upload(u, "n.md", "v1")
		require.Equal(t, http.StatusOK, v.save(u, "n.md", "v2", "").code)
	})

	t.Run("two saves from one version: exactly one wins", func(t *testing.T) {
		u := v.user("trial")
		v.upload(u, "race.md", "base")
		base := shaHex("base")
		codes := make([]int, 8)
		var wg sync.WaitGroup
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i] = v.save(u, "race.md", "edit-"+string(rune('a'+i)), base).code
			}()
		}
		wg.Wait()
		won := 0
		for _, c := range codes {
			if c == http.StatusOK {
				won++
			} else {
				require.Equal(t, http.StatusPreconditionFailed, c)
			}
		}
		require.Equal(t, 1, won)
	})
}
