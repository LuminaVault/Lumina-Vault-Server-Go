package wire

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"
)

// Float64 is a float64 that marshals as JSONEncoder writes a Swift Double: the
// shortest text that round-trips, with an exponent below 0.0001 and above 2^53,
// and no ".0". encoding/json picks other thresholds and writes e-5 for e-05.
type Float64 float64

func (f Float64) MarshalJSON() ([]byte, error) {
	return swiftNumber(float64(f), 64, 1e-4, 1<<53), nil
}

// Float32 is Float64 for a Swift Float, whose upper threshold is 2^24.
type Float32 float32

func (f Float32) MarshalJSON() ([]byte, error) {
	return swiftNumber(float64(f), 32, float64(float32(1e-4)), 1<<24), nil
}

func swiftNumber(f float64, bits int, lower, upper float64) []byte {
	if abs := math.Abs(f); abs != 0 && (abs < lower || abs > upper) {
		return strconv.AppendFloat(nil, f, 'e', -1, bits)
	}
	return strconv.AppendFloat(nil, f, 'f', -1, bits)
}

// nanText is the part of Swift's Double(String) grammar strconv doesn't have: a
// sign, and a payload, whose contents strtod ignores.
var nanText = regexp.MustCompile(`(?i)^[+-]?nan(\(.*\))?$`)

// ParseDouble is Swift's Double(String): false where that returns nil. Past the
// range it gives ±inf or 0, as Swift does, not an error. Unlike strconv it takes
// a hex mantissa with no exponent ("0x10") and a NaN payload ("nan(1)"), and it
// refuses underscores.
func ParseDouble(s string) (float64, bool) {
	if strings.Contains(s, "_") {
		return 0, false
	}
	if nanText.MatchString(s) {
		return math.NaN(), true
	}
	lower := strings.ToLower(strings.TrimLeft(s, "+-"))
	if strings.HasPrefix(lower, "0x") && !strings.Contains(lower, "p") {
		s += "p0"
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange { //nolint:errorlint // ParseFloat returns exactly this
			return v, true
		}
		return 0, false
	}
	return v, true
}

// QueryDouble is `queryParameters[key].flatMap { Double($0) }`.
func QueryDouble(c *echo.Context, key string) (float64, bool) {
	v, ok := QueryParam(c, key)
	if !ok {
		return 0, false
	}
	return ParseDouble(v)
}
