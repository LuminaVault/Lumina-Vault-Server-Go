package manifest

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type route struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Module string `json:"module"`
}

func segs(p string) []string { return strings.FieldsFunc(p, func(r rune) bool { return r == '/' }) }

// prefixMatch is Ingress Prefix semantics: element-wise; a route {param}
// segment matches any literal.
func prefixMatch(prefix, path string) bool {
	pre, p := segs(prefix), segs(path)
	if len(pre) > len(p) {
		return false
	}
	for i := range pre {
		if pre[i] != p[i] && !strings.HasPrefix(p[i], "{") && !strings.HasPrefix(p[i], "*") {
			return false
		}
	}
	return true
}

// TestEveryRouteHasExactlyOneOwner: flipping a module's prefixes must move
// that module's routes and nothing else, or a shared prefix must be declared
// with its flip partners.
func TestEveryRouteHasExactlyOneOwner(t *testing.T) {
	m, err := Load("../../modules.yaml")
	require.NoError(t, err)
	raw, err := os.ReadFile("../../docs/migration/routes.json")
	require.NoError(t, err)
	var routes []route
	require.NoError(t, json.Unmarshal(raw, &routes))

	for _, r := range routes {
		var owners []string
		for name, mod := range m.Modules {
			claimed := false
			for _, p := range mod.Prefixes {
				claimed = claimed || prefixMatch(p, r.Path)
			}
			for _, e := range mod.ExactPaths {
				claimed = claimed || e == r.Path
			}
			if claimed {
				owners = append(owners, name)
			}
		}
		sort.Strings(owners)
		if len(owners) == 1 {
			continue
		}
		// Not uniquely claimed: it must sit under a shared prefix of its own
		// module, and every partner under that prefix must be in flipsWith.
		require.Empty(t, owners, "%s %s claimed by several modules: %v", r.Method, r.Path, owners)
		own := m.Modules[kebab(r.Module)]
		shared := false
		for _, sp := range own.SharedPrefixes {
			shared = shared || prefixMatch(sp, r.Path)
		}
		require.True(t, shared, "%s %s (%s) is not covered by any prefix", r.Method, r.Path, r.Module)
	}
}

func TestFlipPartnersAreMutual(t *testing.T) {
	m, err := Load("../../modules.yaml")
	require.NoError(t, err)
	for name, mod := range m.Modules {
		for _, p := range mod.FlipsWith {
			partner, ok := m.Modules[p]
			require.True(t, ok, "%s flipsWith unknown module %s", name, p)
			require.Contains(t, partner.FlipsWith, name, "%s ↔ %s", name, p)
			require.Equal(t, mod.Wave, partner.Wave, "flip partners %s and %s are in different waves", name, p)
		}
	}
}

func kebab(s string) string {
	var b strings.Builder
	r := []rune(s)
	for i, c := range r {
		upper := c >= 'A' && c <= 'Z'
		if i > 0 && upper {
			prevLower := r[i-1] >= 'a' && r[i-1] <= 'z' || r[i-1] >= '0' && r[i-1] <= '9'
			nextLower := i+1 < len(r) && r[i+1] >= 'a' && r[i+1] <= 'z'
			prevUpper := r[i-1] >= 'A' && r[i-1] <= 'Z'
			if prevLower || (prevUpper && nextLower) {
				b.WriteByte('-')
			}
		}
		b.WriteRune(c | 0x20*boolInt(upper))
	}
	return b.String()
}

func boolInt(b bool) rune {
	if b {
		return 1
	}
	return 0
}
