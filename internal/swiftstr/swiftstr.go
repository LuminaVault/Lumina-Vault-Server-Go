// Package swiftstr reproduces the Swift String semantics that leak onto the
// wire: `String.count` and `prefix(_:)` count extended grapheme clusters (not
// bytes or code points), and `trimmingCharacters(in: .whitespacesAndNewlines)`
// trims a specific Foundation character set.
//
// The segmenter implements the UAX #29 extended grapheme cluster rules with
// the categories derived from Go's unicode tables plus the emoji, Hangul and
// Prepend ranges listed below. Not implemented: GB9c (Indic conjunct
// clusters, Unicode 15.1), so a virama-joined conjunct counts as more than one
// character here. Everything else (CRLF, combining marks, spacing marks, ZWJ
// emoji sequences, skin tones, flags, Hangul syllables) matches Swift.
package swiftstr

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// IsWhitespaceOrNewline reports membership in Foundation's
// CharacterSet.whitespacesAndNewlines: General Category Zs, U+0009, U+000A to
// U+000D, U+0085, U+2028 and U+2029.
func IsWhitespaceOrNewline(r rune) bool {
	switch {
	case r == '\t', r >= '\n' && r <= '\r', r == 0x85, r == 0x2028, r == 0x2029:
		return true
	default:
		return unicode.Is(unicode.Zs, r)
	}
}

// TrimWhitespacesAndNewlines is Swift's
// `s.trimmingCharacters(in: .whitespacesAndNewlines)`.
func TrimWhitespacesAndNewlines(s string) string {
	return strings.TrimFunc(s, IsWhitespaceOrNewline)
}

// Count is Swift's `s.count`: the number of extended grapheme clusters.
func Count(s string) int {
	n := 0
	for i := 0; i < len(s); n++ {
		i += next(s[i:])
	}
	return n
}

// Prefix is Swift's `String(s.prefix(n))`: the first n grapheme clusters.
func Prefix(s string, n int) string {
	i := 0
	for k := 0; k < n && i < len(s); k++ {
		i += next(s[i:])
	}
	return s[:i]
}

type gcb uint8

const (
	other gcb = iota
	cr
	lf
	control
	extend
	zwj
	regional
	prepend
	spacingMark
	hangulL
	hangulV
	hangulT
	hangulLV
	hangulLVT
)

// next returns the byte length of the first grapheme cluster of s (len(s) > 0).
func next(s string) int {
	r, size := utf8.DecodeRuneInString(s)
	prev := category(r)
	pict := isExtPict(r) // inside ExtPict Extend* (ZWJ)? for GB11
	ris := 0
	if prev == regional {
		ris = 1
	}
	i := size
	for i < len(s) {
		r, size = utf8.DecodeRuneInString(s[i:])
		cur := category(r)
		if breakBetween(prev, cur, pict, ris, isExtPict(r)) {
			break
		}
		switch {
		case cur == regional:
			ris++
		case isExtPict(r):
			pict = true
		case cur != extend && cur != zwj:
			pict = false
		}
		if cur != regional {
			ris = 0
		}
		prev = cur
		i += size
	}
	return i
}

func breakBetween(prev, cur gcb, pict bool, ris int, curPict bool) bool {
	switch {
	case prev == cr && cur == lf: // GB3
		return false
	case prev == cr, prev == lf, prev == control: // GB4
		return true
	case cur == cr, cur == lf, cur == control: // GB5
		return true
	case prev == hangulL && (cur == hangulL || cur == hangulV || cur == hangulLV || cur == hangulLVT): // GB6
		return false
	case (prev == hangulLV || prev == hangulV) && (cur == hangulV || cur == hangulT): // GB7
		return false
	case (prev == hangulLVT || prev == hangulT) && cur == hangulT: // GB8
		return false
	case cur == extend, cur == zwj, cur == spacingMark: // GB9, GB9a
		return false
	case prev == prepend: // GB9b
		return false
	case prev == zwj && pict && curPict: // GB11
		return false
	case prev == regional && cur == regional: // GB12, GB13
		return ris%2 == 0
	default: // GB999
		return true
	}
}

func category(r rune) gcb {
	switch {
	case r == '\r':
		return cr
	case r == '\n':
		return lf
	case r == 0x200D:
		return zwj
	case r == 0x200C, r >= 0x1F3FB && r <= 0x1F3FF, r >= 0xE0020 && r <= 0xE007F, r == 0xFF9E, r == 0xFF9F:
		return extend
	case r >= 0x1F1E6 && r <= 0x1F1FF:
		return regional
	case isPrepend(r):
		return prepend
	case r >= 0x1100 && r <= 0x115F, r >= 0xA960 && r <= 0xA97C:
		return hangulL
	case r >= 0x1160 && r <= 0x11A7, r >= 0xD7B0 && r <= 0xD7C6:
		return hangulV
	case r >= 0x11A8 && r <= 0x11FF, r >= 0xD7CB && r <= 0xD7FB:
		return hangulT
	case r >= 0xAC00 && r <= 0xD7A3:
		if (r-0xAC00)%28 == 0 {
			return hangulLV
		}
		return hangulLVT
	case unicode.In(r, unicode.Mn, unicode.Me):
		return extend
	case unicode.Is(unicode.Mc, r):
		return spacingMark
	case unicode.In(r, unicode.Cc, unicode.Zl, unicode.Zp, unicode.Cf):
		return control
	default:
		return other
	}
}

func isPrepend(r rune) bool {
	switch {
	case r >= 0x0600 && r <= 0x0605, r == 0x06DD, r == 0x070F, r == 0x0890, r == 0x0891, r == 0x08E2,
		r == 0x110BD, r == 0x110CD, r == 0x111C2, r == 0x111C3, r == 0x1193F, r == 0x11941, r == 0x11A3A,
		r >= 0x11A84 && r <= 0x11A89, r == 0x11D46, r == 0x11F02:
		return true
	}
	return false
}

// isExtPict approximates Extended_Pictographic: the emoji blocks plus the
// older symbols that emoji presentation applies to.
func isExtPict(r rune) bool {
	switch {
	case r == 0x00A9, r == 0x00AE, r == 0x203C, r == 0x2049, r == 0x2122, r == 0x2139,
		r >= 0x2194 && r <= 0x21AA, r >= 0x231A && r <= 0x23FF, r == 0x24C2,
		r >= 0x25AA && r <= 0x25FE, r >= 0x2600 && r <= 0x27BF, r == 0x2934, r == 0x2935,
		r >= 0x2B05 && r <= 0x2B55, r == 0x3030, r == 0x303D, r == 0x3297, r == 0x3299,
		r >= 0x1FC00 && r <= 0x1FFFD:
		return true
	case r >= 0x1F000 && r <= 0x1FAFF:
		return (r < 0x1F1E6 || r > 0x1F1FF) && (r < 0x1F3FB || r > 0x1F3FF)
	}
	return false
}
