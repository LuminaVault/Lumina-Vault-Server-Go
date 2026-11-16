package vaultprops

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Props maps property names to normalized values (spec D2): strings,
// float64 numbers, bools, nil, ISO 8601 date strings (`2026-11-16`, or RFC
// 3339 UTC for a datetime), []any for lists and map[string]any for nested
// objects. It marshals straight to the `properties` jsonb column.
type Props map[string]any

// HasFrontmatter reports whether the file starts with a frontmatter block.
func HasFrontmatter(data []byte) bool { return locate(data).found }

// Parse returns a note's properties. A file without frontmatter has none and
// no error. Invalid frontmatter returns an error wrapping ErrInvalid; its
// message is what `properties_error` stores.
func Parse(data []byte) (Props, error) {
	b := locate(data)
	if !b.found {
		return Props{}, nil
	}
	m, err := root(data[b.start:b.end])
	if err != nil || m == nil {
		return Props{}, err
	}
	budget := maxNodes
	out := make(Props, len(m.Content)/2)
	for i := 0; i+1 < len(m.Content); i += 2 {
		v, err := valueOf(m.Content[i+1], &budget)
		if err != nil {
			return Props{}, err
		}
		out[m.Content[i].Value] = v
	}
	return out, nil
}

func valueOf(n *yaml.Node, budget *int) (any, error) {
	*budget--
	if *budget < 0 {
		return nil, fmt.Errorf("%w: too many values (aliases expand past %d)", ErrInvalid, maxNodes)
	}
	switch n.Kind {
	case yaml.AliasNode:
		if n.Alias == nil {
			return nil, nil
		}
		return valueOf(n.Alias, budget)
	case yaml.SequenceNode:
		list := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := valueOf(c, budget)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		return list, nil
	case yaml.MappingNode:
		obj := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			v, err := valueOf(n.Content[i+1], budget)
			if err != nil {
				return nil, err
			}
			obj[n.Content[i].Value] = v
		}
		return obj, nil
	case yaml.ScalarNode:
		return scalar(n), nil
	default:
		return nil, nil
	}
}

// scalar normalizes one value. Anything that doesn't decode cleanly as its
// resolved tag stays the text as written.
func scalar(n *yaml.Node) any {
	switch n.ShortTag() {
	case "!!null":
		return nil
	case "!!bool":
		var b bool
		if n.Decode(&b) == nil {
			return b
		}
	case "!!int", "!!float":
		var f float64
		if n.Decode(&f) == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
	case "!!timestamp":
		var t time.Time
		if n.Decode(&t) == nil {
			if isDate(n.Value) {
				return t.Format(time.DateOnly)
			}
			return t.UTC().Format(time.RFC3339Nano)
		}
	}
	return n.Value
}

func isDate(s string) bool {
	if len(s) != len(time.DateOnly) {
		return false
	}
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// isDateTime accepts what YAML and Obsidian write for a moment in time.
func isDateTime(s string) bool {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if _, err := time.Parse(layout, s); err == nil {
			return true
		}
	}
	return false
}

// numberText formats a float the way a person would write it.
func numberText(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
