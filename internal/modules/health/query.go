package health

import (
	"strings"
	"unicode"
)

// trimWhitespaces is `trimmingCharacters(in: .whitespaces)`: CoreFoundation's
// whitespace set is General Category Zs, U+0009 and U+200B (see
// swiftstr.IsWhitespaceOrNewline), without the newlines.
func trimWhitespaces(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return r == '\t' || r == 0x200B || unicode.Is(unicode.Zs, r)
	})
}
