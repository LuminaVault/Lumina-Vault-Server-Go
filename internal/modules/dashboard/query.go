package dashboard

import (
	"strings"

	"github.com/labstack/echo/v5"
)

// queryParam is Hummingbird's `request.uri.queryParameters[key]`, which is
// not net/url's parsing: pairs split on "&", the key is compared raw (not
// decoded), the first match wins, "+" stays "+", and the value is
// percent-decoded, falling back to the raw text when the escapes are
// malformed. A key without "=" has the value "". Go's url.Values would turn
// "from=…T09:00:00+02:00" into a space and drop malformed pairs.
func queryParam(rawQuery, key string) (string, bool) {
	if rawQuery == "" {
		return "", false
	}
	for _, pair := range strings.Split(rawQuery, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		if k != key {
			continue
		}
		if decoded, ok := percentDecode(v); ok {
			return decoded, true
		}
		return v, true
	}
	return "", false
}

// percentDecode is HummingbirdCore's removingURLPercentEncoding: every "%"
// must be followed by two hex digits, or the whole value fails. Invalid UTF-8
// in the result is replaced, as String(decoding:as: UTF8.self) does.
func percentDecode(s string) (string, bool) {
	if !strings.Contains(s, "%") {
		return s, true
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			out = append(out, s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", false
		}
		hi, ok1 := unhex(s[i+1])
		lo, ok2 := unhex(s[i+2])
		if !ok1 || !ok2 {
			return "", false
		}
		out = append(out, hi<<4|lo)
		i += 2
	}
	return strings.ToValidUTF8(string(out), "�"), true
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// query is the request's parameter as Swift reads it, "" when absent.
func query(c *echo.Context, key string) string {
	v, _ := queryParam(c.Request().URL.RawQuery, key)
	return v
}
