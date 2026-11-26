package vaultgit

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// History, the GitHub sync's peeks and its status read HEAD without the
// tenant's lock. A commit in progress must not look like a repository
// without commits to them.
func TestHeadReadDuringCommits(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "0")
	w, _, err := Init(root, DefaultMaxFileSize)
	require.NoError(t, err)
	first, err := w.Head()
	require.NoError(t, err)
	require.NotEmpty(t, first)

	r, err := Open(root)
	require.NoError(t, err)
	var (
		wg                      sync.WaitGroup
		stop                    = make(chan struct{})
		reads, unborn, unknown  int
		noHistory, readFailures int
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			reads++
			head, err := r.Head()
			switch {
			case err != nil:
				readFailures++
			case head == "":
				unborn++
			}
			if has, err := r.Contains(first); err != nil {
				readFailures++
			} else if !has {
				unknown++
			}
			if versions, err := r.Log("a.md", 1, ""); err != nil {
				readFailures++
			} else if len(versions) == 0 {
				noHistory++
			}
		}
	}()
	for i := 1; i <= 300; i++ {
		write(t, root, "a.md", fmt.Sprint(i))
		_, changed, err := w.Commit(Message{Reason: "save"}, DefaultMaxFileSize, t0.Add(time.Duration(i)*time.Second))
		require.NoError(t, err)
		require.True(t, changed)
	}
	close(stop)
	wg.Wait()
	t.Logf("%d reads during 300 commits: %d saw no HEAD, %d no ancestor, %d no history, %d errors", reads, unborn, unknown, noHistory, readFailures)
	require.Zero(t, unborn, "HEAD read as unborn")
	require.Zero(t, unknown, "the first commit read as unknown")
	require.Zero(t, noHistory, "the history read as empty")
	require.Zero(t, readFailures)
}

// The window itself, held open: go-git truncates the branch's ref file and
// then writes the new hash.
func TestHeadWaitsOutAHalfWrittenRef(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "0")
	r, _, err := Init(root, DefaultMaxFileSize)
	require.NoError(t, err)
	head, err := r.Head()
	require.NoError(t, err)
	ref := filepath.Join(root, ".git", "refs", "heads", localBranch)
	require.NoError(t, os.Truncate(ref, 0))

	done := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		done <- os.WriteFile(ref, []byte(head+"\n"), 0o600)
	}()
	got, err := r.Head()
	require.NoError(t, err)
	require.NoError(t, <-done)
	require.Equal(t, head, got)

	// A ref file left empty for good (a crash between the two steps) is not
	// waited for without end: it reads as before.
	require.NoError(t, os.Truncate(ref, 0))
	started := time.Now()
	got, err = r.Head()
	require.NoError(t, err)
	require.Empty(t, got)
	require.Less(t, time.Since(started), 2*time.Second)
}
