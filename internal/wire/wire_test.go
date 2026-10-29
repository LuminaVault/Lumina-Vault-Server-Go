package wire_test

import (
	"encoding/json"
	"errors"
	"lumina/internal/wire"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestTimeMarshalsWholeSecondsUTC(t *testing.T) {
	ts := time.Date(2026, 10, 29, 11, 30, 15, 987_654_321, time.FixedZone("CET", 3600))
	b, err := json.Marshal(wire.Time{Time: ts})
	require.NoError(t, err)
	require.JSONEq(t, `"2026-10-29T10:30:15Z"`, string(b))
}

func TestTimeUnmarshal(t *testing.T) {
	var got wire.Time
	require.NoError(t, json.Unmarshal([]byte(`"2026-10-29T10:30:15Z"`), &got))
	require.True(t, got.Equal(time.Date(2026, 10, 29, 10, 30, 15, 0, time.UTC)))
	require.NoError(t, json.Unmarshal([]byte(`"2026-10-29T12:30:15+02:00"`), &got))
	require.True(t, got.Equal(time.Date(2026, 10, 29, 10, 30, 15, 0, time.UTC)))
	require.NoError(t, json.Unmarshal([]byte(`"2026-10-29T10:30:15.123Z"`), &got), "Swift accepts fractions")
	require.Equal(t, 123_000_000, got.Nanosecond())
	require.Error(t, json.Unmarshal([]byte(`"2026-10-29"`), &got))
}

func TestNilTimePointerOmitted(t *testing.T) {
	type dto struct {
		DueAt *wire.Time `json:"dueAt,omitempty"`
	}
	b, err := json.Marshal(dto{})
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(b))
}

func TestUUIDUppercase(t *testing.T) {
	id := uuid.MustParse("abcdef01-2345-4678-9abc-def012345678")
	b, err := json.Marshal(wire.UUID{UUID: id})
	require.NoError(t, err)
	require.JSONEq(t, `"ABCDEF01-2345-4678-9ABC-DEF012345678"`, string(b))
	var back wire.UUID
	require.NoError(t, json.Unmarshal([]byte(`"abcdef01-2345-4678-9abc-def012345678"`), &back))
	require.Equal(t, id, back.UUID)
}

// Every createdAt/dueAt Swift emitted in todos-http.json must round-trip
// byte-for-byte through wire.Time.
func TestTimeMatchesGolden(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/golden/todos-http.json")
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("golden fixture not captured yet (Phase 0 Task 0.3)")
	}
	require.NoError(t, err)
	var exchanges []struct {
		Body string `json:"body"`
	}
	require.NoError(t, json.Unmarshal(raw, &exchanges))
	checked := 0
	for _, ex := range exchanges {
		for _, m := range isoRe.FindAllString(ex.Body, -1) {
			var ts wire.Time
			require.NoError(t, json.Unmarshal([]byte(m), &ts), m)
			back, err := json.Marshal(ts)
			require.NoError(t, err)
			require.Equal(t, m, string(back))
			checked++
		}
	}
	require.Positive(t, checked)
}

var isoRe = regexp.MustCompile(`"\d{4}-\d{2}-\d{2}T[^"]+"`)
