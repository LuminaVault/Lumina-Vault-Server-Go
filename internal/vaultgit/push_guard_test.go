package vaultgit

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// receives counts the pushes that reach the stub as a request.
func receives(rm *remote) *atomic.Int32 {
	var n atomic.Int32
	rm.gh.Lock()
	rm.gh.Intercept = func(_ http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			n.Add(1)
		}
		return false
	}
	rm.gh.Unlock()
	return &n
}

// go-git copies a push's whole pack into memory before the request. A pack
// over the limit stops that copy and is never sent; under it, the push says
// whether it sent anything.
func TestPushWithRefusesAPackOverTheLimit(t *testing.T) {
	rm := newRemote(t)
	repo := newRepo(t)
	ctx := context.Background()
	require.NoError(t, repo.SetRemote(rm.url))
	posts := receives(rm)

	noise := make([]byte, 256<<10) // does not compress
	_, err := rand.Read(noise)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "big.bin"), noise, 0o600))
	_, _, err = repo.Commit(Message{Reason: "upload"}, DefaultMaxFileSize, time.Now())
	require.NoError(t, err)
	size, err := repo.ObjectsSize()
	require.NoError(t, err)
	require.Greater(t, size, int64(len(noise)), "every object, compressed")

	_, err = repo.PushWith(ctx, "main", rm.token, PushOptions{MaxPackBytes: 64 << 10})
	require.ErrorIs(t, err, ErrPackTooLarge)
	require.Zero(t, posts.Load(), "nothing was sent")
	require.Empty(t, rm.gh.Git.Tip(stubRepo, "main"))

	res, err := repo.PushWith(ctx, "main", rm.token, PushOptions{MaxPackBytes: 1 << 20})
	require.NoError(t, err)
	require.True(t, res.Sent)
	require.Equal(t, mustHead(t, repo), res.SHA)
	require.Equal(t, res.SHA, rm.gh.Git.Tip(stubRepo, "main"))
	require.EqualValues(t, 1, posts.Load())

	again, err := repo.PushWith(ctx, "main", rm.token, PushOptions{MaxPackBytes: 1})
	require.NoError(t, err, "nothing to send is under any limit")
	require.False(t, again.Sent)
	require.Equal(t, res.SHA, again.SHA)
	require.EqualValues(t, 1, posts.Load())
}

// The context reaches the copy of the pack, which go-git's transport makes
// without looking at it: a push whose deadline passes while the pack is being
// built stops there.
func TestPackGuardStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := &packGuard{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("x", 100))), ctx: ctx, limit: 60}
	buf := make([]byte, 40)
	n, err := g.Read(buf)
	require.NoError(t, err)
	require.Equal(t, 40, n)
	_, err = g.Read(buf)
	require.ErrorIs(t, err, ErrPackTooLarge, "80 bytes read, 60 allowed")

	g = &packGuard{ReadCloser: io.NopCloser(strings.NewReader("pack")), ctx: ctx}
	cancel()
	_, err = g.Read(buf)
	require.ErrorIs(t, err, context.Canceled)

	// Through the transport: a context that is done sends no pack.
	rm := newRemote(t)
	repo := newRepo(t)
	require.NoError(t, repo.SetRemote(rm.url))
	posts := receives(rm)
	_, err = repo.PushWith(ctx, "main", rm.token, PushOptions{})
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrPushRejected) || errors.Is(err, ErrNonFastForward), "transient to the caller: %v", err)
	require.Zero(t, posts.Load())
}

// .git/config is replaced whole (a temp file renamed over it), never
// truncated and rewritten in place, and no temp file is left behind.
func TestSetRemoteReplacesTheConfigInOneStep(t *testing.T) {
	repo := newRepo(t)
	path := filepath.Join(repo.Root(), ".git", "config")
	before, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, repo.SetRemote("https://github.com/octocat/notes.git"))
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.False(t, os.SameFile(before, after), "a new file took the old one's place")
	url, err := repo.RemoteURL()
	require.NoError(t, err)
	require.Equal(t, "https://github.com/octocat/notes.git", url)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(b), `[remote "origin"]`)
	require.Contains(t, string(b), "[core]", "what the config already held is kept")

	require.NoError(t, repo.RemoveRemote())
	entries, err := os.ReadDir(filepath.Join(repo.Root(), ".git"))
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, strings.HasSuffix(e.Name(), ".tmp"), e.Name())
	}
	// The repository still opens and commits.
	reopened, err := Open(repo.Root())
	require.NoError(t, err)
	require.Equal(t, mustHead(t, repo), mustHead(t, reopened))
}
