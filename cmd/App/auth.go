package main

import (
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/authservice"
	"lumina/internal/config"
	"lumina/internal/hermesprofile"
	"lumina/internal/httpserver"
	"lumina/internal/modules/authroutes"
	"lumina/internal/modules/billing"
	"lumina/internal/posthog"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultpaths"

	"github.com/jackc/pgx/v5/pgxpool"
)

// authService builds DefaultAuthService as App+build.swift does: one email
// sender per surface from the same EMAIL_KIND, random OTP codes, the
// BILLING_TIER_OVERRIDE_EMAILS allowlist, and the Hermes profile service for
// ensureSoft and hpid. 8B and 8C reuse it for issueTokens.
func authService(pool *pgxpool.Pool, cfg config.Config, keys auth.Keyset, paths vaultpaths.Paths, analytics *posthog.Client) *authservice.Service {
	sender := func(label string) authservice.Sender {
		return authservice.NewSender(authservice.SenderConfig{
			Kind: cfg.EmailKind, APIKey: cfg.EmailResendAPIKey, FromAddress: cfg.EmailFromAddress, ReplyTo: cfg.EmailReplyTo,
		}, slog.Default().With("label", label))
	}
	overrides := billing.ParseTierOverrides(cfg.BillingTierOverrideEmails, slog.Default().With("label", "lv.billing.tier-override"))
	return authservice.New(pool, authservice.Options{
		Keys:         keys,
		Profiles:     hermesprofile.New(pool, hermesprofile.NewGateway(cfg.HermesGatewayKind, cfg.HermesDataRoot), paths),
		Analytics:    analytics,
		TierOverride: overrides.Override,
		MFASender:    sender("lv.mfa"),
		ResetSender:  sender("lv.reset"),
		VerifySender: sender("lv.verify"),
	})
}

// authModule is waves 8A and 8B: the password-session routes, /v1/auth/me and
// /me/privacy, then OAuth, X, phone, magic link and pairing as App+build.swift
// wires them. Phone and magic link share one pre-auth challenge store.
func authModule(pool *pgxpool.Pool, cfg config.Config, service *authservice.Service, limits *ratelimit.Store, analytics *posthog.Client) httpserver.Module {
	providers := map[string]authservice.OAuthProvider{}
	if aud := authservice.ParseOAuthAudiences(cfg.OAuthAppleClientID); len(aud) > 0 {
		providers["apple"] = authservice.NewAppleProvider(aud, "", nil)
	}
	if aud := authservice.ParseOAuthAudiences(cfg.OAuthGoogleClientID); len(aud) > 0 {
		providers["google"] = authservice.NewGoogleProvider(aud, "", nil)
	}
	fixed := func(code string) authservice.CodeGenerator {
		if code == "" {
			return authservice.DefaultCodeGenerator{}
		}
		return authservice.FixedCodeGenerator{Code: code}
	}
	return authroutes.New(pool, authroutes.Options{
		Service: service, Limits: limits, Analytics: analytics,
		OAuthProviders: providers,
		XClient:        authservice.NewXClient(slog.Default().With("label", "lv.auth.x")),
		SMS: authservice.NewSMSSender(authservice.SMSConfig{
			Kind: cfg.SMSKind, AccountSID: cfg.TwilioAccountSID, AuthToken: cfg.TwilioAuthToken, FromNumber: cfg.TwilioFromNumber,
		}, slog.Default().With("label", "lv.sms")),
		MagicSender: authservice.NewSender(authservice.SenderConfig{
			Kind: cfg.EmailKind, APIKey: cfg.EmailResendAPIKey, FromAddress: cfg.EmailFromAddress, ReplyTo: cfg.EmailReplyTo,
		}, slog.Default().With("label", "lv.auth.magic")),
		Challenges:   authservice.NewPreAuthChallengeStore(authservice.PreAuthLifetime, authservice.PreAuthMaxAttempts),
		PhoneCodes:   fixed(cfg.PhoneFixedOTP),
		MagicCodes:   fixed(cfg.MagicFixedOTP),
		PairingCodes: authservice.DefaultCodeGenerator{},
	})
}
