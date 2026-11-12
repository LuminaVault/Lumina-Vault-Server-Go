package authservice_test

import (
	"context"
	"testing"

	"lumina/internal/authservice"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEmailVerification ports EmailVerificationTests
// (Tests/AppTests/Auth/EmailVerificationTests.swift), all 7 cases (HER-87).
func TestEmailVerification(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	register := func(t *testing.T, h harness) string {
		t.Helper()
		email := randomEmail("verify")
		_, err := h.svc.Register(ctx, email, randomUsername("verify"), password)
		require.NoError(t, err)
		return email
	}

	t.Run("new user starts unverified", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		assert.False(t, h.user(t, "email = $1", email).isVerified)
	})

	t.Run("send verification delivers code to email", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		require.NoError(t, h.svc.SendVerification(ctx, email))
		code, dest := h.verify.last()
		assert.Equal(t, "789012", code)
		assert.Equal(t, email, dest)
	})

	t.Run("send verification is silent on unknown email", func(t *testing.T) {
		h := build(t, pool, harness{})
		require.NoError(t, h.svc.SendVerification(ctx, "nobody-"+uuid.NewString()+"@test.luminavault"))
		code, _ := h.verify.last()
		assert.Empty(t, code)
	})

	t.Run("confirm email flips is verified and is idempotent", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		require.NoError(t, h.svc.SendVerification(ctx, email))
		require.NoError(t, h.svc.ConfirmEmail(ctx, email, "789012"))
		assert.True(t, h.user(t, "email = $1", email).isVerified)
		// Calling confirm again with the same code is a no-op (idempotent), not a failure.
		require.NoError(t, h.svc.ConfirmEmail(ctx, email, "789012"))
	})

	t.Run("confirm email rejects wrong code", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		require.NoError(t, h.svc.SendVerification(ctx, email))
		assert.ErrorIs(t, h.svc.ConfirmEmail(ctx, email, "000000"), authservice.ErrVerifyCodeInvalid)
		assert.False(t, h.user(t, "email = $1", email).isVerified)
	})

	t.Run("confirm email rejects when no token issued", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		assert.ErrorIs(t, h.svc.ConfirmEmail(ctx, email, "789012"), authservice.ErrVerifyCodeInvalid)
	})

	t.Run("confirm email locks after repeated failures", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		require.NoError(t, h.svc.SendVerification(ctx, email))
		for range 5 {
			_ = h.svc.ConfirmEmail(ctx, email, "000000")
		}
		// 6th attempt: the token is now locked, even with the correct code.
		assert.ErrorIs(t, h.svc.ConfirmEmail(ctx, email, "789012"), authservice.ErrVerifyLocked)
		assert.False(t, h.user(t, "email = $1", email).isVerified)
	})
}
