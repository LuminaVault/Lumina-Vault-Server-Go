package imports_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/chunkindex"
	"lumina/internal/diskguard"
	"lumina/internal/ids"
	"lumina/internal/modules/imports"
	"lumina/internal/notionimport/notiontest"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultgit/vaultgittest"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const notionRoute = "/v1/vault/import/notion"

type notionFile struct {
	Entry     string  `json:"entry"`
	SavedPath *string `json:"savedPath"`
	Kind      string  `json:"kind"`
	Status    string  `json:"status"`
}

type notionReport struct {
	DryRun      bool    `json:"dryRun"`
	SpaceID     *string `json:"spaceId"`
	SpaceSlug   string  `json:"spaceSlug"`
	Pages       int     `json:"pages"`
	Databases   int     `json:"databases"`
	Rows        int     `json:"rows"`
	Attachments int     `json:"attachments"`
	Written     int     `json:"written"`
	Skipped     int     `json:"skipped"`
	Failed      int     `json:"failed"`
	Links       struct {
		Resolved       int `json:"resolved"`
		External       int `json:"external"`
		Unresolved     int `json:"unresolved"`
		UnresolvedList []struct {
			Page   string `json:"page"`
			Target string `json:"target"`
		} `json:"unresolvedList"`
	} `json:"links"`
	Columns []struct {
		Database string `json:"database"`
		Name     string `json:"name"`
		Type     string `json:"type"`
	} `json:"columns"`
	Views []struct {
		ID     *string `json:"id"`
		Name   string  `json:"name"`
		Folder string  `json:"folder"`
	} `json:"views"`
	Collisions []struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"collisions"`
	Files    []notionFile `json:"files"`
	Warnings []string     `json:"warnings"`
}

// statuses is entry → status for the entries with one line in the report, and
// saved path → status for everything with a path.
func (r notionReport) statuses() map[string]string {
	out := map[string]string{}
	for _, f := range r.Files {
		if f.SavedPath != nil {
			out[f.Kind+" "+*f.SavedPath] = f.Status
		} else {
			out[f.Kind+" "+f.Entry] = f.Status
		}
	}
	return out
}

func (v env) notionRaw(user uuid.UUID, query string, body []byte) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(http.MethodPost, notionRoute+query, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/zip")
	if user != uuid.Nil {
		token, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec
}

func (v env) notion(user uuid.UUID, query string, body []byte) notionReport {
	v.t.Helper()
	rec := v.notionRaw(user, query, body)
	require.Equal(v.t, http.StatusOK, rec.Code, rec.Body.String())
	// Every list is a list, never null: clients iterate them unguarded.
	assert.NotContains(v.t, rec.Body.String(), "null")
	var out notionReport
	require.NoError(v.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func (v env) count(query string, args ...any) int {
	v.t.Helper()
	var n int
	require.NoError(v.t, v.pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func (v env) vaultFile(tenant uuid.UUID, path string) string {
	v.t.Helper()
	data, err := os.ReadFile(filepath.Join(v.root, "tenants", ids.Upper(tenant), "raw", path))
	require.NoError(v.t, err)
	return string(data)
}

func withProperties(t *testing.T) {
	t.Helper()
	vaultfiles.EnableProperties(true)
	t.Cleanup(func() { vaultfiles.EnableProperties(false) })
}

// spoolIsEmpty checks no upload or nested ZIP outlives its request.
func (v env) spoolIsEmpty() {
	v.t.Helper()
	left, err := os.ReadDir(filepath.Join(v.root, "tmp", "notion-import"))
	if err == nil {
		assert.Empty(v.t, left)
	}
}

var wantColumns = map[string]string{
	"Status": "select", "Due": "date", "Done": "checkbox", "Tags": "multiselect", "Estimate": "number", "Link": "url",
}

func TestNotionDryRunWritesNothing(t *testing.T) {
	withProperties(t)
	v := setup(t)
	user := v.user()
	report := v.notion(user, "?dryRun=true", notiontest.Export())

	assert.True(t, report.DryRun)
	assert.Nil(t, report.SpaceID, "the Space doesn't exist yet")
	assert.Equal(t, "notion", report.SpaceSlug)
	assert.Equal(t, []int{5, 1, 3, 1}, []int{report.Pages, report.Databases, report.Rows, report.Attachments})
	assert.Equal(t, []int{0, 1, 0}, []int{report.Written, report.Skipped, report.Failed})
	assert.Equal(t, []int{6, 2, 1}, []int{report.Links.Resolved, report.Links.External, report.Links.Unresolved})
	require.Len(t, report.Links.UnresolvedList, 1)
	assert.Equal(t, "notion/roadmap.md", report.Links.UnresolvedList[0].Page)
	assert.Equal(t, "Old page "+notiontest.IDGone+".md", report.Links.UnresolvedList[0].Target)
	got := map[string]string{}
	for _, c := range report.Columns {
		assert.Equal(t, "Tasks", c.Database)
		got[c.Name] = c.Type
	}
	assert.Equal(t, wantColumns, got)
	require.Len(t, report.Views, 1)
	assert.Nil(t, report.Views[0].ID)
	assert.Equal(t, "Tasks", report.Views[0].Name)
	assert.Equal(t, "notion/roadmap/tasks", report.Views[0].Folder)
	require.Len(t, report.Collisions, 1)
	assert.Equal(t, "notion/untitled-2.md", report.Collisions[0].To)
	assert.Contains(t, report.Warnings, "Tasks: column Due had date ranges; kept the start date")
	assert.Equal(t, map[string]string{
		"page notion/roadmap.md":                 "planned",
		"page notion/roadmap/q4-plan.md":         "planned",
		"page notion/untitled.md":                "planned",
		"page notion/untitled-2.md":              "planned",
		"page notion/untitled-2/child.md":        "planned",
		"attachment notion/roadmap/diagram.png":  "planned",
		"database notion/roadmap/tasks":          "planned",
		"row notion/roadmap/tasks/fix-login.md":  "planned",
		"row notion/roadmap/tasks/write-docs.md": "planned",
		"row notion/roadmap/tasks/no-page.md":    "planned",
		"other .DS_Store":                        "hidden",
	}, report.statuses())

	for _, table := range []string{"vault_files", "spaces", "vault_property_types", "vault_views", "memories"} {
		assert.Zero(t, v.count(`SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, user), table)
	}
	_, err := os.Stat(filepath.Join(v.root, "tenants", ids.Upper(user), "raw", "notion"))
	assert.True(t, os.IsNotExist(err), "nothing on disk")
	v.spoolIsEmpty()
}

func TestNotionImportWritesPagesRowsAndAttachments(t *testing.T) {
	withProperties(t)
	v := setup(t)
	user := v.user()
	ctx := context.Background()
	touches := vaultgittest.Record(t)
	start := time.Now()
	report := v.notion(user, "", notiontest.Export())
	elapsed := time.Since(start)
	assert.Len(t, touches.Reasons(user), report.Written, "every written page, row and attachment signals history")

	assert.False(t, report.DryRun)
	require.NotNil(t, report.SpaceID)
	assert.Equal(t, []int{5, 1, 3, 1}, []int{report.Pages, report.Databases, report.Rows, report.Attachments})
	assert.Equal(t, []int{9, 1, 0}, []int{report.Written, report.Skipped, report.Failed})
	assert.Equal(t, []int{6, 2, 1}, []int{report.Links.Resolved, report.Links.External, report.Links.Unresolved})
	statuses := report.statuses()
	for _, key := range []string{
		"page notion/roadmap.md", "page notion/roadmap/q4-plan.md", "page notion/untitled.md", "page notion/untitled-2.md",
		"page notion/untitled-2/child.md", "attachment notion/roadmap/diagram.png", "database notion/roadmap/tasks",
		"row notion/roadmap/tasks/fix-login.md", "row notion/roadmap/tasks/write-docs.md", "row notion/roadmap/tasks/no-page.md",
	} {
		assert.Equal(t, "imported", statuses[key], key)
	}
	// With the stub embedder this is the cost of everything but embedding.
	t.Logf("notion import: %d notes and 1 attachment in %s, %.1f ms per note (deterministic embedder, local Postgres)",
		8, elapsed.Round(time.Millisecond), float64(elapsed.Milliseconds())/8)

	// Pages: ids gone from paths, links rewritten, provenance stamped.
	assert.Equal(t, "# Roadmap\n\n"+
		"[[notion/roadmap/q4-plan|Q4 plan]]\n\n"+
		"![](roadmap/diagram.png)\n\n"+
		"[Tasks](roadmap/tasks/)\n\n"+
		"[[notion/roadmap/q4-plan|Notion copy]]\n\n"+
		"[Elsewhere](https://www.notion.so/acme/Other-"+notiontest.IDGone+")\n\n"+
		"[Site](https://example.com/a)\n\n"+
		"[Old](Old%20page%20"+notiontest.IDGone+".md)\n\n"+
		"```\n[code](Roadmap%20"+notiontest.IDRoadmap+"/Q4%20plan%20"+notiontest.IDQ4+".md)\n```\n",
		v.vaultFile(user, "notion/roadmap.md"))
	assert.Equal(t, 8, v.count(`SELECT count(*) FROM vault_files WHERE tenant_id = $1 AND metadata->>'provenance' = 'notion'
		AND content_type = 'text/markdown' AND space_id = $2 AND processed_at IS NOT NULL`, user, *report.SpaceID))
	assert.Equal(t, 8, v.count(`SELECT count(*) FROM memories WHERE tenant_id = $1 AND embedding IS NOT NULL`, user))

	// The rewritten links go through the real resolver.
	assert.Equal(t, "resolved", v.linkState(user, "notion/roadmap.md", "notion/roadmap/q4-plan|Q4 plan"))
	assert.Equal(t, "resolved", v.linkState(user, "notion/roadmap/q4-plan.md", "notion/roadmap|Roadmap"))
	assert.Equal(t, "resolved", v.linkState(user, "notion/roadmap/tasks/fix-login.md", "notion/roadmap/q4-plan|plan"))
	var roadmapID uuid.UUID
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT id FROM vault_files WHERE tenant_id = $1 AND path = 'notion/roadmap.md'`, user).Scan(&roadmapID))
	outgoing, err := chunkindex.NewLinkRepository(v.pool).Outgoing(ctx, user, roadmapID)
	require.NoError(t, err)
	require.Len(t, outgoing, 2)
	for _, link := range outgoing {
		require.NotNil(t, link.TargetPath, link.RawTarget)
		assert.Equal(t, "notion/roadmap/q4-plan.md", *link.TargetPath)
	}
	assert.Zero(t, v.count(`SELECT count(*) FROM vault_links WHERE tenant_id = $1 AND resolution_state <> 'resolved'`, user))

	// The attachment: bytes on disk, typed, left for the ingestion worker.
	assert.Equal(t, string(notiontest.PNG), v.vaultFile(user, "notion/roadmap/diagram.png"))
	assert.Equal(t, 1, v.count(`SELECT count(*) FROM vault_files WHERE tenant_id = $1 AND path = 'notion/roadmap/diagram.png'
		AND content_type = 'image/png' AND size_bytes = $2 AND processed_at IS NULL AND space_id = $3
		AND metadata->>'provenance' = 'notion'`, user, len(notiontest.PNG), *report.SpaceID))

	// Rows: frontmatter from the CSV in column order, the property block gone.
	assert.Equal(t, "---\nStatus: Doing\nDue: 2026-11-18\nDone: false\nTags:\n  - bug\n  - auth\nEstimate: 3\nLink: https://example.com/1\n---\n\n"+
		"# Fix login\n\nSee the [[notion/roadmap/q4-plan|plan]].\n", v.vaultFile(user, "notion/roadmap/tasks/fix-login.md"))
	assert.Equal(t, "---\nStatus: Todo\nDue: 2026-11-20\nDone: true\nTags:\n  - docs\nEstimate: 1.5\nLink: https://example.com/2\n---\n\n# Write docs\n",
		v.vaultFile(user, "notion/roadmap/tasks/write-docs.md"))
	assert.Equal(t, "---\nStatus: Doing\nDone: false\nTags:\n  - bug\n  - docs\n---\n\n# No page\n", v.vaultFile(user, "notion/roadmap/tasks/no-page.md"))
	var props, title string
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT (to_jsonb(vault_files)->'properties')::text, metadata->>'title' FROM vault_files
		WHERE tenant_id = $1 AND path = 'notion/roadmap/tasks/fix-login.md'`, user).Scan(&props, &title))
	assert.JSONEq(t, `{"Status":"Doing","Due":"2026-11-18","Done":false,"Tags":["bug","auth"],"Estimate":3,"Link":"https://example.com/1"}`, props)
	assert.Equal(t, "Fix login", title)

	// The registry has the column types, not what one note's value suggests.
	rows, err := v.pool.Query(ctx, `SELECT name, type FROM vault_property_types WHERE tenant_id = $1`, user)
	require.NoError(t, err)
	types := map[string]string{}
	for rows.Next() {
		var name, typ string
		require.NoError(t, rows.Scan(&name, &typ))
		types[name] = typ
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, wantColumns, types)

	// One view, the CSV's columns in order.
	require.Len(t, report.Views, 1)
	require.NotNil(t, report.Views[0].ID)
	var name, scope, columns string
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT name, scope::text, columns::text FROM vault_views WHERE tenant_id = $1 AND id = $2`,
		user, *report.Views[0].ID).Scan(&name, &scope, &columns))
	assert.Equal(t, "Tasks", name)
	assert.JSONEq(t, `{"folder":"notion/roadmap/tasks"}`, scope)
	assert.JSONEq(t, `[{"property":"Status"},{"property":"Due"},{"property":"Done"},{"property":"Tags"},{"property":"Estimate"},{"property":"Link"}]`, columns)

	// The same export again changes nothing and makes no second view.
	again := v.notion(user, "", notiontest.Export())
	assert.Equal(t, []int{0, 10, 0}, []int{again.Written, again.Skipped, again.Failed})
	require.Len(t, again.Views, 1)
	assert.Equal(t, *report.Views[0].ID, *again.Views[0].ID)
	assert.Equal(t, 1, v.count(`SELECT count(*) FROM vault_views WHERE tenant_id = $1`, user))
	assert.Equal(t, 9, v.count(`SELECT count(*) FROM vault_files WHERE tenant_id = $1`, user))
	dry := v.notion(user, "?dryRun=true", notiontest.Export())
	assert.Equal(t, "unchanged", dry.statuses()["page notion/roadmap.md"])
	assert.Equal(t, "unchanged", dry.statuses()["attachment notion/roadmap/diagram.png"])
	assert.Equal(t, *report.SpaceID, *dry.SpaceID)
	v.spoolIsEmpty()
}

func TestNotionOnConflict(t *testing.T) {
	v := setup(t)
	user := v.user()
	// The user already has a note and an image where the import would put its own.
	v.bulk(user, bulkBody(t, "Notion", map[string]string{"Roadmap.md": "mine"}, []string{"Roadmap.md"}, ""))
	export := notiontest.Zip(
		notiontest.Text("Roadmap "+notiontest.IDRoadmap+".md", "# Roadmap\n\ntheirs\n"),
		notiontest.Text("New "+notiontest.IDQ4+".md", "# New\n"),
		notiontest.File{Name: "pic.png", Data: notiontest.PNG},
	)
	first := v.notion(user, "?space=Notion", export)
	assert.Equal(t, "exists", first.statuses()["page notion/roadmap.md"])
	assert.Equal(t, "imported", first.statuses()["page notion/new.md"])
	assert.Equal(t, "mine", v.vaultFile(user, "notion/roadmap.md"))
	assert.Equal(t, []int{2, 1, 0}, []int{first.Written, first.Skipped, first.Failed})

	changed := notiontest.Zip(
		notiontest.Text("Roadmap "+notiontest.IDRoadmap+".md", "# Roadmap\n\ntheirs\n"),
		notiontest.File{Name: "pic.png", Data: []byte("another picture")},
	)
	skip := v.notion(user, "?onConflict=skip", changed)
	assert.Equal(t, "exists", skip.statuses()["attachment notion/pic.png"])
	assert.Equal(t, string(notiontest.PNG), v.vaultFile(user, "notion/pic.png"))
	assert.Equal(t, "exists", v.notion(user, "?dryRun=true", changed).statuses()["page notion/roadmap.md"])
	assert.Equal(t, "planned", v.notion(user, "?dryRun=true&onConflict=overwrite", changed).statuses()["attachment notion/pic.png"])

	over := v.notion(user, "?onConflict=overwrite", changed)
	assert.Equal(t, "updated", over.statuses()["page notion/roadmap.md"])
	assert.Equal(t, "updated", over.statuses()["attachment notion/pic.png"])
	assert.Equal(t, "# Roadmap\n\ntheirs\n", v.vaultFile(user, "notion/roadmap.md"))
	assert.Equal(t, "another picture", v.vaultFile(user, "notion/pic.png"))
	assert.Equal(t, 3, v.count(`SELECT count(*) FROM vault_files WHERE tenant_id = $1`, user))
}

func TestNotionRegistryKeepsExistingTypes(t *testing.T) {
	withProperties(t)
	v := setup(t)
	user := v.user()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO vault_property_types (tenant_id, name, type) VALUES ($1, 'status', 'text'), ($1, 'Estimate', 'checkbox')`, user)
	require.NoError(t, err)
	report := v.notion(user, "", notiontest.Export())
	got := map[string]string{}
	for _, c := range report.Columns {
		got[c.Name] = c.Type
	}
	assert.Equal(t, "text", got["Status"])
	assert.Equal(t, "checkbox", got["Estimate"])
	assert.Equal(t, 1, v.count(`SELECT count(*) FROM vault_property_types WHERE tenant_id = $1 AND lower(name) = 'status' AND type = 'text'`, user))
	// A number is not a checkbox: it is written as text, never dropped.
	assert.Contains(t, v.vaultFile(user, "notion/roadmap/tasks/fix-login.md"), "\nEstimate: \"3\"\n")
	assert.Contains(t, strings.Join(report.Warnings, "\n"), "Tasks: column Estimate has values that are not a checkbox; kept them as text")
	assert.Contains(t, strings.Join(report.Warnings, "\n"), "Tasks: column Status keeps the type it already has in this vault (text, not select)")
}

func TestNotionViewCap(t *testing.T) {
	withProperties(t)
	v := setup(t)
	user := v.user()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO vault_views (tenant_id, name, scope)
		SELECT $1, 'v' || n, '{"folder":"x"}' FROM generate_series(1, $2) AS n`, user, vaultfiles.MaxViews)
	require.NoError(t, err)
	report := v.notion(user, "", notiontest.Export())
	assert.Empty(t, report.Views)
	assert.Contains(t, strings.Join(report.Warnings, "\n"), "Tasks: no view was saved, because this vault already has 200 views")
	assert.Equal(t, 9, report.Written)
}

func TestNotionWithPropertiesOff(t *testing.T) {
	v := setup(t)
	user := v.user()
	for _, query := range []string{"?dryRun=true", ""} {
		report := v.notion(user, query, notiontest.Export())
		assert.Empty(t, report.Views)
		assert.Empty(t, report.Columns)
		assert.Equal(t, []int{7, 1, 0, 1}, []int{report.Pages, report.Databases, report.Rows, report.Attachments})
		require.Len(t, report.Warnings, 1)
		assert.Contains(t, report.Warnings[0], "Databases were imported as plain pages")
		statuses := report.statuses()
		assert.Equal(t, "unsupported", statuses["database notion/roadmap/tasks"])
		want := "imported"
		if report.DryRun {
			want = "planned"
		}
		assert.Equal(t, want, statuses["page notion/roadmap/tasks/fix-login.md"])
		_, hasRow := statuses["row notion/roadmap/tasks/no-page.md"]
		assert.False(t, hasRow)
	}
	// The property block stays as Notion wrote it.
	assert.Equal(t, "# Fix login\n\nStatus: Doing\nDue: November 18, 2026\nDone: No\nTags: bug, auth\nEstimate: 3\nLink: https://example.com/1\n\n"+
		"See the [[notion/roadmap/q4-plan|plan]].\n", v.vaultFile(user, "notion/roadmap/tasks/fix-login.md"))
	assert.Zero(t, v.count(`SELECT count(*) FROM vault_views WHERE tenant_id = $1`, user))
	assert.Zero(t, v.count(`SELECT count(*) FROM vault_property_types WHERE tenant_id = $1`, user))
}

func TestNotionRefusals(t *testing.T) {
	v := setup(t)
	user := v.user()
	var tooMany []notiontest.File
	for n := range imports.MaxNotionNotes + 1 {
		tooMany = append(tooMany, notiontest.Text(fmt.Sprintf("p%d.md", n), "x"))
	}
	ok := notiontest.Zip(notiontest.Text("a.md", "# A\n"))
	for _, c := range []struct {
		name, query string
		body        []byte
		status      int
		message     string
	}{
		{"not a zip", "", []byte("definitely not a zip"), http.StatusBadRequest, "The upload is not a ZIP file"},
		{"empty body", "", nil, http.StatusBadRequest, "The ZIP is empty"},
		{"zip without files", "", notiontest.Zip(), http.StatusBadRequest, "The ZIP is empty"},
		{"dot dot entry", "", notiontest.Zip(notiontest.Text("a.md", "x"), notiontest.Text("../b.md", "x")), http.StatusBadRequest, "points outside it"},
		{"zip bomb", "", notiontest.Zip(notiontest.File{Name: "zeros.png", Data: make([]byte, 4<<20)}), http.StatusBadRequest, "ZIP bomb"},
		{"too many notes", "", notiontest.Zip(tooMany...), http.StatusBadRequest, "Split the export"},
		{"too many notes on a dry run", "?dryRun=true", notiontest.Zip(tooMany...), http.StatusBadRequest, "at most 2000"},
		{"bad onConflict", "?onConflict=merge", ok, http.StatusBadRequest, "invalid_on_conflict"},
		{"bad dryRun", "?dryRun=maybe", ok, http.StatusBadRequest, "invalid_dry_run"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := v.notionRaw(user, c.query, c.body)
			require.Equal(t, c.status, rec.Code, rec.Body.String())
			var body struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Contains(t, body.Error.Message, c.message)
		})
	}
	assert.Equal(t, http.StatusUnauthorized, v.notionRaw(uuid.Nil, "", ok).Code)
	assert.Zero(t, v.count(`SELECT count(*) FROM vault_files WHERE tenant_id = $1`, user))
	v.spoolIsEmpty()

	t.Run("an attachment over 25 MB is skipped, the rest imports", func(t *testing.T) {
		// Random-looking bytes, so the entry is no ZIP bomb.
		big := make([]byte, 25<<20+1)
		state := uint32(1)
		for n := range big {
			state = state*1664525 + 1013904223
			big[n] = byte(state >> 24)
		}
		report := v.notion(user, "", notiontest.Zip(notiontest.Text("a.md", "# A\n"), notiontest.File{Name: "big.pdf", Data: big},
			notiontest.File{Name: "tool.exe", Data: []byte("MZ")}))
		assert.Equal(t, "tooLarge", report.statuses()["attachment notion/big.pdf"])
		assert.Equal(t, "unsupported", report.statuses()["attachment notion/tool.exe"])
		assert.Equal(t, []int{1, 2, 0}, []int{report.Written, report.Skipped, report.Failed})
	})
}

func TestNotionReportConformsToSpec(t *testing.T) {
	withProperties(t)
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)
	v := setup(t)
	user := v.user()
	for _, c := range []struct {
		query string
		body  []byte
	}{
		{"?dryRun=true&space=Notion", notiontest.Export()},
		{"?onConflict=overwrite", notiontest.Export()},
		{"?onConflict=skip", notiontest.Export()},
	} {
		rec := v.notionRaw(user, c.query, c.body)
		req := httptest.NewRequest(http.MethodPost, notionRoute+c.query, bytes.NewReader(c.body))
		req.Header.Set("Content-Type", "application/zip")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		require.NoError(t, openapi3filter.ValidateRequest(ctx, in), c.query)
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: rec.Code,
			Header: http.Header{"Content-Type": {rec.Header().Get("Content-Type")}},
		}
		out.SetBodyBytes(rec.Body.Bytes())
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s: %s", c.query, rec.Body.String())
	}
}

func TestNotionBodyOverTheLimitIs413(t *testing.T) {
	v := setup(t)
	user := v.user()
	req := httptest.NewRequest(http.MethodPost, notionRoute, strings.NewReader("PK"))
	req.ContentLength = 200<<20 + 1
	token, err := scenario.Mint(keys, user)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	v.spoolIsEmpty()
}

func TestNotionFullDiskIs507(t *testing.T) {
	full := diskguard.New("/", 1<<60, 0, diskguard.Options{Provider: func(string) (diskguard.Space, error) {
		return diskguard.Space{FreeBytes: 1, TotalBytes: 1 << 40}, nil
	}})
	v := setupWithGuard(t, full)
	user := v.user()
	rec := v.notionRaw(user, "", notiontest.Export())
	require.Equal(t, http.StatusInsufficientStorage, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "insufficient_storage")
	assert.Zero(t, v.count(`SELECT count(*) FROM vault_files WHERE tenant_id = $1`, user))
	v.spoolIsEmpty()
}

// TestNotionImportScale times a larger import: pages in folders of 20, each
// linking to its neighbour and its folder's first page. NOTION_SCALE sets the
// page count (default 200). The embedder is the deterministic stub, so the
// figure is everything but embedding.
func TestNotionImportScale(t *testing.T) {
	pages := 200
	if raw := os.Getenv("NOTION_SCALE"); raw != "" {
		n, err := strconv.Atoi(raw)
		require.NoError(t, err)
		pages = n
	}
	id := func(n int) string { return fmt.Sprintf("%032x", n+1) }
	name := func(n int) string { return fmt.Sprintf("Section %d %s/Page %d %s.md", n/20, id(100000+n/20), n, id(n)) }
	files := make([]notiontest.File, 0, pages)
	for n := range pages {
		next, first := (n+1)%pages, n/20*20
		body := fmt.Sprintf("# Page %d\n\n%s\n\n[next](../%s) and [first](%s)\n", n, strings.Repeat("Some text for the page. ", 40),
			strings.ReplaceAll(name(next), " ", "%20"), strings.ReplaceAll(name(first)[strings.Index(name(first), "/")+1:], " ", "%20"))
		files = append(files, notiontest.Text(name(n), body))
	}
	v := setup(t)
	user := v.user()
	export := notiontest.Zip(files...)
	start := time.Now()
	report := v.notion(user, "", export)
	elapsed := time.Since(start)
	require.Equal(t, []int{pages, 0, 0}, []int{report.Written, report.Skipped, report.Failed})
	assert.Equal(t, []int{2 * pages, 0, 0}, []int{report.Links.Resolved, report.Links.External, report.Links.Unresolved})
	assert.Zero(t, v.count(`SELECT count(*) FROM vault_links WHERE tenant_id = $1 AND resolution_state <> 'resolved'`, user))
	t.Logf("notion import at scale: %d notes in %s, %.1f ms per note (deterministic embedder, local Postgres)",
		pages, elapsed.Round(time.Millisecond), float64(elapsed.Milliseconds())/float64(pages))
}
