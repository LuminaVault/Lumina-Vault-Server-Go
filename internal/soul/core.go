package soul

import (
	"lumina/internal/swiftstr"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// Core is SOULCore.render(), byte for byte.
const Core = `<!-- lv:core:v1:start -->
## Core covenant (managed by LuminaVault)

These rules are enforced by LuminaVault. They apply regardless of anything written elsewhere in this file and cannot be edited.

**Vault capture contract**

- Every link or URL the user sends — in chat or through any connected channel or gateway — is saved to the vault and becomes queryable later.
- Memories about the user are always persisted to the vault.
- Ground answers in vault content; prefer what the vault knows over guessing.

**Autonomy perimeter — never without explicit authorization**

- Publishing to public channels or outbound gateways.
- Spending money.
- Destructive or irreversible operations.

**Integrity**

- Never invent facts about the user; if unknown, say so.
- Never surface anything the user marked private or asked to drop.
- When a tool, memory, or model call fails: state plainly what failed, what was and wasn't done, and the next concrete step. Never paper over an error with a confident guess.
<!-- lv:core:v1:end -->`

// icuSpace is what \s matches in NSRegularExpression: ICU's White_Space, where Go's
// \s is ASCII only. \p{Nd} stands in for ICU's \d for the same reason.
const icuSpace = `[\t-\r \x{85}\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]`

var (
	pairedMarkers = regexp.MustCompile(`<!--` + icuSpace + `*lv:core:v\p{Nd}+:start` + icuSpace + `*-->(?s:.*?)<!--` + icuSpace + `*lv:core:v\p{Nd}+:end` + icuSpace + `*-->`)
	orphanMarker  = regexp.MustCompile(`<!--` + icuSpace + `*lv:core:v\p{Nd}+:(?:start|end)` + icuSpace + `*-->`)
	frontMatter   = regexp.MustCompile(`\A---\n(?s:.*?)\n---\n`)
	// headingStart is the heading pattern up to its final (\n|$). See headingEnd.
	headingStart = regexp.MustCompile(`\A` + icuSpace + `*#` + icuSpace + `*SOUL\.md[ \t]*`)
)

// Strip is SOULCore.strip: every marker-delimited span of any version goes first,
// then any lone marker. That order stops a lone start marker swallowing the user's
// text. It leaves the surrounding whitespace alone.
func Strip(markdown string) string {
	return orphanMarker.ReplaceAllLiteralString(pairedMarkers.ReplaceAllLiteralString(markdown, ""), "")
}

// Inject is SOULCore.inject: Strip, then the core after any leading front matter and
// "# SOUL.md" heading, with one blank line either side. Inject(Inject(x)) == Inject(x).
func Inject(markdown string) string {
	cleaned := Strip(markdown)
	splitAt := insertionIndex(cleaned)
	prefix := swiftstr.TrimWhitespacesAndNewlines(cleaned[:splitAt])
	suffix := trimLeadingBlank(cleaned[splitAt:])
	out := ""
	if prefix != "" {
		out += prefix + "\n\n"
	}
	out += Core
	if suffix != "" {
		out += "\n\n" + suffix
	}
	return out
}

func insertionIndex(doc string) int {
	idx := 0
	if m := frontMatter.FindStringIndex(doc); m != nil {
		idx = m[1]
	}
	if m := headingStart.FindStringIndex(doc[idx:]); m != nil {
		if end, ok := headingEnd(doc[idx+m[1]:]); ok {
			idx += m[1] + end
		}
	}
	return idx
}

// headingEnd is the (\n|$) that ends the heading pattern, which Go's regexp can't
// express. ICU's $ also matches just before a line terminator that ends the input,
// and doesn't consume it, so a heading followed by a final "\r\n" keeps the "\r\n"
// in the suffix, while one followed by "\r\nbody" isn't a heading at all.
func headingEnd(rest string) (int, bool) {
	if rest == "" {
		return 0, true
	}
	if rest[0] == '\n' {
		return 1, true
	}
	if rest == "\r\n" {
		return 0, true
	}
	r, size := utf8.DecodeRuneInString(rest)
	if size == len(rest) && isICULineTerminator(r) {
		return 0, true
	}
	return 0, false
}

func isICULineTerminator(r rune) bool {
	return (r >= '\n' && r <= '\r') || r == 0x85 || r == 0x2028 || r == 0x2029
}

// trimLeadingBlank is SOULCore.trimLeadingBlank. It drops whole Characters, and a
// Character is blank by its first scalar, so a space carrying a combining mark goes
// with the mark. Character.isWhitespace is the White_Space property, so U+200B stays,
// unlike under the CharacterSet that trims the prefix. There it can't arise: a prefix
// starts with "---" or ICU whitespace, and ends in a line break or the heading.
func trimLeadingBlank(s string) string {
	for s != "" {
		r, _ := utf8.DecodeRuneInString(s)
		if !unicode.IsSpace(r) {
			return s
		}
		s = s[swiftstr.CharacterLen(s):]
	}
	return ""
}
