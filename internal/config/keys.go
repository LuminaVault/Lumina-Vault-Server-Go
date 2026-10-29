package config

import (
	"strings"
	"unicode"
)

// KeyToEnv encodes a swift-configuration key the way its EnvironmentKeyEncoder
// does (see LuminaVaultServer Sources/App/LLM/Routing/ProviderRegistry.swift):
// split on ".", turn camelCase boundaries into "_", uppercase everything.
// "cors.allowedOrigins" → "CORS_ALLOWED_ORIGINS".
//
// An acronym followed by a word ("idleTTLSeconds") also splits before the last
// capital (IDLE_TTL_SECONDS). Not yet verified against the encoder source; see
// docs/migration/decisions.md D-keys.
func KeyToEnv(key string) string {
	var b strings.Builder
	for i, segment := range strings.Split(key, ".") {
		if i > 0 {
			b.WriteByte('_')
		}
		r := []rune(segment)
		for j, c := range r {
			if j > 0 && unicode.IsUpper(c) {
				prev := r[j-1]
				nextLower := j+1 < len(r) && unicode.IsLower(r[j+1])
				if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
					b.WriteByte('_')
				}
			}
			b.WriteRune(unicode.ToUpper(c))
		}
	}
	return b.String()
}
