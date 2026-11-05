package chunkindex

import (
	"fmt"
	"lumina/internal/swiftstr"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MarkdownChunkerTests. The load-bearing one is the source-slice invariant: every
// citation is a promise that path and line range point at the text.

func assertInvariants(t *testing.T, source string, chunks []DocumentChunk) {
	t.Helper()
	lines := swiftstr.SplitLines(source)
	for i, c := range chunks {
		assert.Equal(t, i+1, c.Ordinal, "ordinals must be contiguous from 1")
		assert.GreaterOrEqual(t, c.StartLine, 1)
		assert.GreaterOrEqual(t, c.EndLine, c.StartLine)
		assert.LessOrEqual(t, c.EndLine, len(lines))
		assert.Equal(t, strings.Join(lines[c.StartLine-1:c.EndLine], "\n"), c.Text, "chunk text must equal its claimed source slice")
		assert.Equal(t, SHA256Hex(c.Text), c.ContentSHA256)
	}
}

func headingPaths(chunks []DocumentChunk) [][]string {
	out := make([][]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.HeadingPath
	}
	return out
}

func numbered(format string, from, to int) []string {
	out := make([]string, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf(format, i))
	}
	return out
}

func TestChunkTextIsExactlyTheLinesItClaims(t *testing.T) {
	source := "# Routing\n\nHermes picks a provider per request.\n\n## Fallbacks\n\nWhen the primary provider errors we retry once, then fall through\nto the secondary.\n\n### Timeouts\n\nThirty seconds, then abort."
	chunks := Chunk(source)
	assert.NotEmpty(t, chunks)
	assertInvariants(t, source, chunks)
}

func TestInvariantHoldsWhenSectionsAreSplit(t *testing.T) {
	paragraph := strings.TrimSuffix(strings.Repeat("Lorem ipsum dolor sit amet consectetur. ", 12), " ")
	source := "# Big\n\n" + paragraph + "\n\n" + paragraph + "\n\n" + paragraph + "\n\n## Small\n\nDone."
	chunks := ChunkSized(source, 400, 80)
	assert.Greater(t, len(chunks), 3, "an oversized section must split")
	assertInvariants(t, source, chunks)
}

func TestInvariantHoldsForPlainTextWithNoHeadings(t *testing.T) {
	source := strings.Join(numbered("line %d of an unstructured capture", 1, 200), "\n")
	chunks := ChunkSized(source, 300, 50)
	assert.Greater(t, len(chunks), 1)
	assertInvariants(t, source, chunks)
}

func TestHeadingsCutSectionsAndCarryAncestry(t *testing.T) {
	source := "# Top\n\nintro\n\n## Middle\n\nbody\n\n### Leaf\n\ndetail\n\n## Other\n\ntail"
	chunks := Chunk(source)
	paths := headingPaths(chunks)
	assert.Contains(t, paths, []string{"Top"})
	assert.Contains(t, paths, []string{"Top", "Middle"})
	assert.Contains(t, paths, []string{"Top", "Middle", "Leaf"})
	assert.Contains(t, paths, []string{"Top", "Other"})
	assertInvariants(t, source, chunks)
}

func TestPreambleBeforeTheFirstHeadingKeepsAnEmptyPath(t *testing.T) {
	source := "Some notes with no heading yet.\n\n# Later\n\ncontent"
	chunks := Chunk(source)
	require.NotEmpty(t, chunks)
	assert.Empty(t, chunks[0].HeadingPath)
	assert.NotNil(t, chunks[0].HeadingPath, "encodes as [], not null")
	assertInvariants(t, source, chunks)
}

func TestHashesInsideFencedCodeAreNotHeadings(t *testing.T) {
	source := "# Real\n\n```bash\n# not a heading\necho hi\n```\n\ntext"
	chunks := Chunk(source)
	for _, c := range chunks {
		assert.Equal(t, []string{"Real"}, c.HeadingPath)
	}
	assertInvariants(t, source, chunks)
}

func TestSetextUnderlinesBecomeHeadings(t *testing.T) {
	source := "Title\n=====\n\nbody\n\nSubtitle\n--------\n\nmore"
	chunks := Chunk(source)
	assert.Contains(t, headingPaths(chunks), []string{"Title"})
	assert.Contains(t, headingPaths(chunks), []string{"Title", "Subtitle"})
	assertInvariants(t, source, chunks)
}

func TestATXHeadingFollowedByDashesDoesNotDoubleCount(t *testing.T) {
	source := "# Foo\n---\n\nbody"
	chunks := Chunk(source)
	for _, c := range chunks {
		assert.Equal(t, []string{"Foo"}, c.HeadingPath)
	}
	assertInvariants(t, source, chunks)
}

func TestFrontmatterIsSkippedAndNeverReadAsAHeading(t *testing.T) {
	source := "---\ntitle: Hermes routing\ntags: [infra]\n---\n\n# Routing\n\nbody"
	chunks := Chunk(source)
	for _, c := range chunks {
		assert.GreaterOrEqual(t, c.StartLine, 5, "no chunk may start inside frontmatter")
		assert.Equal(t, []string{"Routing"}, c.HeadingPath)
		assert.NotContains(t, c.Text, "title: Hermes routing")
	}
	assertInvariants(t, source, chunks)
}

func TestUnterminatedFrontmatterFallsBackToBody(t *testing.T) {
	source := "---\ntitle: broken\n\nstill content"
	chunks := Chunk(source)
	assert.NotEmpty(t, chunks, "content must not vanish because the fence never closed")
	assertInvariants(t, source, chunks)
}

func TestIdenticalInputProducesIdenticalChunks(t *testing.T) {
	source := "# A\n\none\n\n## B\n\ntwo"
	assert.Equal(t, Chunk(source), Chunk(source))
}

func TestChunkHashesChangeOnlyWhenTextChanges(t *testing.T) {
	before, after := Chunk("# A\n\nhello"), Chunk("# A\n\nhello there")
	assert.NotEqual(t, before[0].ContentSHA256, after[0].ContentSHA256)
}

func TestEmptyAndBlankDocumentsProduceNoChunks(t *testing.T) {
	assert.Empty(t, Chunk(""))
	assert.Empty(t, Chunk("\n\n   \n\t\n"))
}

func TestASingleLineLongerThanTheCapIsEmittedWhole(t *testing.T) {
	long := strings.Repeat("x", 900)
	chunks := ChunkSized(long, 100, 10)
	require.Len(t, chunks, 1, "we never cut mid-line; that would break the source-slice invariant")
	assert.Equal(t, long, chunks[0].Text)
}

func TestSplittingAlwaysMakesForwardProgress(t *testing.T) {
	source := strings.Join(numbered("sentence number %d.", 1, 60), "\n")
	chunks := ChunkSized(source, 50, 49)
	assert.Greater(t, len(chunks), 1)
	for i := 1; i < len(chunks); i++ {
		assert.Greater(t, chunks[i].StartLine, chunks[i-1].StartLine, "each chunk must start later than the last")
	}
	assertInvariants(t, source, chunks)
}

func TestOverlapCarriesTrailingContextIntoTheNextChunk(t *testing.T) {
	source := strings.Join(numbered("paragraph %d\n", 1, 40), "\n")
	withOverlap, withoutOverlap := ChunkSized(source, 120, 60), ChunkSized(source, 120, 0)
	assert.GreaterOrEqual(t, len(withOverlap), len(withoutOverlap))
	assertInvariants(t, source, withOverlap)
	assertInvariants(t, source, withoutOverlap)
}

// ChunkIDsTests.

func TestDocumentIDIsStableAcrossContentChanges(t *testing.T) {
	tenant := uuid.New()
	first := DocumentID(tenant, "projects/hermes.md")
	assert.Equal(t, first, DocumentID(tenant, "projects/hermes.md"))
	assert.True(t, strings.HasPrefix(first, DocumentPrefix))
}

func TestDocumentIDDiffersAcrossTenants(t *testing.T) {
	assert.NotEqual(t, DocumentID(uuid.New(), "projects/hermes.md"), DocumentID(uuid.New(), "projects/hermes.md"))
}

func TestPathSpellingsThatMeanTheSameFileHashTheSame(t *testing.T) {
	tenant := uuid.New()
	canonical := DocumentID(tenant, "projects/hermes.md")
	assert.Equal(t, canonical, DocumentID(tenant, "./projects/hermes.md"))
	assert.Equal(t, canonical, DocumentID(tenant, "/projects/hermes.md"))
	assert.Equal(t, canonical, DocumentID(tenant, `projects\hermes.md`))
}

func TestChunkIDChangesWhenContentOrPositionChanges(t *testing.T) {
	doc := DocumentID(uuid.New(), "a.md")
	base := ChunkID(doc, 1, "aa")
	assert.Equal(t, base, ChunkID(doc, 1, "aa"))
	assert.NotEqual(t, base, ChunkID(doc, 2, "aa"))
	assert.NotEqual(t, base, ChunkID(doc, 1, "bb"))
}

// The hash's preimage, spelled out, so a reader needn't trust the golden file: the
// tenant is Foundation's UPPERCASE uuidString, and pieces are joined by NUL.
func TestDocumentIDHashesTheUppercaseUUIDString(t *testing.T) {
	tenant := uuid.MustParse("abcdef01-2345-6789-abcd-ef0123456789")
	want := DocumentPrefix + SHA256Hex("ABCDEF01-2345-6789-ABCD-EF0123456789\x00notas/café.md")[:32]
	assert.Equal(t, want, DocumentID(tenant, "notas/café.md"))
	assert.Equal(t, ChunkPrefix+SHA256Hex("lv_doc_x\x00" + strconv.Itoa(12) + "\x00cc")[:32], ChunkID("lv_doc_x", 12, "cc"))
}

// CRLF on Linux: Foundation matches whole Characters, and "\r\n" is one, so a CRLF
// document is a single line. It becomes one chunk whose heading is the whole file.
func TestCRLFDocumentIsOneLine(t *testing.T) {
	source := "# A\r\n\r\nbody\r\n## B\r\nmore"
	chunks := Chunk(source)
	require.Len(t, chunks, 1)
	assert.Equal(t, 1, chunks[0].EndLine)
	assert.Equal(t, []string{"A\r\n\r\nbody\r\n## B\r\nmore"}, chunks[0].HeadingPath)
}
