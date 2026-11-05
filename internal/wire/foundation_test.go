package wire_test

import (
	"lumina/internal/wire"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMarshalFoundation(t *testing.T) {
	type payload struct {
		Zebra string    `json:"zebra"`
		Apple string    `json:"apple"`
		At    wire.Time `json:"at"`
	}
	b, err := wire.MarshalFoundation(payload{
		Zebra: `a/b <c> & "d"`,
		Apple: "é\n",
		At:    wire.Time{Time: time.Date(2026, 11, 5, 12, 0, 0, 500, time.UTC)},
	})
	require.NoError(t, err)
	require.Equal(t, `{"zebra":"a\/b <c> & \"d\"","apple":"é\n","at":"2026-11-05T12:00:00Z"}`, string(b))
}

func TestHeaderValueJoinsLines(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	_, ok := wire.HeaderValue(r, "X-Thing")
	require.False(t, ok, "absent")

	r.Header.Set("X-Thing", "")
	v, ok := wire.HeaderValue(r, "x-thing")
	require.True(t, ok, "present and empty is present")
	require.Empty(t, v)

	r.Header.Set("X-Thing", "a")
	r.Header.Add("X-Thing", "b")
	v, ok = wire.HeaderValue(r, "x-thing")
	require.True(t, ok)
	require.Equal(t, "a, b", v)
}
