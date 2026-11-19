package notionimport

import "strings"

// Property is one "Name: value" line of a database row page.
type Property struct {
	Name  string
	Value string
}

// maxPropertyNameLen bounds what RowPage reads as a property name.
const maxPropertyNameLen = 200

// RowPage splits a database row page into the "Property: value" block Notion
// writes under the heading and the page without it. A line belongs to the
// block when its name is one of columns (any name when columns is nil); the
// block ends at the first line that doesn't. rest keeps the heading and the
// body, a blank line between them. A page without a block comes back whole.
func RowPage(body string, columns []string) (props []Property, rest string) {
	known := map[string]bool{}
	for _, c := range columns {
		known[c] = true
	}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	at := 0
	skipBlank := func() {
		for at < len(lines) && strings.TrimSpace(lines[at]) == "" {
			at++
		}
	}
	skipBlank()
	headEnd := 0
	if at < len(lines) && strings.HasPrefix(lines[at], "# ") {
		at++
		headEnd = at
		skipBlank()
	}
	for at < len(lines) {
		name, value, ok := strings.Cut(lines[at], ":")
		if !ok || name == "" || len(name) > maxPropertyNameLen || name != strings.TrimSpace(name) ||
			(value != "" && !strings.HasPrefix(value, " ")) || (columns != nil && !known[name]) {
			break
		}
		props = append(props, Property{Name: name, Value: strings.TrimSpace(value)})
		at++
	}
	if len(props) == 0 {
		return nil, body
	}
	skipBlank()
	head := strings.Join(lines[:headEnd], "\n")
	tail := strings.Join(lines[at:], "\n")
	switch {
	case head == "":
		return props, tail
	case strings.TrimSpace(tail) == "":
		return props, head + "\n"
	}
	return props, head + "\n\n" + tail
}
