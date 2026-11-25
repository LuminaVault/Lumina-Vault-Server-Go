package githubapp

import (
	"crypto/rsa"
	"errors"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// ParsePrivateKey reads the App's private key from PEM (PKCS#1 or PKCS#8).
// A PEM whose newlines arrived as literal "\n", as some secret stores pass
// multi-line values, is accepted too (spec D16). The error never quotes the key.
func ParsePrivateKey(pem string) (*rsa.PrivateKey, error) {
	text := strings.TrimSpace(pem)
	if !strings.Contains(text, "\n") {
		text = strings.ReplaceAll(text, `\n`, "\n")
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(text))
	if err != nil {
		return nil, errors.New("githubapp: GITHUB_APP_PRIVATE_KEY is not an RSA private key in PEM")
	}
	return key, nil
}
