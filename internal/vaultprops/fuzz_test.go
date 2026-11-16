package vaultprops

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// FuzzApply: an empty edit never changes a file, and an edit to a file that
// parses always yields a file that still parses with the new value set and
// the body after the closing fence untouched.
func FuzzApply(f *testing.F) {
	paths, _ := filepath.Glob("testdata/corpus/*.md")
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			f.Add(b, "status")
		}
	}
	f.Add([]byte("---\na: [1,\n  2]\nb: \"x\n  y\"\n---\n"), "a")
	f.Fuzz(func(t *testing.T, data []byte, key string) {
		out, err := Apply(data, nil, nil)
		if err == nil && !bytes.Equal(out, data) {
			t.Fatalf("empty Apply changed the file")
		}
		if _, err := Parse(data); err != nil {
			return
		}
		out, err = Apply(data, map[string]any{key: "fuzz value"}, nil)
		if errors.Is(err, ErrBadKey) || errors.Is(err, ErrUneditable) || errors.Is(err, ErrInvalid) {
			return
		}
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		got, err := Parse(out)
		if err != nil {
			t.Fatalf("edit made the frontmatter invalid: %v\nin:  %q\nout: %q", err, data, out)
		}
		if got[key] != "fuzz value" {
			t.Fatalf("%q = %#v\nin:  %q\nout: %q", key, got[key], data, out)
		}
		if b := locate(data); b.found {
			if ob := locate(out); !ob.found || !bytes.Equal(out[ob.end:], data[b.end:]) {
				t.Fatalf("body changed\nin:  %q\nout: %q", data, out)
			}
		}
	})
}
