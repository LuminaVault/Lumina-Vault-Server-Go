package skills

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parseSlash(t *testing.T, raw string) Invocation {
	t.Helper()
	inv, ok := ParseSlash(raw)
	require.True(t, ok, raw)
	return inv
}

// Swift SlashCommandParserTests, all 5.
func TestParseSlash(t *testing.T) {
	t.Run("kb ingest aliases route to kb compile", func(t *testing.T) {
		assert.Equal(t, KindKBCompile, parseSlash(t, "/kb-compile").Kind)
		assert.Equal(t, KindKBCompile, parseSlash(t, "/kb-ingest").Kind)
	})
	t.Run("known synthesis aliases preserve topic input", func(t *testing.T) {
		patterns := parseSlash(t, "/patterns sleep quality")
		assert.Equal(t, KindSkill, patterns.Kind)
		assert.Equal(t, "pattern-detector", patterns.SkillName)
		assert.Equal(t, "sleep quality", *patterns.Input)
		assert.Equal(t, map[string]string{"topic": "sleep quality"}, patterns.Arguments)

		contradictions := parseSlash(t, "/contradict diet notes")
		assert.Equal(t, "contradiction-detector", contradictions.SkillName)
		assert.Equal(t, map[string]string{"topic": "diet notes"}, contradictions.Arguments)
	})
	t.Run("beliefs requires a topic", func(t *testing.T) {
		help := parseSlash(t, "/beliefs")
		assert.Equal(t, KindHelp, help.Kind)
		assert.Equal(t, "Usage: `/beliefs <topic>`", help.HelpMarkdown)

		parsed := parseSlash(t, "/beliefs work")
		assert.Equal(t, KindSkill, parsed.Kind)
		assert.Equal(t, "belief-evolution", parsed.SkillName)
		assert.Equal(t, map[string]string{"topic": "work"}, parsed.Arguments)
	})
	t.Run("generic slash maps to skill name", func(t *testing.T) {
		parsed := parseSlash(t, "/weekly-memo last week")
		assert.Equal(t, KindSkill, parsed.Kind)
		assert.Equal(t, "weekly-memo", parsed.SkillName)
		assert.Equal(t, "last week", *parsed.Input)
		assert.Equal(t, map[string]string{"input": "last week"}, parsed.Arguments)
	})
	t.Run("non slash is ignored", func(t *testing.T) {
		_, ok := ParseSlash("hello")
		assert.False(t, ok)
	})
}

// What split(maxSplits: 1, ...), lowercased() and Characters do at the edges.
func TestParseSlashEdges(t *testing.T) {
	for _, raw := range []string{"", "   ", "/", "/   ", "/\u0301x", "x/y"} {
		_, ok := ParseSlash(raw)
		assert.False(t, ok, "%q", raw)
	}
	assert.Equal(t, "weekly-memo", parseSlash(t, "  /WEEKLY-Memo  ").SkillName)
	assert.Nil(t, parseSlash(t, "/weekly-memo").Input)
	assert.Empty(t, parseSlash(t, "/weekly-memo").Arguments)

	spaced := parseSlash(t, "/   foo \t  bar  baz ")
	assert.Equal(t, "foo", spaced.SkillName)
	assert.Equal(t, "bar  baz", *spaced.Input)

	assert.Equal(t, "foo", parseSlash(t, "/foo\u00a0bar").SkillName, "NBSP is whitespace to Character")
	assert.Equal(t, "foo\u200bbar", parseSlash(t, "/foo\u200bbar").SkillName, "U+200B isn't")
	assert.Equal(t, "i\u0307x", parseSlash(t, "/\u0130X").SkillName)
	assert.Equal(t, "\u03b1\u03c3", parseSlash(t, "/\u0391\u03a3").SkillName, "no final sigma")

	patterns := parseSlash(t, "/PATTERNS")
	assert.Equal(t, "pattern-detector", patterns.SkillName)
	assert.Nil(t, patterns.Input)
	assert.Empty(t, patterns.Arguments)
	assert.Equal(t, KindKBCompile, parseSlash(t, "/kb-compile extra words").Kind)
	assert.Equal(t, "extra words", *parseSlash(t, "/kb-compile extra words").Input)
	assert.Equal(t, "belief-evolution", parseSlash(t, "/belief-evolution x").SkillName)
}
