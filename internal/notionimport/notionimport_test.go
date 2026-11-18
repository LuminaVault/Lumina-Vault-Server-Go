package notionimport

import (
	"archive/zip"
	"bytes"
	"errors"
	"lumina/internal/vaultprops"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnpackListsNestedZips(t *testing.T) {
	dir := t.TempDir()
	data := syntheticExport(t)
	a, err := Unpack(bytes.NewReader(data), int64(len(data)), dir)
	require.NoError(t, err)
	var names []string
	for _, e := range a.Entries {
		names = append(names, e.Path)
	}
	assert.Len(t, names, 11)
	assert.Contains(t, names, "Roadmap "+idRoadmap+"/diagram.png")
	assert.NotContains(t, names, "ExportBlock-Part-2.zip")

	var png Entry
	for _, e := range a.Entries {
		if strings.HasSuffix(e.Path, ".png") {
			png = e
		}
	}
	got, err := png.Read(1 << 20)
	require.NoError(t, err)
	assert.Equal(t, pngBytes, got)
	_, err = png.Read(4)
	require.ErrorIs(t, err, ErrEntryTooLarge)

	left, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, left, 1, "the nested ZIP is on disk while the archive is open")
	a.Close()
	left, err = os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, left)
}

func TestUnpackLimits(t *testing.T) {
	var many []zipFile
	for n := range MaxEntries + 1 {
		many = append(many, text("p/"+strings.Repeat("a", n%7)+string(rune('a'+n%26))+time.Duration(n).String()+".md", "x"))
	}
	inner := buildZip(t, text("deep.md", "x"))
	middle := buildZip(t, zipFile{Name: "inner.zip", Data: inner}, text("mid.md", "x"))
	for _, c := range []struct {
		name  string
		data  []byte
		limit string
		is    error
	}{
		{name: "not a zip", data: []byte("hello, not a zip"), is: ErrNotZip},
		{name: "empty body", data: nil, is: ErrEmpty},
		{name: "no files", data: buildZip(t), is: ErrEmpty},
		{name: "dot dot", data: buildZip(t, text("ok.md", "x"), text("../evil.md", "x")), limit: "outside"},
		{name: "dot dot inside", data: buildZip(t, text("a/../../evil.md", "x")), limit: "outside"},
		{name: "absolute", data: buildZip(t, text("/etc/passwd", "x")), limit: "absolute"},
		{name: "bomb ratio", data: buildZip(t, zipFile{Name: "zeros.bin", Data: make([]byte, 4<<20)}), limit: "ZIP bomb"},
		{name: "bomb in nested", data: buildZip(t, zipFile{Name: "part.zip", Data: buildZip(t, zipFile{Name: "zeros.bin", Data: make([]byte, 4<<20)})}), limit: "ZIP bomb"},
		{name: "dot dot in nested", data: buildZip(t, zipFile{Name: "part.zip", Data: buildZip(t, text("../x.md", "x"))}), limit: "outside"},
		{name: "too many", data: buildZip(t, many...), limit: "more than 20000 files"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Unpack(bytes.NewReader(c.data), int64(len(c.data)), t.TempDir())
			require.Error(t, err)
			if c.is != nil {
				require.ErrorIs(t, err, c.is)
				return
			}
			var limit *LimitError
			require.True(t, errors.As(err, &limit), "%v", err)
			assert.Contains(t, err.Error(), c.limit)
		})
	}

	t.Run("nested one level only", func(t *testing.T) {
		data := buildZip(t, zipFile{Name: "outer.zip", Data: middle})
		a := unpack(t, data)
		var names []string
		for _, e := range a.Entries {
			names = append(names, e.Path)
		}
		assert.ElementsMatch(t, []string{"inner.zip", "mid.md"}, names)
	})

	t.Run("uncompressed total", func(t *testing.T) {
		// Stored entries: no ratio to trip, only the declared total.
		var buf bytes.Buffer
		w := zip.NewWriter(&buf)
		chunk := make([]byte, 1<<20)
		for n := range 4 {
			fw, err := w.CreateHeader(&zip.FileHeader{Name: string(rune('a'+n)) + ".bin", Method: zip.Store})
			require.NoError(t, err)
			_, err = fw.Write(chunk)
			require.NoError(t, err)
		}
		require.NoError(t, w.Close())
		a := &Archive{seen: map[string]bool{}, total: MaxUncompressedBytes - 3<<20}
		zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
		require.NoError(t, err)
		err = a.add(zr.File, 0, t.TempDir())
		var limit *LimitError
		require.True(t, errors.As(err, &limit), "%v", err)
		assert.Contains(t, err.Error(), "unpacks to more than")
	})

	t.Run("compressed size", func(t *testing.T) {
		_, err := Unpack(bytes.NewReader(nil), MaxCompressedBytes+1, t.TempDir())
		var limit *LimitError
		require.True(t, errors.As(err, &limit), "%v", err)
	})
}

func TestMapPaths(t *testing.T) {
	a := unpack(t, syntheticExport(t))
	m := MapPaths(a.Entries, "notion")
	roadmap := "Roadmap " + idRoadmap
	tasks := roadmap + "/Tasks " + idTasks
	want := map[string]string{
		roadmap + ".md":                                        "notion/roadmap.md",
		roadmap + "/Q4 plan " + idQ4 + ".md":                   "notion/roadmap/q4-plan.md",
		roadmap + "/diagram.png":                               "notion/roadmap/diagram.png",
		tasks + ".csv":                                         "notion/roadmap/tasks",
		tasks + "_all.csv":                                     "notion/roadmap/tasks",
		tasks + "/Fix login " + idRowLogin + ".md":             "notion/roadmap/tasks/fix-login.md",
		tasks + "/Write docs " + idRowDocs + ".md":             "notion/roadmap/tasks/write-docs.md",
		"Untitled " + idUntitled + ".md":                       "notion/untitled.md",
		"Untitled " + idUntitle2 + ".md":                       "notion/untitled-2.md",
		"Untitled " + idUntitle2 + "/Child " + idChild + ".md": "notion/untitled-2/child.md",
	}
	assert.Equal(t, want, m.New)
	assert.Equal(t, []Collision{{From: "Untitled " + idUntitle2 + ".md", To: "notion/untitled-2.md"}}, m.Collisions)
	assert.Equal(t, []Database{{
		Name: "Tasks", CSV: tasks + "_all.csv", Others: []string{tasks + ".csv"}, Folder: "notion/roadmap/tasks",
	}}, m.Databases)
	old, ok := m.PageByID(strings.ToUpper(idQ4))
	assert.True(t, ok)
	assert.Equal(t, roadmap+"/Q4 plan "+idQ4+".md", old)
	assert.Equal(t, "notion/roadmap/tasks/no-page.md", m.Allocate("notion/roadmap/tasks", "No page"))
	assert.Equal(t, "notion/roadmap/tasks/fix-login-2.md", m.Allocate("notion/roadmap/tasks", "Fix login"))
	assert.Equal(t, "roadmap/q4-plan.md", m.Relative("notion/roadmap/q4-plan.md"))
}

func TestMapPathsEdges(t *testing.T) {
	long := strings.Repeat("long name ", 12)
	entries := func(paths ...string) []Entry {
		out := make([]Entry, len(paths))
		for n, p := range paths {
			out[n] = Entry{Path: p}
		}
		return out
	}
	for _, c := range []struct {
		name string
		in   []string
		want []string
	}{
		{"folder before its page", []string{"A " + idQ4 + "/B.md", "A " + idQ4 + ".md"}, []string{"s/a/b.md", "s/a.md"}},
		{"same name different ids", []string{"A " + idQ4 + ".md", "A " + idGone + "/c.md", "A " + idGone + ".md"}, []string{"s/a.md", "s/a-2/c.md", "s/a-2.md"}},
		{"names that fold together", []string{"Hello World.md", "hello-world.md", "HELLO  WORLD.md"}, []string{"s/hello-world.md", "s/hello-world-2.md", "s/hello-world-3.md"}},
		{"suffix survives the cap", []string{long + ".md", long + "!.md"}, []string{
			"s/" + strings.Repeat("long-name-", 8)[:79] + ".md",
			"s/" + strings.Repeat("long-name-", 8)[:78] + "-2.md",
		}},
		{"leading dot is not hidden later", []string{"a/..env notes.md"}, nil},
		{"symbols only", []string{"???.md", "!!!.md"}, []string{"s/x.md", "s/x-2.md"}},
		{"attachment keeps its extension", []string{"Report " + idQ4 + ".PDF", "report.pdf", "archive.tar.gz"}, []string{"s/report.pdf", "s/report-2.pdf", "s/archive.tar.gz"}},
		{"attachment beside a page of the same name", []string{"Plan " + idQ4 + ".md", "Plan.png", "plan"}, []string{"s/plan.md", "s/plan.png", "s/plan-2"}},
		{"hidden entries are not mapped", []string{".obsidian/x.md", "__MACOSX/a.md", "ok.md"}, []string{"", "", "s/ok.md"}},
		{"markdown extension", []string{"Note.markdown"}, []string{"s/note.md"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := MapPaths(entries(c.in...), "s")
			if c.want == nil {
				assert.Empty(t, m.New)
				return
			}
			got := make([]string, len(c.in))
			for n, p := range c.in {
				got[n] = m.New[p]
			}
			assert.Equal(t, c.want, got)
		})
	}
}

func TestRewriteLinks(t *testing.T) {
	a := unpack(t, syntheticExport(t))
	m := MapPaths(a.Entries, "notion")
	roadmap := "Roadmap " + idRoadmap
	var body []byte
	for _, e := range a.Entries {
		if e.Path == roadmap+".md" {
			var err error
			body, err = e.Read(1 << 20)
			require.NoError(t, err)
		}
	}
	got, stats := RewriteLinks(string(body), roadmap+".md", m)
	assert.Equal(t, "# Roadmap\n\n"+
		"[[notion/roadmap/q4-plan|Q4 plan]]\n\n"+
		"![](roadmap/diagram.png)\n\n"+
		"[Tasks](roadmap/tasks/)\n\n"+
		"[[notion/roadmap/q4-plan|Notion copy]]\n\n"+
		"[Elsewhere](https://www.notion.so/acme/Other-"+idGone+")\n\n"+
		"[Site](https://example.com/a)\n\n"+
		"[Old](Old%20page%20"+idGone+".md)\n\n"+
		"```\n[code](Roadmap%20"+idRoadmap+"/Q4%20plan%20"+idQ4+".md)\n```\n", got)
	assert.Equal(t, LinkStats{Resolved: 4, External: 2, Unresolved: 1, UnresolvedTargets: []string{"Old page " + idGone + ".md"}}, stats)

	q4 := roadmap + "/Q4 plan " + idQ4 + ".md"
	for _, c := range []struct {
		name, from, in, want string
		stats                LinkStats
	}{
		{"up to the parent", q4, "[Roadmap](../Roadmap%20" + idRoadmap + ".md)", "[[notion/roadmap|Roadmap]]", LinkStats{Resolved: 1}},
		{"sibling image from a subpage", q4, "![d](diagram.png)", "![d](diagram.png)", LinkStats{Resolved: 1}},
		{"image from a row page", roadmap + "/Tasks " + idTasks + "/Fix login " + idRowLogin + ".md", "![d](../diagram.png)", "![d](../diagram.png)", LinkStats{Resolved: 1}},
		{"anchor and query dropped", q4, "[r](../Roadmap%20" + idRoadmap + ".md#top)", "[[notion/roadmap|r]]", LinkStats{Resolved: 1}},
		{"moved page found by id", q4, "[r](elsewhere/Roadmap%20" + idRoadmap + ".md)", "[[notion/roadmap|r]]", LinkStats{Resolved: 1}},
		{"empty text", q4, "[](../Roadmap%20" + idRoadmap + ".md)", "[[notion/roadmap]]", LinkStats{Resolved: 1}},
		{"pipe in text", q4, "[a|b](../Roadmap%20" + idRoadmap + ".md)", "[[notion/roadmap|a-b]]", LinkStats{Resolved: 1}},
		{"two on a line", q4, "[a](../Roadmap%20" + idRoadmap + ".md) and [b](https://x.y)", "[[notion/roadmap|a]] and [b](https://x.y)", LinkStats{Resolved: 1, External: 1}},
		{"mailto is external", q4, "[m](mailto:a@b.c)", "[m](mailto:a@b.c)", LinkStats{External: 1}},
		{"in-page anchor untouched", q4, "[top](#top)", "[top](#top)", LinkStats{}},
		{"escapes the export", q4, "[x](../../../etc/passwd)", "[x](../../../etc/passwd)", LinkStats{Unresolved: 1, UnresolvedTargets: []string{"../../../etc/passwd"}}},
		{"dashed notion id", q4, "[n](https://notion.so/1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d)", "[[notion/roadmap|n]]", LinkStats{Resolved: 1}},
		{"not notion's host", q4, "[n](https://evilnotion.so/x-" + idRoadmap + ")", "[n](https://evilnotion.so/x-" + idRoadmap + ")", LinkStats{External: 1}},
		{"tilde fence", q4, "~~~\n[r](../Roadmap%20" + idRoadmap + ".md)\n~~~", "~~~\n[r](../Roadmap%20" + idRoadmap + ".md)\n~~~", LinkStats{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, stats := RewriteLinks(c.in, c.from, m)
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.stats, stats)
		})
	}
}

func TestParseNotionDate(t *testing.T) {
	utc := func(y int, mo time.Month, d, h, mi int) time.Time { return time.Date(y, mo, d, h, mi, 0, 0, time.UTC) }
	for _, c := range []struct {
		in   string
		want Date
		iso  string
		ok   bool
	}{
		{"November 18, 2026", Date{Time: utc(2026, 11, 18, 0, 0)}, "2026-11-18", true},
		{"November 18, 2026 10:00 AM", Date{Time: utc(2026, 11, 18, 10, 0), HasTime: true}, "2026-11-18T10:00:00Z", true},
		{"November 18, 2026 10:00 PM (GMT)", Date{Time: utc(2026, 11, 18, 22, 0), HasTime: true}, "2026-11-18T22:00:00Z", true},
		{"November 18, 2026 10:00 AM (GMT+1)", Date{Time: utc(2026, 11, 18, 9, 0), HasTime: true}, "2026-11-18T09:00:00Z", true},
		{"November 18, 2026 1:15 AM (GMT+5:30)", Date{Time: utc(2026, 11, 17, 19, 45), HasTime: true}, "2026-11-17T19:45:00Z", true},
		{"November 18, 2026 10:00 PM (GMT-3)", Date{Time: utc(2026, 11, 19, 1, 0), HasTime: true}, "2026-11-19T01:00:00Z", true},
		{"November 18, 2026 10:00 AM (UTC)", Date{Time: utc(2026, 11, 18, 10, 0), HasTime: true}, "2026-11-18T10:00:00Z", true},
		{"November 20, 2026 → November 22, 2026", Date{Time: utc(2026, 11, 20, 0, 0), Range: true}, "2026-11-20", true},
		{"November 20, 2026 9:00 AM (GMT) → November 20, 2026 5:00 PM (GMT)", Date{Time: utc(2026, 11, 20, 9, 0), HasTime: true, Range: true}, "2026-11-20T09:00:00Z", true},
		{"Nov 8, 2026", Date{Time: utc(2026, 11, 8, 0, 0)}, "2026-11-08", true},
		{"2026-11-18", Date{Time: utc(2026, 11, 18, 0, 0)}, "2026-11-18", true},
		{"  March 1, 2027  ", Date{Time: utc(2027, 3, 1, 0, 0)}, "2027-03-01", true},
		{"November 31, 2026", Date{}, "", false},
		{"tomorrow", Date{}, "", false},
		{"2026", Date{}, "", false},
		{"", Date{}, "", false},
		{"→", Date{}, "", false},
	} {
		got, ok := ParseNotionDate(c.in)
		assert.Equal(t, c.ok, ok, c.in)
		if ok {
			assert.True(t, c.want.Time.Equal(got.Time), "%s: %s", c.in, got.Time)
			assert.Equal(t, c.want.HasTime, got.HasTime, c.in)
			assert.Equal(t, c.want.Range, got.Range, c.in)
			assert.Equal(t, c.iso, got.ISO(), c.in)
		}
	}
}

func TestInferColumn(t *testing.T) {
	many := func(n int) []string {
		out := make([]string, 0, n+1)
		for i := range n {
			out = append(out, "v"+time.Duration(i).String())
		}
		return append(out, out[0])
	}
	for _, c := range []struct {
		name  string
		cells []string
		want  Inference
	}{
		{"checkbox", []string{"Yes", "No", "", "Yes"}, Inference{Type: vaultprops.Checkbox}},
		{"number", []string{"3", "1.5", "-2", "1e3", ""}, Inference{Type: vaultprops.Number}},
		{"number wins over select", []string{"1", "1", "2"}, Inference{Type: vaultprops.Number}},
		{"date", []string{"November 18, 2026", "March 1, 2027"}, Inference{Type: vaultprops.Date}},
		{"datetime when any has a time", []string{"November 18, 2026", "November 18, 2026 10:00 AM (GMT)"}, Inference{Type: vaultprops.DateTime}},
		{"date range", []string{"November 20, 2026 → November 22, 2026", "November 18, 2026"}, Inference{Type: vaultprops.Date, Ranges: true}},
		{"url", []string{"https://a.b/c", "http://d.e"}, Inference{Type: vaultprops.URL}},
		{"multiselect", []string{"bug, auth", "docs", "bug, docs"}, Inference{Type: vaultprops.MultiSelect, Options: []string{"bug", "auth", "docs"}}},
		{"select", []string{"Doing", "Todo", "Doing"}, Inference{Type: vaultprops.Select, Options: []string{"Doing", "Todo"}}},
		{"text when nothing repeats", []string{"Doing", "Todo", "Done"}, Inference{Type: vaultprops.Text}},
		{"text when a checkbox column has another word", []string{"Yes", "No", "Maybe"}, Inference{Type: vaultprops.Text}},
		{"text when a date column has a word", []string{"November 18, 2026", "soon"}, Inference{Type: vaultprops.Text}},
		{"empty column", []string{"", " "}, Inference{Type: vaultprops.Text}},
		{"more than 20 distinct is text", many(21), Inference{Type: vaultprops.Text}},
		{"20 distinct is select", many(20), Inference{Type: vaultprops.Select, Options: many(20)[:20]}},
		{"long prose with a comma is text", []string{strings.Repeat("word ", 30) + ", and more", "short"}, Inference{Type: vaultprops.Text}},
		{"multi-line cells are text", []string{"a, b\nc", "a, b\nc"}, Inference{Type: vaultprops.Text}},
	} {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, InferColumn(c.cells))
		})
	}

	t.Run("more than 50 list items is not multiselect", func(t *testing.T) {
		var cells []string
		for i := range 26 {
			cells = append(cells, "a"+time.Duration(i).String()+", b"+time.Duration(i).String())
		}
		assert.Equal(t, vaultprops.Text, InferColumn(cells).Type)
		assert.Equal(t, vaultprops.MultiSelect, InferColumn(cells[:25]).Type)
	})
}

func TestConvert(t *testing.T) {
	for _, c := range []struct {
		typ  vaultprops.Type
		cell string
		want any
		ok   bool
	}{
		{vaultprops.Checkbox, "Yes", true, true},
		{vaultprops.Checkbox, "No", false, true},
		{vaultprops.Checkbox, "maybe", nil, false},
		{vaultprops.Number, "1.5", 1.5, true},
		{vaultprops.Number, "abc", nil, false},
		{vaultprops.Date, "November 18, 2026", "2026-11-18", true},
		{vaultprops.Date, "November 18, 2026 10:00 AM", "2026-11-18", true},
		{vaultprops.DateTime, "November 18, 2026 10:00 AM (GMT+1)", "2026-11-18T09:00:00Z", true},
		{vaultprops.DateTime, "November 18, 2026", "2026-11-18", true},
		{vaultprops.Date, "soon", nil, false},
		{vaultprops.URL, "https://a.b", "https://a.b", true},
		{vaultprops.URL, "a.b", "a.b", false},
		{vaultprops.MultiSelect, "bug, auth", []any{"bug", "auth"}, true},
		{vaultprops.MultiSelect, "docs", []any{"docs"}, true},
		{vaultprops.Select, "Doing", "Doing", true},
		{vaultprops.Text, " hi ", "hi", true},
		{vaultprops.Number, "  ", nil, true},
	} {
		got, ok := Convert(c.typ, c.cell)
		assert.Equal(t, c.ok, ok, "%s %q", c.typ, c.cell)
		if ok {
			assert.Equal(t, c.want, got, "%s %q", c.typ, c.cell)
			assert.True(t, vaultprops.Fits(c.typ, got), "%s %q", c.typ, c.cell)
		}
	}
}

func TestParseCSV(t *testing.T) {
	table, err := ParseCSV([]byte("\xef\xbb\xbfName, Status ,Notes\nA,Doing,\"line one\nline two, with a comma\"\nB\n"))
	require.NoError(t, err)
	assert.Equal(t, []string{"Name", "Status", "Notes"}, table.Columns)
	assert.Equal(t, [][]string{{"A", "Doing", "line one\nline two, with a comma"}, {"B", "", ""}}, table.Rows)
	assert.Equal(t, []string{"Doing", ""}, table.Column(1))

	_, err = ParseCSV(nil)
	require.Error(t, err)
}

func TestRowPage(t *testing.T) {
	columns := []string{"Name", "Status", "Due", "Empty"}
	for _, c := range []struct {
		name    string
		body    string
		columns []string
		props   []Property
		rest    string
	}{
		{
			"block and body", "# Fix login\n\nStatus: Doing\nDue: November 18, 2026\nEmpty:\n\nThe body.\n\nMore: text\n", columns,
			[]Property{{"Status", "Doing"}, {"Due", "November 18, 2026"}, {"Empty", ""}},
			"# Fix login\n\nThe body.\n\nMore: text\n",
		},
		{"block only", "# Write docs\n\nStatus: Todo\n", columns, []Property{{"Status", "Todo"}}, "# Write docs\n"},
		{"no block", "# Page\n\nJust text: with a colon.\n", columns, nil, "# Page\n\nJust text: with a colon.\n"},
		{"unknown name ends the block", "# P\n\nStatus: A\nNote: stays\n", columns, []Property{{"Status", "A"}}, "# P\n\nNote: stays\n"},
		{"any name without columns", "# P\n\nAnything: 1\nElse: 2\n\nbody", nil, []Property{{"Anything", "1"}, {"Else", "2"}}, "# P\n\nbody"},
		{"no heading", "Status: A\n\nbody", columns, []Property{{"Status", "A"}}, "body"},
		{"value with colons", "# P\n\nDue: November 18, 2026 10:00 AM\n", columns, []Property{{"Due", "November 18, 2026 10:00 AM"}}, "# P\n"},
		{"crlf", "# P\r\n\r\nStatus: A\r\n\r\nbody\r\n", columns, []Property{{"Status", "A"}}, "# P\n\nbody\n"},
		{"url line is not a property", "# P\n\nhttps://example.com\n", nil, nil, "# P\n\nhttps://example.com\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			props, rest := RowPage(c.body, c.columns)
			assert.Equal(t, c.props, props)
			assert.Equal(t, c.rest, rest)
		})
	}
}
