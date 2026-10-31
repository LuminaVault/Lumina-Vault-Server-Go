package swiftstr

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Expected counts are what Swift's `String.count` returns.
func TestCount(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"\r\n", 1},
		{"\n\r", 2},
		{"e\u0301", 1},              // combining acute
		{"\u00e9", 1},               // precomposed
		{"\U0001F1F5\U0001F1F9", 1}, // flag
		{"\U0001F1F5\U0001F1F9\U0001F1E7", 2},
		{"\U0001F468\u200d\U0001F469\u200d\U0001F467", 1}, // ZWJ family
		{"\U0001F44D\U0001F3FD", 1},                       // skin tone
		{"\u2764\ufe0f", 1},                               // variation selector
		{"\u1100\u1161\u11a8", 1},                         // Hangul L V T
		{"\uac00\u11a8", 1},                               // LV + T
		{"\u0915\u093f", 1},                               // Devanagari ka + vowel sign i (spacing mark)
		{"\n\u0301", 2},                                   // no extend after a control
		{"a\u200db", 2},                                   // ZWJ joins only pictographs
		{"\u200b", 1},
		{"\u0600a", 1}, // prepend
	}
	for _, c := range cases {
		require.Equal(t, c.want, Count(c.s), "%+q", c.s)
	}
}

func TestPrefix(t *testing.T) {
	s := strings.Repeat("a", 139) + "e\u0301xyz"
	require.Equal(t, strings.Repeat("a", 139)+"e\u0301", Prefix(s, 140))
	require.Equal(t, s, Prefix(s, 1000))
	require.Empty(t, Prefix(s, 0))
	crlf := strings.Repeat("c", 139) + "\r\ntail"
	require.Equal(t, strings.Repeat("c", 139)+"\r\n", Prefix(crlf, 140))
}

func TestTrim(t *testing.T) {
	require.Equal(t, "a b", TrimWhitespacesAndNewlines(" \t\n\r\v\f\u0085\u00a0\u2028\u2029\u3000a b\u2003 "))
	require.Equal(t, "a", TrimWhitespacesAndNewlines("\u200ba "), "CoreFoundation counts U+200B as whitespace")
	require.Equal(t, "\ufeff", TrimWhitespacesAndNewlines("\ufeff"))
}
