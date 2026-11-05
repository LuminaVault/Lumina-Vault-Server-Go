package wire

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// MarshalFoundation encodes v as a default Foundation JSONEncoder does: compact,
// no HTML escaping, "/" written as "\/". Keys come out in struct order. Go also
// writes U+2028 and U+2029 as \u escapes where Foundation writes them raw; a JSON
// decoder can't tell the two apart.
func MarshalFoundation(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.ReplaceAll(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), []byte("/"), []byte(`\/`)), nil
}

// HeaderValue is HTTPFields' subscript: every line of the header joined with
// ", ", and false when there is none. Header.Get would see only the first line.
func HeaderValue(r *http.Request, name string) (string, bool) {
	values := r.Header.Values(name)
	if len(values) == 0 {
		return "", false
	}
	return strings.Join(values, ", "), true
}
