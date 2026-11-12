// Package authservice is the Go port of LuminaVaultServer's AuthService
// (Sources/App/Auth/AuthService.swift) and its collaborators: MFAService,
// UsernamePolicy, the email OTP senders and the OTP code generators (wave 8A).
//
// It owns issueTokens, which every session goes through: the password routes
// here, and OAuth, pairing, phone, magic link and WebAuthn in 8B and 8C.
// Minting, hashing and the refresh store are P9 (internal/auth).
//
// The port keeps Swift's behaviour, weaknesses included (wave-6-8-plan.md
// "Auth"; owner rule: security fixes wait until Swift is archived). Each one is
// marked "Swift weakness, kept" where it happens.
package authservice

import (
	"net/http"

	"lumina/internal/wire"
)

// AuthError, verbatim (Sources/App/Auth/Errors/AuthError.swift).
var (
	ErrInvalidCredentials = wire.NewError(http.StatusUnauthorized, "invalid credentials")
	ErrAccountLocked      = wire.NewError(423, "account locked")
	ErrEmailExists        = wire.NewError(http.StatusConflict, "email already registered")
	ErrUsernameTaken      = wire.NewError(http.StatusConflict, "username already taken")
	ErrInvalidUsername    = wire.NewError(http.StatusBadRequest, "username must be 3-31 chars, lowercase a-z/0-9/-, not reserved")
	ErrWeakPassword       = wire.NewError(http.StatusBadRequest, "password too short (min 12)")
	ErrMFAInvalid         = wire.NewError(http.StatusUnauthorized, "mfa code invalid")
	ErrInvalidRefresh     = wire.NewError(http.StatusUnauthorized, "invalid refresh token")
	ErrResetCodeInvalid   = wire.NewError(http.StatusUnauthorized, "reset code invalid or expired")
	ErrResetLocked        = wire.NewError(http.StatusTooManyRequests, "reset locked, try later")
	ErrVerifyCodeInvalid  = wire.NewError(http.StatusUnauthorized, "verification code invalid or expired")
	ErrVerifyLocked       = wire.NewError(http.StatusTooManyRequests, "verification locked, try later")
)
