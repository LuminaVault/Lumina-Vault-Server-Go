package wire_test

import (
	"lumina/internal/wire"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestQueryValue(t *testing.T) {
	for _, c := range []struct {
		raw, key, want string
		ok             bool
	}{
		{"a=1&b=2", "b", "2", true},
		{"a=1&a=2", "a", "1", true},
		{"a", "a", "", true},
		{"a=", "a", "", true},
		{"a=x+y", "a", "x+y", true},
		{"a=x%2By%20z", "a", "x+y z", true},
		{"a=100%", "a", "100%", true},
		{"a=%zz", "a", "%zz", true},
		{"a=%4", "a", "%4", true},
		{"%61=1", "a", "", false},
		{"", "a", "", false},
		{"&&a=3", "a", "3", true},
		{"a=3&", "a", "3", true},
		{"b=1=2", "b", "1=2", true},
		{"a=1#frag", "a", "1", true},
		{"b=1#a=2", "a", "", false},
		{"from=2026-01-01T09:00:00+02:00", "from", "2026-01-01T09:00:00+02:00", true},
		{"limit=+5", "limit", "+5", true},
		{"a=%E2%82%AC", "a", "€", true},
		{"A=1", "a", "", false},
		{"a;b=1", "a", "", false},
	} {
		got, ok := wire.QueryValue(c.raw, c.key)
		require.Equal(t, c.ok, ok, c.raw)
		require.Equal(t, c.want, got, c.raw)
	}
}

// The expected values were printed by swiftc from String(decoding:as: UTF8.self).
func TestQueryValueRepairsUTF8AsSwiftDoes(t *testing.T) {
	const fffd = "�"
	for raw, want := range map[string]string{
		"%FF%FF":       fffd + fffd,
		"%E2%82":       fffd,
		"%F0%9F%98":    fffd,
		"%C0%80":       fffd + fffd,
		"%ED%A0%80":    fffd + fffd + fffd,
		"A%E2%82A":     "A" + fffd + "A",
		"%F4%90%80%80": fffd + fffd + fffd + fffd,
		"%E2A":         fffd + "A",
		"%80%80":       fffd + fffd,
		"%F0%9FA":      fffd + "A",
		"%F0%9F%98%80": "\U0001F600",
	} {
		got, ok := wire.QueryValue("a="+raw, "a")
		require.True(t, ok, raw)
		require.Equal(t, want, got, raw)
	}
}

func TestQueryHelpers(t *testing.T) {
	ctx := func(target string) *echo.Context {
		return echo.New().NewContext(httptest.NewRequest(http.MethodGet, target, nil), httptest.NewRecorder())
	}
	c := ctx("/x?limit=+5&bad=1_0&big=99999999999999999999&neg=-3&space=%205&cursor=")

	for key, want := range map[string]int{"limit": 5, "neg": -3} {
		n, ok := wire.QueryInt(c, key)
		require.True(t, ok, key)
		require.Equal(t, want, n, key)
	}
	for _, key := range []string{"bad", "big", "space", "cursor", "absent"} {
		_, ok := wire.QueryInt(c, key)
		require.False(t, ok, key)
	}

	v, ok := wire.QueryParam(c, "cursor")
	require.True(t, ok)
	require.Empty(t, v)
	_, ok = wire.QueryParam(c, "absent")
	require.False(t, ok)
	require.Equal(t, "+5", wire.Query(c, "limit"))
	require.Empty(t, wire.Query(c, "absent"))
}
