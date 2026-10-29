// Package auth verifies the session JWTs the Swift server mints. Go does not
// mint session tokens until wave 8 (roadmap); until then it only has to accept
// exactly what Swift accepts and reject exactly what Swift rejects.
package auth

import (
	"fmt"
	"lumina/internal/config"
	"strings"
)

const minSecretLength = 32

// Key is one HS256 verification key. Kid may be empty for the legacy
// single-key configuration.
type Key struct {
	Kid    string
	Secret []byte
}

// Keyset holds the verification keys in configured order. The first key is
// the active signer, which also verifies tokens without a kid header.
type Keyset struct {
	keys []Key
}

// LoadKeys mirrors Sources/App/Auth/JWT/JWTKeyLoader.swift: JWT_HMAC_SECRETS
// (csv of kid:secret, first entry active) wins; otherwise JWT_HMAC_SECRET with
// the optional JWT_KID.
func LoadKeys(cfg config.Config) (Keyset, error) {
	entries, err := parseSecrets(cfg.JWTHMACSecrets)
	if err != nil {
		return Keyset{}, err
	}
	if len(entries) == 0 {
		if cfg.JWTHMACSecret == "" {
			return Keyset{}, fmt.Errorf("auth: neither JWT_HMAC_SECRETS nor JWT_HMAC_SECRET is set")
		}
		entries = []Key{{Kid: strings.TrimSpace(cfg.JWTKID), Secret: []byte(cfg.JWTHMACSecret)}}
	}
	return Keyset{keys: entries}, nil
}

// NewKeyset builds a Keyset directly; used by tests and the parity harness.
func NewKeyset(keys ...Key) Keyset {
	return Keyset{keys: keys}
}

// Active returns the signing key (first configured).
func (k Keyset) Active() Key {
	return k.keys[0]
}

// lookup picks the verifying key: the matching kid when the token names one,
// the active key when it does not. An unknown kid verifies nothing.
func (k Keyset) lookup(kid string) (Key, bool) {
	if len(k.keys) == 0 {
		return Key{}, false
	}
	if kid == "" {
		return k.keys[0], true
	}
	for _, key := range k.keys {
		if key.Kid == kid {
			return key, true
		}
	}
	return Key{}, false
}

func parseSecrets(csv string) ([]Key, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil, nil
	}
	var keys []Key
	seen := map[string]bool{}
	for _, raw := range strings.Split(csv, ",") {
		entry := strings.TrimSpace(raw)
		kid, secret, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("malformed JWT_HMAC_SECRETS entry (expected kid:secret): '%s'", entry)
		}
		kid, secret = strings.TrimSpace(kid), strings.TrimSpace(secret)
		if kid == "" {
			return nil, fmt.Errorf("JWT_HMAC_SECRETS entry has empty kid")
		}
		// Swift counts Characters; secrets are ASCII in practice, so runes match.
		if n := len([]rune(secret)); n < minSecretLength {
			return nil, fmt.Errorf("JWT_HMAC_SECRETS kid='%s' secret length=%d (minimum 32)", kid, n)
		}
		if seen[kid] {
			return nil, fmt.Errorf("JWT_HMAC_SECRETS contains duplicate kid '%s'", kid)
		}
		seen[kid] = true
		keys = append(keys, Key{Kid: kid, Secret: []byte(secret)})
	}
	return keys, nil
}
