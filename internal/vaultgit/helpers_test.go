package vaultgit

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

func mustHash(t *testing.T, sha string) plumbing.Hash {
	t.Helper()
	require.True(t, plumbing.IsHash(sha), sha)
	return plumbing.NewHash(sha)
}
