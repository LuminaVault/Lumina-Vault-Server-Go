package wire_test

import (
	"encoding/json"
	"errors"
	"lumina/internal/wire"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

type createProject struct {
	Name        string  `json:"name"`
	Description *string `json:"description,omitempty"`
}

type patchProject struct {
	Name     *string `json:"name,omitempty"`
	Archived *bool   `json:"archived,omitempty"`
}

type createTodo struct {
	Title     string     `json:"title"`
	DueAt     *wire.Time `json:"dueAt,omitempty"`
	ProjectID *wire.UUID `json:"projectID,omitempty"`
}

func bind[T any](t *testing.T, body string) (*T, error) {
	t.Helper()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), httptest.NewRecorder())
	return wire.Bind[T](c)
}

// Every decode case in errors-http.json, replayed against the DTO shape of
// the route it hit, must produce Swift's status and message.
func TestBindMatchesGoldenMessages(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/golden/errors-http.json")
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("golden fixture not captured yet (Phase 0 Task 0.3)")
	}
	require.NoError(t, err)
	var cases []struct {
		Name        string          `json:"name"`
		Method      string          `json:"method"`
		Path        string          `json:"path"`
		RequestBody json.RawMessage `json:"requestBody"`
		Status      int             `json:"status"`
		Body        string          `json:"body"`
	}
	require.NoError(t, json.Unmarshal(raw, &cases))
	decodeCases := map[string]func(string) error{
		"malformed-json":           func(b string) error { _, err := bind[createTodo](t, b); return err },
		"wrong-type":               func(b string) error { _, err := bind[createTodo](t, b); return err },
		"missing-key":              func(b string) error { _, err := bind[createProject](t, b); return err },
		"null-required":            func(b string) error { _, err := bind[createProject](t, b); return err },
		"empty-body":               func(b string) error { _, err := bind[createProject](t, b); return err },
		"not-an-object":            func(b string) error { _, err := bind[createProject](t, b); return err },
		"wrong-type-bool":          func(b string) error { _, err := bind[patchProject](t, b); return err },
		"wrong-type-string-number": func(b string) error { _, err := bind[createProject](t, b); return err },
		"bad-uuid-field":           func(b string) error { _, err := bind[createTodo](t, b); return err },
		"bad-date-field":           func(b string) error { _, err := bind[createTodo](t, b); return err },
		"fractional-date":          func(b string) error { _, err := bind[createTodo](t, b); return err },
		"offset-date":              func(b string) error { _, err := bind[createTodo](t, b); return err },
		"unknown-field-ignored":    func(b string) error { _, err := bind[createProject](t, b); return err },
	}
	seen := 0
	for _, tc := range cases {
		run, ok := decodeCases[tc.Name]
		if !ok {
			continue
		}
		seen++
		body := string(tc.RequestBody)
		if tc.Name == "malformed-json" {
			body = `{"title":`
		}
		err := run(body)
		if tc.Status < 300 {
			require.NoError(t, err, tc.Name)
			continue
		}
		var we *wire.Error
		require.ErrorAs(t, err, &we, tc.Name)
		require.Equal(t, tc.Status, we.Status, tc.Name)
		var want struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(tc.Body), &want), tc.Name)
		require.Equal(t, want.Error.Message, we.Message, tc.Name)
	}
	require.Equal(t, len(decodeCases), seen, "every decode case must exist in the golden file")
}

func TestBindValues(t *testing.T) {
	got, err := bind[createTodo](t, `{"title":"x","dueAt":"2026-11-01T11:00:00.500+02:00","projectID":"abcdef01-2345-4678-9abc-def012345678","extra":true}`)
	require.NoError(t, err)
	require.Equal(t, "x", got.Title)
	b, _ := json.Marshal(got.DueAt)
	require.JSONEq(t, `"2026-11-01T09:00:00Z"`, string(b))
	require.Equal(t, "ABCDEF01-2345-4678-9ABC-DEF012345678", got.ProjectID.String())

	p, err := bind[patchProject](t, `{"name":null}`)
	require.NoError(t, err)
	require.Nil(t, p.Name, "null optional decodes as absent")
}

func TestJSONContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	require.NoError(t, wire.JSON(c, http.StatusOK, map[string]int{"a": 1}))
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
}

type nestedItem struct {
	ID       string             `json:"id"`
	Done     bool               `json:"done"`
	Priority *int64             `json:"priority,omitempty"`
	Tags     *[]string          `json:"tags,omitempty"`
	Args     *map[string]string `json:"args,omitempty"`
	When     *wire.Time         `json:"when,omitempty"`
}

type nestedBatch struct {
	Items []nestedItem `json:"items"`
}

// Hummingbird's Request.decode: a missing key names only the key, while null
// and type mismatches name the dotted coding path with array indices
// (hummingbird Sources/Hummingbird/Server/Request.swift, pathKeyValue).
func TestBindNestedPaths(t *testing.T) {
	cases := []struct{ body, message string }{
		{`{"items":[{"id":"a"}]}`, "Coding key `done` not found."},
		{`{"items":[{"id":"a","done":true},{"id":"b","done":"no"}]}`, "Type mismatch for `items.1.done` key, expected `Bool` type."},
		{`{"items":[{"id":null,"done":true}]}`, "Value not found for `items.0.id` key."},
		{`{"items":[1]}`, "Type mismatch for `items.0` key, expected `Dictionary<String, Any>` type."},
		{`{"items":[null]}`, "Value not found for `items.0` key."},
		{`{"items":{}}`, "Type mismatch for `items` key, expected `Array<Any>` type."},
		{`{"items":[{"id":"a","done":true,"priority":"x"}]}`, "Type mismatch for `items.0.priority` key, expected `Int` type."},
		{`{"items":[{"id":"a","done":true,"tags":"x"}]}`, "Type mismatch for `items.0.tags` key, expected `Array<Any>` type."},
		{`{"items":[{"id":"a","done":true,"tags":["ok",2]}]}`, "Type mismatch for `items.0.tags.1` key, expected `String` type."},
		{`{"items":[{"id":"a","done":true,"args":[]}]}`, "Type mismatch for `items.0.args` key, expected `Dictionary<String, Any>` type."},
		{`{"items":[{"id":"a","done":true,"args":{"n":1}}]}`, "Type mismatch for `items.0.args.n` key, expected `String` type."},
		{`{"items":[{"id":"a","done":true,"when":"soon"}]}`, "The given data was not valid input: Expected date string to be ISO8601-formatted."},
	}
	for _, tc := range cases {
		_, err := bind[nestedBatch](t, tc.body)
		var we *wire.Error
		require.ErrorAs(t, err, &we, tc.body)
		require.Equal(t, http.StatusBadRequest, we.Status, tc.body)
		require.Equal(t, tc.message, we.Message, tc.body)
	}
	got, err := bind[nestedBatch](t, `{"items":[{"id":"a","done":true,"priority":3,"tags":["x"],"args":{"k":"v"},"when":"2026-01-01T00:00:00Z","extra":1}]}`)
	require.NoError(t, err)
	require.Equal(t, int64(3), *got.Items[0].Priority)
	require.Equal(t, []string{"x"}, *got.Items[0].Tags)
	require.Equal(t, map[string]string{"k": "v"}, *got.Items[0].Args)
	require.Equal(t, 2026, got.Items[0].When.Year())
}
