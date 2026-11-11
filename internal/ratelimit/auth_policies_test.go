package ratelimit_test

import (
	"lumina/internal/ratelimit"
	"lumina/internal/wire"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// P10: the auth policies' numbers, read off RateLimitMiddleware.swift:250-266,
// 323-332 one definition at a time.
func TestAuthPolicyNumbersMatchSwift(t *testing.T) {
	const (
		minute = 60 * time.Second
		five   = 300 * time.Second
		day    = 86400 * time.Second
	)
	for _, tc := range []struct {
		p      ratelimit.Policy
		name   string
		max    int
		window time.Duration
	}{
		{ratelimit.RegisterByIP, "registerByIP", 5, minute},
		{ratelimit.LoginByIP, "loginByIP", 10, minute},
		{ratelimit.ForgotPasswordByIP, "forgotPasswordByIP", 5, five},
		{ratelimit.ResendResetByIP, "resendResetByIP", 5, five},
		{ratelimit.ResetPasswordByIP, "resetPasswordByIP", 5, five},
		{ratelimit.RefreshByIP, "refreshByIP", 30, minute},
		{ratelimit.MFAVerifyByIP, "mfaVerifyByIP", 20, minute},
		{ratelimit.MFAResendByIP, "mfaResendByIP", 10, minute},
		{ratelimit.SendVerifyByIP, "sendVerifyByIP", 5, five},
		{ratelimit.ConfirmEmailByIP, "confirmEmailByIP", 10, five},
		{ratelimit.WebAuthnAuthenticateByIP, "webAuthnAuthenticateByIP", 20, minute},
		{ratelimit.WebAuthnEnrolByUser, "webAuthnEnrolByUser", 20, minute},
		{ratelimit.PhoneStartByIPPerMinute, "phoneStartByIPPerMinute", 3, minute},
		{ratelimit.PhoneStartByIPDaily, "phoneStartByIPDaily", 10, day},
		{ratelimit.EmailMagicStartByIPPerMinute, "emailMagicStartByIPPerMinute", 3, minute},
		{ratelimit.EmailMagicStartByIPDaily, "emailMagicStartByIPDaily", 10, day},
	} {
		require.Equal(t, tc.name, tc.p.Name)
		require.Equal(t, tc.max, tc.p.Max, tc.name)
		require.Equal(t, tc.window, tc.p.Window, tc.name)
		require.Nil(t, tc.p.TierScale, tc.name)
	}
}

// RateLimitMiddlewareTests `auth policies stay flat for every tier`.
func TestAuthPoliciesStayFlatForEveryTier(t *testing.T) {
	for _, tier := range []string{"trial", "free", "pro", "ultimate", "lapsed", "archived", ""} {
		require.Equal(t, 10, ratelimit.LoginByIP.EffectiveMax(tier))
	}
}

// RateLimitMiddlewareTests `every policy name is unique`, over Swift's
// allDeclaredPolicies now that every one of them exists in Go.
func TestAllDeclaredPolicyNamesAreUnique(t *testing.T) {
	all := []ratelimit.Policy{
		ratelimit.RegisterByIP, ratelimit.LoginByIP, ratelimit.ForgotPasswordByIP, ratelimit.ResendResetByIP,
		ratelimit.ResetPasswordByIP, ratelimit.RefreshByIP, ratelimit.MFAVerifyByIP, ratelimit.MFAResendByIP,
		ratelimit.SendVerifyByIP, ratelimit.ConfirmEmailByIP, ratelimit.WebAuthnAuthenticateByIP, ratelimit.WebAuthnEnrolByUser,
		ratelimit.ChatByUser, ratelimit.QueryByUser, ratelimit.ConversationByUser, ratelimit.CaptureByUser,
		ratelimit.VaultUploadByUser, ratelimit.VaultExportByUser, ratelimit.SoulByUser, ratelimit.SkillRunByUser,
		ratelimit.JobsByUser, ratelimit.WorkflowsByUser, ratelimit.AchievementsByUser,
		ratelimit.PhoneStartByIPPerMinute, ratelimit.PhoneStartByIPDaily,
		ratelimit.EmailMagicStartByIPPerMinute, ratelimit.EmailMagicStartByIPDaily,
		ratelimit.TranscribeByUserPerMinute, ratelimit.TranscribeByUserDaily,
		ratelimit.AudioTranscriptionsByUserPerMinute, ratelimit.AudioTranscriptionsByUserDaily,
		ratelimit.AudioSpeechByUserPerMinute, ratelimit.AudioSpeechByUserDaily,
		ratelimit.VisionEmbedByUserPerMinute, ratelimit.VisionEmbedByUserDaily,
		ratelimit.TTSByUserPerMinute, ratelimit.TTSByUserDaily, ratelimit.SettingsByUser,
		ratelimit.MCPAnonymousByIP, ratelimit.MCPByUser, ratelimit.MeTodayByUser, ratelimit.SkillOutputsByUser,
	}
	seen := map[string]bool{}
	for _, p := range all {
		require.NotEmpty(t, p.Name)
		require.False(t, seen[p.Name], p.Name)
		seen[p.Name] = true
	}
}

// An auth route's limiter is per IP: callers behind the first X-Forwarded-For
// hop count apart, a 6th register from one IP inside the minute is 429, and a
// bearer doesn't change the key (no "ip:" prefix, no user bucket).
func TestRegisterByIPLimitsPerForwardedIP(t *testing.T) {
	s := ratelimit.NewStore()
	e := echo.New()
	e.HTTPErrorHandler = wire.HTTPErrorHandler
	e.POST("/v1/auth/register", func(c *echo.Context) error { return c.NoContent(http.StatusNoContent) },
		ratelimit.Middleware(s, ratelimit.RegisterByIP, ratelimit.IPOnlyKey))
	call := func(xff string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/register", nil)
		req.Header.Set("X-Forwarded-For", xff)
		req.Header.Set("Authorization", "Bearer whatever")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}
	for range 5 {
		require.Equal(t, http.StatusNoContent, call("203.0.113.7, 10.0.0.1"))
	}
	require.Equal(t, http.StatusTooManyRequests, call("203.0.113.7, 10.0.0.9"))
	require.Equal(t, http.StatusNoContent, call("198.51.100.2, 10.0.0.1"))
}

// RateLimitStorageFactoryTests: `unknown kind defaults to memory`, `empty kind
// defaults to memory`, `kind matching is case insensitive`,
// `RateLimitStorageKind enum maps known values`. The redis cases need the
// Valkey driver, which Go doesn't have (P10: no Valkey for LuminaVault).
func TestRateLimitStorageKind(t *testing.T) {
	for raw, want := range map[string]ratelimit.StorageKind{
		"memory": ratelimit.StorageMemory, "": ratelimit.StorageMemory, "valkey": ratelimit.StorageMemory,
		"bogus": ratelimit.StorageMemory, "redis": ratelimit.StorageRedis, "REDIS": ratelimit.StorageRedis,
		"Redis": ratelimit.StorageRedis, "MEMORY": ratelimit.StorageMemory,
	} {
		require.Equal(t, want, ratelimit.ParseStorageKind(raw), raw)
	}
}

// ValkeyPersistDriverIntegrationTests `valkey persist driver stores reads expires
// and removes`, on the memory store production uses (owner decision
// 2026-11-11): a bucket is stored and read back while it lives, is gone once
// its window + 5 s TTL passes, and buckets of different keys never share a
// count. create-duplicate and remove have no Go caller: the limiter only sets.
func TestMemoryStoreStoresReadsAndExpires(t *testing.T) {
	s, c := newStore()
	p := ratelimit.Policy{Name: "persist", Max: 2, Window: time.Second}
	limited, _ := s.Hit(p, "a")
	require.False(t, limited)
	c.t = c.t.Add(500 * time.Millisecond)
	limited, _ = s.Hit(p, "a")
	require.False(t, limited, "read back: second hit of two")
	limited, _ = s.Hit(p, "a")
	require.True(t, limited, "the stored count survives")
	limited, _ = s.Hit(p, "b")
	require.False(t, limited, "keys are apart")

	c.t = c.t.Add(6*time.Second + time.Millisecond)
	limited, _ = s.Hit(p, "a")
	require.False(t, limited, "expired after window + 5 s")
}
