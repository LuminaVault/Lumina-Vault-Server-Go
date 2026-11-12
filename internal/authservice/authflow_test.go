package authservice_test

import (
	"context"
	"lumina/internal/auth"
	"lumina/internal/authservice"
	"lumina/internal/ids"
	"lumina/internal/modules/billing"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuthFlow ports AuthFlowTests (Tests/AppTests/Auth/AuthFlowTests.swift),
// all 14 cases: register, login, MFA, refresh, logout and reset end to end on
// Postgres, with recording senders and fixed codes.
func TestAuthFlow(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	t.Run("register finishes every write after the client leaves", func(t *testing.T) {
		// Hummingbird runs the handler to the end; a cancelled request context
		// must not leave a user without a trial expiry or a personal vault.
		h := build(t, pool, harness{})
		gone, cancel := context.WithCancel(ctx)
		cancel()
		email := randomEmail("auth")
		_, err := h.svc.Register(gone, email, randomUsername("auth"), password)
		require.NoError(t, err)
		var trialSet, vaults int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email = $1 AND tier_expires_at IS NOT NULL`, email).Scan(&trialSet))
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM vaults v JOIN users u ON v.personal_owner_user_id = u.id WHERE u.email = $1`, email).Scan(&vaults))
		assert.Equal(t, 1, trialSet)
		assert.Equal(t, 1, vaults)
	})

	t.Run("register happy path creates user and hermes profile", func(t *testing.T) {
		h := build(t, pool, harness{})
		email, username := randomEmail("auth"), randomUsername("auth")
		resp, err := h.svc.Register(ctx, email, username, password)
		require.NoError(t, err)
		assert.NotEmpty(t, resp.AccessToken)
		assert.NotEmpty(t, resp.RefreshToken)
		assert.Equal(t, email, resp.Email)

		u := h.user(t, "username = $1", username)
		require.NotNil(t, u)
		assert.Equal(t, username, u.username)
		var profileID, status string
		require.NoError(t, pool.QueryRow(ctx, `SELECT hermes_profile_id, status FROM hermes_profiles WHERE tenant_id = $1`, u.id).
			Scan(&profileID, &status))
		assert.Equal(t, "hermes-"+username, profileID)
		assert.Equal(t, "ready", status)

		// issueTokens puts a ready profile's id in hpid.
		claims, err := auth.Verify(keys, "Bearer "+resp.AccessToken)
		require.NoError(t, err)
		assert.Equal(t, "hermes-"+username, claims.HPID)
		assert.Equal(t, ids.Upper(u.id), claims.Subject)
	})

	t.Run("allowlisted email is stamped with its tier override on every session", func(t *testing.T) {
		allowlist := billing.ParseTierOverrides("Founder@Example.com=ultimate", nil)
		h := build(t, pool, harness{tierOverride: allowlist.Override})

		// Registration stamps immediately, matching case-insensitively.
		founderName := randomUsername("auth")
		_, err := h.svc.Register(ctx, "founder@example.com", founderName, password)
		require.NoError(t, err)
		founder := h.user(t, "username = $1", founderName)
		require.NotNil(t, founder)
		assert.Equal(t, "ultimate", founder.tierOverride)
		assert.Equal(t, "trial", founder.tier, "the RevenueCat-driven tier is untouched; only the override is stamped")

		// Anyone else keeps the default.
		strangerName := randomUsername("auth")
		_, err = h.svc.Register(ctx, randomEmail("auth"), strangerName, password)
		require.NoError(t, err)
		assert.Equal(t, "none", h.user(t, "username = $1", strangerName).tierOverride)

		// An account that predates the allowlist is stamped on its next login.
		_, err = pool.Exec(ctx, `UPDATE users SET tier_override = 'none' WHERE id = $1`, founder.id)
		require.NoError(t, err)
		_, err = h.svc.Login(ctx, "founder@example.com", password, false)
		require.NoError(t, err)
		assert.Equal(t, "ultimate", h.user(t, "username = $1", founderName).tierOverride)

		// issueTokens marks the session internal for analytics, keyed by the uppercase id.
		var internal []bool
		for _, e := range h.analytics.all() {
			if e.name == "$set" && e.distinct == ids.Upper(founder.id) {
				internal = append(internal, e.props["$set"].(map[string]any)["is_internal"].(bool))
			}
		}
		assert.Equal(t, []bool{true, true}, internal)
	})

	t.Run("register stamps trial for 14 days", func(t *testing.T) {
		h := build(t, pool, harness{})
		username := randomUsername("auth")
		before := time.Now()
		_, err := h.svc.Register(ctx, randomEmail("auth"), username, password)
		require.NoError(t, err)
		after := time.Now()

		u := h.user(t, "username = $1", username)
		require.NotNil(t, u)
		assert.Equal(t, "trial", u.tier)
		assert.Equal(t, "none", u.tierOverride)
		assert.Nil(t, u.revenuecat)
		require.NotNil(t, u.tierExpires)
		earliest, latest := before.Add(14*24*time.Hour).Truncate(time.Microsecond), after.Add(14*24*time.Hour)
		assert.False(t, u.tierExpires.Before(earliest) || u.tierExpires.After(latest),
			"trial expiry %s outside expected window [%s, %s]", u.tierExpires, earliest, latest)
	})

	t.Run("register rejects duplicate username", func(t *testing.T) {
		h := build(t, pool, harness{})
		username := randomUsername("auth")
		_, err := h.svc.Register(ctx, randomEmail("auth"), username, password)
		require.NoError(t, err)
		_, err = h.svc.Register(ctx, randomEmail("auth"), username, password)
		assert.ErrorIs(t, err, authservice.ErrUsernameTaken)
	})

	t.Run("register rejects duplicate email", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := randomEmail("auth")
		_, err := h.svc.Register(ctx, email, randomUsername("auth"), password)
		require.NoError(t, err)
		_, err = h.svc.Register(ctx, email, randomUsername("auth"), password)
		assert.ErrorIs(t, err, authservice.ErrEmailExists)
	})

	t.Run("register rejects reserved username", func(t *testing.T) {
		h := build(t, pool, harness{})
		_, err := h.svc.Register(ctx, randomEmail("auth"), "admin", password)
		assert.ErrorIs(t, err, authservice.ErrInvalidUsername)
	})

	t.Run("register rejects weak password", func(t *testing.T) {
		h := build(t, pool, harness{})
		_, err := h.svc.Register(ctx, randomEmail("auth"), randomUsername("auth"), "short")
		assert.ErrorIs(t, err, authservice.ErrWeakPassword)
	})

	t.Run("login happy path", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := randomEmail("auth")
		_, err := h.svc.Register(ctx, email, randomUsername("auth"), password)
		require.NoError(t, err)
		resp, err := h.svc.Login(ctx, email, password, false)
		require.NoError(t, err)
		assert.NotEmpty(t, resp.AccessToken)
		assert.True(t, resp.MFARequired == nil || !*resp.MFARequired)
	})

	t.Run("login rejects wrong password", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := randomEmail("auth")
		_, err := h.svc.Register(ctx, email, randomUsername("auth"), password)
		require.NoError(t, err)
		_, err = h.svc.Login(ctx, email, "WrongPassword12345!", false)
		assert.ErrorIs(t, err, authservice.ErrInvalidCredentials)
	})

	t.Run("login with MFA returns challenge not tokens", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := randomEmail("auth")
		_, err := h.svc.Register(ctx, email, randomUsername("auth"), password)
		require.NoError(t, err)
		resp, err := h.svc.Login(ctx, email, password, true)
		require.NoError(t, err)
		require.NotNil(t, resp.MFARequired)
		assert.True(t, *resp.MFARequired)
		assert.NotNil(t, resp.MFAChallengeID)
		assert.Empty(t, resp.AccessToken)
		code, _ := h.mfa.last()
		assert.Equal(t, "123456", code)
	})

	t.Run("mfa verify issues tokens", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := randomEmail("auth")
		_, err := h.svc.Register(ctx, email, randomUsername("auth"), password)
		require.NoError(t, err)
		pending, err := h.svc.Login(ctx, email, password, true)
		require.NoError(t, err)
		require.NotNil(t, pending.MFAChallengeID)
		verified, err := h.svc.VerifyMFA(ctx, pending.MFAChallengeID.UUID, "123456")
		require.NoError(t, err)
		assert.NotEmpty(t, verified.AccessToken)
		assert.NotEmpty(t, verified.RefreshToken)
	})

	t.Run("refresh rotates token and revokes old", func(t *testing.T) {
		h := build(t, pool, harness{})
		registered, err := h.svc.Register(ctx, randomEmail("auth"), randomUsername("auth"), password)
		require.NoError(t, err)
		original := registered.RefreshToken

		refreshed, err := h.svc.Refresh(ctx, original)
		require.NoError(t, err)
		assert.NotEqual(t, original, refreshed.RefreshToken)
		assert.NotEmpty(t, refreshed.AccessToken)

		_, err = h.svc.Refresh(ctx, original)
		assert.ErrorIs(t, err, authservice.ErrInvalidRefresh)
	})

	t.Run("logout revokes refresh token", func(t *testing.T) {
		h := build(t, pool, harness{})
		registered, err := h.svc.Register(ctx, randomEmail("auth"), randomUsername("auth"), password)
		require.NoError(t, err)
		require.NoError(t, h.svc.RevokeRefresh(ctx, registered.RefreshToken))
		_, err = h.svc.Refresh(ctx, registered.RefreshToken)
		assert.ErrorIs(t, err, authservice.ErrInvalidRefresh)
	})

	t.Run("reset password end to end", func(t *testing.T) {
		h := build(t, pool, harness{})
		email := randomEmail("auth")
		registered, err := h.svc.Register(ctx, email, randomUsername("auth"), password)
		require.NoError(t, err)

		require.NoError(t, h.svc.ForgotPassword(ctx, email))
		reset, err := h.svc.ResetPassword(ctx, email, "654321", "NewPasswordCorrectHorse1!")
		require.NoError(t, err)
		assert.NotEmpty(t, reset.AccessToken)

		login, err := h.svc.Login(ctx, email, "NewPasswordCorrectHorse1!", false)
		require.NoError(t, err)
		assert.NotEmpty(t, login.AccessToken)

		_, err = h.svc.Login(ctx, email, password, false)
		assert.ErrorIs(t, err, authservice.ErrInvalidCredentials)

		// Reset revokes every refresh token issued before it.
		_, err = h.svc.Refresh(ctx, registered.RefreshToken)
		assert.ErrorIs(t, err, authservice.ErrInvalidRefresh)
	})
}
