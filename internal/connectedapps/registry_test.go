package connectedapps

import (
	"lumina/internal/config"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

var (
	appIDShape    = regexp.MustCompile(`^[a-z]+$`)
	toolNameShape = regexp.MustCompile(`^[a-z_]+$`)
	// Tools known to write. None may ever be on a read allowlist.
	knownWriteTools = []string{"ask_coach", "apply_library_patch", "plan_import"}
)

// allowlistFaults is every way the apps break the read-only allowlist rules
// (ruling 7): the id and tool name shapes that keep "<app>__<tool>" splitting
// one way, no duplicates and no tool known to write.
func allowlistFaults(apps []App) []string {
	var faults []string
	seenApps := map[string]bool{}
	for _, app := range apps {
		if !appIDShape.MatchString(app.ID) {
			faults = append(faults, "app id "+app.ID)
		}
		if seenApps[app.ID] {
			faults = append(faults, "duplicate app "+app.ID)
		}
		seenApps[app.ID] = true
		seenTools := map[string]bool{}
		for _, tool := range app.ReadTools {
			name := app.ID + "/" + tool
			if !toolNameShape.MatchString(tool) {
				faults = append(faults, "tool name "+name)
			}
			if strings.Contains(tool, "__") {
				faults = append(faults, "double underscore "+name)
			}
			if seenTools[tool] {
				faults = append(faults, "duplicate tool "+name)
			}
			seenTools[tool] = true
			if slices.Contains(knownWriteTools, tool) {
				faults = append(faults, "write tool "+name)
			}
		}
	}
	return faults
}

func TestRegistryAllowlistsHoldOnlyWellFormedReadTools(t *testing.T) {
	apps := NewRegistry(config.Config{}).All()
	require.Len(t, apps, 4)
	for _, app := range apps {
		require.NotEmpty(t, app.ReadTools, app.ID)
	}
	require.Empty(t, allowlistFaults(apps))
}

// The guard itself: each rule catches what it is for.
func TestAllowlistFaultsCatchEachRule(t *testing.T) {
	for want, app := range map[string]App{
		"app id Aaru":                         {ID: "Aaru"},
		"app id a_b":                          {ID: "a_b"},
		"app id ":                             {ID: ""},
		"tool name aaru/readLibrary":          {ID: "aaru", ReadTools: []string{"readLibrary"}},
		"tool name aaru/read-2":               {ID: "aaru", ReadTools: []string{"read-2"}},
		"tool name aaru/":                     {ID: "aaru", ReadTools: []string{""}},
		"double underscore aaru/a__b":         {ID: "aaru", ReadTools: []string{"a__b"}},
		"duplicate tool aaru/list":            {ID: "aaru", ReadTools: []string{"list", "list"}},
		"write tool khepri/ask_coach":         {ID: "khepri", ReadTools: []string{"ask_coach"}},
		"write tool aaru/plan_import":         {ID: "aaru", ReadTools: []string{"plan_import"}},
		"write tool aaru/apply_library_patch": {ID: "aaru", ReadTools: []string{"apply_library_patch"}},
	} {
		require.Equal(t, []string{want}, allowlistFaults([]App{app}), want)
	}
	require.Equal(t, []string{"duplicate app aaru"}, allowlistFaults([]App{{ID: "aaru"}, {ID: "aaru"}}))
}
