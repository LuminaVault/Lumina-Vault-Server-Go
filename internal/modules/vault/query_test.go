package vault

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type queryResult struct {
	Rows []struct {
		File       fileDTO        `json:"file"`
		Properties map[string]any `json:"properties"`
	} `json:"rows"`
	Columns    []VaultPropertyType `json:"columns"`
	Groups     []VaultQueryGroup   `json:"groups"`
	NextCursor *string             `json:"nextCursor"`
}

func (r queryResult) names() []string {
	out := []string{}
	for _, row := range r.Rows {
		out = append(out, row.File.Path)
	}
	return out
}

func (v env) query(user uuid.UUID, body any) (int, queryResult, string) {
	v.t.Helper()
	b, err := json.Marshal(body)
	require.NoError(v.t, err)
	r := v.do(user, http.MethodPost, "/v1/vault/query", map[string]string{"Content-Type": "application/json"}, b)
	var out queryResult
	if r.code == http.StatusOK {
		require.NoError(v.t, json.Unmarshal(r.body, &out))
	}
	return r.code, out, string(r.body)
}

// queryFixture uploads four notes and a photo into inbox/.
func queryFixture(t *testing.T) (env, uuid.UUID) {
	t.Helper()
	propsOn(t)
	v := setup(t)
	u := v.user("free")
	v.upload(u, "a.md", "---\nstatus: Doing\nrating: 4\ntags: [work, urgent]\ndue: 2026-12-01\n---\nA\n")
	v.upload(u, "b.md", "---\nstatus: Done\nrating: 9\ntags: [work]\ndue: 2026-11-01\n---\nB\n")
	v.upload(u, "c.md", "---\nstatus: Doing\nrating: 1\ntags: [home]\n---\nC\n")
	v.upload(u, "d.md", "no frontmatter\n")
	r := v.do(u, http.MethodPost, "/v1/vault/files?path=photo.jpg", map[string]string{"Content-Type": "image/jpeg"}, []byte("jpg"))
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	return v, u
}

func inbox(extra map[string]any) map[string]any {
	body := map[string]any{"scope": map[string]any{"folder": "inbox"}}
	for k, val := range extra {
		body[k] = val
	}
	return body
}

func filter(property, op string, value any) map[string]any {
	f := map[string]any{"property": property, "op": op}
	if value != nil {
		f["value"] = value
	}
	return f
}

func TestQueryIsOffByDefault(t *testing.T) {
	v := setup(t)
	u := v.user("free")
	code, _, _ := v.query(u, inbox(nil))
	require.Equal(t, http.StatusNotFound, code)
}

func TestQueryScopesToNotesInAFolder(t *testing.T) {
	v, u := queryFixture(t)
	code, res, body := v.query(u, inbox(nil))
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{"inbox/a.md", "inbox/b.md", "inbox/c.md", "inbox/d.md"}, res.names(), "notes only, by path")
	require.Equal(t, "Doing", res.Rows[0].Properties["status"])
	require.Empty(t, res.Rows[3].Properties)

	cols := map[string]VaultPropertyTypeType{}
	for _, c := range res.Columns {
		cols[c.Name] = c.Type
	}
	require.Equal(t, map[string]VaultPropertyTypeType{
		"status": "text", "rating": "number", "tags": "multiselect", "due": "date",
	}, cols)

	code, res, _ = v.query(u, map[string]any{"scope": map[string]any{"folder": "elsewhere"}})
	require.Equal(t, http.StatusOK, code)
	require.Empty(t, res.Rows)
}

func TestQueryFilters(t *testing.T) {
	v, u := queryFixture(t)
	for _, c := range []struct {
		name string
		f    []map[string]any
		want []string
	}{
		{"eq text", []map[string]any{filter("status", "eq", "Doing")}, []string{"inbox/a.md", "inbox/c.md"}},
		{"neq keeps missing", []map[string]any{filter("status", "neq", "Doing")}, []string{"inbox/b.md", "inbox/d.md"}},
		{"eq on a list matches an item", []map[string]any{filter("tags", "eq", "urgent")}, []string{"inbox/a.md"}},
		{"contains multiselect", []map[string]any{filter("tags", "contains", "work")}, []string{"inbox/a.md", "inbox/b.md"}},
		{"contains text", []map[string]any{filter("status", "contains", "do")}, []string{"inbox/a.md", "inbox/b.md", "inbox/c.md"}},
		{"gt number", []map[string]any{filter("rating", "gt", 3)}, []string{"inbox/a.md", "inbox/b.md"}},
		{"lte number as text", []map[string]any{filter("rating", "lte", "4")}, []string{"inbox/a.md", "inbox/c.md"}},
		{"eq number", []map[string]any{filter("rating", "eq", 9)}, []string{"inbox/b.md"}},
		{"lt date", []map[string]any{filter("due", "lt", "2026-11-15")}, []string{"inbox/b.md"}},
		{"gte datetime", []map[string]any{filter("due", "gte", "2026-11-15T00:00:00+01:00")}, []string{"inbox/a.md"}},
		{"isEmpty", []map[string]any{filter("due", "isEmpty", nil)}, []string{"inbox/c.md", "inbox/d.md"}},
		{"isNotEmpty", []map[string]any{filter("tags", "isNotEmpty", nil)}, []string{"inbox/a.md", "inbox/b.md", "inbox/c.md"}},
		{"ANDed", []map[string]any{filter("status", "eq", "Doing"), filter("rating", "gte", 2)}, []string{"inbox/a.md"}},
		{"built-in name", []map[string]any{filter("name", "eq", "c.md")}, []string{"inbox/c.md"}},
		{"built-in updatedAt", []map[string]any{filter("updatedAt", "gt", "2000-01-01")}, []string{"inbox/a.md", "inbox/b.md", "inbox/c.md", "inbox/d.md"}},
		{"a name is a value, never SQL", []map[string]any{filter("x') OR true --", "eq", "y")}, []string{}},
	} {
		code, res, body := v.query(u, inbox(map[string]any{"filter": c.f}))
		require.Equal(t, http.StatusOK, code, "%s: %s", c.name, body)
		require.Equal(t, c.want, res.names(), c.name)
	}
}

func TestQuerySortsGroupsAndPages(t *testing.T) {
	v, u := queryFixture(t)
	code, res, body := v.query(u, inbox(map[string]any{
		"sort": []map[string]any{{"property": "rating", "dir": "desc"}},
	}))
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{"inbox/b.md", "inbox/a.md", "inbox/c.md", "inbox/d.md"}, res.names(), "missing values last")

	code, res, body = v.query(u, inbox(map[string]any{"group": "status"}))
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []VaultQueryGroup{{Value: "Doing", Count: 2}, {Value: "Done", Count: 1}, {Count: 1}}, res.Groups)
	require.Equal(t, []string{"inbox/a.md", "inbox/c.md", "inbox/b.md", "inbox/d.md"}, res.names(), "rows come in group order")

	code, res, body = v.query(u, inbox(map[string]any{"group": "tags"}))
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []VaultQueryGroup{{Value: "home", Count: 1}, {Value: "urgent", Count: 1}, {Value: "work", Count: 2}, {Count: 1}}, res.Groups)

	code, res, body = v.query(u, inbox(map[string]any{"group": "due"}))
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []VaultQueryGroup{{Value: "2026-11-01", Count: 1}, {Value: "2026-12-01", Count: 1}, {Count: 2}}, res.Groups)

	var seen []string
	var cursor *string
	for range 5 {
		body := inbox(map[string]any{"limit": 3})
		if cursor != nil {
			body["cursor"] = *cursor
		}
		code, res, raw := v.query(u, body)
		require.Equal(t, http.StatusOK, code, raw)
		seen = append(seen, res.names()...)
		if cursor = res.NextCursor; cursor == nil {
			break
		}
	}
	require.Equal(t, []string{"inbox/a.md", "inbox/b.md", "inbox/c.md", "inbox/d.md"}, seen)
}

func TestQueryScopesByTagAndRefusesBadRequests(t *testing.T) {
	v, u := queryFixture(t)
	code, res, body := v.query(u, map[string]any{"scope": map[string]any{"tag": "#work"}})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{"inbox/a.md", "inbox/b.md"}, res.names())

	for _, bad := range []map[string]any{
		{"scope": map[string]any{}},
		inbox(map[string]any{"filter": []map[string]any{filter("status", "like", "x")}}),
		inbox(map[string]any{"filter": []map[string]any{filter("status", "eq", nil)}}),
		inbox(map[string]any{"filter": []map[string]any{filter("status", "gt", "x")}}),
		inbox(map[string]any{"filter": []map[string]any{filter("due", "gt", "soon")}}),
		inbox(map[string]any{"sort": []map[string]any{{"property": "a"}, {"property": "b"}, {"property": "c"}, {"property": "d"}}}),
		inbox(map[string]any{"cursor": "nope"}),
		{"scope": map[string]any{"folder": "../x"}},
	} {
		code, _, body := v.query(u, bad)
		require.Equal(t, http.StatusBadRequest, code, "%v: %s", bad, body)
	}
}

func TestQueryStaysInTheVault(t *testing.T) {
	v, u := queryFixture(t)
	_ = u
	other := v.user("free")
	code, res, body := v.query(other, inbox(nil))
	require.Equal(t, http.StatusOK, code, body)
	require.Empty(t, res.Rows)
	require.Empty(t, res.Columns)
}
