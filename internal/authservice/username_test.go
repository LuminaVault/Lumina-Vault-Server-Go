package authservice_test

import (
	"lumina/internal/authservice"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUsernamePolicy ports UsernamePolicyTests
// (Tests/AppTests/Auth/UsernamePolicyTests.swift), all 6 cases.
func TestUsernamePolicy(t *testing.T) {
	invalid := func(t *testing.T, raw string) {
		t.Helper()
		_, err := authservice.ValidateUsername(raw)
		assert.ErrorIs(t, err, authservice.ErrInvalidUsername, raw)
	}

	t.Run("validates good slug", func(t *testing.T) {
		for raw, want := range map[string]string{"alice": "alice", "john-doe": "john-doe", "a1b2c3": "a1b2c3", "  Alice  ": "alice"} {
			got, err := authservice.ValidateUsername(raw)
			require.NoError(t, err, raw)
			assert.Equal(t, want, got)
		}
	})

	t.Run("rejects too short", func(t *testing.T) { invalid(t, "ab") })

	t.Run("rejects too long", func(t *testing.T) {
		invalid(t, strings.Repeat("a", 32))
		got, err := authservice.ValidateUsername(strings.Repeat("a", 31))
		require.NoError(t, err)
		assert.Len(t, got, 31)
	})

	t.Run("rejects invalid chars", func(t *testing.T) {
		for _, raw := range []string{"with space", "under_score", "dot.dot", "-leading"} {
			invalid(t, raw)
		}
	})

	t.Run("rejects reserved", func(t *testing.T) {
		for _, word := range []string{"admin", "root", "hermes", "system", "support", "api", "www", "luminavault"} {
			invalid(t, word)
		}
	})

	t.Run("placeholder matches pattern", func(t *testing.T) {
		placeholder := authservice.PlaceholderUsername()
		got, err := authservice.ValidateUsername(placeholder)
		require.NoError(t, err)
		assert.Equal(t, placeholder, got)
		assert.True(t, strings.HasPrefix(placeholder, "oauth-"))
	})

	// Swift's normalize is trimmingCharacters(.whitespacesAndNewlines) then
	// lowercased(): newlines and U+00A0 are trimmed, and U+0130 lowercases to two
	// scalars, which the pattern then refuses.
	t.Run("normalizes as Swift", func(t *testing.T) {
		got, err := authservice.ValidateUsername("\n Bob-1\r\n")
		require.NoError(t, err)
		assert.Equal(t, "bob-1", got)
		invalid(t, "İlker")
	})
}
