package manifest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The wave 5 split (wave-5-plan.md, P6).
func TestWaveFivePseudoModules(t *testing.T) {
	m, routes := load(t)

	require.NotContains(t, m.Modules, "llm", "llm is four surfaces, each with its own module")
	for name, want := range map[string]struct {
		wave     int
		prefixes []string
		routes   int
	}{
		"llm-router":  {5, []string{"/v1/router"}, 17},
		"llm-voice":   {5, []string{"/v1/audio", "/v1/transcribe"}, 3},
		"llm-chat":    {5, []string{"/v1/llm"}, 1},
		"llm-admin":   {5, []string{"/admin/llm/health"}, 1},
		"ws":          {5, []string{"/v1/ws"}, 1},
		"skills-cron": {6, []string{"/v1/me/hermes/cron"}, 7},
	} {
		mod, ok := m.Modules[name]
		require.True(t, ok, name)
		require.Equal(t, want.wave, mod.Wave, name)
		require.Equal(t, want.prefixes, mod.Prefixes, name)
		count := 0
		for _, r := range routes {
			if mod.claimsByElement(r.Path) {
				count++
			}
		}
		require.Equal(t, want.routes, count, name)
	}

	require.Equal(t, []string{"/v1/jobs", "/v1/skills"}, m.Modules["skills"].Prefixes)
	require.NotContains(t, m.Modules["app"].Prefixes, "/v1/ws")
}

// Each hold names the batch that releases it (wave-5-plan.md, "Flip order"), and
// flipping its module never sends the held route to Go.
func TestWaveFiveHolds(t *testing.T) {
	m, routes := load(t)
	for _, c := range []struct{ module, path, route, batch string }{
		{"kb-compile", "/v1/memory-compile", "/v1/memory-compile", "batch 5-4"},
		{"apple", "/v1/devices/command", "/v1/devices/command/{id}/result", "batch 5-4"},
		{"reminders", "/v1/reminders/detect", "/v1/reminders/detect", "batch 5-3b"},
		{"me", "/v1/me/recap/period", "/v1/me/recap/period", "batch 5-3b"},
		{"community", "/v1/admin/community", "/v1/admin/community/daily/run", "batch 5-3b"},
	} {
		mod := m.Modules[c.module]
		var held *HeldPath
		for i := range mod.HeldPaths {
			if mod.HeldPaths[i].Path == c.path {
				held = &mod.HeldPaths[i]
			}
		}
		require.NotNil(t, held, c.path)
		require.Equal(t, c.batch, held.ReleasedBy, c.path)
		require.True(t, mod.holds(c.route), c.route)
		require.Contains(t, routeList(routes), c.route)
	}

	// The prefixes around a hold keep moving their other routes.
	require.False(t, m.Modules["me"].holds("/v1/me/recap"))
	require.False(t, m.Modules["community"].holds("/v1/community/posts"))
	require.False(t, m.Modules["apple"].holds("/v1/devices/commands/pending"))
}

func routeList(routes []Route) []string {
	out := make([]string, len(routes))
	for i, r := range routes {
		out[i] = r.Path
	}
	return out
}
