package quickassistant

import (
	"encoding/json"
	"lumina/internal/wire"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var testID = uuid.MustParse("0a1b2c3d-1111-2222-3333-444455556666")

const testIDUpper = "0A1B2C3D-1111-2222-3333-444455556666"

func TestActionFrameMatchesShared(t *testing.T) {
	exp := wire.Time{Time: time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC)}
	got, err := json.Marshal(Frame(Action{ID: wire.UUID{UUID: testID}, Kind: KindNeedsConfirm, Summary: "s", ExpiresAt: &exp}))
	require.NoError(t, err)
	require.Equal(t,
		`{"type":"assistant_action","payload":{"id":"`+testIDUpper+`","kind":"needsConfirm","summary":"s","expiresAt":"2030-01-01T09:00:00Z"}}`,
		string(got))
}

func TestDoneActionOmitsNilFields(t *testing.T) {
	got, err := json.Marshal(Action{ID: wire.UUID{UUID: testID}, Kind: KindDone, Summary: "ok"})
	require.NoError(t, err)
	for _, k := range []string{"screen", "memoryID", "expiresAt"} {
		require.NotContains(t, string(got), `"`+k+`"`)
	}
}

func TestOpenScreenCarriesScreenAndMemoryID(t *testing.T) {
	s := ScreenMemory
	m := wire.UUID{UUID: testID}
	got, err := json.Marshal(Action{ID: m, Kind: KindOpenScreen, Summary: "s", Screen: &s, MemoryID: &m})
	require.NoError(t, err)
	require.Contains(t, string(got), `"screen":"memory","memoryID":"`+testIDUpper+`"`)
}

func TestParseScreenRejectsUnknown(t *testing.T) {
	for _, bad := range []string{"unknown", "Brain", "", "future"} {
		_, ok := ParseScreen(bad)
		require.False(t, ok, bad)
	}
	for _, s := range strings.Fields("home spaces ai brain insights settings dataAccess brief learned memory") {
		got, ok := ParseScreen(s)
		require.True(t, ok, s)
		require.Equal(t, Screen(s), got)
	}
}

func TestContextDecodesSharedShape(t *testing.T) {
	c, err := wire.Decode[Context]([]byte(`{"currentTab":"home","activeSpaceID":"` + testID.String() + `","timeZone":"America/Santiago"}`))
	require.NoError(t, err)
	require.Equal(t, "home", c.CurrentTab)
	require.Equal(t, testID, c.ActiveSpaceID.UUID)
	require.Equal(t, "America/Santiago", *c.TimeZone)

	old, err := wire.Decode[Context]([]byte(`{"currentTab":"home"}`))
	require.NoError(t, err)
	require.Nil(t, old.ActiveSpaceID)
	require.Nil(t, old.TimeZone)

	_, err = wire.Decode[Context]([]byte(`{}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "currentTab")
}

func TestResolutionShape(t *testing.T) {
	got, err := json.Marshal(Resolution{Status: StatusAlreadyResolved, Message: "m"})
	require.NoError(t, err)
	require.JSONEq(t, `{"status":"alreadyResolved","message":"m"}`, string(got))
	for _, s := range []ResolutionStatus{StatusConfirmed, StatusCancelled, StatusFailed, StatusExpired} {
		require.NotEmpty(t, s)
	}
}

// Ruling 7: a value this build does not know decodes to "unknown", never an error.
func TestLenientDecodingMapsUnknownValues(t *testing.T) {
	var a Action
	require.NoError(t, json.Unmarshal([]byte(`{"id":"`+testIDUpper+`","kind":"future","summary":"s","screen":"future"}`), &a))
	require.Equal(t, KindUnknown, a.Kind)
	require.Equal(t, ScreenUnknown, *a.Screen)

	var r Resolution
	require.NoError(t, json.Unmarshal([]byte(`{"status":"future","message":"m"}`), &r))
	require.Equal(t, StatusUnknown, r.Status)

	require.NoError(t, json.Unmarshal([]byte(`{"id":"`+testIDUpper+`","kind":"openScreen","summary":"s","screen":"memory"}`), &a))
	require.Equal(t, KindOpenScreen, a.Kind)
	require.Equal(t, ScreenMemory, *a.Screen)
}
