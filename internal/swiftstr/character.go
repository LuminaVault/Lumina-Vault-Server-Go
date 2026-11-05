package swiftstr

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// This file is the exact counterpart of Count and Prefix for code whose output
// must match Swift byte for byte (the chunker decides where to cut a document by
// counting Characters, and the cut decides the chunk's hash). It is a port of the
// state machine in the Swift 6.3.3 stdlib (StringGraphemeBreaking.swift,
// _GraphemeBreakingState.shouldBreak) over tables observed from that stdlib, so
// it agrees where Count only approximates: Indic conjuncts (GB9c), Thai and Lao
// AM, the Myanmar marks that are Mc but not SpacingMark. Count and Prefix are left
// alone because modules on main depend on their results.

type charClass uint8

const (
	charAny charClass = iota
	charControl
	charCR
	charLF
	charExtend
	charZWJ
	charRegional
	charPrepend
	charSpacingMark
	charL
	charV
	charT
	charLV
	charLVT
)

type charRange struct {
	lo, hi rune
	class  charClass
}

type runeRange struct{ lo, hi rune }

func classOf(r rune) charClass {
	if r < 0x80 {
		switch {
		case r == '\r':
			return charCR
		case r == '\n':
			return charLF
		case r < 0x20, r == 0x7F:
			return charControl
		}
		return charAny
	}
	if r >= 0xAC00 && r <= 0xD7A3 {
		if (r-0xAC00)%28 == 0 {
			return charLV
		}
		return charLVT
	}
	i := sort.Search(len(charClasses), func(i int) bool { return charClasses[i].hi >= r })
	if i < len(charClasses) && charClasses[i].lo <= r {
		return charClasses[i].class
	}
	return charAny
}

func inRanges(table []runeRange, r rune) bool {
	i := sort.Search(len(table), func(i int) bool { return table[i].hi >= r })
	return i < len(table) && table[i].lo <= r
}

func isExtendedPictographic(r rune) bool {
	return r >= 0xA9 && inRanges(extendedPictographic, r)
}

func isInCBConsonant(r rune) bool {
	return r >= 0x900 && inRanges(incbConsonant, r)
}

func isInCBLinker(r rune) bool {
	switch r {
	case 0x94D, 0x9CD, 0xACD, 0xB4D, 0xC4D, 0xD4D:
		return true
	}
	return false
}

// isInCBExtend assumes r is already Extend or ZWJ, as Swift's does.
func isInCBExtend(r rune) bool {
	return !isInCBConsonant(r) && !isInCBLinker(r) && r != 0x200C
}

// hasBreakWhenPaired is _quickHasGraphemeBreakBetween's table of scalars that
// break against each other whatever surrounds them.
func hasBreakWhenPaired(r rune) bool {
	switch {
	case r >= 0x3400 && r <= 0xA4CF, r <= 0x02FF,
		r >= 0x3041 && r <= 0x3096, r >= 0x30A1 && r <= 0x30FC,
		r >= 0x0400 && r <= 0x0482, r >= 0x061D && r <= 0x064A,
		r >= 0xAC00 && r <= 0xD7AF, r >= 0x2010 && r <= 0x2029,
		r >= 0x3000 && r <= 0x3029, r >= 0xFF01 && r <= 0xFF9D:
		return true
	}
	return false
}

// breakState is _GraphemeBreakingState. A fresh one starts every Character.
type breakState struct {
	seenLinker, inEmoji, inIndic, breakRI bool
}

func (st *breakState) shouldBreak(r1, r2 rune) bool {
	if r1 == '\r' && r2 == '\n' {
		return false
	}
	if hasBreakWhenPaired(r1) && hasBreakWhenPaired(r2) {
		return true
	}
	x := classOf(r1)
	if x == charControl || x == charCR || x == charLF {
		return true
	}
	enterEmoji, enterIndic := false, false
	defer func() {
		st.inEmoji = enterEmoji
		st.inIndic = enterIndic
	}()
	y := classOf(r2)
	switch {
	case x == charAny && y == charAny:
		return true
	case y == charControl || y == charCR || y == charLF:
		return true
	case x == charL && (y == charL || y == charV || y == charLV || y == charLVT):
		return false
	case (x == charLV || x == charV) && (y == charV || y == charT):
		return false
	case (x == charLVT || x == charT) && y == charT:
		return false
	case y == charExtend || y == charZWJ:
		if isExtendedPictographic(r1) || (st.inEmoji && x == charExtend) {
			enterEmoji = true
		}
		consonant, ext, linker := isInCBConsonant(r1), isInCBExtend(r2), isInCBLinker(r2)
		switch {
		case consonant && ext && !linker:
			enterIndic = true
		case consonant && !ext && linker:
			enterIndic = true
			st.seenLinker = true
		case !consonant && ext && !linker:
			enterIndic = st.inIndic
		case !consonant && !ext && linker:
			if st.inIndic {
				enterIndic = true
				st.seenLinker = true
			}
		}
		return false
	case y == charSpacingMark:
		return false
	case x == charPrepend:
		return false
	case x == charZWJ && isExtendedPictographic(r2):
		return !st.inEmoji
	case x == charRegional && y == charRegional:
		r := st.breakRI
		st.breakRI = !st.breakRI
		return r
	default:
		if st.inIndic && st.seenLinker && isInCBConsonant(r2) {
			st.seenLinker = false
			return false
		}
		return true
	}
}

// CharacterLen is the byte length of s's first Character (0 when s is empty).
func CharacterLen(s string) int {
	if s == "" {
		return 0
	}
	var st breakState
	r1, i := utf8.DecodeRuneInString(s)
	for i < len(s) {
		r2, size := utf8.DecodeRuneInString(s[i:])
		if st.shouldBreak(r1, r2) {
			break
		}
		r1 = r2
		i += size
	}
	return i
}

// Characters is Swift's Array(s): the string cut into Characters.
func Characters(s string) []string {
	out := make([]string, 0, len(s))
	for len(s) > 0 {
		n := CharacterLen(s)
		out = append(out, s[:n])
		s = s[n:]
	}
	return out
}

// CharacterCount is Swift's s.count, exactly.
func CharacterCount(s string) int {
	n := 0
	for len(s) > 0 {
		s = s[CharacterLen(s):]
		n++
	}
	return n
}

// CharacterIs is Swift's `c == "x"` for one Character and an ASCII literal. Swift
// compares Characters by canonical equivalence, and exactly three scalars are
// canonically equal to a printable ASCII one: U+037E is ";", U+212A is "K" and
// U+1FEF is the backtick.
func CharacterIs(c string, ascii byte) bool {
	if len(c) == 1 {
		return c[0] == ascii
	}
	switch ascii {
	case '`':
		return c == "`"
	case ';':
		return c == ";"
	case 'K':
		return c == "K"
	}
	return false
}

// HasASCIIPrefix is Swift's s.hasPrefix(prefix) for an ASCII prefix. It compares
// Characters, so "./́x" does not start with "./".
func HasASCIIPrefix(s, prefix string) bool {
	for i := range len(prefix) {
		if s == "" {
			return false
		}
		n := CharacterLen(s)
		if !CharacterIs(s[:n], prefix[i]) {
			return false
		}
		s = s[n:]
	}
	return true
}

// HasASCIISuffix is Swift's s.hasSuffix(suffix) for an ASCII suffix.
func HasASCIISuffix(s, suffix string) bool {
	if len(s) < len(suffix) {
		// Each suffix byte needs at least one byte of s.
		return false
	}
	chars := Characters(s)
	if len(chars) < len(suffix) {
		return false
	}
	tail := chars[len(chars)-len(suffix):]
	for i := range len(suffix) {
		if !CharacterIs(tail[i], suffix[i]) {
			return false
		}
	}
	return true
}

// IsWhitespace reports membership in Foundation's CharacterSet.whitespaces on
// Linux, read from the toolchain: U+0009, U+0020, U+00A0, U+1680, U+2000 to
// U+200B, U+202F, U+205F and U+3000.
func IsWhitespace(r rune) bool {
	switch {
	case r == '\t', r == ' ', r == 0xA0, r == 0x1680, r >= 0x2000 && r <= 0x200B, r == 0x202F, r == 0x205F, r == 0x3000:
		return true
	}
	return false
}

// TrimWhitespaces is Swift's s.trimmingCharacters(in: .whitespaces). It works on
// scalars, not Characters, so it can leave a combining mark at the front.
func TrimWhitespaces(s string) string {
	return strings.TrimFunc(s, IsWhitespace)
}

// SplitLines is Swift's s.components(separatedBy: "\n") on Linux, which matches
// whole Characters: the "\n" of a "\r\n" is part of a different Character and does
// not split. A document with CRLF line endings is therefore one long line. (On
// Apple platforms Foundation splits there; production is Linux.)
func SplitLines(s string) []string {
	lines := make([]string, 0, strings.Count(s, "\n")+1)
	start := 0
	for i := range len(s) {
		if s[i] != '\n' || (i > 0 && s[i-1] == '\r') {
			continue
		}
		lines = append(lines, s[start:i])
		start = i + 1
	}
	return append(lines, s[start:])
}

// ReplaceBackslashes is Swift's s.replacingOccurrences(of: "\\", with: "/") on
// Linux. CoreFoundation's search does not match a backslash followed by a scalar in
// its combining table. That is neither "same Character" (U+0903 joins the Character
// and the backslash is still replaced) nor a current Unicode, so the table is read
// from the toolchain.
func ReplaceBackslashes(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	b := []byte(s)
	for i := range len(b) {
		if b[i] != '\\' {
			continue
		}
		if r, _ := utf8.DecodeRune(b[i+1:]); i+1 < len(b) && inRanges(foundationCombining, r) {
			continue
		}
		b[i] = '/'
	}
	return string(b)
}

// CanonicalKey maps s to a form under which Go's == agrees with Swift's: String
// equality and hashing use canonical equivalence, so "é" and "é" are the
// same Dictionary key.
func CanonicalKey(s string) string {
	return norm.NFC.String(s)
}

// IsLetterOrNumber is Character.isLetter || Character.isNumber: a letter looks at the
// first scalar's Alphabetic property, a number needs a single scalar with a numeric type.
func IsLetterOrNumber(c string) bool {
	runes := []rune(c)
	if len(runes) == 0 {
		return false
	}
	r := runes[0]
	if unicode.IsLetter(r) || unicode.Is(unicode.Other_Alphabetic, r) || unicode.Is(unicode.Nl, r) {
		return true
	}
	return len(runes) == 1 && unicode.IsNumber(r)
}
