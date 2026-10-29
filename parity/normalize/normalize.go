// Package normalize turns a response body into a value two servers can be
// compared on. Random values are replaced with placeholders, but their *form*
// is kept, so a format difference between Swift and Go still shows up as a
// diff. Examples: uppercase vs lowercase UUIDs, whole vs fractional seconds.
package normalize

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	tsRe   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)
)

// Ignored replaces values at ignore paths.
const Ignored = "<ignored>"

// Normalizer numbers UUIDs by first appearance. Use one per server run so the
// same id gets the same placeholder in every step and snapshot of that run.
type Normalizer struct {
	ids map[string]int // keyed by lowercase UUID
}

func New() *Normalizer { return &Normalizer{ids: map[string]int{}} }

// Body is a convenience for a single document with a fresh Normalizer.
func Body(raw []byte, ignore []string) (any, error) {
	return New().Body(raw, ignore)
}

// Body parses raw as JSON (numbers kept exact), blanks the ignore paths, and
// replaces UUIDs and timestamps with placeholders. A body that is not JSON is
// returned as its string.
func (n *Normalizer) Body(raw []byte, ignore []string) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return string(raw), nil //nolint:nilerr // non-JSON bodies compare as text
	}
	for _, p := range ignore {
		steps, err := parsePath(p)
		if err != nil {
			return nil, err
		}
		v = blank(v, steps)
	}
	return n.Value(v), nil
}

// Value normalizes an already-decoded JSON value.
func (n *Normalizer) Value(v any) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic numbering
		out := make(map[string]any, len(t))
		for _, k := range keys {
			out[k] = n.Value(t[k])
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = n.Value(e)
		}
		return out
	case string:
		return n.str(t)
	default:
		return v
	}
}

func (n *Normalizer) str(s string) string {
	if uuidRe.MatchString(s) {
		key := strings.ToLower(s)
		id, ok := n.ids[key]
		if !ok {
			id = len(n.ids) + 1
			n.ids[key] = id
		}
		return fmt.Sprintf("<uuid#%d:%s>", id, letterCase(s))
	}
	if m := tsRe.FindStringSubmatch(s); m != nil {
		class := "iso-s"
		switch {
		case m[1] != "" && m[2] != "Z":
			class = "iso-ms-offset"
		case m[1] != "":
			class = "iso-ms"
		case m[2] != "Z":
			class = "iso-offset"
		}
		return "<ts:" + class + ">"
	}
	return s
}

func letterCase(s string) string {
	hasUpper := strings.ContainsAny(s, "ABCDEF")
	hasLower := strings.ContainsAny(s, "abcdef")
	switch {
	case hasUpper && hasLower:
		return "mixed"
	case hasUpper:
		return "upper"
	case hasLower:
		return "lower"
	default:
		return "nocase"
	}
}

// Get returns the value at a concrete path ($.a.b, $.a[0].b; no wildcard).
func Get(doc any, path string) (any, error) {
	steps, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	cur := doc
	for _, s := range steps {
		switch t := cur.(type) {
		case map[string]any:
			v, ok := t[s.key]
			if s.isIdx || !ok {
				return nil, fmt.Errorf("normalize: %s not found", path)
			}
			cur = v
		case []any:
			if !s.isIdx || s.index < 0 || s.index >= len(t) {
				return nil, fmt.Errorf("normalize: %s not found", path)
			}
			cur = t[s.index]
		default:
			return nil, fmt.Errorf("normalize: %s not found", path)
		}
	}
	return cur, nil
}

// step is one path element: a key, an index, or a wildcard (index -1).
type step struct {
	key   string
	index int
	isIdx bool
}

// parsePath accepts the subset $.a.b, $.a[*].b, $.a[0].b.
func parsePath(p string) ([]step, error) {
	rest, ok := strings.CutPrefix(p, "$")
	if !ok {
		return nil, fmt.Errorf("normalize: path %q must start with $", p)
	}
	var steps []step
	for rest != "" {
		switch rest[0] {
		case '.':
			rest = rest[1:]
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			if end == 0 {
				return nil, fmt.Errorf("normalize: empty key in %q", p)
			}
			steps = append(steps, step{key: rest[:end]})
			rest = rest[end:]
		case '[':
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, fmt.Errorf("normalize: unclosed [ in %q", p)
			}
			inner := rest[1:end]
			if inner == "*" {
				steps = append(steps, step{index: -1, isIdx: true})
			} else {
				i, err := strconv.Atoi(inner)
				if err != nil || i < 0 {
					return nil, fmt.Errorf("normalize: bad index %q in %q", inner, p)
				}
				steps = append(steps, step{index: i, isIdx: true})
			}
			rest = rest[end+1:]
		default:
			return nil, fmt.Errorf("normalize: unexpected %q in %q", rest[0], p)
		}
	}
	return steps, nil
}

func blank(v any, steps []step) any {
	if len(steps) == 0 {
		return Ignored
	}
	s := steps[0]
	switch t := v.(type) {
	case map[string]any:
		if s.isIdx {
			return v
		}
		if child, ok := t[s.key]; ok {
			t[s.key] = blank(child, steps[1:])
		}
	case []any:
		if !s.isIdx {
			return v
		}
		for i := range t {
			if s.index == -1 || s.index == i {
				t[i] = blank(t[i], steps[1:])
			}
		}
	}
	return v
}
