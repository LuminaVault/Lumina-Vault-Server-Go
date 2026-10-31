// Package devices serves /v1/devices (APNS / FCM token registration): the Go
// port of LuminaVaultServer Sources/App/Devices/DeviceController.swift. It
// flips together with apple (decisions.md D-shared-prefixes).
package devices

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/devices/oapi-codegen.yaml api/openapi.yaml"

import (
	"encoding/json"
	"errors"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/modules/devices/db"
	"lumina/internal/wire"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

var errTokenRequired = wire.NewError(http.StatusBadRequest, "token required")

type Module struct {
	q *db.Queries
	// bundleID is `apns.bundleId` (APNS_BUNDLE_ID).
	bundleID string
	now      func() time.Time
}

func New(pool *pgxpool.Pool, apnsBundleID string) *Module {
	return &Module{q: db.New(pool), bundleID: apnsBundleID, now: time.Now}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.POST("/v1/devices", m.register, authMW)
	r.DELETE("/v1/devices/:token", m.unregister, authMW)
}

// registrationRequest is DeviceRegistrationRequest in Swift field order.
type registrationRequest struct {
	Token    string   `json:"token"`
	Platform platform `json:"platform"`
	BundleID *string  `json:"bundleId,omitempty"`
}

// platform is DevicePlatform: an unknown raw value fails as Swift's
// RawRepresentable decoding does.
type platform string

func (p *platform) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	if s != "ios" && s != "android" {
		return &wire.CorruptedError{Message: "Cannot initialize DevicePlatform from invalid String value " + s}
	}
	*p = platform(s)
	return nil
}

func (m *Module) register(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	body, err := wire.Bind[registrationRequest](c)
	if err != nil {
		return err
	}
	if body.Token == "" { // not trimmed, as in Swift
		return errTokenRequired
	}
	bundleID := m.allowedBundleID(body.BundleID)
	now := m.now()

	existing, err := m.q.FindDeviceToken(ctx, db.FindDeviceTokenParams{TenantID: tenant, Token: body.Token})
	switch {
	case err == nil:
		// An older client sends no bundle id; keep what a newer one stored.
		stored := existing.BundleID
		if body.BundleID != nil {
			stored = bundleID
		}
		if err := m.q.TouchDeviceToken(ctx, db.TouchDeviceTokenParams{
			ID: existing.ID, Platform: string(body.Platform), LastSeenAt: &now, BundleID: stored,
		}); err != nil {
			return err
		}
		return wire.JSON(c, http.StatusOK, DeviceRegistrationResponse{
			Id: wire.UUID{UUID: existing.ID}, Token: existing.Token, Platform: string(body.Platform),
		})
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}

	// device_tokens.token is globally unique: a token another account holds
	// fails this insert and answers 500, exactly as Swift does.
	id := uuid.New()
	if err := m.q.InsertDeviceToken(ctx, db.InsertDeviceTokenParams{
		ID: id, TenantID: tenant, Token: body.Token, Platform: string(body.Platform), LastSeenAt: &now, BundleID: bundleID,
	}); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, DeviceRegistrationResponse{
		Id: wire.UUID{UUID: id}, Token: body.Token, Platform: string(body.Platform),
	})
}

// allowedBundleID is DeviceController.allowedBundleID: the trimmed requested
// id when the server may push to it, else nil (a foreign id is dropped, not
// refused).
func (m *Module) allowedBundleID(requested *string) *string {
	if requested == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*requested)
	if trimmed == "" {
		return nil
	}
	if !IsAllowedBundleID(trimmed, m.bundleID) {
		slog.Debug("device bundle id is not the configured one or a variant; using the default topic", "bundleId", trimmed)
		return nil
	}
	return &trimmed
}

// IsAllowedBundleID is APNSNotificationService.isAllowedBundleID.
func IsAllowedBundleID(candidate, configured string) bool {
	if configured == "" || candidate == "" {
		return false
	}
	return candidate == configured || strings.HasPrefix(candidate, configured+".")
}

// Topic is APNSNotificationService.topic(for:configured:).
func Topic(deviceBundleID *string, configured string) string {
	if deviceBundleID != nil && IsAllowedBundleID(*deviceBundleID, configured) {
		return *deviceBundleID
	}
	return configured
}

// unregister deletes the caller's rows for the token and always answers 204.
// Hummingbird matches on the raw request path, so the token is the segment
// as sent (percent escapes are not decoded).
func (m *Module) unregister(c *echo.Context) error {
	token := strings.TrimPrefix(c.Request().URL.EscapedPath(), "/v1/devices/")
	if err := m.q.DeleteDeviceTokens(c.Request().Context(), db.DeleteDeviceTokensParams{TenantID: auth.UserID(c), Token: token}); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
