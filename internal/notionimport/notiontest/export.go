// Package notiontest builds a small, synthetic Notion export for tests: the
// pure package's and the import route's.
package notiontest

import (
	"archive/zip"
	"bytes"
)

// The Notion ids of the synthetic export. IDGone is a page that is not in it.
const (
	IDRoadmap   = "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d"
	IDQ4        = "aa11bb22cc33dd44ee55ff6677889900"
	IDTasks     = "9f8e7d6c5b4a39281706f5e4d3c2b1a0"
	IDRowLogin  = "0123456789abcdef0123456789abcdef"
	IDRowDocs   = "fedcba9876543210fedcba9876543210"
	IDUntitled  = "11111111111111111111111111111111"
	IDUntitled2 = "22222222222222222222222222222222"
	IDGone      = "deaddeaddeaddeaddeaddeaddeaddead"
	IDChild     = "33333333333333333333333333333333"
)

// File is one entry of a ZIP under construction.
type File struct {
	Name string
	Data []byte
}

// Zip builds a ZIP in memory. It panics on a write error, which a buffer
// never returns.
func Zip(files ...File) []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, f := range files {
		fw, err := w.CreateHeader(&zip.FileHeader{Name: f.Name, Method: zip.Deflate})
		if err == nil {
			_, err = fw.Write(f.Data)
		}
		if err != nil {
			panic(err)
		}
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// Text is a File from a string.
func Text(name, body string) File { return File{Name: name, Data: []byte(body)} }

// PNG is the bytes of the export's one image.
var PNG = []byte("\x89PNG\r\n\x1a\n synthetic")

// Export is a small Notion export: a page with a subpage, an image,
// external and broken links; a database with both CSVs and two row pages (a
// third row has none); two Untitled siblings; and a hidden file. The subpage
// and the database sit in a nested ZIP, as Notion splits large exports.
func Export() []byte {
	roadmap := "Roadmap " + IDRoadmap
	tasks := roadmap + "/Tasks " + IDTasks
	part2 := Zip(
		Text(roadmap+"/Q4 plan "+IDQ4+".md", "# Q4 plan\n\nBack to [Roadmap](../Roadmap%20"+IDRoadmap+".md).\n"),
		File{Name: roadmap + "/diagram.png", Data: PNG},
		Text(tasks+".csv", "\xef\xbb\xbfName,Status,Due,Done,Tags,Estimate,Link\nFix login,Doing,\"November 18, 2026\",No,\"bug, auth\",3,https://example.com/1\n"),
		Text(tasks+"_all.csv", "\xef\xbb\xbfName,Status,Due,Done,Tags,Estimate,Link\n"+
			"Fix login,Doing,\"November 18, 2026\",No,\"bug, auth\",3,https://example.com/1\n"+
			"Write docs,Todo,\"November 20, 2026 → November 22, 2026\",Yes,docs,1.5,https://example.com/2\n"+
			"No page,Doing,,No,\"bug, docs\",,\n"),
		Text(tasks+"/Fix login "+IDRowLogin+".md", "# Fix login\n\nStatus: Doing\nDue: November 18, 2026\nDone: No\nTags: bug, auth\nEstimate: 3\nLink: https://example.com/1\n\nSee the [plan](../Q4%20plan%20"+IDQ4+".md).\n"),
		Text(tasks+"/Write docs "+IDRowDocs+".md", "# Write docs\n\nStatus: Todo\nDone: Yes\n"),
	)
	return Zip(
		Text(roadmap+".md", "# Roadmap\n\n"+
			"[Q4 plan](Roadmap%20"+IDRoadmap+"/Q4%20plan%20"+IDQ4+".md)\n\n"+
			"![](Roadmap%20"+IDRoadmap+"/diagram.png)\n\n"+
			"[Tasks](Roadmap%20"+IDRoadmap+"/Tasks%20"+IDTasks+".csv)\n\n"+
			"[Notion copy](https://www.notion.so/acme/Q4-plan-"+IDQ4+"?pvs=4)\n\n"+
			"[Elsewhere](https://www.notion.so/acme/Other-"+IDGone+")\n\n"+
			"[Site](https://example.com/a)\n\n"+
			"[Old](Old%20page%20"+IDGone+".md)\n\n"+
			"```\n[code](Roadmap%20"+IDRoadmap+"/Q4%20plan%20"+IDQ4+".md)\n```\n"),
		Text("Untitled "+IDUntitled+".md", "# Untitled\n\nfirst\n"),
		Text("Untitled "+IDUntitled2+".md", "# Untitled\n\nsecond\n"),
		Text("Untitled "+IDUntitled2+"/Child "+IDChild+".md", "# Child\n"),
		Text(".DS_Store", "junk"),
		File{Name: "ExportBlock-Part-2.zip", Data: part2},
	)
}
