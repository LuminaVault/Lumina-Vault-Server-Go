package authservice

import (
	"regexp"
	"strings"

	"lumina/internal/swiftstr"

	"github.com/google/uuid"
)

// usernamePattern is UsernamePolicy.pattern. NSRegularExpression's `$` also
// matches before a final line terminator, but normalize has trimmed those.
var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,30}$`)

// reservedUsernames is UsernamePolicy.reserved.
var reservedUsernames = map[string]bool{
	"admin": true, "root": true, "hermes": true, "system": true, "support": true, "api": true, "www": true, "luminavault": true,
}

// NormalizeUsername is UsernamePolicy.normalize: trim whitespace and
// newlines, then lowercase.
func NormalizeUsername(raw string) string {
	return swiftstr.Lowercased(swiftstr.TrimWhitespacesAndNewlines(raw))
}

// ValidateUsername is UsernamePolicy.validate: the normalized name, or
// ErrInvalidUsername.
func ValidateUsername(raw string) (string, error) {
	s := NormalizeUsername(raw)
	if !usernamePattern.MatchString(s) || reservedUsernames[s] {
		return "", ErrInvalidUsername
	}
	return s, nil
}

// PlaceholderUsername is UsernamePolicy.placeholder: "oauth-" and the first 8
// hex digits of a random UUID, lowercased. 8B's OAuth upsert uses it.
func PlaceholderUsername() string {
	return "oauth-" + strings.ToLower(strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
}
