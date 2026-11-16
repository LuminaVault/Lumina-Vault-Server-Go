package vaultgit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// fakeVault lays out <root>/tenants/<UPPER UUID>/raw like vaultpaths.
func fakeVault(t *testing.T) (tenantsDir string, rawOf func(uuid.UUID) string) {
	t.Helper()
	tenantsDir = filepath.Join(t.TempDir(), "tenants")
	return tenantsDir, func(id uuid.UUID) string {
		return filepath.Join(tenantsDir, strings.ToUpper(id.String()), "raw")
	}
}

func TestSweepInitsAndCommitsMissedWrites(t *testing.T) {
	tenantsDir, rawOf := fakeVault(t)
	a, b := uuid.New(), uuid.New()
	write(t, rawOf(a), "a.md", "a")
	write(t, rawOf(b), "b.md", "b")
	require.NoError(t, os.MkdirAll(filepath.Join(tenantsDir, "not-a-uuid", "raw"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(tenantsDir, strings.ToUpper(uuid.NewString())), 0o750)) // no raw: skipped

	c := NewCommitter(Options{RawRoot: rawOf})
	committed, failed, err := c.Sweep(context.Background(), tenantsDir)
	require.NoError(t, err)
	require.Zero(t, failed)
	require.Zero(t, committed, "init commits are the backfill; nothing else was pending")
	require.Equal(t, 1, commitCount(t, rawOf(a)))
	require.Equal(t, 1, commitCount(t, rawOf(b)))

	// A writer that never touched: the next sweep picks it up.
	write(t, rawOf(a), "missed.md", "x")
	committed, _, err = c.Sweep(context.Background(), tenantsDir)
	require.NoError(t, err)
	require.Equal(t, 1, committed)
	require.Equal(t, 2, commitCount(t, rawOf(a)))
	require.Equal(t, 1, commitCount(t, rawOf(b)))
}

func TestSweepSkipsLockedTenants(t *testing.T) {
	tenantsDir, rawOf := fakeVault(t)
	write(t, rawOf(uuid.New()), "a.md", "a")
	c := NewCommitter(Options{RawRoot: rawOf, Lock: deniedLock{}})
	committed, failed, err := c.Sweep(context.Background(), tenantsDir)
	require.NoError(t, err)
	require.Zero(t, committed)
	require.Zero(t, failed)
}

func TestGCRepacksAndKeepsHistory(t *testing.T) {
	tenantsDir, rawOf := fakeVault(t)
	id := uuid.New()
	write(t, rawOf(id), "a.md", "v1")
	c := NewCommitter(Options{RawRoot: rawOf})
	_, err := c.Flush(context.Background(), id, Message{Reason: "upload"})
	require.NoError(t, err)
	write(t, rawOf(id), "a.md", "v2")
	sha, err := c.Flush(context.Background(), id, Message{Reason: "upload"})
	require.NoError(t, err)

	repacked, failed, err := c.GC(context.Background(), tenantsDir)
	require.NoError(t, err)
	require.Zero(t, failed)
	require.Equal(t, 1, repacked)

	r, err := Open(rawOf(id))
	require.NoError(t, err)
	got, err := r.Blob(sha, "a.md")
	require.NoError(t, err)
	require.Equal(t, "v2", string(got))
	versions, err := r.Log("a.md", 0, "")
	require.NoError(t, err)
	require.Len(t, versions, 2)
}

func TestSweepOnMissingTenantsDir(t *testing.T) {
	c := NewCommitter(Options{RawRoot: func(uuid.UUID) string { return "" }})
	committed, failed, err := c.Sweep(context.Background(), filepath.Join(t.TempDir(), "nope"))
	require.NoError(t, err)
	require.Zero(t, committed+failed)
}
