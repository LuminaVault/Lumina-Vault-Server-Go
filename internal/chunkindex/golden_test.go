package chunkindex

import (
	"encoding/json"
	"lumina/internal/swiftstr"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goldenCase is one input and what Swift's own Indexing sources answered on Linux
// (Swift 6.3.3, tools/swiftgolden). Nothing here is computed by reading Swift's
// algorithm: run.sh compiles and runs the sources. Set LUMINA_SWIFT_GOLDEN to run a
// larger file made with `gen_cases.py --fuzz N` and run.sh's cases argument.
type goldenCase struct {
	Case struct {
		Op    string   `json:"op"`
		S     *string  `json:"s"`
		Max   *int     `json:"max"`
		Ov    *int     `json:"ov"`
		T     string   `json:"t"`
		D     string   `json:"d"`
		O     int      `json:"o"`
		H     string   `json:"h"`
		Links []string `json:"links"`
		Cands []string `json:"cands"`
	} `json:"case"`
	Want struct {
		Chunks []struct {
			Ordinal     int      `json:"ordinal"`
			HeadingPath []string `json:"headingPath"`
			StartLine   int      `json:"startLine"`
			EndLine     int      `json:"endLine"`
			Text        string   `json:"text"`
			SHA         string   `json:"sha"`
			ChunkID     string   `json:"chunkID"`
		} `json:"chunks"`
		Links []struct {
			Line          int     `json:"line"`
			RawTarget     string  `json:"rawTarget"`
			TargetSlug    string  `json:"targetSlug"`
			TargetHeading *string `json:"targetHeading"`
			Label         *string `json:"label"`
		} `json:"links"`
		Str *string `json:"str"`
		Res []struct {
			State  string `json:"state"`
			Target *int   `json:"target"`
		} `json:"res"`
		Headings []struct {
			Ordinal int      `json:"ordinal"`
			Level   int      `json:"level"`
			Text    string   `json:"text"`
			Line    int      `json:"line"`
			Path    []string `json:"path"`
		} `json:"headings"`
		Ints []int    `json:"ints"`
		Strs []string `json:"strs"`
	} `json:"want"`
}

const goldenTenant = "11111111-2222-3333-4444-555555555555"

func loadGolden(t *testing.T) []goldenCase {
	t.Helper()
	path := os.Getenv("LUMINA_SWIFT_GOLDEN")
	if path == "" {
		path = "testdata/swift_golden.json"
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var cases []goldenCase
	require.NoError(t, json.Unmarshal(raw, &cases))
	require.NotEmpty(t, cases)
	return cases
}

func TestSwiftGolden(t *testing.T) {
	seen := map[string]int{}
	failures := 0
	for i, c := range loadGolden(t) {
		seen[c.Case.Op]++
		if !checkGolden(t, c) {
			failures++
			if failures > 20 {
				t.Fatalf("stopping after %d failures (case %d)", failures, i)
			}
		}
	}
	if os.Getenv("LUMINA_SWIFT_GOLDEN") != "" {
		return // a fuzz file has no curated ops
	}
	// The file proves nothing if a regeneration dropped an op.
	for _, op := range []string{"sha", "docid", "docid-memory", "chunkid", "lines", "count", "trim", "norm", "slug", "stem", "resolve", "links", "headings", "chunk"} {
		assert.Positive(t, seen[op], op)
	}
}

func checkGolden(t *testing.T, g goldenCase) bool {
	t.Helper()
	c, want := g.Case, g.Want
	s := ""
	if c.S != nil {
		s = *c.S
	}
	switch c.Op {
	case "chunk":
		return checkGoldenChunks(t, g, s)
	case "links":
		got := ExtractWikilinks(s)
		ok := assert.Lenf(t, got, len(want.Links), "links %q", s)
		for i := 0; ok && i < len(got); i++ {
			w := want.Links[i]
			ok = assert.Equalf(t, w.Line, got[i].Line, "line %q", s) &&
				assert.Equalf(t, w.RawTarget, got[i].RawTarget, "raw %q", s) &&
				assert.Equalf(t, w.TargetSlug, got[i].TargetSlug, "slug %q", s) &&
				assert.Equalf(t, w.TargetHeading, got[i].TargetHeading, "heading %q", s) &&
				assert.Equalf(t, w.Label, got[i].Label, "label %q", s)
		}
		return ok
	case "headings":
		got := ExtractHeadings(swiftstr.SplitLines(s))
		paths := HeadingHierarchy(got)
		ok := assert.Lenf(t, got, len(want.Headings), "headings %q", s)
		for i := 0; ok && i < len(got); i++ {
			w := want.Headings[i]
			ok = assert.Equalf(t, w.Ordinal, got[i].Ordinal, "ordinal %q", s) &&
				assert.Equalf(t, w.Level, got[i].Level, "level %q", s) &&
				assert.Equalf(t, w.Text, got[i].Text, "text %q", s) &&
				assert.Equalf(t, w.Line, got[i].Line, "line %q", s) &&
				assert.Equalf(t, w.Path, paths[got[i].Ordinal], "path %q", s)
		}
		return ok
	case "docid":
		return assert.Equalf(t, *want.Str, DocumentID(uuid.MustParse(c.T), s), "docid %q", s)
	case "docid-memory":
		return assert.Equal(t, *want.Str, DocumentID(uuid.MustParse(c.T), swiftUUIDString(uuid.MustParse(s))))
	case "chunkid":
		return assert.Equal(t, *want.Str, ChunkID(c.D, c.O, c.H))
	case "norm":
		return assert.Equalf(t, *want.Str, NormalizePath(s), "norm %q", s)
	case "slug":
		return assert.Equalf(t, *want.Str, NormalizeSlug(s), "slug %q", s)
	case "stem":
		return assert.Equalf(t, *want.Str, LinkStem(s), "stem %q", s)
	case "sha":
		return assert.Equal(t, *want.Str, SHA256Hex(s))
	case "count":
		return assert.Equalf(t, want.Ints[0], swiftstr.CharacterCount(s), "count %q", s) &&
			assert.Equalf(t, want.Strs, swiftstr.Characters(s), "characters %q", s)
	case "lines":
		return assert.Equalf(t, want.Strs, swiftstr.SplitLines(s), "lines %q", s)
	case "trim":
		return assert.Equalf(t, *want.Str, swiftstr.TrimWhitespaces(s), "trim %q", s)
	case "resolve":
		return checkGoldenResolve(t, g)
	}
	t.Fatalf("unknown op %q", c.Op)
	return false
}

func checkGoldenChunks(t *testing.T, g goldenCase, s string) bool {
	t.Helper()
	var got []DocumentChunk
	if g.Case.Max != nil {
		got = ChunkSized(s, *g.Case.Max, *g.Case.Ov)
	} else {
		got = Chunk(s)
	}
	doc := DocumentID(uuid.MustParse(goldenTenant), "golden.md")
	ok := assert.Lenf(t, got, len(g.Want.Chunks), "chunks of %q", s)
	for i := 0; ok && i < len(got); i++ {
		w := g.Want.Chunks[i]
		path := w.HeadingPath
		ok = assert.Equalf(t, w.ChunkID, ChunkID(doc, got[i].Ordinal, got[i].ContentSHA256), "chunk id %d of %q", i, s) &&
			assert.Equalf(t, w.Ordinal, got[i].Ordinal, "ordinal of %q", s) &&
			assert.Equalf(t, path, got[i].HeadingPath, "heading path of %q", s) &&
			assert.Equalf(t, w.StartLine, got[i].StartLine, "start of %q", s) &&
			assert.Equalf(t, w.EndLine, got[i].EndLine, "end of %q", s) &&
			assert.Equalf(t, w.Text, got[i].Text, "text of %q", s) &&
			assert.Equalf(t, w.SHA, got[i].ContentSHA256, "sha of %q", s)
	}
	return ok
}

func checkGoldenResolve(t *testing.T, g goldenCase) bool {
	t.Helper()
	cands := make([]WikilinkTarget, len(g.Case.Cands))
	index := map[uuid.UUID]int{}
	for i, p := range g.Case.Cands {
		cands[i] = WikilinkTarget{VaultFileID: uuid.New(), Path: p}
		index[cands[i].VaultFileID] = i
	}
	var links []ParsedWikilink
	for _, l := range g.Case.Links {
		if parsed := ParseWikilink(l, 1); parsed != nil {
			links = append(links, *parsed)
		}
	}
	got := ResolveWikilinks(links, cands)
	ok := assert.Lenf(t, got, len(g.Want.Res), "resolve %q", strings.Join(g.Case.Links, ","))
	for i := 0; ok && i < len(got); i++ {
		w := g.Want.Res[i]
		ok = assert.Equalf(t, w.State, string(got[i].State), "state %q in %q", got[i].Link.RawTarget, g.Case.Cands)
		if ok && w.Target != nil {
			ok = assert.NotNil(t, got[i].TargetVaultFileID) && assert.Equalf(t, *w.Target, index[*got[i].TargetVaultFileID], "target %q in %q", got[i].Link.RawTarget, g.Case.Cands)
		}
		if ok && w.Target == nil {
			ok = assert.Nil(t, got[i].TargetVaultFileID)
		}
	}
	return ok
}
