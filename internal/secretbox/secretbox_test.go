package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// 32 bytes of 0x01, the master key Task 0.3 uses for the golden fixture.
var testKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\x01", 32)))

var tenant = uuid.MustParse("abcdef01-2345-4678-9abc-def012345678")

func TestOpensSwiftSealedGolden(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/golden/secretbox.json")
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("golden fixture not captured yet (Phase 0 Task 0.3)")
	}
	require.NoError(t, err)
	var g struct {
		MasterKeyB64  string `json:"masterKeyB64"`
		TenantID      string `json:"tenantID"`
		Plaintext     string `json:"plaintext"`
		CiphertextB64 string `json:"ciphertextB64"`
		NonceB64      string `json:"nonceB64"`
	}
	require.NoError(t, json.Unmarshal(raw, &g))
	box, err := New(g.MasterKeyB64)
	require.NoError(t, err)
	ct, _ := base64.StdEncoding.DecodeString(g.CiphertextB64)
	nonce, _ := base64.StdEncoding.DecodeString(g.NonceB64)
	got, err := box.Open(Sealed{Ciphertext: ct, Nonce: nonce}, uuid.MustParse(g.TenantID))
	require.NoError(t, err)
	require.Equal(t, g.Plaintext, got)
}

func TestRoundTrip(t *testing.T) {
	box, err := New(testKey)
	require.NoError(t, err)
	s, err := box.Seal("sk-golden", tenant)
	require.NoError(t, err)
	require.Len(t, s.Nonce, 12)
	require.Len(t, s.Ciphertext, len("sk-golden")+16)
	got, err := box.Open(s, tenant)
	require.NoError(t, err)
	require.Equal(t, "sk-golden", got)
}

func TestWrongTenantFails(t *testing.T) {
	box, _ := New(testKey)
	s, _ := box.Seal("sk-golden", tenant)
	_, err := box.Open(s, uuid.New())
	require.ErrorIs(t, err, ErrDecryptionFailed)
}

func TestTamperedAndShortFail(t *testing.T) {
	box, _ := New(testKey)
	s, _ := box.Seal("sk-golden", tenant)
	s.Ciphertext[0] ^= 0xff
	_, err := box.Open(s, tenant)
	require.ErrorIs(t, err, ErrDecryptionFailed)
	_, err = box.Open(Sealed{Ciphertext: []byte{1, 2, 3}, Nonce: s.Nonce}, tenant)
	require.ErrorIs(t, err, ErrDecryptionFailed)
	_, err = box.Open(Sealed{Ciphertext: s.Ciphertext, Nonce: []byte{1}}, tenant)
	require.ErrorIs(t, err, ErrDecryptionFailed)
}

// Documents the trap: a key derived from the lowercase uuid.String() salt
// cannot open what the uppercase (Swift) salt sealed.
func TestLowercaseSaltWouldFail(t *testing.T) {
	box, _ := New(testKey)
	s, _ := box.Seal("sk-golden", tenant)
	key, err := deriveKey(box.masterKey, []byte(tenant.String()))
	require.NoError(t, err)
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	_, err = aead.Open(nil, s.Nonce, s.Ciphertext, nil)
	require.Error(t, err)
}

func TestNewRejectsBadKeys(t *testing.T) {
	_, err := New("")
	require.ErrorIs(t, err, ErrMasterKeyMissing)
	_, err = New("not base64!")
	require.ErrorIs(t, err, ErrMasterKeyMalformed)
	_, err = New(base64.StdEncoding.EncodeToString([]byte("short")))
	require.ErrorIs(t, err, ErrMasterKeyMalformed)
}
