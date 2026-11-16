package vaultprops

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// hasAnchor reports an `&anchor` on n or inside it: rewriting or removing
// it would break the `*alias`es that point at it.
func hasAnchor(n *yaml.Node) bool {
	if n.Anchor != "" {
		return true
	}
	for _, c := range n.Content {
		if hasAnchor(c) {
			return true
		}
	}
	return false
}

// maxKeyLen bounds a property name.
const maxKeyLen = 200

// Apply sets and removes properties in a note's frontmatter, creating the
// block if the note has none. Only the lines of the keys that change are
// rewritten; everything else keeps its bytes. When nothing changes, it
// returns data itself.
//
// set values are JSON-shaped: string, float64 (or any Go number), bool, nil,
// or []any of those. A string that is an ISO date or datetime is written
// unquoted, so YAML (and Obsidian) read it back as a date.
func Apply(data []byte, set map[string]any, unset []string) ([]byte, error) {
	for k := range set {
		if err := checkKey(k); err != nil {
			return nil, err
		}
	}
	for _, k := range unset {
		if err := checkKey(k); err != nil {
			return nil, err
		}
	}
	// set wins over unset for the same key.
	drop := make(map[string]bool, len(unset))
	for _, k := range unset {
		if _, ok := set[k]; !ok {
			drop[k] = true
		}
	}

	b := locate(data)
	if !b.found {
		if len(set) == 0 {
			return data, nil
		}
		var buf bytes.Buffer
		buf.WriteString("---\n")
		for _, k := range sortedKeys(set) {
			entry, err := encodeEntry(k, set[k], "")
			if err != nil {
				return nil, err
			}
			buf.WriteString(entry)
		}
		buf.WriteString("---\n")
		if len(data) > 0 && !bytes.HasPrefix(data, []byte("\n")) {
			buf.WriteString("\n")
		}
		return append(buf.Bytes(), data...), nil
	}

	content := data[b.start:b.end]
	m, err := root(content)
	if err != nil {
		return nil, err
	}
	if m != nil && m.Style&yaml.FlowStyle != 0 {
		return nil, fmt.Errorf("%w: a {flow: mapping} can't be edited key by key", ErrInvalid)
	}

	// YAML counts a lone "\r" as a line break; the line edits below count
	// "\n" only, so their line numbers would disagree.
	if bytes.Contains(bytes.ReplaceAll(content, []byte("\r\n"), nil), []byte("\r")) {
		return nil, fmt.Errorf("%w: the frontmatter uses bare carriage returns", ErrUneditable)
	}
	lines := splitLines(content)
	if m == nil {
		// An empty or comment-only block takes new keys; a bare `~` or
		// `null` would need replacing, which isn't worth guessing at.
		for _, l := range lines {
			t := bytes.TrimLeft(bytes.TrimRight(l, "\r\n"), " \t")
			if len(t) > 0 && t[0] != '#' {
				return nil, fmt.Errorf("%w: the frontmatter holds a single value, not keys", ErrUneditable)
			}
		}
	}
	// Keys may sit indented as a whole; new lines follow the first key.
	indent := ""
	if m != nil && len(m.Content) > 0 && m.Content[0].Column > 1 {
		indent = strings.Repeat(" ", m.Content[0].Column-1)
	}
	type edit struct {
		start, end int // line range [start, end) to replace
		with       string
	}
	var edits []edit
	existing := map[string]bool{}
	if m != nil {
		budget := maxNodes
		for i := 0; i+1 < len(m.Content); i += 2 {
			k, v := m.Content[i], m.Content[i+1]
			existing[k.Value] = true
			newV, setIt := set[k.Value]
			if !setIt && !drop[k.Value] {
				continue
			}
			if hasAnchor(k) || hasAnchor(v) {
				return nil, fmt.Errorf("%w: %q defines a YAML anchor (&) that other keys may use", ErrUneditable, k.Value)
			}
			start := k.Line - 1
			next := len(lines)
			if i+2 < len(m.Content) {
				next = m.Content[i+2].Line - 1
			}
			if start < 0 || next > len(lines) || start >= next {
				return nil, fmt.Errorf("%w: can't find %q's lines", ErrUneditable, k.Value)
			}
			end := blockEnd(lines, start, next)
			if drop[k.Value] {
				edits = append(edits, edit{start, end, ""})
				continue
			}
			old, err := valueOf(v, &budget)
			if err != nil {
				return nil, err
			}
			want, err := normalizeInput(newV)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k.Value, err)
			}
			if reflect.DeepEqual(old, want) {
				continue
			}
			entry, err := encodeEntry(k.Value, newV, v.LineComment)
			if err != nil {
				return nil, err
			}
			edits = append(edits, edit{start, end, indentLines(entry, indent)})
		}
	}

	var added strings.Builder
	for _, k := range sortedKeys(set) {
		if existing[k] {
			continue
		}
		entry, err := encodeEntry(k, set[k], "")
		if err != nil {
			return nil, err
		}
		added.WriteString(indentLines(entry, indent))
	}
	if len(edits) == 0 && added.Len() == 0 {
		return data, nil
	}

	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	for _, e := range edits {
		repl := splitLines([]byte(convertNewlines(e.with, b.newline)))
		lines = append(lines[:e.start], append(repl, lines[e.end:]...)...)
	}
	if added.Len() > 0 {
		if n := len(lines); n > 0 && !bytes.HasSuffix(lines[n-1], []byte("\n")) {
			lines[n-1] = append(append([]byte{}, lines[n-1]...), b.newline...)
		}
		lines = append(lines, splitLines([]byte(convertNewlines(added.String(), b.newline)))...)
	}

	out := make([]byte, 0, len(data)+added.Len())
	out = append(out, data[:b.start]...)
	out = append(out, bytes.Join(lines, nil)...)
	out = append(out, data[b.end:]...)
	if err := verify(data, out, set, drop); err != nil {
		return nil, err
	}
	return out, nil
}

// verify is the safety net for line edits on unusual YAML: the result must
// parse, carry the new values, drop the removed keys and leave every other
// key as it was. Otherwise nothing is written.
func verify(before, after []byte, set map[string]any, drop map[string]bool) error {
	old, err := Parse(before)
	if err != nil {
		return err
	}
	got, err := Parse(after)
	if err != nil {
		return fmt.Errorf("%w: this frontmatter's layout can't be edited key by key", ErrUneditable)
	}
	for k, v := range old {
		if _, changed := set[k]; changed || drop[k] {
			continue
		}
		if !reflect.DeepEqual(got[k], v) {
			return fmt.Errorf("%w: editing would change %q", ErrUneditable, k)
		}
	}
	for k, v := range set {
		want, err := normalizeInput(v)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got[k], want) {
			return fmt.Errorf("%w: %q did not take its new value", ErrUneditable, k)
		}
	}
	for k := range drop {
		if _, still := got[k]; still {
			return fmt.Errorf("%w: %q could not be removed", ErrUneditable, k)
		}
	}
	if len(got) != len(old)+countNew(old, set)-countPresent(old, drop) {
		return fmt.Errorf("%w: editing would add or lose keys", ErrUneditable)
	}
	return nil
}

func countNew(old Props, set map[string]any) int {
	n := 0
	for k := range set {
		if _, ok := old[k]; !ok {
			n++
		}
	}
	return n
}

func countPresent(old Props, drop map[string]bool) int {
	n := 0
	for k := range drop {
		if _, ok := old[k]; ok {
			n++
		}
	}
	return n
}

func checkKey(k string) error {
	if k == "" || len(k) > maxKeyLen || strings.ContainsAny(k, "\n\r\x00") || !utf8.ValidString(k) {
		return fmt.Errorf("%w: %q", ErrBadKey, k)
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// splitLines splits after each "\n", keeping it.
func splitLines(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			out = append(out, b)
			break
		}
		out = append(out, b[:i+1])
		b = b[i+1:]
	}
	return out
}

// blockEnd is the line after a top-level key's value: everything up to the
// next top-level key (line `next`, 0-based; len(lines) for the last key),
// minus the blank and flush-left `#` lines just above it, which belong to
// the next key or separate sections. Neither can be part of the value: a
// block scalar's lines are indented, and a `#` line is a comment anywhere.
func blockEnd(lines [][]byte, start, next int) int {
	end := next
	for end > start+1 {
		l := bytes.TrimRight(lines[end-1], "\r\n")
		if len(bytes.TrimSpace(l)) != 0 && l[0] != '#' {
			break
		}
		end--
	}
	return end
}

func indentLines(s, indent string) string {
	if indent == "" {
		return s
	}
	parts := strings.SplitAfter(s, "\n")
	for i, p := range parts {
		if p != "" {
			parts[i] = indent + p
		}
	}
	return strings.Join(parts, "")
}

func convertNewlines(s, nl string) string {
	if nl == "\n" {
		return s
	}
	return strings.ReplaceAll(s, "\n", nl)
}

// encodeEntry renders `key: value\n` (a list on following lines), keeping a
// trailing `# comment` from the old value when the new one is a single line.
func encodeEntry(key string, value any, comment string) (string, error) {
	vn, err := toNode(value)
	if err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	kn := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	if needsQuoting(key) {
		kn.Style = yaml.DoubleQuotedStyle
	}
	if value == nil {
		var kb bytes.Buffer
		enc := yaml.NewEncoder(&kb)
		if err := enc.Encode(kn); err != nil {
			return "", err
		}
		_ = enc.Close()
		return strings.TrimSuffix(kb.String(), "\n") + ":" + suffixComment(comment) + "\n", nil
	}
	doc := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{kn, vn}}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	out := buf.String()
	if comment != "" && strings.Count(out, "\n") == 1 {
		out = strings.TrimSuffix(out, "\n") + suffixComment(comment) + "\n"
	}
	return out, nil
}

func suffixComment(c string) string {
	if c == "" {
		return ""
	}
	return " " + c
}

// needsQuoting: a key that YAML would read as something other than the same
// plain string.
func needsQuoting(k string) bool {
	var n yaml.Node
	if yaml.Unmarshal([]byte(k+": x"), &n) != nil || len(n.Content) == 0 {
		return true
	}
	m := n.Content[0]
	return m.Kind != yaml.MappingNode || len(m.Content) != 2 || m.Content[0].Value != k ||
		m.Content[0].ShortTag() != "!!str"
}

// toNode builds the YAML for one PATCH value.
func toNode(v any) (*yaml.Node, error) {
	switch x := v.(type) {
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	case string:
		if !utf8.ValidString(x) {
			return nil, fmt.Errorf("%w: text is not UTF-8", ErrBadValue)
		}
		if isDate(x) || isDateTime(x) && !strings.Contains(x, " ") {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!timestamp", Value: x}, nil
		}
		n := &yaml.Node{}
		return n, n.Encode(x)
	case bool:
		n := &yaml.Node{}
		return n, n.Encode(x)
	case []any:
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, item := range x {
			if _, nested := item.([]any); nested {
				return nil, fmt.Errorf("%w: a list inside a list", ErrBadValue)
			}
			if _, nested := item.(map[string]any); nested {
				return nil, fmt.Errorf("%w: an object inside a list", ErrBadValue)
			}
			c, err := toNode(item)
			if err != nil {
				return nil, err
			}
			seq.Content = append(seq.Content, c)
		}
		return seq, nil
	case []string:
		items := make([]any, len(x))
		for i, s := range x {
			items[i] = s
		}
		return toNode(items)
	case map[string]any:
		return nil, fmt.Errorf("%w: objects can't be set as a property", ErrBadValue)
	}
	if f, ok := asFloat(v); ok {
		// No tag: the encoder writes it plain, and it reads back as a number.
		return &yaml.Node{Kind: yaml.ScalarNode, Value: numberText(f)}, nil
	}
	return nil, fmt.Errorf("%w: %T", ErrBadValue, v)
}

// normalizeInput turns a PATCH value into what Parse would read back, to
// tell a no-op from a change.
func normalizeInput(v any) (any, error) {
	n, err := toNode(v)
	if err != nil {
		return nil, err
	}
	budget := maxNodes
	return valueOf(n, &budget)
}
