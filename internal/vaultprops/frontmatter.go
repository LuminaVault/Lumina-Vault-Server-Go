// Package vaultprops reads and edits the YAML frontmatter of Markdown notes:
// a note's properties (backlog E8, spec
// docs/superpowers/specs/2026-11-16-e8-properties-and-views.md, D1–D4).
//
// Pure functions, no DB. Edits are byte-stable: a no-op returns the input
// unchanged, and changing one key rewrites only that key's lines. The rest of
// the frontmatter (order, comments, quoting) and the body are left as written.
package vaultprops

import (
	"bytes"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// MaxFrontmatterBytes caps the YAML a note may carry. Larger is treated as
// invalid, which also bounds the cost of a hostile document.
const MaxFrontmatterBytes = 64 << 10

// maxNodes bounds the nodes visited (aliases included) when reading values,
// so `&a [*a, *a, …]` chains cannot blow up.
const maxNodes = 10_000

var (
	// ErrInvalid wraps every reason the frontmatter can't be used: bad YAML,
	// too large, not a mapping, duplicate keys. The file is never rewritten.
	ErrInvalid = errors.New("invalid frontmatter")
	// ErrBadValue is a PATCH value that can't be written as a property.
	ErrBadValue = errors.New("unsupported property value")
	// ErrUneditable is a key whose lines can't be rewritten safely (it
	// defines an anchor other keys use). The file stays valid; edit its text.
	ErrUneditable = errors.New("property can't be edited here")
	// ErrBadKey is a property name that can't be written.
	ErrBadKey = errors.New("invalid property name")
)

// block locates the frontmatter in a file.
type block struct {
	found bool
	// content is data[start:end]: the YAML between the fences.
	start, end int
	// newline is "\r\n" when the opening fence uses it, else "\n".
	newline string
}

// locate finds `---` on the first line and the next line that is exactly
// `---` or `...`. Anything else is not frontmatter.
func locate(data []byte) block {
	var nl string
	switch {
	case bytes.HasPrefix(data, []byte("---\r\n")):
		nl = "\r\n"
	case bytes.HasPrefix(data, []byte("---\n")):
		nl = "\n"
	default:
		return block{}
	}
	start := 3 + len(nl)
	for i := start; i <= len(data); {
		j := bytes.IndexByte(data[i:], '\n')
		lineEnd := len(data)
		next := len(data) + 1
		if j >= 0 {
			lineEnd = i + j
			next = lineEnd + 1
		}
		line := bytes.TrimSuffix(data[i:lineEnd], []byte("\r"))
		if bytes.Equal(line, []byte("---")) || bytes.Equal(line, []byte("...")) {
			return block{found: true, start: start, end: i, newline: nl}
		}
		if j < 0 {
			break
		}
		i = next
	}
	return block{}
}

// root decodes the frontmatter YAML into its top-level mapping, or nil for an
// empty block. Errors wrap ErrInvalid.
func root(content []byte) (*yaml.Node, error) {
	if len(content) > MaxFrontmatterBytes {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrInvalid, MaxFrontmatterBytes)
	}
	// Only YAML whitespace: TrimSpace would also drop \f, which YAML rejects.
	if len(bytes.Trim(content, " \t\r\n")) == 0 {
		return nil, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, nil
	}
	m := doc.Content[0]
	if m.Kind == yaml.ScalarNode && m.ShortTag() == "!!null" {
		return nil, nil
	}
	if m.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: not a mapping of keys to values", ErrInvalid)
	}
	seen := make(map[string]bool, len(m.Content)/2)
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		if k.Kind != yaml.ScalarNode || k.Value == "" || k.ShortTag() == "!!null" {
			return nil, fmt.Errorf("%w: line %d: a key must be plain text", ErrInvalid, k.Line)
		}
		if seen[k.Value] {
			return nil, fmt.Errorf("%w: line %d: %q appears twice", ErrInvalid, k.Line, k.Value)
		}
		seen[k.Value] = true
	}
	// Walk every value once so an alias bomb fails here, for Apply as well.
	budget := maxNodes
	for i := 1; i < len(m.Content); i += 2 {
		if _, err := valueOf(m.Content[i], &budget); err != nil {
			return nil, err
		}
	}
	return m, nil
}
