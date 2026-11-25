package vaultgit

import (
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

// ValidBranchName reports whether name is a branch git would accept
// (git check-ref-format), so callers outside this package need no go-git.
func ValidBranchName(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || name == "HEAD" {
		return false
	}
	return plumbing.NewBranchReferenceName(name).Validate() == nil
}
