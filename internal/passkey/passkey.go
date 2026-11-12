// Package passkey is LuminaVaultServer's WebAuthnService (Sources/App/Auth/WebAuthnService.swift)
// without its routes: the origin list, the in-memory challenge store, the ceremony
// options and the two ceremony verifications of swift-webauthn 1.0.0-beta.1, the
// library Swift pins (Package.swift:33-34).
//
// The vocabulary (algorithm identifiers, key types, curves, flags, ceremony types,
// conveyance and verification values) is github.com/go-webauthn/webauthn's (O6). The
// checks are not: go-webauthn's ceremony API accepts and refuses different inputs than
// swift-webauthn does (it verifies packed, tpm and the other attestation formats, warns
// on a sign count that goes backwards instead of failing, checks the backup flags,
// hashes RSA by algorithm, supports EdDSA), and every one of those differences would
// change which passkey ceremonies succeed. So the verification here is swift-webauthn's,
// step for step, including what it gets wrong. Each kept weakness is marked "Swift:".
//
// Every verification failure is one error: Swift throws the library's own error types,
// which Hummingbird answers with a bare 500, so which check failed never reaches a client.
package passkey

import (
	"encoding/base64"
	"strings"

	"lumina/internal/swiftstr"
)

// ParseOrigins is WebAuthnService.parseOrigins: comma separated, each entry trimmed of
// whitespace and newlines, blanks dropped, duplicates dropped keeping the first.
func ParseOrigins(raw string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = swiftstr.TrimWhitespacesAndNewlines(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return out
}

// Config is WebAuthnService's configuration. A zero RPID or no origins is
// "not configured": the routes mount but every ceremony is a 503.
type Config struct {
	RPID    string
	RPName  string
	Origins []string
}

// Configured is WebAuthnService.isConfigured (with enabled checked by the caller):
// a manager exists for at least one non-empty origin.
func (c Config) Configured() bool {
	if c.RPID == "" {
		return false
	}
	for _, o := range c.Origins {
		if o != "" {
			return true
		}
	}
	return false
}

func (c Config) acceptsOrigin(origin string) bool {
	for _, o := range c.Origins {
		if o != "" && o == origin {
			return true
		}
	}
	return false
}

// URLEncode is swift-webauthn's base64URLEncodedString: base64url without padding.
func URLEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// URLDecoded is URLEncodedBase64.urlDecoded: "-" to "+", "_" to "/", then "=" until the
// length is a multiple of four. It accepts standard base64 as well, and pads a string
// that already has padding. The length is Swift's count: Characters.
func URLDecoded(s string) string {
	s = strings.NewReplacer("-", "+", "_", "/").Replace(s)
	for swiftstr.Count(s)%4 != 0 {
		s += "="
	}
	return s
}

// DecodeBytes is URLEncodedBase64.decodedBytes: URLDecoded, then Foundation's
// Data(base64Encoded:), which refuses any character outside the alphabet (Go's decoder
// would skip CR and LF).
func DecodeBytes(s string) ([]byte, bool) {
	padded := URLDecoded(s)
	if strings.ContainsAny(padded, "\r\n") {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(padded)
	if err != nil {
		return nil, false
	}
	return b, true
}
