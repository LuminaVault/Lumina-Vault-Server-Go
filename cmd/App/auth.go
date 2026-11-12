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

// authModule is wave 8A: the password-session routes and /v1/auth/me, /me/privacy.
func authModule(pool *pgxpool.Pool, service *authservice.Service, limits *ratelimit.Store, analytics *posthog.Client) httpserver.Module {
	return authroutes.New(pool, authroutes.Options{Service: service, Limits: limits, Analytics: analytics})
}
