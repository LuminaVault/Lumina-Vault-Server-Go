package swiftstr

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Every expectation here was observed from Swift 6.3.3 on aarch64 Linux, the production
// toolchain. internal/chunkindex's golden test covers these functions at volume.

func TestCharactersAreGraphemeClusters(t *testing.T) {
	for s, want := range map[string]int{
		"":      0,
		"abc":   3,
		"café": 4,
		"\r\n":  1,
		"\n\r":  2,
		"👍🏽":    1,
		"👨‍👩‍👧": 1,
		"🇧🇷🇺🇸":  2,
		"क्ष":   1, // GB9c: consonant, virama, consonant
		"각":     1,
	} {
		assert.Equal(t, want, CharacterCount(s), "%q", s)
		assert.Equal(t, s, strings.Join(Characters(s), ""), "%q", s)
	}
}

func TestPrefixAndSuffixCompareWholeCharacters(t *testing.T) {
	assert.True(t, HasASCIIPrefix("# x", "#"))
	assert.False(t, HasASCIIPrefix("#́ x", "#"), "a combining mark makes it another Character")
	assert.True(t, HasASCIISuffix("x #", "#"))
	assert.False(t, HasASCIISuffix("x #́", "#"))
	assert.False(t, HasASCIIPrefix("", "#"))
	// U+037E is canonically equal to ";", and Swift compares by canonical equivalence.
	assert.True(t, CharacterIs(";", ';'))
	assert.True(t, CharacterIs("`", '`'))
	assert.True(t, CharacterIs("K", 'K'))
	assert.False(t, CharacterIs("é", 'e'))
}

func TestWhitespacesIsFoundationsSet(t *testing.T) {
	for _, r := range []rune{'\t', ' ', 0x00A0, 0x1680, 0x2000, 0x200B, 0x202F, 0x205F, 0x3000} {
		assert.True(t, IsWhitespace(r), "%U", r)
	}
	for _, r := range []rune{'\n', '\r', 0x000B, 0x0085, 0x2028, 0xFEFF, 'x'} {
		assert.False(t, IsWhitespace(r), "%U", r)
	}
	assert.Equal(t, "a b", TrimWhitespaces(" \t a b　 "))
	assert.Equal(t, "\na\n", TrimWhitespaces(" \na\n "), "newlines are not in .whitespaces")
}

func TestSplitLinesKeepsCRLFWhole(t *testing.T) {
	assert.Equal(t, []string{"a", "b"}, SplitLines("a\nb"))
	assert.Equal(t, []string{"a\r\nb"}, SplitLines("a\r\nb"), "\\r\\n is one Character, not a match for \\n")
	assert.Equal(t, []string{"a\r\n", "b"}, SplitLines("a\r\n\nb"))
	assert.Equal(t, []string{""}, SplitLines(""))
	assert.Equal(t, []string{"", ""}, SplitLines("\n"))
}

func TestReplaceBackslashesRefusesAMatchBeforeACombiningMark(t *testing.T) {
	assert.Equal(t, "a/b/", ReplaceBackslashes(`a\b\`))
	assert.Equal(t, "\\́x", ReplaceBackslashes("\\́x"), "U+0301 is in CoreFoundation's table")
	assert.Equal(t, "/ःx", ReplaceBackslashes("\\ःx"), "U+0903 is not")
}

func TestCanonicalKeyMatchesEquivalentSpellings(t *testing.T) {
	assert.Equal(t, CanonicalKey("café"), CanonicalKey("café"))
	assert.NotEqual(t, CanonicalKey("cafe"), CanonicalKey("café"))
}

func TestCharacterPrefix(t *testing.T) {
	for _, tc := range []struct {
		s    string
		n    int
		want string
	}{
		{"abc", 2, "ab"},
		{"abc", 5, "abc"},
		{"abc", 0, ""},
		{"", 3, ""},
		{"éx", 1, "é"},
		{"\r\nx", 1, "\r\n"},
		{"🇵🇹🇧🇷", 1, "🇵🇹"},
	} {
		if got := CharacterPrefix(tc.s, tc.n); got != tc.want {
			t.Errorf("CharacterPrefix(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}
