package notionimport

import (
	"archive/zip"
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// Ids of the synthetic export.
const (
	idRoadmap  = "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d"
	idQ4       = "aa11bb22cc33dd44ee55ff6677889900"
	idTasks    = "9f8e7d6c5b4a39281706f5e4d3c2b1a0"
	idRowLogin = "0123456789abcdef0123456789abcdef"
	idRowDocs  = "fedcba9876543210fedcba9876543210"
	idUntitled = "11111111111111111111111111111111"
	idUntitle2 = "22222222222222222222222222222222"
	idGone     = "deaddeaddeaddeaddeaddeaddeaddead"
	idChild    = "33333333333333333333333333333333"
)

type zipFile struct {
	name string
	data []byte
}

func buildZip(t *testing.T, files ...zipFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, f := range files {
		// CreateHeader, not Create: it takes names (../x) Create would too, but
		// keeps the method explicit.
		fw, err := w.CreateHeader(&zip.FileHeader{Name: f.name, Method: zip.Deflate})
		require.NoError(t, err)
		_, err = fw.Write(f.data)
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func text(name, body string) zipFile { return zipFile{name: name, data: []byte(body)} }

var pngBytes = []byte("\x89PNG\r\n\x1a\n synthetic")

// syntheticExport is a small Notion export: a page with a subpage, an image,
// external and broken links; a database with both CSVs and two row pages (a
// third row has none); two Untitled siblings; and a hidden file. The subpage
// and the database sit in a nested ZIP, as Notion splits large exports.
func syntheticExport(t *testing.T) []byte {
	t.Helper()
	roadmap := "Roadmap " + idRoadmap
	tasks := roadmap + "/Tasks " + idTasks
	part2 := buildZip(t,
		text(roadmap+"/Q4 plan "+idQ4+".md", "# Q4 plan\n\nBack to [Roadmap](../Roadmap%20"+idRoadmap+".md).\n"),
		zipFile{roadmap + "/diagram.png", pngBytes},
		text(tasks+".csv", "\xef\xbb\xbfName,Status,Due,Done,Tags,Estimate,Link\nFix login,Doing,\"November 18, 2026\",No,\"bug, auth\",3,https://example.com/1\n"),
		text(tasks+"_all.csv", "\xef\xbb\xbfName,Status,Due,Done,Tags,Estimate,Link\n"+
			"Fix login,Doing,\"November 18, 2026\",No,\"bug, auth\",3,https://example.com/1\n"+
			"Write docs,Todo,\"November 20, 2026 → November 22, 2026\",Yes,docs,1.5,https://example.com/2\n"+
			"No page,Doing,,No,\"bug, docs\",,\n"),
		text(tasks+"/Fix login "+idRowLogin+".md", "# Fix login\n\nStatus: Doing\nDue: November 18, 2026\nDone: No\nTags: bug, auth\nEstimate: 3\nLink: https://example.com/1\n\nSee the [plan](../Q4%20plan%20"+idQ4+".md).\n"),
		text(tasks+"/Write docs "+idRowDocs+".md", "# Write docs\n\nStatus: Todo\nDone: Yes\n"),
	)
	return buildZip(t,
		text(roadmap+".md", "# Roadmap\n\n"+
			"[Q4 plan](Roadmap%20"+idRoadmap+"/Q4%20plan%20"+idQ4+".md)\n\n"+
			"![](Roadmap%20"+idRoadmap+"/diagram.png)\n\n"+
			"[Tasks](Roadmap%20"+idRoadmap+"/Tasks%20"+idTasks+".csv)\n\n"+
			"[Notion copy](https://www.notion.so/acme/Q4-plan-"+idQ4+"?pvs=4)\n\n"+
			"[Elsewhere](https://www.notion.so/acme/Other-"+idGone+")\n\n"+
			"[Site](https://example.com/a)\n\n"+
			"[Old](Old%20page%20"+idGone+".md)\n\n"+
			"```\n[code](Roadmap%20"+idRoadmap+"/Q4%20plan%20"+idQ4+".md)\n```\n"),
		text("Untitled "+idUntitled+".md", "# Untitled\n\nfirst\n"),
		text("Untitled "+idUntitle2+".md", "# Untitled\n\nsecond\n"),
		text("Untitled "+idUntitle2+"/Child "+idChild+".md", "# Child\n"),
		text(".DS_Store", "junk"),
		zipFile{"ExportBlock-Part-2.zip", part2},
	)
}

func unpack(t *testing.T, data []byte) *Archive {
	t.Helper()
	a, err := Unpack(bytes.NewReader(data), int64(len(data)), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(a.Close)
	return a
}
