package vault

import (
	"encoding/json"
	"lumina/internal/vaultfiles"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// propsOn turns indexing on for one test; it is process-wide.
func propsOn(t *testing.T) {
	t.Helper()
	vaultfiles.EnableProperties(true)
	t.Cleanup(func() { vaultfiles.EnableProperties(false) })
}

func (v env) fileAt(user uuid.UUID, path string) fileDTO {
	v.t.Helper()
	for _, f := range v.list(user, "").Files {
		if f.Path == path {
			return f
		}
	}
	v.t.Fatalf("%s not listed", path)
	return fileDTO{}
}

func (v env) diskText(user uuid.UUID, path string) string {
	v.t.Helper()
	b, err := os.ReadFile(filepath.Join(v.module.paths.Raw(user), path))
	require.NoError(v.t, err)
	return string(b)
}

func (v env) patchProps(user uuid.UUID, path string, body any, ifMatch string) response {
	v.t.Helper()
	b, err := json.Marshal(body)
	require.NoError(v.t, err)
	h := map[string]string{"Content-Type": "application/json"}
	if ifMatch != "" {
		h["If-Match"] = ifMatch
	}
	return v.do(user, http.MethodPatch, "/v1/vault/properties?path="+url.QueryEscape(path), h, b)
}

func (v env) propertyTypes(user uuid.UUID) map[string]VaultPropertyType {
	v.t.Helper()
	r := v.do(user, http.MethodGet, "/v1/vault/property-types", nil, nil)
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var out VaultPropertyTypesResponse
	require.NoError(v.t, json.Unmarshal(r.body, &out))
	byName := map[string]VaultPropertyType{}
	for _, t := range out.Types {
		byName[t.Name] = t
	}
	return byName
}

const planNote = "---\ntitle: Plan\ntags: [a, b]\nstatus: Doing # for now\nrating: 4\ndue: 2026-12-01\n---\n\n# Plan\n"

func TestPropertiesAreOffByDefault(t *testing.T) {
	v := setup(t)
	u := v.user("free")
	path := v.upload(u, "plan.md", planNote)
	require.Nil(t, v.fileAt(u, path).Properties, "nothing is indexed while off")
	require.Equal(t, http.StatusNotFound, v.patchProps(u, path, map[string]any{"set": map[string]any{"a": 1}}, "").code)
	require.Equal(t, http.StatusNotFound, v.do(u, http.MethodGet, "/v1/vault/property-types", nil, nil).code)
}

func TestUploadIndexesFrontmatterProjectsAndInfersTypes(t *testing.T) {
	propsOn(t)
	v := setup(t)
	u := v.user("free")
	path := v.upload(u, "plan.md", planNote)

	f := v.fileAt(u, path)
	require.Equal(t, map[string]any{
		"title": "Plan", "tags": []any{"a", "b"}, "status": "Doing", "rating": 4.0, "due": "2026-12-01",
	}, f.Properties)
	require.Nil(t, f.PropertiesError)
	require.NotNil(t, f.Metadata)
	require.Equal(t, "Plan", *f.Metadata.Title)
	require.Equal(t, []string{"a", "b"}, *f.Metadata.Tags)
	require.NotNil(t, f.Metadata.DueAt)

	types := v.propertyTypes(u)
	require.Equal(t, VaultPropertyTypeType("text"), types["status"].Type)
	require.Equal(t, VaultPropertyTypeType("number"), types["rating"].Type)
	require.Equal(t, VaultPropertyTypeType("date"), types["due"].Type)
	require.Equal(t, VaultPropertyTypeType("multiselect"), types["tags"].Type)

	// A later value of another shape doesn't change a type once recorded.
	v.upload(u, "other.md", "---\nrating: lots\n---\n")
	require.Equal(t, VaultPropertyTypeType("number"), v.propertyTypes(u)["rating"].Type)
}

func TestInvalidFrontmatterIsRecordedAndNeverRewritten(t *testing.T) {
	propsOn(t)
	v := setup(t)
	u := v.user("free")
	bad := "---\ntitle: [unclosed\n---\nbody\n"
	path := v.upload(u, "bad.md", bad)

	f := v.fileAt(u, path)
	require.Nil(t, f.Properties)
	require.NotNil(t, f.PropertiesError)
	require.Contains(t, *f.PropertiesError, "invalid frontmatter")

	r := v.patchProps(u, path, map[string]any{"set": map[string]any{"status": "x"}}, "")
	require.Equal(t, http.StatusUnprocessableEntity, r.code, string(r.body))
	require.Equal(t, bad, v.diskText(u, path))
}

func TestPatchEditsOnlyTheChangedLines(t *testing.T) {
	propsOn(t)
	v := setup(t)
	u := v.user("free")
	path := v.upload(u, "plan.md", planNote)
	before := v.fileAt(u, path)

	r := v.patchProps(u, path, map[string]any{
		"set":   map[string]any{"status": "Done", "owner": "Ada"},
		"unset": []string{"rating"},
	}, `"`+before.SHA256+`"`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	require.Equal(t,
		"---\ntitle: Plan\ntags: [a, b]\nstatus: Done # for now\ndue: 2026-12-01\nowner: Ada\n---\n\n# Plan\n",
		v.diskText(u, path))

	var saved fileDTO
	require.NoError(t, json.Unmarshal(r.body, &saved))
	require.Equal(t, "Done", saved.Properties["status"])
	require.Equal(t, "Ada", saved.Properties["owner"])
	require.NotContains(t, saved.Properties, "rating")
	require.NotEqual(t, before.SHA256, saved.SHA256)

	// The old version is stale now.
	stale := v.patchProps(u, path, map[string]any{"set": map[string]any{"status": "Again"}}, `"`+before.SHA256+`"`)
	require.Equal(t, http.StatusPreconditionFailed, stale.code)
	require.Contains(t, v.diskText(u, path), "status: Done")
}

func TestPatchThatChangesNothingWritesNothing(t *testing.T) {
	propsOn(t)
	v := setup(t)
	u := v.user("free")
	path := v.upload(u, "plan.md", planNote)
	before := v.fileAt(u, path)
	r := v.patchProps(u, path, map[string]any{"set": map[string]any{"status": "Doing"}}, "")
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	require.Equal(t, planNote, v.diskText(u, path))
	require.Equal(t, before.SHA256, v.fileAt(u, path).SHA256)
}

func TestPatchCreatesFrontmatterAndProjectsDone(t *testing.T) {
	propsOn(t)
	v := setup(t)
	u := v.user("free")
	path := v.upload(u, "todo.md", "Buy milk\n")
	r := v.patchProps(u, path, map[string]any{"set": map[string]any{"done": true}}, "")
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	require.Equal(t, "---\ndone: true\n---\n\nBuy milk\n", v.diskText(u, path))
	f := v.fileAt(u, path)
	require.NotNil(t, f.Metadata)
	require.True(t, *f.Metadata.Done)
}

func TestPatchRefusesValuesThatDontFit(t *testing.T) {
	propsOn(t)
	v := setup(t)
	u := v.user("free")
	path := v.upload(u, "plan.md", planNote)

	cases := []struct {
		body map[string]any
		code int
	}{
		{map[string]any{"set": map[string]any{"rating": "four"}}, http.StatusUnprocessableEntity}, // declared number
		{map[string]any{"set": map[string]any{"done": "yes"}}, http.StatusUnprocessableEntity},    // reserved checkbox
		{map[string]any{"set": map[string]any{"due": "soon"}}, http.StatusUnprocessableEntity},    // reserved date
		{map[string]any{"set": map[string]any{"nested": map[string]any{"a": 1}}}, http.StatusBadRequest},
		{map[string]any{"set": map[string]any{"": "x"}}, http.StatusBadRequest},
		{map[string]any{}, http.StatusBadRequest},
	}
	for _, c := range cases {
		r := v.patchProps(u, path, c.body, "")
		require.Equal(t, c.code, r.code, "%v: %s", c.body, r.body)
	}
	require.Equal(t, planNote, v.diskText(u, path))

	// A datetime is fine for `due`.
	r := v.patchProps(u, path, map[string]any{"set": map[string]any{"due": "2026-12-01T10:00:00Z"}}, "")
	require.Equal(t, http.StatusOK, r.code, string(r.body))
}

func TestPatchOnlyForExistingMarkdownNotes(t *testing.T) {
	propsOn(t)
	v := setup(t)
	u := v.user("free")
	set := map[string]any{"set": map[string]any{"a": "b"}}
	require.Equal(t, http.StatusNotFound, v.patchProps(u, "inbox/missing.md", set, "").code)
	require.Equal(t, http.StatusBadRequest, v.patchProps(u, "inbox/photo.jpg", set, "").code)
	require.Equal(t, http.StatusBadRequest, v.do(u, http.MethodPatch, "/v1/vault/properties", nil, []byte(`{}`)).code)
}

func TestPutPropertyType(t *testing.T) {
	propsOn(t)
	v := setup(t)
	u := v.user("free")
	path := v.upload(u, "plan.md", planNote)

	put := func(name string, body any) response {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		return v.do(u, http.MethodPut, "/v1/vault/property-types/"+url.PathEscape(name),
			map[string]string{"Content-Type": "application/json"}, b)
	}
	r := put("Status", map[string]any{"type": "select", "options": []string{"Doing", "Done"}})
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	got := v.propertyTypes(u)["status"]
	require.Equal(t, VaultPropertyTypeType("select"), got.Type)
	require.Equal(t, []string{"Doing", "Done"}, got.Options)

	// Options are kept when absent, and no file is rewritten.
	require.Equal(t, http.StatusOK, put("status", map[string]any{"type": "text"}).code)
	require.Equal(t, []string{"Doing", "Done"}, v.propertyTypes(u)["status"].Options)
	require.Equal(t, planNote, v.diskText(u, path))

	require.Equal(t, http.StatusBadRequest, put("done", map[string]any{"type": "text"}).code)
	require.Equal(t, http.StatusOK, put("due", map[string]any{"type": "datetime"}).code)
	require.Equal(t, http.StatusBadRequest, put("x", map[string]any{"type": "color"}).code)
}

func TestPropertiesStayPerVault(t *testing.T) {
	propsOn(t)
	v := setup(t)
	a, b := v.user("free"), v.user("free")
	v.upload(a, "plan.md", planNote)
	require.NotEmpty(t, v.propertyTypes(a))
	require.Empty(t, v.propertyTypes(b))
}
