package authservice_test

import (
	"lumina/internal/authservice"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func newStore() *authservice.PreAuthChallengeStore {
	return authservice.NewPreAuthChallengeStore(authservice.PreAuthLifetime, authservice.PreAuthMaxAttempts)
}

// TestPreAuthChallengeStore ports PreAuthChallengeStoreTests
// (Tests/AppTests/Auth/PreAuthChallengeStoreTests.swift), the store's 8 cases.
// The file's PhoneE164ValidatorTests (2) are in
// internal/modules/authroutes/flows_test.go, where validateE164 lives.
func TestPreAuthChallengeStore(t *testing.T) {
	t.Run("issued code consumes once", func(t *testing.T) {
		s := newStore()
		id, expiresAt := s.Issue("sms", "+15551112222", "phone_signin", "123456")
		assert.NotEqual(t, uuid.Nil, id)
		assert.True(t, expiresAt.After(time.Now()))

		dest, purpose, ok := s.Consume("+15551112222", "123456")
		assert.True(t, ok)
		assert.Equal(t, "+15551112222", dest)
		assert.Equal(t, "phone_signin", purpose)

		// Burn-on-success: a second consume fails.
		_, _, ok = s.Consume("+15551112222", "123456")
		assert.False(t, ok)
	})

	t.Run("wrong code fails and counts", func(t *testing.T) {
		s := newStore()
		s.Issue("sms", "+15552223333", "phone_signin", "ok-code")
		for range 5 {
			_, _, ok := s.Consume("+15552223333", "wrong")
			assert.False(t, ok)
		}
		// Even the right code fails now: the challenge was burned.
		_, _, ok := s.Consume("+15552223333", "ok-code")
		assert.False(t, ok)
	})

	t.Run("reissue burns prior challenge", func(t *testing.T) {
		s := newStore()
		s.Issue("email", "alice@x.io", "magic_link", "AAA")
		s.Issue("email", "alice@x.io", "magic_link", "BBB")
		_, _, ok := s.Consume("alice@x.io", "AAA")
		assert.False(t, ok)
		_, _, ok = s.Consume("alice@x.io", "BBB")
		assert.True(t, ok)
	})

	t.Run("different destinations are independent", func(t *testing.T) {
		s := newStore()
		s.Issue("sms", "+1", "phone_signin", "AAA")
		s.Issue("sms", "+2", "phone_signin", "BBB")
		_, _, ok := s.Consume("+1", "AAA")
		assert.True(t, ok)
		_, _, ok = s.Consume("+2", "BBB")
		assert.True(t, ok)
	})

	t.Run("consume typed returns ok on happy path", func(t *testing.T) {
		s := newStore()
		s.Issue("sms", "+15551234567", "phone_signin", "111111")
		outcome, dest, purpose := s.ConsumeTyped("+15551234567", "111111")
		assert.Equal(t, authservice.ConsumeOK, outcome)
		assert.Equal(t, "+15551234567", dest)
		assert.Equal(t, "phone_signin", purpose)
	})

	t.Run("consume typed returns expired", func(t *testing.T) {
		// A negative lifetime: the challenge is born expired.
		s := authservice.NewPreAuthChallengeStore(-time.Second, authservice.PreAuthMaxAttempts)
		s.Issue("sms", "+15551234567", "phone_signin", "111111")
		outcome, _, _ := s.ConsumeTyped("+15551234567", "111111")
		assert.Equal(t, authservice.ConsumeExpired, outcome)
		// Expiry burns it.
		outcome, _, _ = s.ConsumeTyped("+15551234567", "111111")
		assert.Equal(t, authservice.ConsumeNotFound, outcome)
	})

	t.Run("consume typed returns wrong code then locked out", func(t *testing.T) {
		s := newStore()
		s.Issue("sms", "+15551234567", "phone_signin", "111111")
		for range 4 {
			outcome, _, _ := s.ConsumeTyped("+15551234567", "wrong")
			assert.Equal(t, authservice.ConsumeWrongCode, outcome)
		}
		outcome, _, _ := s.ConsumeTyped("+15551234567", "wrong")
		assert.Equal(t, authservice.ConsumeLockedOut, outcome)
		outcome, _, _ = s.ConsumeTyped("+15551234567", "111111")
		assert.Equal(t, authservice.ConsumeNotFound, outcome)
	})

	t.Run("consume typed returns not found for unknown destination", func(t *testing.T) {
		outcome, _, _ := newStore().ConsumeTyped("+15551234567", "111111")
		assert.Equal(t, authservice.ConsumeNotFound, outcome)
	})

	// Not in Swift: the store keys on destination alone, so a phone code can be
	// consumed through either verify route's store call, whatever its channel.
	t.Run("channel and purpose are not part of the key", func(t *testing.T) {
		s := newStore()
		s.Issue("sms", "dest", "phone_signin", "1")
		s.Issue("email", "dest", "magic_link", "2")
		_, purpose, ok := s.Consume("dest", "2")
		assert.True(t, ok)
		assert.Equal(t, "magic_link", purpose)
	})
}
