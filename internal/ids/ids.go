// Package ids converts UUIDs to and from the textual form the Swift server
// uses. Swift's UUID.uuidString is UPPERCASE, and that string appears in JSON
// bodies, JWT sub/jti claims and the SecretBox HKDF salt. Go's uuid.String()
// is lowercase, so never call it at those points; use Upper instead.
package ids

import (
	"errors"

	"github.com/google/uuid"
)

// Upper returns id in the Swift form, e.g. "11111111-2222-3333-4444-55555555ABCD".
func Upper(id uuid.UUID) string {
	const hex = "0123456789ABCDEF"
	var b [36]byte
	j := 0
	for i, v := range id {
		switch i {
		case 4, 6, 8, 10:
			b[j] = '-'
			j++
		}
		b[j] = hex[v>>4]
		b[j+1] = hex[v&0x0f]
		j += 2
	}
	return string(b[:])
}

// ErrNotCanonical is Parse's answer to a form uuid.Parse takes and Swift doesn't.
var ErrNotCanonical = errors.New("ids: not a 36-character UUID string")

// Parse is Swift's UUID(uuidString:): only the 36-character form, in either
// case. uuid.Parse also takes bare hex, braces and a urn: prefix, which Swift
// refuses.
func Parse(s string) (uuid.UUID, error) {
	if len(s) != 36 {
		return uuid.Nil, ErrNotCanonical
	}
	return uuid.Parse(s)
}
