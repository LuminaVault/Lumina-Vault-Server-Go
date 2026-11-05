package wire_test

import (
	"encoding/json"
	"lumina/internal/wire"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// The expected text is what JSONEncoder wrote on Swift 6.4.1.
func TestFloatsMarshalAsSwiftWrites(t *testing.T) {
	floats, err := json.Marshal([]wire.Float32{0.1, 0, 1, 5.9604645e-08, 0.00001, 0.0001, 1.5e7, 16777216, 33554432, 1e10, 1.2345678e-5, -0.25})
	require.NoError(t, err)
	require.Equal(t, `[0.1,0,1,5.9604645e-08,1e-05,0.0001,15000000,16777216,3.3554432e+07,1e+10,1.2345678e-05,-0.25]`, string(floats))
	doubles, err := json.Marshal([]wire.Float64{0.72, 1, 1e-5, 0.0001, 1e15, 1e16, 1.8014398509481984e16, 3.6e16})
	require.NoError(t, err)
	require.Equal(t, `[0.72,1,1e-05,0.0001,1000000000000000,1e+16,1.8014398509481984e+16,3.6e+16]`, string(doubles))
}

// The table is Double(String) on Swift 6.4.1.
func TestParseDoubleIsSwiftsDouble(t *testing.T) {
	for in, want := range map[string]float64{
		"0.1": 0.1, "+.5": 0.5, "5.": 5, "1e5": 1e5, "1E5": 1e5,
		"0x10": 16, "0X10": 16, "-0x10": -16, "0x1.8": 1.5, "0x1p-1": 0.5,
		"inf": math.Inf(1), "INF": math.Inf(1), "infinity": math.Inf(1), "-Infinity": math.Inf(-1),
		"1e999": math.Inf(1), "1e-999": 0,
	} {
		got, ok := wire.ParseDouble(in)
		require.True(t, ok, in)
		require.Equal(t, want, got, in) //nolint:testifylint // exact on purpose
	}
	for _, in := range []string{"nan", "Nan", "NaN", "-nan", "nan()", "nan(0x1)", "+nan(abc)", "nan(a b)"} {
		got, ok := wire.ParseDouble(in)
		require.True(t, ok, in)
		require.True(t, math.IsNaN(got), in)
	}
	for _, in := range []string{"", " 1", "1 ", "abc", "1_000", "0x_1p1", "0x", "0xg", "1e", ".", "nan(", "١"} {
		_, ok := wire.ParseDouble(in)
		require.False(t, ok, in)
	}
}
