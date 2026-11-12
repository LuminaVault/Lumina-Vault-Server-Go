package bundled

import (
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// BeliefEvolutionTests: the belief-evolution built-in. Its manifest ships in the
// embed (Swift's Bundle.module), its prompt carries the structural contract the
// iOS client and the runner depend on, and the seed vault the live run will use
// is well formed.

const beliefSkill = "belief-evolution"

func beliefSkillMD(t *testing.T) string {
	t.Helper()
	data, err := fs.ReadFile(Skills(), beliefSkill+"/SKILL.md")
	require.NoError(t, err, "missing SKILL.md for "+beliefSkill)
	return string(data)
}

// BeliefEvolutionTests: "builtin SKILL md is bundled and declares belief-evolution".
func TestBeliefEvolutionSkillIsBundled(t *testing.T) {
	contents := beliefSkillMD(t)
	require.True(t, strings.HasPrefix(contents, "---\n"), "frontmatter must lead")
	require.Contains(t, contents, "name: "+beliefSkill)
	require.Contains(t, contents, "capability: high")
	require.Contains(t, contents, "reflections/{date}/beliefs-{slug}.md")
}

// BeliefEvolutionTests: "prompt body documents required structural contract".
func TestBeliefEvolutionPromptDocumentsTheContract(t *testing.T) {
	body := beliefSkillMD(t)
	for _, want := range []string{
		"requires a `topic`", // topic-required validation
		"[[memory:<uuid>]]",  // citation format iOS renders
		"### <YYYY-MM-DD>",   // timeline anchor heading
		"### Pattern",        // closing summary block
		"timelineEntries",    // structured response key
		"reflections/<YYYY-MM-DD>/beliefs-<slug>.md", // save=true persistence path
		"Minimum 3 anchors",                          // acceptance minimum
	} {
		require.Contains(t, body, want)
	}
}

type fixtureMemory struct {
	id        uuid.UUID
	createdAt string
	body      string
}

// beliefFixtureMemories is the Swift suite's seed vault: three months of opinions
// on remote work with a deliberate shift in the middle.
var beliefFixtureMemories = []fixtureMemory{
	{
		uuid.MustParse("11111111-1111-4111-8111-111111111111"), "2026-01-12T09:00:00Z",
		"Remote work is non-negotiable for me. The flexibility is the single biggest reason I took this role over the on-site offer.",
	},
	{
		uuid.MustParse("22222222-2222-4222-8222-222222222222"), "2026-01-30T18:20:00Z",
		"Two weeks in and remote is paying off — I finished the entire migration without a single context-switch interruption.",
	},
	{
		uuid.MustParse("33333333-3333-4333-8333-333333333333"), "2026-02-28T11:45:00Z",
		"After the offsite I realised how much faster decisions happen in person. Maybe full-remote isn't strictly better, it's a tradeoff.",
	},
	{
		uuid.MustParse("44444444-4444-4444-8444-444444444444"), "2026-03-22T14:10:00Z",
		"Pairing in person twice a week genuinely fixed the design-review bottleneck. I'd take hybrid over full remote now if I'm honest.",
	},
	{
		uuid.MustParse("55555555-5555-4555-8555-555555555555"), "2026-04-15T08:30:00Z",
		"Settling on the truth: I want hybrid, leaning 3/2. Full remote was a phase, not a value.",
	},
}

// BeliefEvolutionTests: "fixture memories are chronologically ordered and span 3 months".
func TestBeliefEvolutionFixtureIsChronologicalAndSpansThreeMonths(t *testing.T) {
	dates := make([]string, len(beliefFixtureMemories))
	seen := map[uuid.UUID]bool{}
	for i, m := range beliefFixtureMemories {
		dates[i] = m.createdAt
		seen[m.id] = true
		require.NotEmpty(t, m.body)
	}
	require.True(t, slices.IsSorted(dates), "fixture must be chronologically ordered")
	require.Len(t, beliefFixtureMemories, 5, "spec requires exactly 5 seed memories")
	require.Equal(t, "2026-01", dates[0][:7])
	require.Equal(t, "2026-04", dates[len(dates)-1][:7])
	require.Len(t, seen, len(beliefFixtureMemories), "memory ids must be unique")
}
