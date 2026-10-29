// Package secretbox is the Go twin of LuminaVaultServer's SecretBox
// (Sources/App/Crypto/SecretBox.swift). It must open every value Swift sealed
// and seal values Swift can open, so the format is fixed:
//
//   - key   = HKDF-SHA256(ikm = 32-byte master key, salt = UTF-8 of the tenant
//     UUID in UPPERCASE, info = "lv.secretbox.v1"), 32 bytes
//   - nonce = 12 random bytes, stored in its own BYTEA column
//   - ciphertext column = AES-256-GCM ciphertext ‖ 16-byte tag, no AAD
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"lumina/internal/ids"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/crypto/hkdf"
)

var (
	ErrMasterKeyMissing   = errors.New("secretbox: LV_SECRET_MASTER_KEY is empty")
	ErrMasterKeyMalformed = errors.New("secretbox: LV_SECRET_MASTER_KEY must be base64 of 32 bytes")
	ErrDecryptionFailed   = errors.New("secretbox: decryption failed")
)

const (
	keySize   = 32
	nonceSize = 12
	tagSize   = 16
)

var hkdfInfo = []byte("lv.secretbox.v1")

// Sealed is the persisted form: two BYTEA columns.
type Sealed struct {
	Ciphertext []byte
	Nonce      []byte
}

type Box struct {
	masterKey []byte
}

func New(masterKeyB64 string) (*Box, error) {
	if masterKeyB64 == "" {
		return nil, ErrMasterKeyMissing
	}
	raw, err := base64.StdEncoding.DecodeString(masterKeyB64)
	if err != nil || len(raw) != keySize {
		return nil, ErrMasterKeyMalformed
	}
	return &Box{masterKey: raw}, nil
}

func (b *Box) Seal(plaintext string, tenantID uuid.UUID) (Sealed, error) {
	aead, err := b.aead(tenantID)
	if err != nil {
		return Sealed{}, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Sealed{}, err
	}
	return Sealed{Ciphertext: aead.Seal(nil, nonce, []byte(plaintext), nil), Nonce: nonce}, nil
}

// Open returns ErrDecryptionFailed for a wrong tenant, a tampered value, a
// short ciphertext, a bad nonce or non-UTF-8 plaintext, as Swift does.
func (b *Box) Open(s Sealed, tenantID uuid.UUID) (string, error) {
	if len(s.Ciphertext) < tagSize || len(s.Nonce) != nonceSize {
		return "", ErrDecryptionFailed
	}
	aead, err := b.aead(tenantID)
	if err != nil {
		return "", err
	}
	plain, err := aead.Open(nil, s.Nonce, s.Ciphertext, nil)
	if err != nil || !utf8.Valid(plain) {
		return "", ErrDecryptionFailed
	}
	return string(plain), nil
}

func (b *Box) aead(tenantID uuid.UUID) (cipher.AEAD, error) {
	key, err := deriveKey(b.masterKey, []byte(ids.Upper(tenantID)))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func deriveKey(masterKey, salt []byte) ([]byte, error) {
	key := make([]byte, keySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, masterKey, salt, hkdfInfo), key); err != nil {
		return nil, err
	}
	return key, nil
}
