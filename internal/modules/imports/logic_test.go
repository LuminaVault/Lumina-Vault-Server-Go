package imports

// Ports Tests/AppTests/Import/ImportLogicTests.swift

import (
	"lumina/internal/notionimport"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBookmarksHTMLExtractsHTTPLinksDedupesDropsNonHTTP(t *testing.T) {
	html := `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<DL><p>
  <DT><A HREF="https://news.ycombinator.com/">HN</A>
  <DT><A HREF="http://example.com/a">A</A>
  <DT><A HREF="not-a-url">bad</A>
  <DT><A HREF="javascript:void(0)">js</A>
  <DT><A HREF="https://news.ycombinator.com/">HN dup</A>
</DL><p>`
	assert.Equal(t, []string{"https://news.ycombinator.com/", "http://example.com/a"}, ParseBookmarksHTML(html))
}

func TestParseBookmarksHTMLReturnsEmptyForNoLinks(t *testing.T) {
	assert.Empty(t, ParseBookmarksHTML("<html><body>no links</body></html>"))
}

func TestSlugifyProducesAValidSpaceSlug(t *testing.T) {
	assert.Equal(t, "ai-machine-learning", Slugify("AI & Machine Learning!"))
	assert.Equal(t, "cooking-101", Slugify("  Cooking 101  "))
	assert.Equal(t, "imported", Slugify("***"))
	assert.GreaterOrEqual(t, len(Slugify("A")), 2)
}

func TestParseMappingsReadsTheCanonicalObject(t *testing.T) {
	m := ParseMappings(`{"mappings":[{"id":"a","space":"ai"},{"id":"b","space":"new:Cooking"}]}`)
	assert.Equal(t, "ai", m["a"])
	assert.Equal(t, "new:Cooking", m["b"])
}

func TestParseMappingsToleratesCodeFencesAndSurroundingProse(t *testing.T) {
	assert.Equal(t, "health", ParseMappings("```json\n{\"mappings\":[{\"id\":\"x\",\"space\":\"health\"}]}\n```")["x"])
	assert.Equal(t, "imported", ParseMappings(`Sure! Here is the mapping: {"mappings":[{"id":"y","space":"imported"}]} Done.`)["y"])
}

func TestParseMappingsReturnsEmptyOnGarbage(t *testing.T) {
	assert.Empty(t, ParseMappings("not json"))
}

// Title falls back on the name without its extension, as corelibs' NSString computes it.
func TestTitleFallbackFollowsNSString(t *testing.T) {
	for fallback, want := range map[string]string{
		"note.md":   "note",
		"a.b.md":    "a.b",
		"":          "",
		"/":         "",
		"trailing.": "trailing.",
		".hidden":   ".hidden",
		"plain":     "plain",
	} {
		assert.Equal(t, want, Title("no heading", fallback), fallback)
	}
}

// Bulk ingest slugifies a note's path again, so the Notion path map must be a
// fixed point of SafePath: otherwise a rewritten link names a path the note
// is not at.
func TestNotionPathsSurviveSafePath(t *testing.T) {
	var entries []notionimport.Entry
	for _, p := range []string{
		"Roadmap 1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d.md",
		"Roadmap 1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d/Q4 plan v1.2.md",
		"Untitled.md", "untitled.md", "UNTITLED.md",
		"a/_.env notes.md", "???.md", "Über Café/Señor.md", "v1..md", "trailing dot..md",
		strings.Repeat("long name ", 12) + ".md", strings.Repeat("long name ", 12) + "!.md",
		strings.Repeat("é", 120) + ".md", "emoji 🎉 party.md", "a.b.c/d.e.md",
	} {
		entries = append(entries, notionimport.Entry{Path: p})
	}
	m := notionimport.MapPaths(entries, "notion")
	seen := map[string]bool{}
	for _, e := range entries {
		to := m.New[e.Path]
		require.NotEmpty(t, to, e.Path)
		assert.Equal(t, to, SafePath("notion", m.Relative(to)), e.Path)
		assert.False(t, seen[to], "%s maps to a taken path %s", e.Path, to)
		seen[to] = true
	}
}
