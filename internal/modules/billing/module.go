// Package billing is wave 7A: the RevenueCat webhook
// (Billing/RevenueCatWebhookController.swift) and the two billing read paths
// App+build.swift mounts beside it, GET /v1/auth/me/billing (meBillingHandler)
// and GET /v1/auth/me/usage (UsageMetricsService).
//
// The webhook keeps Swift's shape on purpose (wave-6-8-plan.md "Billing"): the
// idempotency check, the effect and the log row are not one transaction, and a
// duplicate that loses the unique index is a 500 so RevenueCat retries.
package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/entitlement"
	"lumina/internal/httpserver"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Options configure the module.
type Options struct {
	WebhookSecret      string
	AdminToken         string // ADMIN_TOKEN, for PUT /v1/admin/users/:userID/tier-override
	EnforcementEnabled bool
	Now                func() time.Time
	Log                *slog.Logger
}

// Module serves /v1/billing and the /v1/auth/me billing reads.
type Module struct {
	pool *pgxpool.Pool
	opt  Options
}

func New(pool *pgxpool.Pool, opt Options) *Module {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Log == nil {
		opt.Log = slog.Default().With("label", "lv.billing.revenuecat-webhook")
	}
	return &Module{pool: pool, opt: opt}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.POST("/v1/billing/revenuecat-webhook", m.webhook)
	r.GET("/v1/auth/me/billing", m.meBilling, authMW)
	r.GET("/v1/auth/me/usage", m.meUsage, authMW)
	r.PUT("/v1/admin/users/:userID/tier-override", m.setTierOverride, m.requireAdminToken)
}

// maxWebhookBody is `request.body.collect(upTo: 1024 * 1024)`.
const maxWebhookBody = 1 << 20

// BillingRetryGrace is billingRetryGrace: a BILLING_ISSUE holds the tier 16 days.
const BillingRetryGrace = 16 * 24 * time.Hour

type rcEvent struct {
	ID             string  `json:"id"`
	Type           string  `json:"type"`
	AppUserID      string  `json:"app_user_id"`
	ExpirationAtMs *int64  `json:"expiration_at_ms,omitempty"`
	ProductID      *string `json:"product_id,omitempty"`
	IsRefund       *bool   `json:"is_refund,omitempty"`
}

type rcPayload struct {
	Event rcEvent `json:"event"`
}

// UnknownProductError is RevenueCatWebhookController.UnknownProductError: a 500
// with no log row, so RevenueCat retries instead of stranding a paying user.
type UnknownProductError struct{ ProductID *string }

func (e *UnknownProductError) Error() string {
	id := "<none>"
	if e.ProductID != nil {
		id = *e.ProductID
	}
	return "unmapped RevenueCat product: " + id
}

func constantTimeEquals(a, b string) bool {
	return len(a) == len(b) && hmac.Equal([]byte(a), []byte(b))
}

func (m *Module) webhook(c *echo.Context) error {
	if m.opt.WebhookSecret == "" {
		m.opt.Log.Error("webhook secret not configured")
		return echo.ErrInternalServerError
	}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, maxWebhookBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxWebhookBody {
		return wire.ErrBodyTooLarge
	}

	authHeader := strings.ReplaceAll(c.Request().Header.Get("Authorization"), "Bearer ", "")
	signature := c.Request().Header.Get("X-RevenueCat-Signature")
	valid := authHeader != "" && constantTimeEquals(authHeader, m.opt.WebhookSecret)
	if !valid && signature != "" {
		mac := hmac.New(sha256.New, []byte(m.opt.WebhookSecret))
		mac.Write(body)
		valid = constantTimeEquals(hex.EncodeToString(mac.Sum(nil)), signature)
	}
	if !valid {
		m.opt.Log.Warn("invalid webhook signature")
		return echo.ErrUnauthorized
	}

	payload, err := wire.Decode[rcPayload](body)
	if err != nil {
		m.opt.Log.Error("failed to decode webhook payload", "error", err)
		return echo.ErrBadRequest
	}
	event := payload.Event
	ctx := c.Request().Context()

	var seen bool
	if err := m.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM billing_event_logs WHERE event_id = $1)`, event.ID).Scan(&seen); err != nil {
		return err
	}
	if seen {
		m.opt.Log.Info("webhook event already processed", "event_id", event.ID)
		return c.NoContent(http.StatusOK)
	}

	userID, err := m.processEvent(ctx, event)
	if err != nil {
		return err
	}
	if _, err := m.pool.Exec(ctx, `INSERT INTO billing_event_logs (id, event_id, event_type, user_id, processed_at)
		VALUES ($1, $2, $3, $4, $5)`, uuid.New(), event.ID, event.Type, userID, m.opt.Now()); err != nil {
		return err
	}
	return c.NoContent(http.StatusOK)
}

// swiftUUID is UUID(uuidString:): only the 36-character hyphenated form.
func swiftUUID(s string) (uuid.UUID, bool) {
	if len(s) != 36 {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(s)
	return id, err == nil
}

// processEvent applies the event to its user and returns that user's id, or nil
// when no user matches (the log row is still written: a 200 ends the retries).
func (m *Module) processEvent(ctx context.Context, event rcEvent) (*uuid.UUID, error) {
	var (
		id        uuid.UUID
		tier      string
		expiresAt *time.Time
	)
	q := `SELECT id, tier, tier_expires_at FROM users WHERE revenuecat_user_id = $1 LIMIT 1`
	args := []any{event.AppUserID}
	if asID, ok := swiftUUID(event.AppUserID); ok {
		q = `SELECT id, tier, tier_expires_at FROM users WHERE revenuecat_user_id = $1 OR id = $2 LIMIT 1`
		args = append(args, asID)
	}
	err := m.pool.QueryRow(ctx, q, args...).Scan(&id, &tier, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		m.opt.Log.Warn("user not found for revenuecat_user_id", "rc_user_id", event.AppUserID)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	now := m.opt.Now()
	var newTier *string
	var newExpiry *time.Time
	switch event.Type {
	case "INITIAL_PURCHASE", "PRODUCT_CHANGE", "RENEWAL", "UNCANCELLATION":
		t, ok := "", false
		if event.ProductID != nil {
			t, ok = TierForProduct(*event.ProductID)
		}
		if !ok {
			m.opt.Log.Error("revenuecat reported a product we do not sell", "event_type", event.Type)
			return nil, &UnknownProductError{ProductID: event.ProductID}
		}
		newTier = &t
		if event.ExpirationAtMs != nil {
			exp := time.UnixMicro(*event.ExpirationAtMs * 1000)
			newExpiry = &exp
		}
	case "CANCELLATION":
		var exp int64
		if event.ExpirationAtMs != nil {
			exp = *event.ExpirationAtMs
		}
		if (event.IsRefund != nil && *event.IsRefund) || exp < now.UnixMilli() {
			lapsed := "lapsed"
			newTier = &lapsed
		}
	case "EXPIRATION":
		lapsed := "lapsed"
		newTier = &lapsed
	case "BILLING_ISSUE":
		hold := now.Add(BillingRetryGrace)
		if expiresAt == nil || expiresAt.Before(hold) {
			newExpiry = &hold
		}
		m.opt.Log.Warn("billing issue — holding tier through the retry window", "rc_user_id", event.AppUserID, "tier", tier)
	case "SUBSCRIBER_ALIAS":
		m.opt.Log.Info("subscriber alias event", "rc_user_id", event.AppUserID)
	default:
		m.opt.Log.Info("unhandled event type", "type", event.Type)
	}

	set := []string{"revenuecat_user_id = $2", "updated_at = $3"}
	args = []any{id, event.AppUserID, now}
	if newTier != nil {
		args = append(args, *newTier)
		set = append(set, fmt.Sprintf("tier = $%d", len(args)))
	}
	if newExpiry != nil {
		args = append(args, *newExpiry)
		set = append(set, fmt.Sprintf("tier_expires_at = $%d", len(args)))
	}
	if _, err := m.pool.Exec(ctx, `UPDATE users SET `+strings.Join(set, ", ")+` WHERE id = $1`, args...); err != nil {
		return nil, err
	}
	return &id, nil
}

// MeBillingResponse is LuminaVaultShared.MeBillingResponse.
type MeBillingResponse struct {
	Tier               string     `json:"tier"`
	TierOverride       string     `json:"tierOverride"`
	TierExpiresAt      *wire.Time `json:"tierExpiresAt,omitempty"`
	InTrial            bool       `json:"inTrial"`
	DaysRemaining      int        `json:"daysRemaining"`
	EnforcementEnabled bool       `json:"enforcementEnabled"`
}

// MeBilling is meBillingHandler: the effective tier (override folded in), the
// raw override beside it, and whole days left (never negative).
func MeBilling(tier, override string, expiresAt *time.Time, now time.Time, enforcement bool) MeBillingResponse {
	days := 0
	if expiresAt != nil {
		if d := int(expiresAt.Sub(now) / (24 * time.Hour)); d > 0 {
			days = d
		}
	}
	effective := entitlement.EffectiveTier(tier, override)
	out := MeBillingResponse{
		Tier: effective, TierOverride: override, InTrial: effective == "trial" && days > 0,
		DaysRemaining: days, EnforcementEnabled: enforcement,
	}
	if expiresAt != nil {
		out.TierExpiresAt = &wire.Time{Time: *expiresAt}
	}
	return out
}

func (m *Module) meBilling(c *echo.Context) error {
	var (
		tier, override string
		expiresAt      *time.Time
	)
	if err := m.pool.QueryRow(c.Request().Context(), `SELECT tier, tier_override, tier_expires_at FROM users WHERE id = $1`,
		auth.UserID(c)).Scan(&tier, &override, &expiresAt); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, MeBilling(tier, override, expiresAt, m.opt.Now(), m.opt.EnforcementEnabled))
}

// requireAdminToken is AdminTokenMiddleware on BillingAdminController's group.
func (m *Module) requireAdminToken(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if m.opt.AdminToken == "" {
			return wire.NewError(http.StatusNotFound, "admin disabled")
		}
		presented := c.Request().Header.Values("X-Admin-Token")
		if len(presented) == 0 || !constantTimeEquals(presented[0], m.opt.AdminToken) {
			return wire.NewError(http.StatusUnauthorized, "admin token invalid")
		}
		return next(c)
	}
}

type setTierOverrideRequest struct {
	TierOverride string `json:"tierOverride"`
}

// UserBillingSummary is Admin/BillingAdminController.swift's response.
type UserBillingSummary struct {
	ID               wire.UUID  `json:"id"`
	Tier             string     `json:"tier"`
	TierExpiresAt    *wire.Time `json:"tierExpiresAt,omitempty"`
	TierOverride     string     `json:"tierOverride"`
	RevenuecatUserID *string    `json:"revenuecatUserID,omitempty"`
}

// setTierOverride is BillingAdminController.setTierOverride.
func (m *Module) setTierOverride(c *echo.Context) error {
	id, ok := swiftUUID(c.Param("userID"))
	if !ok {
		return wire.NewError(http.StatusBadRequest, "invalid userID")
	}
	body, err := wire.Bind[setTierOverrideRequest](c)
	if err != nil {
		return err
	}
	switch body.TierOverride {
	case "none", "pro", "ultimate":
	default:
		return wire.NewError(http.StatusBadRequest, "tierOverride must be one of: none, pro, ultimate")
	}
	var (
		out UserBillingSummary
		exp *time.Time
		uid uuid.UUID
	)
	err = m.pool.QueryRow(c.Request().Context(), `UPDATE users SET tier_override = $2, updated_at = $3 WHERE id = $1
		RETURNING id, tier, tier_expires_at, tier_override, revenuecat_user_id`, id, body.TierOverride, m.opt.Now()).
		Scan(&uid, &out.Tier, &exp, &out.TierOverride, &out.RevenuecatUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return wire.NewError(http.StatusNotFound, "user not found")
	}
	if err != nil {
		return err
	}
	out.ID = wire.UUID{UUID: uid}
	if exp != nil {
		out.TierExpiresAt = &wire.Time{Time: *exp}
	}
	return wire.JSON(c, http.StatusOK, out)
}
