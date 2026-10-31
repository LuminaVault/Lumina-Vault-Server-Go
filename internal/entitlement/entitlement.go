// Package entitlement is the Go port of LuminaVaultServer's billing gate:
// Billing/EntitlementChecker.swift (tier × capability), BYOEntitlementPolicy
// (bring-your-own exemption) and EntitlementMiddleware (the 402 paywall).
//
// Only what a migrated route needs is here. Wave 3 mounts it on POST
// /v1/health (.healthIngest); chat, capture and the compile routes mount it
// with their own capabilities when they move.
package entitlement

import (
	"context"
	"lumina/internal/auth"
	"lumina/internal/wire"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Capability is Swift's Capability raw value.
type Capability string

const (
	VaultRead            Capability = "vaultRead"
	VaultExport          Capability = "vaultExport"
	Capture              Capability = "capture"
	HealthIngest         Capability = "healthIngest"
	Chat                 Capability = "chat"
	MemoryQuery          Capability = "memoryQuery"
	MemoGenerator        Capability = "memoGenerator"
	SkillBuiltinRun      Capability = "skillBuiltinRun"
	SkillVaultRun        Capability = "skillVaultRun"
	KBCompile            Capability = "kbCompile"
	MemoryCompile        Capability = "memoryCompile"
	PrivacyBYOKey        Capability = "privacyBYOKey"
	PrivacyContextRouter Capability = "privacyContextRouter"
	MLXOnDevice          Capability = "mlxOnDevice"
	WorkflowAutomation   Capability = "workflowAutomation"
)

// RequiresUltimate mirrors Capability.requiresUltimate.
func (c Capability) RequiresUltimate() bool {
	switch c {
	case SkillVaultRun, PrivacyBYOKey, PrivacyContextRouter, MLXOnDevice:
		return true
	}
	return false
}

var knownTiers = map[string]bool{"free": true, "trial": true, "pro": true, "ultimate": true, "lapsed": true, "archived": true}

// EffectiveTier is EntitlementChecker.effectiveTier over the raw column
// values: an unknown tier reads as lapsed, an unknown override as none, and
// an override only ever raises the tier.
func EffectiveTier(tier, override string) string {
	if !knownTiers[tier] {
		tier = "lapsed"
	}
	switch override {
	case "ultimate":
		return "ultimate"
	case "pro":
		if tier == "ultimate" {
			return "ultimate"
		}
		return "pro"
	}
	return tier
}

// Entitled is EntitlementChecker.matchEntitlement on the effective tier.
func Entitled(tier, override string, c Capability) bool {
	eff := EffectiveTier(tier, override)
	switch c {
	case VaultRead, VaultExport, Chat, MemoryQuery, Capture:
		return eff != "archived"
	case HealthIngest, MemoGenerator, SkillBuiltinRun, KBCompile, MemoryCompile, WorkflowAutomation:
		return eff == "trial" || eff == "pro" || eff == "ultimate"
	case SkillVaultRun, PrivacyBYOKey, PrivacyContextRouter, MLXOnDevice:
		return eff == "ultimate"
	}
	return false
}

// paywall is Swift's PaywallResponse, in declaration order.
type paywall struct {
	Paywall      bool   `json:"paywall"`
	PaywallID    string `json:"paywallId"`
	RequiredTier string `json:"requiredTier"`
}

// HasUsableCredential answers BYOEntitlementPolicy's "own key" question.
type HasUsableCredential func(ctx context.Context, tenant uuid.UUID) bool

// Middleware is EntitlementMiddleware(requires:enforcementEnabled:
// hasUsableCredential:) with platformFunded false, on a group that has no
// Hermes resolution (so the own-Hermes exemption never applies). Mount it
// after auth: like Swift it calls requireIdentity first.
func Middleware(pool *pgxpool.Pool, c Capability, enforcementEnabled bool, hasCredential HasUsableCredential) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		if !enforcementEnabled {
			return next
		}
		return func(ctx *echo.Context) error {
			tenant, ok := auth.Identity(ctx)
			if !ok {
				return wire.ErrUnauthenticated
			}
			var tier, override string
			if err := pool.QueryRow(ctx.Request().Context(), `SELECT tier, tier_override FROM users WHERE id = $1`, tenant).Scan(&tier, &override); err != nil {
				return err
			}
			if Entitled(tier, override, c) {
				return next(ctx)
			}
			// BYOEntitlementPolicy.exemption with hasOwnHermes false.
			if !c.RequiresUltimate() && tier != "archived" && hasCredential != nil && hasCredential(ctx.Request().Context(), tenant) {
				return next(ctx)
			}
			p := paywall{Paywall: true, PaywallID: "default", RequiredTier: "pro"}
			if c.RequiresUltimate() {
				p.PaywallID, p.RequiredTier = "ultimate_upsell", "ultimate"
			}
			return wire.JSON(ctx, http.StatusPaymentRequired, p)
		}
	}
}

// EnforcementEnabled parses billing.enforcementEnabled as Swift does:
// enabled only when the value lowercases to exactly "true".
func EnforcementEnabled(raw string) bool {
	return strings.ToLower(raw) == "true"
}
