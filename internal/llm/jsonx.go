package llm

import (
	"bytes"
	"encoding/json"
	"lumina/internal/swiftstr"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// decodeObject parses a JSON object. Numbers stay json.Number so a payload's
// values pass through re-encoding untouched.
func decodeObject(data []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil || dec.More() {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

// encodeJSON marshals without Go's HTML escaping, so text reaches the upstream
// as the caller wrote it.
func encodeJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// swiftJSON is JSONSerialization with .sortedKeys: Go sorts map keys too, and
// Foundation also writes "/" as "\/". A slash can only occur inside a JSON
// string, so replacing it after encoding is safe.
func swiftJSON(v any) ([]byte, error) {
	b, err := encodeJSON(v)
	if err != nil {
		return nil, err
	}
	return bytes.ReplaceAll(b, []byte("/"), []byte(`\/`)), nil
}

// objects is `as? [[String: Any]]`: it fails if any element is not an object.
func objects(v any) ([]map[string]any, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}

// stringsOf is `as? [String]`.
func stringsOf(v any) ([]string, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// intOf is `as? Int` on a JSONSerialization number: an integral value in any
// spelling converts, 1.5 does not. Non-JSON ints appear when a body was built
// in Go, not parsed.
func intOf(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case json.Number:
		if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
			return int(i), true
		}
		f, err := strconv.ParseFloat(n.String(), 64)
		if err != nil || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
			return 0, false
		}
		return int(f), true
	}
	return 0, false
}

// doubleOf is `as? Double` on a JSONSerialization number.
func doubleOf(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

func isBlank(s string) bool { return swiftstr.TrimWhitespacesAndNewlines(s) == "" }

// trimWhitespaces is trimmingCharacters(in: .whitespaces): spaces and tabs, not
// newlines.
func trimWhitespaces(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r == '\t' || unicode.Is(unicode.Zs, r) })
}

// swiftLines is record.split(separator: "\n", omittingEmptySubsequences: true).
// A Swift Character "\r\n" is not "\n", so a CRLF does not end a line.
func swiftLines(record string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(record); i++ {
		if record[i] != '\n' || (i > 0 && record[i-1] == '\r') {
			continue
		}
		if i > start {
			lines = append(lines, record[start:i])
		}
		start = i + 1
	}
	if start < len(record) {
		lines = append(lines, record[start:])
	}
	return lines
}
