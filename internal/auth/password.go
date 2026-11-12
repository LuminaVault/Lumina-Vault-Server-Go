package auth

import (
	"bytes"

	"golang.org/x/crypto/bcrypt"
)

// PasswordCost is BcryptPasswordHasher's default cost.
const PasswordCost = 12

// bcryptMaxBytes is how much of a password bcrypt reads. HummingbirdBcrypt
// (OpenBSD's C bcrypt) silently uses the first 72 bytes; x/crypto refuses a
// longer password instead, so both sides cut it here to hash what Swift hashes.
const bcryptMaxBytes = 72

// C bcrypt also reads the password with strlen, so it stops at the first NUL;
// Go must cut there too or a password containing \x00 checks differently.
func bcryptKey(password string) []byte {
	b := []byte(password)
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	if len(b) > bcryptMaxBytes {
		b = b[:bcryptMaxBytes]
	}
	return b
}

// HashPassword is BcryptPasswordHasher.hash. x/crypto writes $2a$; Swift writes
// $2b$. Both verify on both servers (TestBcryptPrefixesCrossVerify).
func HashPassword(password string, cost int) (string, error) {
	h, err := bcrypt.GenerateFromPassword(bcryptKey(password), cost)
	return string(h), err
}

// VerifyPassword is BcryptPasswordHasher.verify: a malformed hash is false.
func VerifyPassword(password, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), bcryptKey(password)) == nil
}
