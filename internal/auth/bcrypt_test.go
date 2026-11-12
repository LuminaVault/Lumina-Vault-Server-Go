package auth

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// The hash Swift's HummingbirdBcrypt wrote for the bootstrap admin must verify
// in Go (wave 8 moves password login), at the cost Swift uses.
func TestBcryptGolden(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/golden/bcrypt.json")
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("golden fixture not captured yet (Phase 0 Task 0.3)")
	}
	require.NoError(t, err)
	var g struct{ Password, Hash string }
	require.NoError(t, json.Unmarshal(raw, &g))
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(g.Hash), []byte(g.Password)))
	require.Error(t, bcrypt.CompareHashAndPassword([]byte(g.Hash), []byte(g.Password+"x")))
	cost, err := bcrypt.Cost([]byte(g.Hash))
	require.NoError(t, err)
	require.Equal(t, 12, cost)
}

// C bcrypt reads the password with strlen, so HummingbirdBcrypt ignores
// everything from the first NUL; Go must hash and verify the same prefix.
func TestBcryptStopsAtNUL(t *testing.T) {
	h, err := HashPassword("abc\x00ignored-tail", 4)
	require.NoError(t, err)
	require.True(t, VerifyPassword("abc", h))
	require.True(t, VerifyPassword("abc\x00other", h))
	require.False(t, VerifyPassword("abcd", h))
}
