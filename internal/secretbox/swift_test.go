package secretbox

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ports SecretBoxTests (Tests/AppTests/Crypto/SecretBoxTests.swift), all 12 cases.
// Swift's SecretBox.Error is ErrMasterKeyMissing or ErrMasterKeyMalformed from New
// and ErrDecryptionFailed from Open.
func TestSecretBoxTests(t *testing.T) {
	zeroKey := base64.StdEncoding.EncodeToString(make([]byte, 32)) // validKeyB64
	box := func(t *testing.T) *Box {
		t.Helper()
		b, err := New(zeroKey)
		require.NoError(t, err)
		return b
	}
	seal := func(t *testing.T, b *Box, plaintext string, tenant uuid.UUID) Sealed {
		t.Helper()
		s, err := b.Seal(plaintext, tenant)
		require.NoError(t, err)
		return s
	}

	t.Run("init rejects empty master key", func(t *testing.T) {
		_, err := New("")
		require.ErrorIs(t, err, ErrMasterKeyMissing)
	})
	t.Run("init rejects malformed base64", func(t *testing.T) {
		_, err := New("not-base-64!!")
		require.ErrorIs(t, err, ErrMasterKeyMalformed)
	})
	t.Run("init rejects a key of the wrong length", func(t *testing.T) {
		_, err := New(base64.StdEncoding.EncodeToString(make([]byte, 16)))
		require.ErrorIs(t, err, ErrMasterKeyMalformed)
	})

	t.Run("seal then open returns the plaintext", func(t *testing.T) {
		b, tenant := box(t), uuid.New()
		got, err := b.Open(seal(t, b, "sk-live-abc123", tenant), tenant)
		require.NoError(t, err)
		assert.Equal(t, "sk-live-abc123", got)
	})
	t.Run("round trip survives unicode and empty strings", func(t *testing.T) {
		b, tenant := box(t), uuid.New()
		for _, plaintext := range []string{"", "Bearer ünïcødé 🔑", strings.Repeat("x", 8192)} {
			got, err := b.Open(seal(t, b, plaintext, tenant), tenant)
			require.NoError(t, err)
			assert.Equal(t, plaintext, got)
		}
	})
	t.Run("the same plaintext never seals to the same bytes twice", func(t *testing.T) {
		b, tenant := box(t), uuid.New()
		first, second := seal(t, b, "same", tenant), seal(t, b, "same", tenant)
		assert.NotEqual(t, first.Ciphertext, second.Ciphertext)
		assert.NotEqual(t, first.Nonce, second.Nonce)
	})

	t.Run("a credential sealed for one tenant does not open for another", func(t *testing.T) {
		b := box(t)
		sealed := seal(t, b, "sk-live-secret", uuid.New())
		_, err := b.Open(sealed, uuid.New())
		require.ErrorIs(t, err, ErrDecryptionFailed)
	})
	t.Run("different tenants produce different ciphertexts", func(t *testing.T) {
		b := box(t)
		assert.NotEqual(t, seal(t, b, "same", uuid.New()).Ciphertext, seal(t, b, "same", uuid.New()).Ciphertext)
	})

	t.Run("a flipped ciphertext bit fails authentication", func(t *testing.T) {
		b, tenant := box(t), uuid.New()
		sealed := seal(t, b, "sk-live-abc123", tenant)
		flipped := bytes.Clone(sealed.Ciphertext)
		flipped[0] ^= 0x01
		_, err := b.Open(Sealed{Ciphertext: flipped, Nonce: sealed.Nonce}, tenant)
		require.ErrorIs(t, err, ErrDecryptionFailed)
	})
	t.Run("a swapped nonce fails authentication", func(t *testing.T) {
		b, tenant := box(t), uuid.New()
		first, second := seal(t, b, "first", tenant), seal(t, b, "second", tenant)
		_, err := b.Open(Sealed{Ciphertext: first.Ciphertext, Nonce: second.Nonce}, tenant)
		require.ErrorIs(t, err, ErrDecryptionFailed)
	})
	t.Run("a truncated ciphertext is rejected", func(t *testing.T) {
		b, tenant := box(t), uuid.New()
		sealed := seal(t, b, "sk-live-abc123", tenant)
		_, err := b.Open(Sealed{Ciphertext: sealed.Ciphertext[:8], Nonce: sealed.Nonce}, tenant)
		require.ErrorIs(t, err, ErrDecryptionFailed)
	})
	t.Run("a different master key cannot open the ciphertext", func(t *testing.T) {
		tenant := uuid.New()
		sealed := seal(t, box(t), "sk-live-abc123", tenant)
		other, err := New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
		require.NoError(t, err)
		_, err = other.Open(sealed, tenant)
		require.ErrorIs(t, err, ErrDecryptionFailed)
	})
}
