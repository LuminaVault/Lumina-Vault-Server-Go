package vaultfiles

import (
	"lumina/internal/swiftstr"
	"strings"
)

// maxSlugSegment caps one slugified path segment, in Characters.
const maxSlugSegment = 80

// SlugifySegment is bulk import's segment rule: lowercase, letters, numbers, `.`
// and `_` kept, every other run of Characters one `-`, dashes trimmed, at most 80
// Characters. An empty result is "x", so a segment never vanishes from a path.
func SlugifySegment(s string) string {
	if out := FoldSegment(s); out != "" {
		return out
	}
	return "x"
}

// FoldSegment is SlugifySegment without the "x" stand-in: "" when nothing
// survives. The wikilink resolver folds link targets with it, so [[The Last
// Light]] finds the-last-light.md that bulk import wrote, and a link made only of
// symbols folds to nothing rather than to a file called x.
func FoldSegment(s string) string {
	var out []string
	lastDash := false
	for _, ch := range swiftstr.Characters(Lowercased(s)) {
		if swiftstr.IsLetterOrNumber(ch) || ch == "." || ch == "_" {
			out = append(out, ch)
			lastDash = false
		} else if !lastDash {
			out = append(out, "-")
			lastDash = true
		}
	}
	trimmed := strings.Trim(strings.Join(out, ""), "-")
	if trimmed == "" {
		return ""
	}
	return swiftstr.Prefix(trimmed, maxSlugSegment)
}
