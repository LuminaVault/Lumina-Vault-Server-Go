package vaultfiles_test

import (
	"encoding/json"
	"lumina/internal/vaultfiles"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// swiftJSON is what Swift's JSONEncoder wrote for these fields (Swift 6.4.1 on
// Darwin, default date strategy, the encoder Fluent stores the column with).
const swiftJSON = `{"dueAt":784000000.123456,"tags":[],"title":"a\/b'é<","isTodo":true,"projectID":"AAAAAAAA-0000-4000-8000-00000000ABCD"}`

var project = uuid.MustParse("aaaaaaaa-0000-4000-8000-00000000abcd")

func ptr[T any](v T) *T { return &v }

func TestMetadataDecodesSwiftBytes(t *testing.T) {
	var m vaultfiles.Metadata
	require.NoError(t, json.Unmarshal([]byte(swiftJSON), &m))
	require.Equal(t, "a/b'é<", *m.Title)
	require.True(t, *m.IsTodo)
	require.Nil(t, m.Done)
	require.Equal(t, project, *m.ProjectID)
	require.NotNil(t, m.Tags)
	require.Empty(t, m.Tags)
	require.Equal(t, time.Date(2025, 11, 5, 1, 46, 40, 123456000, time.UTC), m.DueAt.UTC().Round(time.Microsecond))

	out, err := json.Marshal(m)
	require.NoError(t, err)
	require.JSONEq(t, swiftJSON, string(out))
	require.Contains(t, string(out), `"dueAt":784000000.123456`)
	require.Contains(t, string(out), `"projectID":"AAAAAAAA-0000-4000-8000-00000000ABCD"`)
}

func TestMetadataEncodesLikeSwift(t *testing.T) {
	out, err := json.Marshal(vaultfiles.Metadata{})
	require.NoError(t, err)
	require.Equal(t, `{}`, string(out), "nil fields have no key")

	reference := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		date time.Time
		want string
	}{
		{reference, `{"dueAt":0}`},
		{reference.Add(784000000 * time.Second), `{"dueAt":784000000}`}, // no ".0": a jsonb numeric keeps its scale
		{reference.Add(784000000*time.Second + 500*time.Millisecond), `{"dueAt":784000000.5}`},
		{reference.Add(-time.Hour), `{"dueAt":-3600}`},
	} {
		out, err := json.Marshal(vaultfiles.Metadata{DueAt: &c.date})
		require.NoError(t, err)
		require.Equal(t, c.want, string(out))
	}

	out, err = json.Marshal(vaultfiles.Metadata{
		EnrichmentStatus: ptr("pending"),
		Done:             ptr(false),
		Provenance:       ptr("hermes-mirror"),
		SuggestedSpaceID: &project,
		Tags:             []string{"a"},
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"enrichmentStatus":"pending","done":false,"provenance":"hermes-mirror",
		"suggestedSpaceID":"AAAAAAAA-0000-4000-8000-00000000ABCD","tags":["a"]}`, string(out))
}

// A date read from a row must go back as the same Double, or re-saving a row
// would change it. That holds wherever a Double is coarser than a nanosecond, so
// beyond about 97 days from 2001-01-01 (2^23 seconds), which covers any real date.
func TestMetadataDateRoundTripsTheDouble(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	values := []float64{821692800.0999999, 815486400.123, 784000000.123456, 0, 0.5, -3600.25}
	for range 20000 {
		values = append(values, 1e7+r.Float64()*2e9, -1e7-r.Float64()*1e9)
	}
	for _, v := range values {
		in, err := json.Marshal(map[string]float64{"dueAt": v})
		require.NoError(t, err)
		var m vaultfiles.Metadata
		require.NoError(t, json.Unmarshal(in, &m))
		out, err := json.Marshal(m)
		require.NoError(t, err)
		require.JSONEq(t, string(in), string(out))
		var back struct{ DueAt float64 }
		require.NoError(t, json.Unmarshal(out, &back))
		require.Equal(t, math.Float64bits(v), math.Float64bits(back.DueAt), "%v", v)
	}
}

// Each expectation is what Swift's JSONDecoder did with the same text (Swift
// 6.4.1 on Darwin), decoding a struct with these fields.
func TestMetadataDecodeIsAsStrictAsSwift(t *testing.T) {
	for _, ok := range []string{
		`{}`,
		`{"dueAt":784000000}`,
		`{"tags":null}`,
		`{"title":null,"dueAt":null,"projectID":null,"done":null}`,
		`{"projectID":"AAAAAAAA-0000-4000-8000-00000000ABCD"}`,
		`{"projectID":"aaaaaaaa-0000-4000-8000-00000000abcd"}`,
		`{"unknown":{"nested":[1,2]},"title":"x"}`,
	} {
		var m vaultfiles.Metadata
		require.NoError(t, json.Unmarshal([]byte(ok), &m), ok)
	}
	for _, bad := range []string{
		`[]`,
		`1`,
		`"x"`,
		`{"dueAt":"x"}`,
		`{"dueAt":"2026-11-04T12:00:00Z"}`, // the column holds numbers, not ISO strings
		`{"dueAt":true}`,
		`{"tags":[null]}`,
		`{"tags":"a"}`,
		`{"tags":[1]}`,
		`{"isTodo":1}`,
		`{"done":"true"}`,
		`{"title":1}`,
		`{"projectID":"aaaaaaaa00004000800000000000abcd"}`,
		`{"projectID":"urn:uuid:aaaaaaaa-0000-4000-8000-00000000abcd"}`,
		`{"projectID":1}`,
	} {
		var m vaultfiles.Metadata
		require.Error(t, json.Unmarshal([]byte(bad), &m), bad)
	}
}

func TestMetadataKeysAreCaseSensitive(t *testing.T) {
	var m vaultfiles.Metadata
	require.NoError(t, json.Unmarshal([]byte(`{"DueAt":1,"TITLE":"x","istodo":true,"projectid":"nonsense"}`), &m))
	require.Equal(t, vaultfiles.Metadata{}, m)
}

func TestParseSidecar(t *testing.T) {
	seconds := func(m *vaultfiles.Metadata) string {
		out, err := json.Marshal(m)
		require.NoError(t, err)
		return string(out)
	}
	// The doubles are what Swift's .iso8601 strategy produced, on Darwin.
	require.Equal(t, `{"dueAt":815486400}`, seconds(vaultfiles.ParseSidecar(`{"dueAt":"2026-11-04T12:00:00Z"}`)))
	require.Equal(t, `{"dueAt":815486400.123}`, seconds(vaultfiles.ParseSidecar(`{"dueAt":"2026-11-04T12:00:00.123Z"}`)))
	require.Equal(t, `{"dueAt":815482800}`, seconds(vaultfiles.ParseSidecar(`{"dueAt":"2026-11-04T12:00:00+01:00"}`)))
	require.Equal(t, `{"dueAt":815482800.5}`, seconds(vaultfiles.ParseSidecar(`{"dueAt":"2026-11-04T12:00:00.5+01:00"}`)))

	m := vaultfiles.ParseSidecar(`{"title":"t","tags":["a"],"isTodo":true,"done":false,"projectID":"AAAAAAAA-0000-4000-8000-00000000ABCD","other":1}`)
	require.Equal(t, "t", *m.Title)
	require.Equal(t, []string{"a"}, m.Tags)
	require.Equal(t, project, *m.ProjectID)

	require.Equal(t, vaultfiles.Metadata{}, *vaultfiles.ParseSidecar(`{}`), "empty, not malformed")

	for _, bad := range []string{
		"", "not json", "null", "[]", "1", `{"title":1}`, `{"tags":[null]}`,
		`{"dueAt":784000000}`, // the header takes ISO strings, not numbers
		`{"dueAt":"2026-11-04"}`,
		`{"dueAt":"2026-11-04 12:00:00Z"}`,
		`{"projectID":"nope"}`,
	} {
		require.Nil(t, vaultfiles.ParseSidecar(bad), bad)
	}
}

func TestMergeMetadata(t *testing.T) {
	due := time.Now()
	old := &vaultfiles.Metadata{
		EnrichmentStatus: ptr("done"),
		Title:            ptr("old"),
		Tags:             []string{"old"},
		IsTodo:           ptr(true),
		Done:             ptr(true),
		DueAt:            &due,
		ProjectID:        &project,
		Provenance:       ptr("hermes-mirror"),
		SuggestedSpaceID: &project,
	}
	// Owned fields survive; note fields are overwritten even with nil.
	require.Equal(t, vaultfiles.Metadata{EnrichmentStatus: ptr("done"), ProjectID: &project},
		vaultfiles.MergeMetadata(old, vaultfiles.Metadata{}))

	other := uuid.New()
	require.Equal(t,
		vaultfiles.Metadata{EnrichmentStatus: ptr("failed"), Title: ptr("new"), Tags: []string{}, Done: ptr(false), ProjectID: &other},
		vaultfiles.MergeMetadata(old, vaultfiles.Metadata{EnrichmentStatus: ptr("failed"), Title: ptr("new"), Tags: []string{}, Done: ptr(false), ProjectID: &other}))

	// QUIRK from Swift: provenance and suggestedSpaceID go, even from the new side.
	require.Equal(t, vaultfiles.Metadata{Title: ptr("t")},
		vaultfiles.MergeMetadata(nil, vaultfiles.Metadata{Title: ptr("t"), Provenance: ptr("p"), SuggestedSpaceID: &project}))
}
