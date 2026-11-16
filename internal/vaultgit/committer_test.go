package vaultgit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func commitCount(t *testing.T, root string) int {
	t.Helper()
	repo, err := git.PlainOpen(root)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		return 0
	}
	require.NoError(t, err)
	iter, err := repo.Log(&git.LogOptions{})
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return 0 // initialized, first commit not written yet
	}
	require.NoError(t, err)
	n := 0
	for {
		_, err := iter.Next()
		if errors.Is(err, io.EOF) {
			return n
		}
		require.NoError(t, err)
		n++
	}
}

// newTestCommitter closes itself before t.TempDir is removed (cleanups run LIFO).
func newTestCommitter(t *testing.T, root string) *Committer {
	t.Helper()
	c := NewCommitter(Options{
		RawRoot:  func(uuid.UUID) string { return root },
		Debounce: 40 * time.Millisecond,
		MaxWait:  200 * time.Millisecond,
	})
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func TestBurstOfTouchesIsOneCommit(t *testing.T) {
	root := t.TempDir()
	c := newTestCommitter(t, root)
	tenant := uuid.New()
	for i := range 20 {
		write(t, root, fmt.Sprintf("n%d.md", i), "x")
		c.Touch(tenant, "import", "U", "web")
	}
	require.Eventually(t, func() bool { return commitCount(t, root) == 1 }, 2*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	c.Close(context.Background())
	// The initial commit took the files that were already there; no second commit.
	require.Equal(t, 1, commitCount(t, root))
}

func TestMaxWaitCommitsDuringAContinuousBurst(t *testing.T) {
	root := t.TempDir()
	c := newTestCommitter(t, root)
	tenant := uuid.New()
	deadline := time.Now().Add(1200 * time.Millisecond)
	i := 0
	for time.Now().Before(deadline) {
		write(t, root, fmt.Sprintf("n%d.md", i), "x")
		c.Touch(tenant, "upload", "", "")
		i++
		time.Sleep(10 * time.Millisecond)
	}
	// Touches never paused for the 40 ms debounce, so only MaxWait committed.
	require.Eventually(t, func() bool { return commitCount(t, root) >= 3 }, 2*time.Second, 20*time.Millisecond)
}

func TestCloseFlushesPending(t *testing.T) {
	root := t.TempDir()
	c := NewCommitter(Options{RawRoot: func(uuid.UUID) string { return root }, Debounce: time.Hour, MaxWait: time.Hour})
	tenant := uuid.New()
	_, err := c.Flush(context.Background(), tenant, Message{Reason: "init"})
	require.NoError(t, err)
	write(t, root, "late.md", "x")
	c.Touch(tenant, "upload", "", "")
	c.Close(context.Background())
	require.Equal(t, 2, commitCount(t, root))
	c.Touch(tenant, "upload", "", "") // after Close: ignored, no panic
}

func TestParallelWritesEndCleanAndComplete(t *testing.T) {
	root := t.TempDir()
	c := newTestCommitter(t, root)
	tenant := uuid.New()
	_, err := c.Flush(context.Background(), tenant, Message{Reason: "init"})
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			write(t, root, fmt.Sprintf("p/%d.md", i), fmt.Sprint(i))
			c.Touch(tenant, "upload", "", "")
		})
	}
	wg.Wait()
	c.Close(context.Background())

	repo, err := Open(root)
	require.NoError(t, err)
	dirty, err := repo.Dirty(DefaultMaxFileSize)
	require.NoError(t, err)
	require.False(t, dirty, "every file is committed")
	head, err := repo.repo.Head()
	require.NoError(t, err)
	for i := range 50 {
		b, err := repo.Blob(head.Hash().String(), fmt.Sprintf("p/%d.md", i))
		require.NoError(t, err)
		require.Equal(t, fmt.Sprint(i), string(b))
	}
}

type deniedLock struct{}

func (deniedLock) TryLock(context.Context, uuid.UUID) (func(), bool, error) { return nil, false, nil }

func TestFlushReportsAnotherReplicasLock(t *testing.T) {
	root := t.TempDir()
	c := NewCommitter(Options{RawRoot: func(uuid.UUID) string { return root }, Lock: deniedLock{}})
	_, err := c.Flush(context.Background(), uuid.New(), Message{Reason: "sweep"})
	require.ErrorIs(t, err, ErrLocked)
	require.Equal(t, 0, commitCount(t, root))
}

func TestLowDiskSkipsCommit(t *testing.T) {
	root := t.TempDir()
	c := NewCommitter(Options{
		RawRoot: func(uuid.UUID) string { return root },
		Space:   func(int64) error { return errors.New("full") },
	})
	sha, err := c.Flush(context.Background(), uuid.New(), Message{Reason: "sweep"})
	require.NoError(t, err)
	require.Empty(t, sha)
	require.Equal(t, 0, commitCount(t, root))
}

func TestPackageTouchIsNoOpUntilStarted(t *testing.T) {
	Touch(uuid.New(), "upload", "", "") // no committer: nothing happens
	root := t.TempDir()
	c := newTestCommitter(t, root)
	Start(c)
	t.Cleanup(func() { Start(nil) })
	write(t, root, "a.md", "x")
	Touch(uuid.New(), "upload", "", "")
	require.Eventually(t, func() bool { return commitCount(t, root) == 1 }, 2*time.Second, 10*time.Millisecond)
	c.Close(context.Background())
}

func TestBurstMessageMixesReasons(t *testing.T) {
	b := &burst{reasons: map[string]struct{}{"upload": {}, "move": {}}, reason: "move", actor: "A"}
	require.Equal(t, Message{Reason: "mixed", Actor: "A"}, b.message())
	require.Equal(t, "sweep", (&burst{reasons: map[string]struct{}{}}).message().Reason)
}
