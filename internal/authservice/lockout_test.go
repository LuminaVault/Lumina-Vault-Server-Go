package authservice_test

import (
	"context"
	"strings"
	"testing"

	"lumina/internal/authservice"
	"lumina/internal/modules/billing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuthServiceSwiftRules pins the AuthService.swift rules no Swift test
// names: the lockouts, the MFA burn, the allowlist's only-raises rule and the
// no-enumeration answers, weaknesses included.
func TestAuthServiceSwiftRules(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	register := func(t *testing.T, h harness) string {
		t.Helper()
		email := randomEmail("rule")
		_, err := h.svc.Register(ctx, email, randomUsername("rule"), password)
		require.NoError(t, err)
		return email
	}

	t.Run("five failed logins lock for fifteen minutes, before the password is checked", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		for range 5 {
			_, err := h.svc.Login(ctx, email, "WrongPassword12345!", false)
			assert.ErrorIs(t, err, authservice.ErrInvalidCredentials)
		}
		_, err := h.svc.Login(ctx, email, password, false)
		assert.ErrorIs(t, err, authservice.ErrAccountLocked)
		var minutes float64
		require.NoError(t, pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM lockout_until - now()) / 60 FROM users WHERE email = $1`, email).Scan(&minutes))
		assert.InDelta(t, 15, minutes, 0.5)
		assert.Equal(t, int64(5), h.user(t, "email = $1", email).failed)
	})

	t.Run("a successful login clears the failure count", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		_, _ = h.svc.Login(ctx, email, "WrongPassword12345!", false)
		_, err := h.svc.Login(ctx, email, password, false)
		require.NoError(t, err)
		assert.Equal(t, int64(0), h.user(t, "email = $1", email).failed)
	})

	t.Run("unknown email is invalid credentials, email lookup is case-insensitive", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		_, err := h.svc.Login(ctx, "nobody-"+uuid.NewString()+"@test.luminavault", password, false)
		assert.ErrorIs(t, err, authservice.ErrInvalidCredentials)
		resp, err := h.svc.Login(ctx, strings.ToUpper(email), password, false)
		require.NoError(t, err)
		assert.Equal(t, email, resp.Email)
	})

	t.Run("mfa: wrong codes burn the challenge at five; a used code is refused", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		pending, err := h.svc.Login(ctx, email, password, true)
		require.NoError(t, err)
		id := pending.MFAChallengeID.UUID
		for range 5 {
			_, err := h.svc.VerifyMFA(ctx, id, "000000")
			assert.ErrorIs(t, err, authservice.ErrMFAInvalid)
		}
		_, err = h.svc.VerifyMFA(ctx, id, "123456")
		assert.ErrorIs(t, err, authservice.ErrMFAInvalid)

		again, err := h.svc.Login(ctx, email, password, true)
		require.NoError(t, err)
		_, err = h.svc.VerifyMFA(ctx, again.MFAChallengeID.UUID, "123456")
		require.NoError(t, err)
		_, err = h.svc.VerifyMFA(ctx, again.MFAChallengeID.UUID, "123456")
		assert.ErrorIs(t, err, authservice.ErrMFAInvalid)
		_, err = h.svc.VerifyMFA(ctx, uuid.New(), "123456")
		assert.ErrorIs(t, err, authservice.ErrMFAInvalid)
	})

	t.Run("mfa resend: unknown email is 401, a known one gets a new challenge", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		assert.ErrorIs(t, h.svc.ResendMFA(ctx, "nobody-"+uuid.NewString()+"@test.luminavault"), authservice.ErrInvalidCredentials)
		require.NoError(t, h.svc.ResendMFA(ctx, email))
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM mfa_challenges c JOIN users u ON u.id = c.tenant_id
			WHERE u.email = $1 AND c.purpose = 'login' AND c.channel = 'email' AND c.destination = $1`, email).Scan(&n))
		assert.Equal(t, 1, n)
	})

	t.Run("forgot and resend are silent for an unknown email", func(t *testing.T) {
		h := build(t, pool, harness{})
		require.NoError(t, h.svc.ForgotPassword(ctx, "nobody-"+uuid.NewString()+"@test.luminavault"))
		require.NoError(t, h.svc.ResendReset(ctx, "nobody-"+uuid.NewString()+"@test.luminavault"))
		code, _ := h.reset.last()
		assert.Empty(t, code)
	})

	t.Run("reset: five wrong codes lock the code row; a new code starts afresh", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		require.NoError(t, h.svc.ForgotPassword(ctx, email))
		for range 5 {
			_, err := h.svc.ResetPassword(ctx, email, "000000", "NewPasswordCorrectHorse1!")
			assert.ErrorIs(t, err, authservice.ErrResetCodeInvalid)
		}
		_, err := h.svc.ResetPassword(ctx, email, "654321", "NewPasswordCorrectHorse1!")
		assert.ErrorIs(t, err, authservice.ErrResetLocked)
		// Swift weakness, kept: the lock is per row.
		require.NoError(t, h.svc.ResendReset(ctx, email))
		_, err = h.svc.ResetPassword(ctx, email, "654321", "NewPasswordCorrectHorse1!")
		require.NoError(t, err)
	})

	t.Run("reset checks the new password first and refuses an unknown email", func(t *testing.T) {
		h := build(t, pool, harness{})
		_, err := h.svc.ResetPassword(ctx, "nobody@test.luminavault", "654321", "short")
		assert.ErrorIs(t, err, authservice.ErrWeakPassword)
		_, err = h.svc.ResetPassword(ctx, "nobody@test.luminavault", "654321", "NewPasswordCorrectHorse1!")
		assert.ErrorIs(t, err, authservice.ErrResetCodeInvalid)
		email := register(t, h)
		_, err = h.svc.ResetPassword(ctx, email, "654321", "NewPasswordCorrectHorse1!")
		assert.ErrorIs(t, err, authservice.ErrResetCodeInvalid)
	})

	t.Run("an expired code is invalid", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		require.NoError(t, h.svc.ForgotPassword(ctx, email))
		_, err := pool.Exec(ctx, `UPDATE password_reset_tokens SET expires_at = now() - interval '1 second'
			WHERE tenant_id = (SELECT id FROM users WHERE email = $1)`, email)
		require.NoError(t, err)
		_, err = h.svc.ResetPassword(ctx, email, "654321", "NewPasswordCorrectHorse1!")
		assert.ErrorIs(t, err, authservice.ErrResetCodeInvalid)
	})

	t.Run("an expired refresh token is invalid", func(t *testing.T) {
		h := build(t, pool, harness{})
		resp, err := h.svc.Register(ctx, randomEmail("rule"), randomUsername("rule"), password)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `UPDATE refresh_tokens SET expires_at = now() - interval '1 second' WHERE tenant_id = $1`, resp.UserID.UUID)
		require.NoError(t, err)
		_, err = h.svc.Refresh(ctx, resp.RefreshToken)
		assert.ErrorIs(t, err, authservice.ErrInvalidRefresh)
		// Logout of an unknown token is not an error.
		require.NoError(t, h.svc.RevokeRefresh(ctx, "not-a-token"))
	})

	t.Run("the allowlist only raises: ultimate is never lowered to pro", func(t *testing.T) {
		allowlist := billing.ParseTierOverrides("raise@example.com=pro", nil)
		h := build(t, pool, harness{tierOverride: allowlist.Override})
		_, err := h.svc.Register(ctx, "raise@example.com", randomUsername("rule"), password)
		require.NoError(t, err)
		assert.Equal(t, "pro", h.user(t, "email = $1", "raise@example.com").tierOverride)
		_, err = pool.Exec(ctx, `UPDATE users SET tier_override = 'ultimate' WHERE email = 'raise@example.com'`)
		require.NoError(t, err)
		_, err = h.svc.Login(ctx, "raise@example.com", password, false)
		require.NoError(t, err)
		assert.Equal(t, "ultimate", h.user(t, "email = $1", "raise@example.com").tierOverride)
	})

	t.Run("refresh, mfa and reset sessions also pass through the allowlist", func(t *testing.T) {
		allowlist := billing.ParseTierOverrides("every@example.com", nil)
		h := build(t, pool, harness{tierOverride: allowlist.Override})
		resp, err := h.svc.Register(ctx, "every@example.com", randomUsername("rule"), password)
		require.NoError(t, err)
		reset := func() {
			_, err := pool.Exec(ctx, `UPDATE users SET tier_override = 'none' WHERE email = 'every@example.com'`)
			require.NoError(t, err)
		}
		reset()
		_, err = h.svc.Refresh(ctx, resp.RefreshToken)
		require.NoError(t, err)
		assert.Equal(t, "ultimate", h.user(t, "email = $1", "every@example.com").tierOverride)

		reset()
		pending, err := h.svc.Login(ctx, "every@example.com", password, true)
		require.NoError(t, err)
		assert.Equal(t, "none", h.user(t, "email = $1", "every@example.com").tierOverride, "a pending MFA login issues no session")
		_, err = h.svc.VerifyMFA(ctx, pending.MFAChallengeID.UUID, "123456")
		require.NoError(t, err)
		assert.Equal(t, "ultimate", h.user(t, "email = $1", "every@example.com").tierOverride)

		reset()
		require.NoError(t, h.svc.ForgotPassword(ctx, "every@example.com"))
		_, err = h.svc.ResetPassword(ctx, "every@example.com", "654321", "NewPasswordCorrectHorse1!")
		require.NoError(t, err)
		assert.Equal(t, "ultimate", h.user(t, "email = $1", "every@example.com").tierOverride)
	})

	t.Run("register writes the personal vault and its admin membership", func(t *testing.T) {
		h := build(t, pool, harness{})
		username := randomUsername("rule")
		resp, err := h.svc.Register(ctx, randomEmail("rule"), username, password)
		require.NoError(t, err)
		var name, role string
		require.NoError(t, pool.QueryRow(ctx, `SELECT v.name, m.role FROM vaults v JOIN vault_memberships m ON m.vault_id = v.id
			WHERE v.id = $1 AND v.personal_owner_user_id = $1 AND m.user_id = $1 AND m.can_use_ai`, resp.UserID.UUID).Scan(&name, &role))
		assert.Equal(t, username+"'s Vault", name)
		assert.Equal(t, "admin", role)
	})
}
