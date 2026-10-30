package vaultpaths_test

import (
	"lumina/internal/vaultpaths"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestLayoutMatchesSwift(t *testing.T) {
	root := t.TempDir()
	p := vaultpaths.Paths{Root: root}
	id := uuid.MustParse("a11ce000-0000-4000-8000-000000000001")
	require.Equal(t, filepath.Join(root, "tenants", "A11CE000-0000-4000-8000-000000000001", "raw"), p.Raw(id))
	require.NoError(t, p.EnsureTenantDirectories(id))
	for _, d := range []string{"raw", "wiki", "memories"} {
		st, err := os.Stat(filepath.Join(p.TenantRoot(id), d))
		require.NoError(t, err)
		require.True(t, st.IsDir())
	}
}
