package authservice

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
)

// CodeGenerator is OTPCodeGenerator.
type CodeGenerator interface {
	Generate() string
}

// DefaultCodeGenerator is DefaultOTPCodeGenerator: six digits, zero padded,
// from the system CSPRNG (Swift's Int.random uses SystemRandomNumberGenerator).
type DefaultCodeGenerator struct{}

var million = big.NewInt(1_000_000)

func (DefaultCodeGenerator) Generate() string {
	n, err := rand.Int(rand.Reader, million)
	if err != nil {
		// crypto/rand does not fail on supported platforms.
		panic(err)
	}
	return fmt.Sprintf("%06d", n.Int64())
}

// FixedCodeGenerator is FixedOTPCodeGenerator: always Code. Tests only.
type FixedCodeGenerator struct{ Code string }

func (g FixedCodeGenerator) Generate() string { return g.Code }

// sha256Hex is AuthService.sha256Hex and MFAService.sha256Hex: lowercase hex
// SHA-256 of the UTF-8 code. Codes are stored only as this.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
