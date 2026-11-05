package manifest

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func load(t *testing.T) (Manifest, []Route) {
	t.Helper()
	m, err := Load("../../modules.yaml")
	require.NoError(t, err)
	routes, err := LoadRoutes("../../docs/migration/routes.json")
	require.NoError(t, err)
	return m, routes
}

// TestEveryRouteHasExactlyOneOwner: flipping a module's prefixes must move
// that module's routes and nothing else, or a shared prefix must be declared
// with its flip partners.
func TestEveryRouteHasExactlyOneOwner(t *testing.T) {
	m, routes := load(t)
	require.Empty(t, m.Check(routes))
}

func TestFlipPartnersAreMutual(t *testing.T) {
	m, _ := load(t)
	for name, mod := range m.Modules {
		for _, p := range mod.FlipsWith {
			partner, ok := m.Modules[p]
			require.True(t, ok, "%s flipsWith unknown module %s", name, p)
			require.Contains(t, partner.FlipsWith, name, "%s ↔ %s", name, p)
			require.Equal(t, mod.Wave, partner.Wave, "flip partners %s and %s are in different waves", name, p)
		}
	}
}

// The wave 4 split (wave-4-plan.md): 62 routes, 15 of them with the LLM.
func TestWaveFourPseudoModules(t *testing.T) {
	m, routes := load(t)
	owner := func(path string) string {
		t.Helper()
		var owners []string
		for name, mod := range m.Modules {
			if mod.claimsByElement(path) && !mod.holds(path) {
				owners = append(owners, name)
			}
			for _, e := range mod.ExactPaths {
				if e == path && mod.holds(path) {
					owners = append(owners, name)
				}
			}
		}
		require.Len(t, owners, 1, "%s: %v", path, owners)
		return owners[0]
	}
	for path, want := range map[string]string{
		"/v1/memory/search":           "memory-agent",
		"/v1/query":                   "memory-agent",
		"/v1/query/stream":            "memory-agent",
		"/v1/memos":                   "memory-agent",
		"/v1/knowledge/reason":        "knowledge-reason",
		"/v1/knowledge/reason/stream": "knowledge-reason",
		"/v1/memory-compile":          "kb-compile",
		"/v1/memory-compile/pending":  "kb",
		"/v1/memory":                  "memory",
		"/v1/memory/facets":           "memory",
		"/v1/memory/{id}":             "memory",
		"/v1/me/suggestions":          "memory",
		"/v1/knowledge/graph":         "knowledge",
		"/v1/import/{id}/approve":     "import",
	} {
		require.Equal(t, want, owner(path), path)
	}

	counts := map[string]int{}
	for _, r := range routes {
		if r.Module == "Memory" || r.Module == "Knowledge" || r.Module == "KB" {
			counts[owner(r.Path)]++
		}
	}
	require.Equal(t, map[string]int{"memory": 13, "memory-agent": 5, "knowledge": 4, "knowledge-reason": 2, "kb": 1, "kb-compile": 1}, counts)

	require.Contains(t, m.Modules["memory"].StringPrefixCatches, "/v1/memory-compile")
	require.Contains(t, m.Modules["memory"].StringPrefixCatches, "/v1/memory-compile/pending")
	require.Contains(t, m.Modules["vault"].StringPrefixCatches, "/v1/vaults")
	require.Equal(t, "/v1/memory/search", m.Modules["memory"].HeldPaths[0].Path)
	require.Equal(t, "wave-4-agent", m.Modules["memory"].HeldPaths[0].ReleasedBy)
	require.Empty(t, m.Modules["memory"].FlipsWith, "a hold replaces flipping together")
}

func TestPrefixMatchers(t *testing.T) {
	for _, c := range []struct {
		prefix, route   string
		element, string bool
	}{
		{"/v1/vault", "/v1/vault", true, true},
		{"/v1/vault", "/v1/vault/files", true, true},
		{"/v1/vault", "/v1/vaults", false, true},
		{"/v1/vault", "/v1/vaults/{vaultID}/members", false, true},
		{"/v1/memory", "/v1/memory-compile", false, true},
		{"/v1/memory/search", "/v1/memory/{id}", true, false},
		{"/v1/vaults", "/v1/vault", false, false},
	} {
		require.Equal(t, c.element, ElementMatch(c.prefix, c.route), "%s %s", c.prefix, c.route)
		require.Equal(t, c.string, StringMatch(c.prefix, c.route), "%s %s", c.prefix, c.route)
		require.Equal(t, c.element || c.string, PrefixMatch(c.prefix, c.route), "%s %s", c.prefix, c.route)
	}
}

func TestCheckFindsViolations(t *testing.T) {
	routes := []Route{
		{"GET", "/v1/vault/files", "Vault"},
		{"GET", "/v1/vaults", "Teams"},
		{"GET", "/v1/memory", "Memory"},
		{"POST", "/v1/memory/search", "Memory"},
		{"GET", "/v1/memory/{id}", "Memory"},
	}
	good := func() map[string]Module {
		return map[string]Module{
			"vault":  {Prefixes: []string{"/v1/vault"}, StringPrefixCatches: []string{"/v1/vaults"}},
			"teams":  {Prefixes: []string{"/v1/vaults"}},
			"memory": {Prefixes: []string{"/v1/memory"}, HeldPaths: []HeldPath{{"/v1/memory/search", "r", "g"}}},
			"agent":  {ExactPaths: []string{"/v1/memory/search"}},
		}
	}
	require.Empty(t, Manifest{good()}.Check(routes))

	for name, c := range map[string]struct {
		mutate func(map[string]Module)
		want   string
	}{
		"a string catch not declared": {func(m map[string]Module) {
			v := m["vault"]
			v.StringPrefixCatches = nil
			m["vault"] = v
		}, "catches GET /v1/vaults as a string"},
		"a stale declaration": {func(m map[string]Module) {
			v := m["vault"]
			v.StringPrefixCatches = []string{"/v1/vaults", "/v1/vaultz"}
			m["vault"] = v
		}, "/v1/vaultz catches no other module's route"},
		"a route under another module's prefix, not held": {func(m map[string]Module) {
			v := m["memory"]
			v.HeldPaths = nil
			m["memory"] = v
		}, "claimed by several modules"},
		"a held route nobody owns is uncovered": {func(m map[string]Module) {
			delete(m, "agent")
		}, "/v1/memory/search (Memory) is not covered"},
		"a held path that is no route": {func(m map[string]Module) {
			v := m["memory"]
			v.HeldPaths = append(v.HeldPaths, HeldPath{"/v1/memory/nope", "r", "g"})
			m["memory"] = v
		}, "held path /v1/memory/nope is no route"},
		"a held path outside the module": {func(m map[string]Module) {
			v := m["teams"]
			v.HeldPaths = []HeldPath{{"/v1/memory/search", "r", "g"}}
			m["teams"] = v
		}, "held path /v1/memory/search is not under any of its own entries"},
	} {
		mods := good()
		c.mutate(mods)
		var msgs []string
		for _, err := range (Manifest{mods}).Check(routes) {
			msgs = append(msgs, err.Error())
		}
		require.Contains(t, strings.Join(msgs, "\n"), c.want, name)
	}
}

// A hold doesn't reach a sibling route through its {param}.
func TestHoldIsLiteral(t *testing.T) {
	m := Module{HeldPaths: []HeldPath{{"/v1/memory/search", "r", "g"}}}
	require.True(t, m.holds("/v1/memory/search"))
	require.True(t, m.holds("/v1/memory/search/more"))
	require.False(t, m.holds("/v1/memory/{id}"))
	require.False(t, m.holds("/v1/memory/searchable"))
}

func TestLoadRefusesAHoldWithoutItsReason(t *testing.T) {
	path := t.TempDir() + "/m.yaml"
	require.NoError(t, os.WriteFile(path, []byte(`modules:
  x:
    status: swift
    prefixes: [/x]
    heldPaths:
      - {path: /x/y, releasedBy: g}
`), 0o600))
	_, err := Load(path)
	require.ErrorContains(t, err, "held path")
}
