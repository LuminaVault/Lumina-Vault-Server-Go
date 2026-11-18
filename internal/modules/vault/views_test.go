package vault

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var jsonCT = map[string]string{"Content-Type": "application/json"}

func (v env) viewReq(user uuid.UUID, method, path string, body any) response {
	v.t.Helper()
	var b []byte
	if body != nil {
		var err error
		b, err = json.Marshal(body)
		require.NoError(v.t, err)
	}
	return v.do(user, method, path, jsonCT, b)
}

func decodeView(t *testing.T, r response) VaultView {
	t.Helper()
	var out VaultView
	require.NoError(t, json.Unmarshal(r.body, &out), string(r.body))
	return out
}

var boardView = map[string]any{
	"name":    "Board",
	"scope":   map[string]any{"folder": "inbox"},
	"columns": []map[string]any{{"property": "status", "width": 120}, {"property": "rating"}},
	"filter":  []map[string]any{{"property": "rating", "op": "gte", "value": 2}},
	"sort":    []map[string]any{{"property": "rating", "dir": "desc"}},
	"group":   "status",
}

func TestViewsAreOffByDefault(t *testing.T) {
	v := setup(t)
	u := v.user("free")
	require.Equal(t, http.StatusNotFound, v.viewReq(u, http.MethodGet, "/v1/vault/views", nil).code)
	require.Equal(t, http.StatusNotFound, v.viewReq(u, http.MethodPost, "/v1/vault/views", boardView).code)
}

func TestViewCRUD(t *testing.T) {
	v, u := queryFixture(t)

	r := v.viewReq(u, http.MethodPost, "/v1/vault/views", boardView)
	require.Equal(t, http.StatusCreated, r.code, string(r.body))
	created := decodeView(t, r)
	require.Equal(t, "Board", created.Name)
	require.Equal(t, "inbox", *created.Scope.Folder)
	require.Len(t, created.Columns, 2)
	require.Equal(t, 120, *created.Columns[0].Width)
	require.Nil(t, created.Columns[1].Width)
	require.Equal(t, "gte", string(created.Filter[0].Op))
	require.Equal(t, 2.0, created.Filter[0].Value)
	require.Equal(t, "status", *created.Group)
	path := "/v1/vault/views/" + created.Id.String()

	// The stored query runs as is.
	code, res, body := v.query(u, map[string]any{
		"scope": boardView["scope"], "filter": boardView["filter"], "sort": boardView["sort"], "group": boardView["group"],
	})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{"inbox/a.md", "inbox/b.md"}, res.names(), "grouped by status, Doing first")

	got := decodeView(t, v.viewReq(u, http.MethodGet, path, nil))
	require.Equal(t, created, got)

	minimal := map[string]any{"name": "  Work  ", "scope": map[string]any{"tag": "work"}}
	r = v.viewReq(u, http.MethodPut, path, minimal)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	put := decodeView(t, r)
	require.Equal(t, "Work", put.Name, "trimmed")
	require.Empty(t, put.Columns)
	require.Empty(t, put.Filter)
	require.Empty(t, put.Sort)
	require.Nil(t, put.Group)
	require.Equal(t, created.CreatedAt, put.CreatedAt)

	r = v.viewReq(u, http.MethodPost, "/v1/vault/views", map[string]any{"name": "all", "scope": map[string]any{"folder": "inbox"}})
	require.Equal(t, http.StatusCreated, r.code, string(r.body))
	var list VaultViewsResponse
	r = v.viewReq(u, http.MethodGet, "/v1/vault/views", nil)
	require.Equal(t, http.StatusOK, r.code)
	require.NoError(t, json.Unmarshal(r.body, &list))
	require.Equal(t, []string{"all", "Work"}, []string{list.Views[0].Name, list.Views[1].Name}, "by name, ignoring case")

	require.Equal(t, http.StatusNoContent, v.viewReq(u, http.MethodDelete, path, nil).code)
	require.Equal(t, http.StatusNotFound, v.viewReq(u, http.MethodGet, path, nil).code)
	require.Equal(t, http.StatusNotFound, v.viewReq(u, http.MethodDelete, path, nil).code)
	require.Equal(t, http.StatusNotFound, v.viewReq(u, http.MethodPut, path, minimal).code)
	require.Equal(t, http.StatusNotFound, v.viewReq(u, http.MethodGet, "/v1/vault/views/not-an-id", nil).code)
}

func TestViewsRefuseWhatAQueryRefuses(t *testing.T) {
	v, u := queryFixture(t)
	with := func(k string, val any) map[string]any {
		out := map[string]any{"name": "x", "scope": map[string]any{"folder": "inbox"}}
		out[k] = val
		return out
	}
	for _, bad := range []map[string]any{
		with("name", ""),
		with("name", "two\nlines"),
		with("scope", map[string]any{}),
		with("filter", []map[string]any{{"property": "rating", "op": "like", "value": 1}}),
		with("filter", []map[string]any{{"property": "rating", "op": "gt", "value": "lots"}}),
		with("sort", []map[string]any{{"property": "a"}, {"property": "b"}, {"property": "c"}, {"property": "d"}}),
		with("sort", []map[string]any{{"property": "a", "dir": "up"}}),
		with("columns", []map[string]any{{"property": ""}}),
		with("columns", []map[string]any{{"property": "a", "width": 5}}),
	} {
		r := v.viewReq(u, http.MethodPost, "/v1/vault/views", bad)
		require.Equal(t, http.StatusBadRequest, r.code, "%v: %s", bad, r.body)
	}
}

func TestViewsStayInTheVault(t *testing.T) {
	v, u := queryFixture(t)
	r := v.viewReq(u, http.MethodPost, "/v1/vault/views", boardView)
	require.Equal(t, http.StatusCreated, r.code, string(r.body))
	path := "/v1/vault/views/" + decodeView(t, r).Id.String()

	other := v.user("free")
	require.Equal(t, http.StatusNotFound, v.viewReq(other, http.MethodGet, path, nil).code)
	require.Equal(t, http.StatusNotFound, v.viewReq(other, http.MethodPut, path, boardView).code)
	require.Equal(t, http.StatusNotFound, v.viewReq(other, http.MethodDelete, path, nil).code)
	var list VaultViewsResponse
	r = v.viewReq(other, http.MethodGet, "/v1/vault/views", nil)
	require.NoError(t, json.Unmarshal(r.body, &list))
	require.Empty(t, list.Views)
}

func TestViewsAreCapped(t *testing.T) {
	v, u := queryFixture(t)
	_, err := v.pool.Exec(t.Context(), `INSERT INTO vault_views (tenant_id, name, scope)
		SELECT $1, 'v' || n, '{"folder":"inbox"}' FROM generate_series(1, $2) AS n`, u, maxViews)
	require.NoError(t, err)
	r := v.viewReq(u, http.MethodPost, "/v1/vault/views", boardView)
	require.Equal(t, http.StatusConflict, r.code, string(r.body))
}
