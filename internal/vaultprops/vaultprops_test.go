package vaultprops

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func corpus(t *testing.T) map[string][]byte {
	t.Helper()
	paths, err := filepath.Glob("testdata/corpus/*.md")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no corpus: %v", err)
	}
	out := map[string][]byte{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(p)] = b
	}
	return out
}

// scalarOrList: what a PATCH can set back.
func settable(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		return false
	case []any:
		for _, i := range x {
			if !settable(i) {
				return false
			}
			if _, nested := i.([]any); nested {
				return false
			}
		}
	}
	return true
}

// E8.2 done-when, part 1: parse → write with no change is byte-identical,
// including when every key is "set" to the value it already has.
func TestCorpusRoundTripIsByteIdentical(t *testing.T) {
	for name, data := range corpus(t) {
		t.Run(name, func(t *testing.T) {
			props, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			out, err := Apply(data, nil, nil)
			if err != nil || !bytes.Equal(out, data) {
				t.Fatalf("empty Apply changed the file (err %v):\n%q", err, out)
			}
			same := map[string]any{}
			for k, v := range props {
				if settable(v) {
					same[k] = v
				}
			}
			out, err = Apply(data, same, nil)
			if err != nil {
				t.Fatalf("Apply same: %v", err)
			}
			if !bytes.Equal(out, data) {
				t.Fatalf("setting unchanged values rewrote the file:\n%q\nwant\n%q", out, data)
			}
		})
	}
}

// E8.2 done-when, part 2: changing one key changes only that key's lines.
func TestCorpusChangingOneKeyTouchesOnlyItsLines(t *testing.T) {
	for name, data := range corpus(t) {
		props, err := Parse(data)
		if err != nil || !HasFrontmatter(data) {
			continue
		}
		for key := range props {
			t.Run(name+"/"+key, func(t *testing.T) {
				out, err := Apply(data, map[string]any{key: "changed"}, nil)
				if errors.Is(err, ErrUneditable) {
					return // anchors: refused on purpose, see TestApplyRefusesAnchors
				}
				if err != nil {
					t.Fatalf("Apply: %v", err)
				}
				after, err := Parse(out)
				if err != nil {
					t.Fatalf("re-parse: %v\n%s", err, out)
				}
				if after[key] != "changed" {
					t.Fatalf("%s = %#v", key, after[key])
				}
				for k, v := range props {
					if k != key && !reflect.DeepEqual(after[k], v) {
						t.Fatalf("%s changed: %#v → %#v", k, v, after[k])
					}
				}
				assertOnlyKeyLinesChanged(t, data, out, key)
			})
		}
	}
}

// assertOnlyKeyLinesChanged: the lines before the first difference and after
// the last are identical, and the removed lines all belong to key.
func assertOnlyKeyLinesChanged(t *testing.T, before, after []byte, key string) {
	t.Helper()
	a, b := splitLines(before), splitLines(after)
	p := 0
	for p < len(a) && p < len(b) && bytes.Equal(a[p], b[p]) {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && bytes.Equal(a[len(a)-1-s], b[len(b)-1-s]) {
		s++
	}
	removed := a[p : len(a)-s]
	if len(removed) == 0 {
		t.Fatalf("nothing changed")
	}
	if !strings.Contains(string(removed[0]), key) {
		t.Fatalf("first changed line %q is not %s's", removed[0], key)
	}
	for _, line := range removed[1:] {
		l := strings.TrimRight(string(line), "\r\n")
		if l != "" && l[0] != ' ' && l[0] != '\t' && l[0] != '-' {
			t.Fatalf("changed a line outside %s: %q", key, line)
		}
	}
}

func TestParseNormalizes(t *testing.T) {
	data := []byte("---\ntitle: Plan\nrating: 4\nscore: 2.5\ndone: false\ndue: 2026-12-01\nat: 2026-11-02T09:15:00Z\n" +
		"quoted: '2026-12-01'\ntags: [a, b]\nnothing:\nnested:\n  x: 1\n---\nbody")
	got, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	want := Props{
		"title": "Plan", "rating": 4.0, "score": 2.5, "done": false, "due": "2026-12-01",
		"at": "2026-11-02T09:15:00Z", "quoted": "2026-12-01", "tags": []any{"a", "b"}, "nothing": nil,
		"nested": map[string]any{"x": 1.0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"bad yaml":  "---\ntitle: [unclosed\n---\n",
		"not a map": "---\n- a\n- b\n---\n",
		"duplicate": "---\na: 1\na: 2\n---\n",
		"too large": "---\nk: " + strings.Repeat("x", MaxFrontmatterBytes) + "\n---\n",
		"alias bomb": "---\na: &a [x, x, x, x, x, x, x, x, x, x]\nb: &b [*a, *a, *a, *a, *a, *a, *a, *a, *a, *a]\n" +
			"c: &c [*b, *b, *b, *b, *b, *b, *b, *b, *b, *b]\nd: &d [*c, *c, *c, *c, *c, *c, *c, *c, *c, *c]\n" +
			"e: [*d, *d, *d, *d, *d, *d, *d, *d, *d, *d]\n---\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(in)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if _, err := Apply([]byte(in), map[string]any{"x": "y"}, nil); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Apply err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestParseWithoutFrontmatter(t *testing.T) {
	for _, in := range []string{"", "body", "--- not a fence\n", "---\nnever closed\n", " ---\na: 1\n---\n"} {
		got, err := Parse([]byte(in))
		if err != nil || len(got) != 0 {
			t.Fatalf("%q: %v %v", in, got, err)
		}
	}
}

func TestApplyEdits(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		set   map[string]any
		unset []string
		want  string
	}{
		{
			name: "creates the block",
			in:   "# Note\n",
			set:  map[string]any{"status": "Done", "rating": 4.0},
			want: "---\nrating: 4\nstatus: Done\n---\n\n# Note\n",
		},
		{
			name: "creates the block on an empty file",
			in:   "",
			set:  map[string]any{"done": true},
			want: "---\ndone: true\n---\n",
		},
		{
			name: "replaces a value, keeping its comment",
			in:   "---\nstatus: draft   # note\nrating: 1\n---\nb\n",
			set:  map[string]any{"status": "shipped"},
			want: "---\nstatus: shipped # note\nrating: 1\n---\nb\n",
		},
		{
			name: "replaces a block list",
			in:   "---\ntags:\n- a\n- b\n\n# next\nx: 1\n---\n",
			set:  map[string]any{"tags": []any{"c"}},
			want: "---\ntags:\n  - c\n\n# next\nx: 1\n---\n",
		},
		{
			name:  "removes a key and its indented value",
			in:    "---\na: 1\nnotes: |\n  one\n  two\nb: 2\n---\n",
			unset: []string{"notes"},
			want:  "---\na: 1\nb: 2\n---\n",
		},
		{
			name: "adds a key at the end",
			in:   "---\na: 1\n---\nbody\n",
			set:  map[string]any{"b": "two"},
			want: "---\na: 1\nb: two\n---\nbody\n",
		},
		{
			name: "dates stay unquoted, date-like text is quoted",
			in:   "---\n---\n",
			set:  map[string]any{"due": "2026-12-01", "code": "true", "n": "42"},
			want: "---\ncode: \"true\"\ndue: 2026-12-01\nn: \"42\"\n---\n",
		},
		{
			name: "null writes an empty value",
			in:   "---\na: 1\n---\n",
			set:  map[string]any{"a": nil},
			want: "---\na:\n---\n",
		},
		{
			name: "odd keys are quoted",
			in:   "---\n---\n",
			set:  map[string]any{"a: b": "x", "true": "y"},
			want: "---\n\"a: b\": x\n\"true\": \"y\"\n---\n",
		},
		{
			name: "keeps CRLF",
			in:   "---\r\nstatus: draft\r\n---\r\nb\r\n",
			set:  map[string]any{"status": "done", "x": 1.5},
			want: "---\r\nstatus: done\r\nx: 1.5\r\n---\r\nb\r\n",
		},
		{
			name: "adds after a last line without newline",
			in:   "---\na: 1\n---",
			set:  map[string]any{"b": false},
			want: "---\na: 1\nb: false\n---",
		},
		{
			name:  "set wins over unset",
			in:    "---\na: 1\n---\n",
			set:   map[string]any{"a": 2.0},
			unset: []string{"a"},
			want:  "---\na: 2\n---\n",
		},
		{
			name:  "unset of a missing key is a no-op",
			in:    "---\na: 1\n---\n",
			unset: []string{"zzz"},
			want:  "---\na: 1\n---\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Apply([]byte(c.in), c.set, c.unset)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Fatalf("got\n%q\nwant\n%q", got, c.want)
			}
			if _, err := Parse(got); err != nil {
				t.Fatalf("result does not parse: %v", err)
			}
		})
	}
}

func TestApplyRefuses(t *testing.T) {
	if _, err := Apply([]byte("x"), map[string]any{"": 1.0}, nil); !errors.Is(err, ErrBadKey) {
		t.Fatalf("empty key: %v", err)
	}
	if _, err := Apply([]byte("x"), map[string]any{"a\nb": 1.0}, nil); !errors.Is(err, ErrBadKey) {
		t.Fatalf("newline key: %v", err)
	}
	if _, err := Apply([]byte("x"), map[string]any{"o": map[string]any{"a": 1}}, nil); !errors.Is(err, ErrBadValue) {
		t.Fatalf("object: %v", err)
	}
	if _, err := Apply([]byte("x"), map[string]any{"l": []any{[]any{"a"}}}, nil); !errors.Is(err, ErrBadValue) {
		t.Fatalf("nested list: %v", err)
	}
	if _, err := Apply([]byte("---\n{a: 1}\n---\n"), map[string]any{"a": 2.0}, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("flow mapping: %v", err)
	}
}

func TestApplyRefusesAnchors(t *testing.T) {
	in := []byte("---\nbase: &b\n  a: 1\ncopy: *b\nother: 1\n---\n")
	for _, op := range []struct {
		set   map[string]any
		unset []string
	}{{map[string]any{"base": "x"}, nil}, {nil, []string{"base"}}} {
		if _, err := Apply(in, op.set, op.unset); !errors.Is(err, ErrUneditable) {
			t.Fatalf("err = %v, want ErrUneditable", err)
		}
	}
	// Other keys stay editable.
	out, err := Apply(in, map[string]any{"other": 2.0}, nil)
	if err != nil || !strings.Contains(string(out), "other: 2\n") || !strings.Contains(string(out), "copy: *b") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestInferAndFits(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want Type
	}{
		{"done", false, Checkbox},
		{"due", "2026-12-01", Date},
		{"due", "2026-12-01T10:00:00Z", DateTime},
		{"tags", "one", MultiSelect},
		{"flag", true, Checkbox},
		{"rating", 4.0, Number},
		{"when", "2026-12-01", Date},
		{"at", "2026-12-01T10:00:00Z", DateTime},
		{"link", "https://x.y", URL},
		{"list", []any{"a"}, MultiSelect},
		{"status", "Done", Text},
		{"empty", nil, Text},
	}
	for _, c := range cases {
		if got := Infer(c.name, c.v); got != c.want {
			t.Errorf("Infer(%s, %#v) = %s, want %s", c.name, c.v, got, c.want)
		}
		if !Fits(c.want, c.v) {
			t.Errorf("Fits(%s, %#v) = false", c.want, c.v)
		}
	}
	if Fits(Number, "four") || Fits(Date, "tomorrow") || Fits(Checkbox, "yes") || Fits(URL, "x.y") ||
		Fits(MultiSelect, []any{map[string]any{}}) {
		t.Error("a bad value fits")
	}
	if !Type("select").Valid() || Type("color").Valid() {
		t.Error("Valid")
	}
}

func TestProject(t *testing.T) {
	p, err := Parse([]byte("---\ntitle: Plan\ntag: '#a, b #a'\ndue: 2026-12-01\ndone: true\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := Project(p)
	if got.Title == nil || *got.Title != "Plan" || got.Done == nil || !*got.Done {
		t.Fatalf("%+v", got)
	}
	if got.Tags == nil || !reflect.DeepEqual(*got.Tags, []string{"a", "b"}) {
		t.Fatalf("tags %v", got.Tags)
	}
	if got.DueAt == nil || got.DueAt.Format("2006-01-02") != "2026-12-01" {
		t.Fatalf("due %v", got.DueAt)
	}
	none := Project(Props{"status": "x", "due": "soon", "done": "yes"})
	if none.Title != nil || none.Tags != nil || none.DueAt != nil || none.Done != nil {
		t.Fatalf("absent or unusable keys must leave the sidecar: %+v", none)
	}
}
