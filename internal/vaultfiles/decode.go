package vaultfiles

import (
	"strings"
	"unicode/utf8"
)

// DecodeUTF8 is `String(data:encoding: .utf8)` and `String(contentsOf:encoding:
// .utf8)`, which on Linux drop one leading byte order mark (parity: vault/05-notes,
// vault/07-index).
func DecodeUTF8(data []byte) (string, bool) {
	if !utf8.Valid(data) {
		return "", false
	}
	return strings.TrimPrefix(string(data), "\ufeff"), true
}
