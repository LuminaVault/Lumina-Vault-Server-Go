package spaces

import (
	"lumina/internal/wire"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Slug rules: SpaceSlugPolicy in Sources/App/Spaces/SpacesService.swift.

var (
	slugPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}$`)
	reservedSlug = map[string]bool{"raw": true, "compiled": true, "trash": true, "_deleted": true, "tmp": true}

	errInvalidSlug = wire.NewError(http.StatusBadRequest, "slug must be 2-31 chars, lowercase a-z/0-9/-, no leading dash")
)

// validateSlug trims, lowercases and checks the pattern and reserved words.
func validateSlug(raw string) (string, error) {
	s := strings.ToLower(trim(raw))
	if !slugPattern.MatchString(s) || reservedSlug[s] {
		return "", errInvalidSlug
	}
	return s, nil
}

// deriveSlug is SpaceSlugPolicy.derive(from:). Swift walks Characters
// (grapheme clusters) and keeps only single-scalar ASCII letters and digits,
// so an ASCII letter carrying a combining mark, variation selector or joiner
// is a separator, like any other non-ASCII character.
func deriveSlug(name string) string {
	var b strings.Builder
	lastDash := false
	rs := []rune(lowerSwift(name))
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		extended := false
		for i+1 < len(rs) && extendsCluster(rs[i+1]) {
			i++
			extended = true
		}
		if !extended && r < utf8.RuneSelf && (isASCIILetter(r) || (r >= '0' && r <= '9')) {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 31 {
		out = out[:31]
	}
	out = strings.Trim(out, "-")
	if len(out) < 2 {
		out = "space"
	}
	if reservedSlug[out] {
		out += "-space"
	}
	return out
}

// resolveSlug: an explicit slug wins; an absent or blank one is derived.
func resolveSlug(slug *string, name string) string {
	if slug != nil {
		if s := trim(*slug); s != "" {
			return s
		}
	}
	return deriveSlug(name)
}

func isASCIILetter(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }

// extendsCluster reports scalars that join the previous one into a single
// Character in Swift: combining marks, variation selectors, ZWJ, emoji
// modifiers and the keycap mark.
func extendsCluster(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Mc) ||
		r == 0x200D || (r >= 0xFE00 && r <= 0xFE0F) || (r >= 0x1F3FB && r <= 0x1F3FF)
}

// lowerSwift lowercases as String.lowercased() does where it matters for
// slugs: U+0130 (İ) becomes "i̇" (i + combining dot), a non-ASCII Character,
// where Go's ToLower gives a plain "i".
func lowerSwift(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "İ", "i̇"))
}

// trim is trimmingCharacters(in: .whitespacesAndNewlines).
func trim(s string) string { return strings.TrimFunc(s, unicode.IsSpace) }
