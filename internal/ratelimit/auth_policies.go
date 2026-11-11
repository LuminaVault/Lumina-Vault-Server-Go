package ratelimit

import (
	"strings"
	"time"
)

// P10: the per-IP auth policies of RateLimitMiddleware.swift (registerByIP …
// emailMagicStartByIPDaily), keyed by IPOnlyKey, and webAuthnEnrolByUser, keyed
// by UserOrIPKey. None scales by tier: a credential attempt costs the same
// whoever makes it. Mounted by AuthController per route (8A, 8B, 8C);
// logout and oauth/:provider/exchange have none.
var (
	RegisterByIP       = Policy{Name: "registerByIP", Max: 5, Window: 60 * time.Second}
	LoginByIP          = Policy{Name: "loginByIP", Max: 10, Window: 60 * time.Second}
	ForgotPasswordByIP = Policy{Name: "forgotPasswordByIP", Max: 5, Window: 300 * time.Second}
	ResendResetByIP    = Policy{Name: "resendResetByIP", Max: 5, Window: 300 * time.Second}
	ResetPasswordByIP  = Policy{Name: "resetPasswordByIP", Max: 5, Window: 300 * time.Second}
	RefreshByIP        = Policy{Name: "refreshByIP", Max: 30, Window: 60 * time.Second}
	MFAVerifyByIP      = Policy{Name: "mfaVerifyByIP", Max: 20, Window: 60 * time.Second}
	MFAResendByIP      = Policy{Name: "mfaResendByIP", Max: 10, Window: 60 * time.Second}
	SendVerifyByIP     = Policy{Name: "sendVerifyByIP", Max: 5, Window: 300 * time.Second}
	ConfirmEmailByIP   = Policy{Name: "confirmEmailByIP", Max: 10, Window: 300 * time.Second}

	WebAuthnAuthenticateByIP = Policy{Name: "webAuthnAuthenticateByIP", Max: 20, Window: 60 * time.Second}
	// WebAuthnEnrolByUser is keyed by UserOrIPKey (JWT-protected enrolment).
	WebAuthnEnrolByUser = Policy{Name: "webAuthnEnrolByUser", Max: 20, Window: 60 * time.Second}

	PhoneStartByIPPerMinute      = Policy{Name: "phoneStartByIPPerMinute", Max: 3, Window: 60 * time.Second}
	PhoneStartByIPDaily          = Policy{Name: "phoneStartByIPDaily", Max: 10, Window: 86400 * time.Second}
	EmailMagicStartByIPPerMinute = Policy{Name: "emailMagicStartByIPPerMinute", Max: 3, Window: 60 * time.Second}
	EmailMagicStartByIPDaily     = Policy{Name: "emailMagicStartByIPDaily", Max: 10, Window: 86400 * time.Second}
)

// StorageKind is RateLimitStorageKind: "redis" (any case) or memory for
// anything else, empty included.
type StorageKind string

const (
	StorageMemory StorageKind = "memory"
	StorageRedis  StorageKind = "redis"
)

// ParseStorageKind is RateLimitStorageKind(raw:).
func ParseStorageKind(raw string) StorageKind {
	if strings.ToLower(raw) == "redis" {
		return StorageRedis
	}
	return StorageMemory
}
