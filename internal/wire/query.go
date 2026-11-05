package wire

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/labstack/echo/v5"
)

// RawPath is Hummingbird's request.uri.path: the path as sent, up to the first
// "?", with percent escapes intact. Hummingbird routes on this and never decodes
// it, so its path parameters, ":id" and "**" alike, keep their escapes.
func RawPath(r *http.Request) string {
	if strings.HasPrefix(r.RequestURI, "/") {
		path, _, _ := strings.Cut(r.RequestURI, "?")
		return path
	}
	return r.URL.EscapedPath()
}

// PathSegments is RawPath split as Hummingbird's router splits it: empty
// segments don't count.
func PathSegments(r *http.Request) []string {
	return strings.FieldsFunc(RawPath(r), func(c rune) bool { return c == '/' })
}

// QueryValue is Hummingbird's `request.uri.queryParameters[key]` over a raw
// query string, which is not net/url's parsing. Pairs split on "&" and the
// query ends at "#". The key is compared raw, never decoded, and the first
// match wins. A "+" stays a "+", where url.Values would make it a space, so
// "from=...T09:00:00+02:00" survives. The value is percent-decoded, falling
// back to the raw text when an escape is malformed, where url.Values would
// drop the pair. A key without "=" has the value "".
func QueryValue(rawQuery, key string) (string, bool) {
	rawQuery, _, _ = strings.Cut(rawQuery, "#")
	if rawQuery == "" {
		return "", false
	}
	for pair := range strings.SplitSeq(rawQuery, "&") {
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

// QueryParam is QueryValue for the current request: the value, and whether the
// parameter is present at all (Swift tells `?cursor=` from no cursor).
func QueryParam(c *echo.Context, key string) (string, bool) {
	return QueryValue(c.Request().URL.RawQuery, key)
}

// Query is QueryParam with "" for an absent parameter.
func Query(c *echo.Context, key string) string {
	v, _ := QueryParam(c, key)
	return v
}

// QueryInt is `queryParameters[key].flatMap { Int($0) }`: false when the
// parameter is absent or is not a base-10 Int (an optional sign, ASCII digits,
// in range).
func QueryInt(c *echo.Context, key string) (int, bool) {
	v, ok := QueryParam(c, key)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// percentDecode is HummingbirdCore's removingURLPercentEncoding: every "%" must
// be followed by two hex digits, or the whole value fails. Invalid UTF-8 in the
// result is repaired as String(decoding:as: UTF8.self) repairs it.
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
	return repairUTF8(out), true
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

// repairUTF8 writes one U+FFFD per maximal ill-formed subsequence, which is
// what Swift's String(decoding:as:) does (checked against swiftc: FF FF is two,
// E2 82 is one, ED A0 80 is three). strings.ToValidUTF8 would merge a whole run
// into one.
func repairUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out strings.Builder
	out.Grow(len(b))
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			out.Write(b[i : i+size])
			i += size
			continue
		}
		out.WriteRune(utf8.RuneError)
		i += illFormedLen(b[i:])
	}
	return out.String()
}

// illFormedLen is the length of the ill-formed subsequence at the start of b:
// the lead byte plus every continuation byte that was still legal for it.
func illFormedLen(b []byte) int {
	lead := b[0]
	var need int
	lo, hi := byte(0x80), byte(0xBF)
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 1
	case lead >= 0xE0 && lead <= 0xEF:
		need = 2
		switch lead {
		case 0xE0:
			lo = 0xA0
		case 0xED:
			hi = 0x9F
		}
	case lead >= 0xF0 && lead <= 0xF4:
		need = 3
		switch lead {
		case 0xF0:
			lo = 0x90
		case 0xF4:
			hi = 0x8F
		}
	default:
		return 1
	}
	n := 1
	for n <= need && n < len(b) && b[n] >= lo && b[n] <= hi {
		lo, hi = 0x80, 0xBF
		n++
	}
	return n
}
