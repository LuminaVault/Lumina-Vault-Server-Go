package normalize_test

import (
	"lumina/parity/normalize"
	"testing"

	"github.com/stretchr/testify/require"
)

func body(t *testing.T, raw string, ignore ...string) any {
	t.Helper()
	v, err := normalize.Body([]byte(raw), ignore)
	require.NoError(t, err)
	return v
}

func TestUUIDPlaceholdersKeepCase(t *testing.T) {
	a := body(t, `{"id":"AAAAAAAA-0000-0000-0000-000000000001"}`)
	b := body(t, `{"id":"aaaaaaaa-0000-0000-0000-000000000001"}`)
	require.NotEqual(t, a, b, "case differences must surface as diffs")
	require.Equal(t, map[string]any{"id": "<uuid#1:upper>"}, a)
}

func TestSameUUIDSamePlaceholder(t *testing.T) {
	v := body(t, `{"a":"AAAAAAAA-0000-0000-0000-000000000001","b":"AAAAAAAA-0000-0000-0000-000000000001","c":"BBBBBBBB-0000-0000-0000-000000000002"}`)
	require.Equal(t, map[string]any{"a": "<uuid#1:upper>", "b": "<uuid#1:upper>", "c": "<uuid#2:upper>"}, v)
}

func TestNumberingSpansCallsOnOneNormalizer(t *testing.T) {
	n := normalize.New()
	first, _ := n.Body([]byte(`{"id":"BBBBBBBB-0000-0000-0000-000000000002"}`), nil)
	second, _ := n.Body([]byte(`[{"todoId":"BBBBBBBB-0000-0000-0000-000000000002"}]`), nil)
	require.Equal(t, map[string]any{"id": "<uuid#1:upper>"}, first)
	require.Equal(t, []any{map[string]any{"todoId": "<uuid#1:upper>"}}, second)
}

func TestTimestampClassesDiffer(t *testing.T) {
	require.Equal(t, "<ts:iso-s>", body(t, `"2026-10-29T10:00:00Z"`))
	require.Equal(t, "<ts:iso-ms>", body(t, `"2026-10-29T10:00:00.123Z"`))
	require.Equal(t, "<ts:iso-offset>", body(t, `"2026-10-29T10:00:00+02:00"`))
	require.NotEqual(t, body(t, `"2026-10-29T10:00:00Z"`), body(t, `"2026-10-29T10:00:00.123Z"`))
}

func TestIgnorePaths(t *testing.T) {
	v := body(t, `{"items":[{"t":"a","updatedAt":1},{"t":"b","updatedAt":2}],"meta":{"seed":7}}`,
		"$.items[*].updatedAt", "$.meta.seed")
	require.Equal(t, map[string]any{
		"items": []any{
			map[string]any{"t": "a", "updatedAt": normalize.Ignored},
			map[string]any{"t": "b", "updatedAt": normalize.Ignored},
		},
		"meta": map[string]any{"seed": normalize.Ignored},
	}, v)
}

func TestIgnoredUUIDsDoNotConsumeNumbers(t *testing.T) {
	v := body(t, `{"a":"AAAAAAAA-0000-0000-0000-000000000001","b":"BBBBBBBB-0000-0000-0000-000000000002"}`, "$.a")
	require.Equal(t, map[string]any{"a": normalize.Ignored, "b": "<uuid#1:upper>"}, v)
}

func TestNonJSONAndEmpty(t *testing.T) {
	require.Equal(t, "ok", body(t, "ok"))
	require.Equal(t, "", body(t, ""))
}

func TestNumbersKeptExact(t *testing.T) {
	require.NotEqual(t, body(t, `{"n":1}`), body(t, `{"n":1.0}`))
}

func TestBadPath(t *testing.T) {
	_, err := normalize.Body([]byte(`{}`), []string{"items"})
	require.Error(t, err)
}
