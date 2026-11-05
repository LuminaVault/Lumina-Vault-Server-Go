package chunkindex

import "lumina/internal/swiftstr"

// MaxTargetLength is the longest wikilink target accepted, in Characters.
const MaxTargetLength = 512

// ParsedWikilink is a [[wikilink]] as written, before resolution.
type ParsedWikilink struct {
	// Line is 1-based.
	Line int
	// RawTarget is everything between the brackets, trimmed.
	RawTarget string
	// TargetSlug is the target with alias and heading stripped, forward slashes.
	TargetSlug string
	// TargetHeading is the fragment after "#", or nil.
	TargetHeading *string
	// Label is the alias after "|", or nil.
	Label *string
}

// ExtractWikilinks is Wikilinks.extract(from: String). Fenced code and inline code
// spans are skipped. Lines split as SplitLines does, so a CRLF document is one line.
func ExtractWikilinks(source string) []ParsedWikilink {
	return ExtractWikilinksFromLines(swiftstr.SplitLines(source))
}

// ExtractWikilinksFromLines is Wikilinks.extract(from: [String]).
func ExtractWikilinksFromLines(lines []string) []ParsedWikilink {
	links := []ParsedWikilink{}
	fence := ""
	for index, raw := range lines {
		trimmed := swiftstr.TrimWhitespaces(raw)
		if fence != "" {
			if swiftstr.HasASCIIPrefix(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if opened := fenceMarker(trimmed); opened != "" {
			fence = opened
			continue
		}
		links = append(links, extractLine(raw, index+1)...)
	}
	return links
}

// extractLine scans one line for [[...]], tracking backticks. An unterminated
// "[[" ends the line: no further link can start on it.
func extractLine(raw string, line int) []ParsedWikilink {
	var results []ParsedWikilink
	chars := swiftstr.Characters(raw)
	index := 0
	inCode := false
	for index < len(chars) {
		if swiftstr.CharacterIs(chars[index], '`') {
			inCode = !inCode
			index++
			continue
		}
		if inCode || !swiftstr.CharacterIs(chars[index], '[') || index+1 >= len(chars) || !swiftstr.CharacterIs(chars[index+1], '[') {
			index++
			continue
		}
		closing, ok := closingIndex(chars, index+2)
		if !ok {
			break
		}
		inner := ""
		for _, c := range chars[index+2 : closing] {
			inner += c
		}
		if link := ParseWikilink(inner, line); link != nil {
			results = append(results, *link)
		}
		index = closing + 2
	}
	return results
}

// closingIndex finds the "]]" closing a link opened before start. A nested "[["
// means the outer one was never a link.
func closingIndex(chars []string, start int) (int, bool) {
	for index := start; index+1 < len(chars); index++ {
		if swiftstr.CharacterIs(chars[index], ']') && swiftstr.CharacterIs(chars[index+1], ']') {
			return index, true
		}
		if swiftstr.CharacterIs(chars[index], '[') && swiftstr.CharacterIs(chars[index+1], '[') {
			return 0, false
		}
	}
	return 0, false
}

// ParseWikilink is Wikilinks.parse: splits folder/note#Heading|Label. Nil when
// the target is empty, implausibly long, or a same-document anchor ([[#Heading]]).
func ParseWikilink(inner string, line int) *ParsedWikilink {
	raw := swiftstr.TrimWhitespaces(inner)
	if raw == "" || swiftstr.CharacterCount(raw) > MaxTargetLength {
		return nil
	}

	// Alias first: a "|" always separates target from display text, and a heading
	// fragment belongs to the target side.
	targetPart := raw
	var label *string
	if pipe := firstCharacter(raw, '|'); pipe >= 0 {
		targetPart = swiftstr.TrimWhitespaces(raw[:pipe])
		label = nonEmpty(swiftstr.TrimWhitespaces(raw[pipe+1:]))
	}

	var heading *string
	if hash := firstCharacter(targetPart, '#'); hash >= 0 {
		heading = nonEmpty(swiftstr.TrimWhitespaces(targetPart[hash+1:]))
		targetPart = swiftstr.TrimWhitespaces(targetPart[:hash])
	}
	if targetPart == "" {
		return nil
	}
	return &ParsedWikilink{
		Line:          line,
		RawTarget:     raw,
		TargetSlug:    NormalizeSlug(targetPart),
		TargetHeading: heading,
		Label:         label,
	}
}

// firstCharacter is the byte offset of the first Character equal to ascii, or -1.
func firstCharacter(s string, ascii byte) int {
	for offset := 0; offset < len(s); {
		n := swiftstr.CharacterLen(s[offset:])
		if swiftstr.CharacterIs(s[offset:offset+n], ascii) {
			return offset
		}
		offset += n
	}
	return -1
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// NormalizeSlug is Wikilinks.normalizeSlug: forward slashes, no leading "./" or
// "/", no trailing "/", case preserved.
func NormalizeSlug(target string) string {
	slug := swiftstr.ReplaceBackslashes(target)
	for swiftstr.HasASCIIPrefix(slug, "./") {
		slug = slug[2:]
	}
	for swiftstr.HasASCIIPrefix(slug, "/") {
		slug = slug[1:]
	}
	for swiftstr.HasASCIISuffix(slug, "/") {
		slug = slug[:len(slug)-1]
	}
	return slug
}
