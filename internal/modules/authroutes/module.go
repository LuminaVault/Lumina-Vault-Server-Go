// Package authroutes serves wave 8A: the eleven password-session routes of
// LuminaVaultServer's AuthController (Sources/App/Auth/AuthController.swift)
// and GET /v1/auth/me and PUT /v1/auth/me/privacy (meHandler and
// updatePrivacyHandler in App+build.swift). /v1/auth/me/billing and
// /v1/auth/me/usage are the billing module's (7A). The work is
// internal/authservice.
//
// Each limited route gets its own per-IP policy, as AuthController mounts a
// fresh group per route; logout has none. The OAuth, X, phone, magic-link and
// pairing routes are 8B, WebAuthn 8C.
//
// Not ported: RouteTelemetry.observe("auth.*"), Swift's per-route timer, as in
// the other modules.
package authroutes

import (
	"net/http"
	"strings"

	"lumina/internal/authservice"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/ratelimit"
	"lumina/internal/wire"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Options holds the module's collaborators.
type Options struct {
	Service *authservice.Service
	Limits  *ratelimit.Store // the process-wide store
	// Analytics receives user_registered and user_logged_in. Nil sends nothing.
	Analytics authservice.Analytics
}

type Module struct {
	pool *pgxpool.Pool
	opt  Options
}

func New(pool *pgxpool.Pool, opt Options) *Module {
	return &Module{pool: pool, opt: opt}
}

// Register mounts AuthController's routes, then App+build's meGroup
// (jwtAuthenticator, then requireIdentity in the handler).
func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	ip := func(p ratelimit.Policy) echo.MiddlewareFunc {
		return ratelimit.Middleware(m.opt.Limits, p, ratelimit.IPOnlyKey)
	}
	r.POST("/v1/auth/register", m.register, ip(ratelimit.RegisterByIP))
	r.POST("/v1/auth/login", m.login, ip(ratelimit.LoginByIP))
	r.POST("/v1/auth/refresh", m.refresh, ip(ratelimit.RefreshByIP))
	r.POST("/v1/auth/mfa/verify", m.mfaVerify, ip(ratelimit.MFAVerifyByIP))
	r.POST("/v1/auth/mfa/resend", m.mfaResend, ip(ratelimit.MFAResendByIP))
	r.POST("/v1/auth/forgot-password", m.forgotPassword, ip(ratelimit.ForgotPasswordByIP))
	r.POST("/v1/auth/resend-reset", m.resendReset, ip(ratelimit.ResendResetByIP))
	r.POST("/v1/auth/reset-password", m.resetPassword, ip(ratelimit.ResetPasswordByIP))
	r.POST("/v1/auth/email/send-verify", m.sendVerification, ip(ratelimit.SendVerifyByIP))
	r.POST("/v1/auth/email/confirm", m.confirmEmail, ip(ratelimit.ConfirmEmailByIP))
	r.POST("/v1/auth/logout", m.logout)

	r.GET("/v1/auth/me", m.me, authMW)
	r.PUT("/v1/auth/me/privacy", m.updatePrivacy, authMW)
}

// Request bodies: LuminaVaultShared's DTOs. A field without omitempty is
// required, as in the Codable synthesized init.
type registerRequest struct {
	Email    string `json:"email"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	// MFACode is decoded (a wrong type is a 400) and never read, as in Swift.
	MFACode *string `json:"mfaCode,omitempty"`
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type mfaVerifyRequest struct {
	ChallengeID wire.UUID `json:"challengeId"`
	Code        string    `json:"code"`
}

// emailRequest is MFAResendRequest, ForgotPasswordRequest and SendVerificationRequest.
type emailRequest struct {
	Email string `json:"email"`
}

type resetPasswordRequest struct {
	Email       string `json:"email"`
	Code        string `json:"code"`
	NewPassword string `json:"newPassword"`
}

type confirmEmailRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

// writeJSON encodes as Foundation's JSONEncoder: an email may hold "/".
func writeJSON(c *echo.Context, v any) error {
	b, err := wire.MarshalFoundation(v)
	if err != nil {
		return err
	}
	return c.Blob(http.StatusOK, wire.ContentTypeJSON, b)
}

func (m *Module) capture(name string, resp *authservice.AuthResponse, props map[string]any) {
	if m.opt.Analytics != nil {
		m.opt.Analytics.Capture(name, ids.Upper(resp.UserID.UUID), props)
	}
}

func (m *Module) register(c *echo.Context) error {
	body, err := wire.Bind[registerRequest](c)
	if err != nil {
		return err
	}
	resp, err := m.opt.Service.Register(c.Request().Context(), body.Email, body.Username, body.Password)
	if err != nil {
		return err
	}
	m.capture("user_registered", resp, map[string]any{"auth_method": "password"})
	return writeJSON(c, resp)
}

func (m *Module) login(c *echo.Context) error {
	body, err := wire.Bind[loginRequest](c)
	if err != nil {
		return err
	}
	header, _ := wire.HeaderValue(c.Request(), "mfa-auth-v1")
	requireMFA := strings.ToLower(header) == "true"
	resp, err := m.opt.Service.Login(c.Request().Context(), body.Email, body.Password, requireMFA)
	if err != nil {
		return err
	}
	// Fires even when MFA is pending, as in Swift.
	m.capture("user_logged_in", resp, map[string]any{"auth_method": "password", "mfa_required": requireMFA})
	return writeJSON(c, resp)
}

func (m *Module) mfaVerify(c *echo.Context) error {
	body, err := wire.Bind[mfaVerifyRequest](c)
	if err != nil {
		return err
	}
	resp, err := m.opt.Service.VerifyMFA(c.Request().Context(), body.ChallengeID.UUID, body.Code)
	if err != nil {
		return err
	}
	return writeJSON(c, resp)
}

func (m *Module) mfaResend(c *echo.Context) error {
	body, err := wire.Bind[emailRequest](c)
	if err != nil {
		return err
	}
	if err := m.opt.Service.ResendMFA(c.Request().Context(), body.Email); err != nil {
		return err
	}
	return c.NoContent(http.StatusAccepted)
}

func (m *Module) refresh(c *echo.Context) error {
	body, err := wire.Bind[refreshRequest](c)
	if err != nil {
		return err
	}
	resp, err := m.opt.Service.Refresh(c.Request().Context(), body.RefreshToken)
	if err != nil {
		return err
	}
	return writeJSON(c, resp)
}

func (m *Module) logout(c *echo.Context) error {
	body, err := wire.Bind[refreshRequest](c)
	if err != nil {
		return err
	}
	if err := m.opt.Service.RevokeRefresh(c.Request().Context(), body.RefreshToken); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (m *Module) forgotPassword(c *echo.Context) error {
	body, err := wire.Bind[emailRequest](c)
	if err != nil {
		return err
	}
	if err := m.opt.Service.ForgotPassword(c.Request().Context(), body.Email); err != nil {
		return err
	}
	return c.NoContent(http.StatusAccepted)
}

func (m *Module) resendReset(c *echo.Context) error {
	body, err := wire.Bind[emailRequest](c)
	if err != nil {
		return err
	}
	if err := m.opt.Service.ResendReset(c.Request().Context(), body.Email); err != nil {
		return err
	}
	return c.NoContent(http.StatusAccepted)
}

func (m *Module) resetPassword(c *echo.Context) error {
	body, err := wire.Bind[resetPasswordRequest](c)
	if err != nil {
		return err
	}
	resp, err := m.opt.Service.ResetPassword(c.Request().Context(), body.Email, body.Code, body.NewPassword)
	if err != nil {
		return err
	}
	return writeJSON(c, resp)
}

func (m *Module) sendVerification(c *echo.Context) error {
	body, err := wire.Bind[emailRequest](c)
	if err != nil {
		return err
	}
	if err := m.opt.Service.SendVerification(c.Request().Context(), body.Email); err != nil {
		return err
	}
	return c.NoContent(http.StatusAccepted)
}

func (m *Module) confirmEmail(c *echo.Context) error {
	body, err := wire.Bind[confirmEmailRequest](c)
	if err != nil {
		return err
	}
	if err := m.opt.Service.ConfirmEmail(c.Request().Context(), body.Email, body.Code); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
