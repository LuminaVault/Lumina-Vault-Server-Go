package chunkindex

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The slug-folded fallback: bulk import lowercases and slugifies Obsidian paths,
// so links written in Obsidian's own spelling must still find the imported file.

func resolveOneLink(t *testing.T, inner string, candidates ...WikilinkTarget) ResolvedWikilink {
	t.Helper()
	got := ResolveWikilinks([]ParsedWikilink{link(t, inner)}, candidates)
	require.Len(t, got, 1)
	return got[0]
}

func TestFoldedStemResolvesAnObsidianTitleToItsSlugifiedFile(t *testing.T) {
	lore := target("worldbuilding/lore/the-last-light.md")
	other := target("worldbuilding/factions.md")
	for _, inner := range []string{"The Last Light", "The Last Light#Prologue", "The Last Light|the book", "the last light.md"} {
		got := resolveOneLink(t, inner, lore, other)
		assert.Equal(t, Resolved, got.State, inner)
		assert.Equal(t, lore.VaultFileID, *got.TargetVaultFileID, inner)
	}
}

func TestFoldedPathResolvesAFolderQualifiedLink(t *testing.T) {
	lore := target("worldbuilding/lore/the-last-light.md")
	got := resolveOneLink(t, "Lore/The Last Light", lore)
	require.Equal(t, Resolved, got.State)
	assert.Equal(t, lore.VaultFileID, *got.TargetVaultFileID)

	assert.Equal(t, Unresolved, resolveOneLink(t, "Factions/The Last Light", lore).State,
		"a folder that is not in the path must not match")
	assert.Equal(t, Unresolved, resolveOneLink(t, "re/The Last Light", lore).State,
		"a folder matches whole segments, not a suffix of one")
}

func TestFoldedMatchesFollowTheUniquenessRule(t *testing.T) {
	a := target("notes/a/x-y.md")
	b := target("notes/b/x-y.md")
	assert.Equal(t, Ambiguous, resolveOneLink(t, "X Y", a, b).State)

	got := resolveOneLink(t, "A/X Y", a, b)
	require.Equal(t, Resolved, got.State)
	assert.Equal(t, a.VaultFileID, *got.TargetVaultFileID)
}

func TestAnExactStemStillWinsOverAFoldedOne(t *testing.T) {
	exact := target("inbox/Foo.md")
	folded := target("notes/foo.md")
	got := resolveOneLink(t, "Foo", exact, folded)
	require.Equal(t, Resolved, got.State)
	assert.Equal(t, exact.VaultFileID, *got.TargetVaultFileID)
}

func TestFoldingKeepsDotsAndRefusesSymbolOnlyTargets(t *testing.T) {
	bible := target("lore/the_last_light_universe_bible_v0.1.md")
	got := resolveOneLink(t, "The_Last_Light_Universe_Bible_v0.1", bible)
	require.Equal(t, Resolved, got.State)

	x := target("notes/x.md")
	assert.Equal(t, Unresolved, resolveOneLink(t, "🚀", x).State,
		"a target that folds to nothing must not become the x stand-in")
}

func TestFoldLinkPath(t *testing.T) {
	assert.Equal(t, "lore/the-last-light", FoldLinkPath("Lore/The Last Light.md"))
	assert.Equal(t, "my-note-draft", FoldLinkPath("My Note [draft]"))
	assert.Equal(t, "café", FoldLinkPath("Café"))
	assert.Empty(t, FoldLinkPath("a/🚀/b"))
	assert.Empty(t, FoldLinkPath(""))
}
