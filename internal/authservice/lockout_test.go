package authservice_test

import (
	"context"
	"lumina/internal/authservice"
	"lumina/internal/modules/billing"
	"strings"
	"sync"
	"testing"
	"time"

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

	t.Run("five failed logins lock for fifteen minutes", func(t *testing.T) {
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

	// Post-archive fixes (backlog: auth weaknesses kept by #85).
	t.Run("a locked account answers 423 only to the right password, and does not count", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		for range 5 {
			_, _ = h.svc.Login(ctx, email, "WrongPassword12345!", false)
		}
		var until1 time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT lockout_until FROM users WHERE email = $1`, email).Scan(&until1))
		_, err := h.svc.Login(ctx, email, "WrongPassword12345!", false)
		assert.ErrorIs(t, err, authservice.ErrInvalidCredentials, "a wrong password does not reveal the lock")
		var until2 time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT lockout_until FROM users WHERE email = $1`, email).Scan(&until2))
		assert.True(t, until1.Equal(until2), "the lock is not extended")
		assert.Equal(t, int64(5), h.user(t, "email = $1", email).failed)
		_, err = h.svc.Login(ctx, email, password, false)
		assert.ErrorIs(t, err, authservice.ErrAccountLocked)
	})

	t.Run("a failure after the lock expired starts the count afresh", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		_, err := pool.Exec(ctx, `UPDATE users SET failed_login_attempts = 5, lockout_until = now() - interval '1 second' WHERE email = $1`, email)
		require.NoError(t, err)
		_, err = h.svc.Login(ctx, email, "WrongPassword12345!", false)
		assert.ErrorIs(t, err, authservice.ErrInvalidCredentials)
		assert.Equal(t, int64(1), h.user(t, "email = $1", email).failed)
		var until *time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT lockout_until FROM users WHERE email = $1`, email).Scan(&until))
		assert.Nil(t, until, "one failure does not lock again")
		_, err = h.svc.Login(ctx, email, password, false)
		require.NoError(t, err)
	})

	t.Run("concurrent failures all count", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() { _, _ = h.svc.Login(ctx, email, "WrongPassword12345!", false) })
		}
		wg.Wait()
		assert.Equal(t, int64(4), h.user(t, "email = $1", email).failed)
	})

	t.Run("an unknown email costs a bcrypt check", func(t *testing.T) {
		// Cost 10, so bcrypt (tens of ms) outweighs the database round trips.
		svc := authservice.New(pool, authservice.Options{Keys: keys, PasswordCost: 10})
		email := randomEmail("timing")
		_, err := svc.Register(ctx, email, randomUsername("timing"), password)
		require.NoError(t, err)
		timed := func(e string) time.Duration {
			start := time.Now()
			_, _ = svc.Login(ctx, e, "WrongPassword12345!", false)
			return time.Since(start)
		}
		_ = timed("warm-" + uuid.NewString() + "@test.luminavault") // builds the dummy hash once
		known, unknown := timed(email), timed("nobody-"+uuid.NewString()+"@test.luminavault")
		assert.Greater(t, unknown, known/2, "known %v, unknown %v", known, unknown)
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

	// Post-archive fix: Swift answered an unknown email 401, so the route told
	// the caller which addresses have accounts.
	t.Run("mfa resend: an unknown email answers like a known one, which gets a new challenge", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		require.NoError(t, h.svc.ResendMFA(ctx, "nobody-"+uuid.NewString()+"@test.luminavault"))
		code, _ := h.mfa.last()
		assert.Empty(t, code, "nothing is sent for an unknown email")
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

	// Post-archive fix: Swift locked only the code row, so a new code started afresh.
	t.Run("reset: five wrong codes lock; a new code keeps the lock", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		require.NoError(t, h.svc.ForgotPassword(ctx, email))
		for range 5 {
			_, err := h.svc.ResetPassword(ctx, email, "000000", "NewPasswordCorrectHorse1!")
			assert.ErrorIs(t, err, authservice.ErrResetCodeInvalid)
		}
		_, err := h.svc.ResetPassword(ctx, email, "654321", "NewPasswordCorrectHorse1!")
		assert.ErrorIs(t, err, authservice.ErrResetLocked)
		require.NoError(t, h.svc.ResendReset(ctx, email))
		_, err = h.svc.ResetPassword(ctx, email, "654321", "NewPasswordCorrectHorse1!")
		assert.ErrorIs(t, err, authservice.ErrResetLocked)

		// Once the lock runs out, a wrong code counts from one and the right one works.
		_, err = pool.Exec(ctx, `UPDATE password_reset_tokens SET locked_until = now() - interval '1 second'
			WHERE tenant_id = (SELECT id FROM users WHERE email = $1)`, email)
		require.NoError(t, err)
		_, err = h.svc.ResetPassword(ctx, email, "000000", "NewPasswordCorrectHorse1!")
		assert.ErrorIs(t, err, authservice.ErrResetCodeInvalid)
		var failed int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT failed_attempts FROM password_reset_tokens
			WHERE tenant_id = (SELECT id FROM users WHERE email = $1) ORDER BY created_at DESC LIMIT 1`, email).Scan(&failed))
		assert.Equal(t, int64(1), failed)
		_, err = h.svc.ResetPassword(ctx, email, "654321", "NewPasswordCorrectHorse1!")
		require.NoError(t, err)
	})

	t.Run("reset: a new code keeps the failures so far", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		require.NoError(t, h.svc.ForgotPassword(ctx, email))
		for range 4 {
			_, _ = h.svc.ResetPassword(ctx, email, "000000", "NewPasswordCorrectHorse1!")
		}
		require.NoError(t, h.svc.ResendReset(ctx, email))
		_, err := h.svc.ResetPassword(ctx, email, "000000", "NewPasswordCorrectHorse1!")
		assert.ErrorIs(t, err, authservice.ErrResetCodeInvalid)
		_, err = h.svc.ResetPassword(ctx, email, "654321", "NewPasswordCorrectHorse1!")
		assert.ErrorIs(t, err, authservice.ErrResetLocked, "the fifth failure across codes locks")
	})

	t.Run("verify: a new code keeps the lock", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		require.NoError(t, h.svc.SendVerification(ctx, email))
		for range 5 {
			_ = h.svc.ConfirmEmail(ctx, email, "000000")
		}
		require.ErrorIs(t, h.svc.ConfirmEmail(ctx, email, "789012"), authservice.ErrVerifyLocked)
		require.NoError(t, h.svc.SendVerification(ctx, email))
		require.ErrorIs(t, h.svc.ConfirmEmail(ctx, email, "789012"), authservice.ErrVerifyLocked)
	})

	t.Run("concurrent wrong codes all count", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := register(t, h)
		require.NoError(t, h.svc.ForgotPassword(ctx, email))
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() { _, _ = h.svc.ResetPassword(ctx, email, "000000", "NewPasswordCorrectHorse1!") })
		}
		wg.Wait()
		var failed int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT failed_attempts FROM password_reset_tokens
			WHERE tenant_id = (SELECT id FROM users WHERE email = $1)`, email).Scan(&failed))
		assert.Equal(t, int64(4), failed)
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
