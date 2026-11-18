package connectedapps

import (
	"testing"

	"github.com/stretchr/testify/require"

	"lumina/internal/config"
)

func TestRegistryOrderAndLookup(t *testing.T) {
	r := NewRegistry(config.Config{ConnectedAppsAaruMCPURL: "https://a.test/v1/mcp"})
	ids := []string{}
	for _, a := range r.All() {
		ids = append(ids, a.ID)
	}
	require.Equal(t, []string{"norviq", "aaru", "khepri", "loci"}, ids)
	a, ok := r.Get("aaru")
	require.True(t, ok)
	require.Equal(t, "https://a.test/v1/mcp", a.MCPURL)
	require.Equal(t, AuthMode("oauth"), a.Auth)
	require.ElementsMatch(t, []string{"read_library", "list_airs", "search_titles"}, a.ReadTools)
	_, ok = r.Get("evil")
	require.False(t, ok)
}

func TestRegistryNorviqIsPAT(t *testing.T) {
	a, ok := NewRegistry(config.Config{}).Get("norviq")
	require.True(t, ok)
	require.Equal(t, AuthMode("pat"), a.Auth)
	require.Equal(t, "nvq_pat_", a.PATPrefix)
}
