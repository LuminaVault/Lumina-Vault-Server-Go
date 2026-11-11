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
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/ids"
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

// paywallFor is EntitlementMiddleware.paywallResponse(for:).
func paywallFor(c Capability) paywall {
	if c.RequiresUltimate() {
		return paywall{Paywall: true, PaywallID: "ultimate_upsell", RequiredTier: "ultimate"}
	}
	return paywall{Paywall: true, PaywallID: "default", RequiredTier: "pro"}
}

// WritePaywall writes EntitlementMiddleware.paywallResponse(for:) as a 402, for
// handlers that check entitlement themselves (Swift's EntitlementDeniedError).
func WritePaywall(c *echo.Context, capability Capability) error {
	return wire.JSON(c, http.StatusPaymentRequired, paywallFor(capability))
}

// HasUsableCredential answers BYOEntitlementPolicy's "own key" question.
type HasUsableCredential func(ctx context.Context, tenant uuid.UUID) bool

// Middleware is EntitlementMiddleware(requires:enforcementEnabled:
// hasUsableCredential:) with platformFunded false, on a group that has no
// Hermes resolution (so the own-Hermes exemption never applies). Mount it
// after auth: like Swift it calls requireIdentity first.
func Middleware(pool *pgxpool.Pool, c Capability, enforcementEnabled bool, hasCredential HasUsableCredential) echo.MiddlewareFunc {
	return MiddlewareWithHermes(pool, c, enforcementEnabled, hasCredential, nil)
}

// MiddlewareWithHermes is Middleware on a group behind HermesResolutionMiddleware:
// ownHermes is context.hermesResolution?.isUserOverride, the free check Swift makes
// before it spends reads on hasCredential. Nil means no resolution on this group.
func MiddlewareWithHermes(pool *pgxpool.Pool, c Capability, enforcementEnabled bool, hasCredential HasUsableCredential, ownHermes func(*echo.Context) bool) echo.MiddlewareFunc {
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
			// BYOEntitlementPolicy.exemption.
			if !c.RequiresUltimate() && tier != "archived" {
				if ownHermes != nil && ownHermes(ctx) {
					return next(ctx)
				}
				if hasCredential != nil && hasCredential(ctx.Request().Context(), tenant) {
					return next(ctx)
				}
			}
			return wire.JSON(ctx, http.StatusPaymentRequired, paywallFor(c))
		}
	}
}

// PlatformFundedMiddleware is EntitlementMiddleware with platformFunded true: the
// route spends our key whoever calls it, so there is no bring-your-own exemption,
// and on top of the capability the effective tier must be trial or better. An
// override clears the floor. Mount it after auth, as Middleware.
func PlatformFundedMiddleware(pool *pgxpool.Pool, c Capability, enforcementEnabled bool) echo.MiddlewareFunc {
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
			if Entitled(tier, override, c) && meetsPlatformFundedFloor(tier, override) {
				return next(ctx)
			}
			return wire.JSON(ctx, http.StatusPaymentRequired, paywallFor(c))
		}
	}
}

func meetsPlatformFundedFloor(tier, override string) bool {
	switch EffectiveTier(tier, override) {
	case "trial", "pro", "ultimate":
		return true
	}
	return false
}

// EnforcementEnabled parses billing.enforcementEnabled as Swift does:
// enabled only when the value lowercases to exactly "true".
func EnforcementEnabled(raw string) bool {
	return strings.ToLower(raw) == "true"
}

// PremiumGuardMiddleware is PremiumGuardMiddleware.swift, on the /v1/grok routes:
// pro and ultimate pass, everyone else gets a 402 whose requiresXaiConnect sends iOS
// back to the Linked Accounts pane. Mount it after auth.
//
// It reads the raw columns on purpose, not EffectiveTier: any override other than
// "none" replaces the tier, and an unknown tier goes into the body as it is.
func PremiumGuardMiddleware(pool *pgxpool.Pool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(ctx *echo.Context) error {
			tenant, ok := auth.Identity(ctx)
			if !ok {
				return wire.ErrUnauthenticated
			}
			var tier, override string
			if err := pool.QueryRow(ctx.Request().Context(), `SELECT tier, tier_override FROM users WHERE id = $1`, tenant).Scan(&tier, &override); err != nil {
				return err
			}
			effective := tier
			if override != "none" {
				effective = override
			}
			if effective == "pro" || effective == "ultimate" {
				return next(ctx)
			}
			slog.InfoContext(ctx.Request().Context(), "premium guard rejected request",
				"userID", ids.Upper(tenant), "tier", effective, "path", ctx.Request().URL.Path)
			body := `{"error":"premium_required","tier":"` + effective + `","requiresXaiConnect":true}`
			return ctx.Blob(http.StatusPaymentRequired, echo.MIMEApplicationJSON, []byte(body))
		}
	}
}
