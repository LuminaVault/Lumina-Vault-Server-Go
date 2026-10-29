// Package ids converts UUIDs to and from the textual form the Swift server
// uses. Swift's UUID.uuidString is UPPERCASE, and that string appears in JSON
// bodies, JWT sub/jti claims and the SecretBox HKDF salt. Go's uuid.String()
// is lowercase, so never call it at those points; use Upper instead.
package ids

import "github.com/google/uuid"

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

// Parse accepts either case, as Swift's UUID(uuidString:) does.
func Parse(s string) (uuid.UUID, error) {
	return uuid.Parse(s)
}
