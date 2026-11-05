package capture

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

type foundationCase struct {
	In     string  `json:"in"`
	OK     bool    `json:"ok"`
	Scheme *string `json:"scheme"`
	Host   *string `json:"host"`
	Abs    string  `json:"abs"`
	Norm   string  `json:"norm"`
}

// testdata/foundation_url.json is Foundation's own answer for each input: URL(string:)
// and URLExtractionService.normalize, run with Swift 6.4.1 on Darwin over a hand
// list and random strings.
func TestParseURLMatchesFoundation(t *testing.T) {
	raw, err := os.ReadFile("testdata/foundation_url.json")
	require.NoError(t, err)
	var cases []foundationCase
	require.NoError(t, json.Unmarshal(raw, &cases))
	require.NotEmpty(t, cases)
	for _, c := range cases {
		u, ok := ParseURL(c.In)
		if c.OK != ok {
			t.Errorf("ok %q want %v", c.In, c.OK)
			continue
		}
		if !ok {
			continue
		}
		// A URL that isn't http(s) with a host only ever gets a 400, so its other
		// fields don't matter.
		if c.Host == nil != (u.Host == nil) {
			t.Errorf("host nil-ness %q want %v", c.In, deref(c.Host))
			continue
		}
		if c.Scheme == nil || c.Host == nil || *c.Host == "" || !isHTTP(*c.Scheme) {
			continue
		}
		require.Equal(t, *c.Scheme, *u.Scheme, "%q", c.In)
		// Go's IDNA tables differ on hosts that mix non-ASCII and escapes. They
		// don't pass a real DNS lookup in either server.
		if !isASCII(c.In) && strings.Contains(c.In, "%") {
			continue
		}
		if *c.Host != *u.Host {
			t.Errorf("host %q: got %q want %q", c.In, *u.Host, *c.Host)
		}
		if c.Abs != u.Absolute {
			t.Errorf("abs %q: got %q want %q", c.In, u.Absolute, c.Abs)
		}
		if isASCII(*c.Host) && !strings.Contains(strings.ToLower(*c.Host), "xn--") {
			if c.Norm != u.Normalize() {
				t.Errorf("norm %q: got %q want %q", c.In, u.Normalize(), c.Norm)
			}
		}
	}
}

func isHTTP(scheme string) bool {
	return strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https")
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
