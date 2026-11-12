package authroutes

import (
	"lumina/internal/auth"
	"lumina/internal/wire"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// MeResponse is LuminaVaultShared.MeResponse, keys in declaration order.
type MeResponse struct {
	UserID            wire.UUID `json:"userId"`
	Email             string    `json:"email"`
	Username          string    `json:"username"`
	IsVerified        bool      `json:"isVerified"`
	PrivacyNoCNOrigin bool      `json:"privacyNoCNOrigin"`
	ContextRouting    bool      `json:"contextRouting"`
	AutoSaveLinks     bool      `json:"autoSaveLinks"`
	MnemosyneEnabled  bool      `json:"mnemosyneEnabled"`
	IsAdmin           bool      `json:"isAdmin"`
}

// updatePrivacyRequest is UpdatePrivacyRequest: every field optional, and
// only the ones present are written.
type updatePrivacyRequest struct {
	PrivacyNoCNOrigin *bool `json:"privacyNoCNOrigin,omitempty"`
	ContextRouting    *bool `json:"contextRouting,omitempty"`
	AutoSaveLinks     *bool `json:"autoSaveLinks,omitempty"`
	MnemosyneEnabled  *bool `json:"mnemosyneEnabled,omitempty"`
}

const meColumns = `id, email, username, is_verified, privacy_no_cn_origin, context_routing, auto_save_links, mnemosyne_enabled, is_admin`

func scanMe(row interface{ Scan(...any) error }) (MeResponse, error) {
	var (
		out MeResponse
		id  uuid.UUID
	)
	err := row.Scan(&id, &out.Email, &out.Username, &out.IsVerified, &out.PrivacyNoCNOrigin, &out.ContextRouting,
		&out.AutoSaveLinks, &out.MnemosyneEnabled, &out.IsAdmin)
	out.UserID = wire.UUID{UUID: id}
	return out, err
}

// me is meHandler: the identity JWTAuthenticator loaded.
func (m *Module) me(c *echo.Context) error {
	out, err := scanMe(m.pool.QueryRow(c.Request().Context(), `SELECT `+meColumns+` FROM users WHERE id = $1`, auth.UserID(c)))
	if err != nil {
		return err
	}
	return writeJSON(c, out)
}

// updatePrivacy is updatePrivacyHandler. Fluent's save writes only the fields
// that were set (plus updated_at), and nothing at all when none was.
func (m *Module) updatePrivacy(c *echo.Context) error {
	body, err := wire.Bind[updatePrivacyRequest](c)
	if err != nil {
		return err
	}
	ctx, id := c.Request().Context(), auth.UserID(c)
	if body.PrivacyNoCNOrigin == nil && body.ContextRouting == nil && body.AutoSaveLinks == nil && body.MnemosyneEnabled == nil {
		return m.me(c)
	}
	out, err := scanMe(m.pool.QueryRow(ctx, `UPDATE users SET
		privacy_no_cn_origin = COALESCE($2, privacy_no_cn_origin),
		context_routing = COALESCE($3, context_routing),
		auto_save_links = COALESCE($4, auto_save_links),
		mnemosyne_enabled = COALESCE($5, mnemosyne_enabled),
		updated_at = $6
		WHERE id = $1 RETURNING `+meColumns,
		id, body.PrivacyNoCNOrigin, body.ContextRouting, body.AutoSaveLinks, body.MnemosyneEnabled, time.Now()))
	if err != nil {
		return err
	}
	return writeJSON(c, out)
}
