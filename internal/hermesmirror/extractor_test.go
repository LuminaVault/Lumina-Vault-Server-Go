package hermesmirror

import (
	"lumina/internal/bundled"
	"lumina/internal/hermesdash"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HermesArtifactExtractorTests.

func TestMarkdownImageBecomesAnImageArtifact(t *testing.T) {
	stamp := time.Unix(1_700_000_000, 0).UTC()
	msg := time.Unix(1_700_000_100, 0).UTC()
	records := ExtractArtifacts("s1", "Sketch", &stamp, []hermesdash.Message{
		{Role: "assistant", Content: "Here you go: ![logo](https://cdn.example.com/logo.png)", Timestamp: &msg},
	})
	require.Len(t, records, 1)
	assert.Equal(t, "image", records[0].Kind)
	assert.Equal(t, "https://cdn.example.com/logo.png", records[0].Value)
	assert.Equal(t, "logo.png", records[0].Label)
	assert.Equal(t, "Sketch", records[0].SessionTitle)
	assert.True(t, records[0].OccurredAt.Equal(msg))
}

func TestBareURLBecomesALink(t *testing.T) {
	records := ExtractArtifacts("s1", "Research", nil, []hermesdash.Message{
		{Role: "assistant", Content: "Read https://docs.hermes-agent.dev/skills"},
	})
	require.Len(t, records, 1)
	assert.Equal(t, "link", records[0].Kind)
	assert.Equal(t, "https://docs.hermes-agent.dev/skills", records[0].Value)
}

func TestAbsolutePathWithAnExtensionBecomesAFile(t *testing.T) {
	records := ExtractArtifacts("s1", "Export", nil, []hermesdash.Message{
		{Role: "assistant", Content: "Wrote /home/hermes/.hermes/output/brief.md"},
	})
	require.Len(t, records, 1)
	assert.Equal(t, "file", records[0].Kind)
	assert.Equal(t, "/home/hermes/.hermes/output/brief.md", records[0].Value)
	assert.Equal(t, "brief.md", records[0].Label)
}

func TestToolJSONPathIsHarvested(t *testing.T) {
	records := ExtractArtifacts("s1", "Tool", nil, []hermesdash.Message{
		{Role: "tool", Content: `{"output":"/tmp/chart.png","ok":true}`},
	})
	found := false
	for _, r := range records {
		found = found || (r.Kind == "image" && r.Value == "/tmp/chart.png")
	}
	assert.True(t, found, records)
}

func TestUserMessagesAreIgnoredAndDuplicatesCollapse(t *testing.T) {
	records := ExtractArtifacts("s1", "Dup", nil, []hermesdash.Message{
		{Role: "user", Content: "https://example.com/a.png"},
		{Role: "assistant", Content: "https://example.com/a.png once"},
		{Role: "assistant", Content: "https://example.com/a.png twice"},
	})
	require.Len(t, records, 1)
	assert.Equal(t, "image", records[0].Kind)
}

func TestContentHashIsStableForTheSameTriple(t *testing.T) {
	a := ArtifactHash("link", "https://x.test", "s1")
	b := ArtifactHash("link", "https://x.test", "s1")
	c := ArtifactHash("link", "https://x.test", "s2")
	assert.Equal(t, a, b)
	assert.NotEqual(t, a, c)
	assert.Len(t, a, 64)
}

func TestUntitledSessionsGetAFallbackTitle(t *testing.T) {
	records := ExtractArtifacts("s1", "  ", nil, []hermesdash.Message{{Role: "assistant", Content: "https://x.test/a"}})
	require.Len(t, records, 1)
	assert.Equal(t, "Untitled session", records[0].SessionTitle)
	assert.True(t, records[0].OccurredAt.Equal(time.Unix(0, 0)))
}

// HermesJobRunModelTests.

func TestShortOutputIsStoredVerbatim(t *testing.T) {
	assert.Equal(t, "# Digest\n", Truncate("# Digest\n"))
	assert.Empty(t, Truncate(""))
}

func TestOversizedOutputIsCutAtTheCapOnAUTF8BoundaryAndMarked(t *testing.T) {
	long := strings.Repeat("é", MaxOutputBytes)
	truncated := Truncate(long)
	assert.NotEqual(t, long, truncated)
	assert.True(t, strings.HasSuffix(truncated, TruncationMarker))
	assert.True(t, utf8.ValidString(truncated))
	body := strings.TrimSuffix(truncated, TruncationMarker)
	assert.LessOrEqual(t, len(body), MaxOutputBytes)
	assert.Empty(t, strings.ReplaceAll(body, "é", ""))
}

// HermesBundledSkillsTests.

func TestBundleShipsEveryKBSkillWithFrontmatter(t *testing.T) {
	skills, err := KBSkills(bundled.HermesSkills())
	require.NoError(t, err)
	var names []string
	for _, s := range skills {
		names = append(names, s.Name)
		assert.True(t, strings.HasPrefix(s.Content, "---\nname: "+s.Name+"\n"), s.Name)
		assert.NotEmpty(t, hermesdash.FrontmatterDescription(s.Content), s.Name)
	}
	assert.Equal(t, []string{"kb-compile", "kb-import", "kb-ingest", "kb-merge-vault", "kb-output"}, names)
	assert.Contains(t, names, CompileSkillName)
}

func TestMissingRootYieldsNoSkills(t *testing.T) {
	skills, err := KBSkills(nil)
	require.NoError(t, err)
	assert.Empty(t, skills)
}
