package vaultgit

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidBranchName(t *testing.T) {
	for _, ok := range []string{"main", "drafts", "feature/notes", "v1.2"} {
		require.True(t, ValidBranchName(ok), ok)
	}
	for _, bad := range []string{"", "-x", "HEAD", "a..b", "a b", "a~1", "x.lock", "a/", "/a", "a//b", "a:b", "a?b", "a@{b", `a\b`} {
		require.False(t, ValidBranchName(bad), bad)
	}
}
