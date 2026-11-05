package chunkindex

import "lumina/internal/swiftstr"

// Heading is one markdown heading. Line is 1-based and points at the line with
// the text: for a setext heading that is the text line, not the underline.
type Heading struct {
	Ordinal int
	Level   int
	Text    string
	Line    int
}

const (
	maxHeadingLevel  = 6
	maxHeadingIndent = 3
)

// ExtractHeadings is MarkdownHeadings.extract. Fenced code is skipped. Two
// headings never share a line: an ATX heading followed by an underline stays one
// heading.
func ExtractHeadings(lines []string) []Heading {
	var headings []Heading
	fence := ""
	ordinal := 0
	previousWasHeading := false

	for index, raw := range lines {
		lineNumber := index + 1
		trimmed := swiftstr.TrimWhitespaces(raw)

		if fence != "" {
			if swiftstr.HasASCIIPrefix(trimmed, fence) {
				fence = ""
			}
			previousWasHeading = false
			continue
		}
		if opened := fenceMarker(trimmed); opened != "" {
			fence = opened
			previousWasHeading = false
			continue
		}

		if level, ok := atxLevel(raw, trimmed); ok {
			ordinal++
			headings = append(headings, Heading{Ordinal: ordinal, Level: level, Text: atxText(trimmed, level), Line: lineNumber})
			previousWasHeading = true
			continue
		}

		// Setext: the underline attaches to the previous line, which must be
		// non-blank and not already claimed by an ATX heading.
		if level := setextLevel(trimmed); level != 0 && index > 0 && !previousWasHeading {
			if candidate := swiftstr.TrimWhitespaces(lines[index-1]); candidate != "" {
				ordinal++
				headings = append(headings, Heading{Ordinal: ordinal, Level: level, Text: candidate, Line: lineNumber - 1})
			}
		}
		previousWasHeading = false
	}
	return headings
}

// HeadingHierarchy is MarkdownHeadings.hierarchy: each ordinal's ancestor path,
// self last. A deeper heading with no parent starts a shallower path.
func HeadingHierarchy(headings []Heading) map[int][]string {
	type entry struct {
		level int
		text  string
	}
	paths := make(map[int][]string, len(headings))
	var stack []entry
	for _, h := range headings {
		for len(stack) > 0 && stack[len(stack)-1].level >= h.Level {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, entry{h.Level, h.Text})
		path := make([]string, len(stack))
		for i, e := range stack {
			path[i] = e.text
		}
		paths[h.Ordinal] = path
	}
	return paths
}

// fenceMarker is the marker when trimmed opens a fenced block, else "".
func fenceMarker(trimmed string) string {
	for _, marker := range []string{"```", "~~~"} {
		if swiftstr.HasASCIIPrefix(trimmed, marker) {
			return marker
		}
	}
	return ""
}

// atxLevel is the ATX level of a line. A tab of indent is not counted (Swift only
// counts spaces), so "\t# x" is a heading.
func atxLevel(raw, trimmed string) (int, bool) {
	indent := 0
	for rest := raw; rest != ""; indent++ {
		n := swiftstr.CharacterLen(rest)
		if !swiftstr.CharacterIs(rest[:n], ' ') {
			break
		}
		rest = rest[n:]
	}
	if indent > maxHeadingIndent || !swiftstr.HasASCIIPrefix(trimmed, "#") {
		return 0, false
	}
	hashes := countHashes(trimmed)
	if hashes > maxHeadingLevel {
		return 0, false
	}
	// `#foo` is not a heading; `#` alone is an empty one.
	rest := trimmed[hashes:]
	if rest == "" {
		return hashes, true
	}
	first := rest[:swiftstr.CharacterLen(rest)]
	if swiftstr.CharacterIs(first, ' ') || swiftstr.CharacterIs(first, '\t') {
		return hashes, true
	}
	return 0, false
}

func countHashes(s string) int {
	hashes := 0
	for s != "" {
		n := swiftstr.CharacterLen(s)
		if !swiftstr.CharacterIs(s[:n], '#') {
			break
		}
		s = s[n:]
		hashes++
	}
	return hashes
}

// atxText strips the markers and any closing run of hashes, which takes the "#"
// of "# C#" with it, as Swift does.
func atxText(trimmed string, level int) string {
	text := swiftstr.TrimWhitespaces(trimmed[level:])
	for swiftstr.HasASCIISuffix(text, "#") {
		text = text[:len(text)-1]
	}
	return swiftstr.TrimWhitespaces(text)
}

// setextLevel is 1 for an underline of "=", 2 for "-", 0 otherwise.
func setextLevel(trimmed string) int {
	if trimmed == "" {
		return 0
	}
	for _, level := range []struct {
		char  byte
		level int
	}{{'=', 1}, {'-', 2}} {
		all := true
		for s := trimmed; s != ""; {
			n := swiftstr.CharacterLen(s)
			if !swiftstr.CharacterIs(s[:n], level.char) {
				all = false
				break
			}
			s = s[n:]
		}
		if all {
			return level.level
		}
	}
	return 0
}
