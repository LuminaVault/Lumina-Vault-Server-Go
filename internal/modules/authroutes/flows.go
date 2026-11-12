package authroutes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/authservice"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"regexp"
	"strings"

	"github.com/labstack/echo/v5"
)

// Wave 8B's sign-in flows: AuthController.oauthExchange, XOAuthController,
// PhoneAuthController and EmailMagicLinkController. Each ends in
// UpsertOAuthUser and IssueTokens.

// Errors, verbatim.
var (
	errUnsupportedProvider = wire.NewError(http.StatusNotFound, "unsupported provider")
	errInvalidIDToken      = wire.NewError(http.StatusUnauthorized, "invalid id_token")
	errAccessTokenRequired = wire.NewError(http.StatusBadRequest, "accessToken required")
	errInvalidPhone        = wire.NewError(http.StatusBadRequest, `phone must be E.164 (^\+[1-9]\d{6,14}$)`)
	errInvalidEmail        = wire.NewError(http.StatusBadRequest, "email looks invalid")
	errOTPInvalid          = wire.NewError(http.StatusUnauthorized, "invalid or expired code")
	errOTPExpired          = wire.NewError(http.StatusGone, "challenge expired; request a new code")
)

type oauthExchangeRequest struct {
	IDToken string `json:"idToken"`
}

type xExchangeRequest struct {
	AccessToken string `json:"accessToken"`
}

type phoneStartRequest struct {
	Phone string `json:"phone"`
}

type phoneVerifyRequest struct {
	Phone string `json:"phone"`
	Code  string `json:"code"`
}

type emailMagicStartRequest struct {
	Email string `json:"email"`
}

type emailMagicVerifyRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

// challengeResponse is PhoneStartResponse and EmailMagicStartResponse.
type challengeResponse struct {
	ChallengeID wire.UUID `json:"challengeId"`
	ExpiresAt   wire.Time `json:"expiresAt"`
}

// oauthExchange is AuthController.oauthExchange: the provider is looked up
// before the body is read, and only an OAuthError becomes the 401; a JWKS that
// can't be fetched is a 500.
func (m *Module) oauthExchange(c *echo.Context) error {
	provider, ok := m.opt.OAuthProviders[c.Param("provider")]
	if !ok {
		return errUnsupportedProvider
	}
	body, err := wire.Bind[oauthExchangeRequest](c)
	if err != nil {
		return err
	}
	resp, err := m.opt.Service.ExchangeOAuth(finish(c), provider, body.IDToken)
	var oauthErr authservice.OAuthError
	if errors.As(err, &oauthErr) {
		return errInvalidIDToken
	}
	if err != nil {
		return err
	}
	return writeJSON(c, resp)
}

// xExchange is XOAuthController.exchange. X may return no email: the account
// then gets "<id>@x.luminavault.local", unverified.
func (m *Module) xExchange(c *echo.Context) error {
	body, err := wire.Bind[xExchangeRequest](c)
	if err != nil {
		return err
	}
	if body.AccessToken == "" {
		return errAccessTokenRequired
	}
	ctx := finish(c)
	xUser, err := m.opt.XClient.FetchMe(ctx, body.AccessToken)
	if err != nil {
		return err
	}
	email := xUser.ID + "@x.luminavault.local"
	if xUser.Email != nil {
		email = swiftstr.Lowercased(*xUser.Email)
	}
	u, err := m.opt.Service.UpsertOAuthUser(ctx, "x", xUser.ID, email, xUser.Email != nil)
	if err != nil {
		return err
	}
	slog.Default().With("label", "lv.auth.x").Info(fmt.Sprintf("x oauth: linked id=%s username=%s", xUser.ID, xUser.Username))
	resp, err := m.opt.Service.IssueTokens(ctx, u)
	if err != nil {
		return err
	}
	return writeJSON(c, resp)
}

// finish is the request context without its cancellation: Hummingbird runs a
// handler to the end after the client leaves, so a sign-in that has consumed
// its code or fetched its identity still writes the user, the identity and the
// refresh token.
func finish(c *echo.Context) context.Context { return context.WithoutCancel(c.Request().Context()) }

// e164 is PhoneAuthController's pattern under NSRegularExpression, where \d is
// any Unicode decimal digit.
var e164 = regexp.MustCompile(`^\+[1-9]\p{Nd}{6,14}$`)

// ValidateE164 is PhoneAuthController.validateE164: trimmed of whitespace and
// newlines, then matched.
func ValidateE164(raw string) (string, error) {
	trimmed := swiftstr.TrimWhitespacesAndNewlines(raw)
	if !e164.MatchString(trimmed) {
		return "", errInvalidPhone
	}
	return trimmed, nil
}

// phoneStart is PhoneAuthController.start. The challenge is issued before the
// send, so a failed send leaves it in the store. Swift weakness, kept: anyone
// can make the server text any number (the per-IP limits are the only bound).
func (m *Module) phoneStart(c *echo.Context) error {
	body, err := wire.Bind[phoneStartRequest](c)
	if err != nil {
		return err
	}
	phone, err := ValidateE164(body.Phone)
	if err != nil {
		return err
	}
	code := m.opt.PhoneCodes.Generate()
	id, expiresAt := m.opt.Challenges.Issue("sms", phone, "phone_signin", code)
	if err := m.opt.SMS.Send(c.Request().Context(), code, phone, "phone_signin"); err != nil {
		return err
	}
	slog.Default().With("label", "lv.auth.phone").Info("phone OTP issued: phone=" + phone)
	return writeJSON(c, challengeResponse{ChallengeID: wire.UUID{UUID: id}, ExpiresAt: wire.Time{Time: expiresAt}})
}

// phoneVerify is PhoneAuthController.verify: 410 for an expired challenge, 401
// for a missing, wrong or locked-out one. The account's email is a placeholder
// built from the number.
func (m *Module) phoneVerify(c *echo.Context) error {
	body, err := wire.Bind[phoneVerifyRequest](c)
	if err != nil {
		return err
	}
	phone, err := ValidateE164(body.Phone)
	if err != nil {
		return err
	}
	switch outcome, _, _ := m.opt.Challenges.ConsumeTyped(phone, body.Code); outcome {
	case authservice.ConsumeOK:
	case authservice.ConsumeExpired:
		return errOTPExpired
	default:
		return errOTPInvalid
	}
	ctx := finish(c)
	placeholder := strings.TrimPrefix(phone, "+") + "@phone.luminavault.local"
	u, err := m.opt.Service.UpsertOAuthUser(ctx, "phone", phone, placeholder, false)
	if err != nil {
		return err
	}
	resp, err := m.opt.Service.IssueTokens(ctx, u)
	if err != nil {
		return err
	}
	return writeJSON(c, resp)
}

// NormalizeMagicEmail is EmailMagicLinkController.normalizeEmail: trimmed,
// lowercased, holding "@", 5 to 254 characters.
func NormalizeMagicEmail(raw string) (string, error) {
	s := swiftstr.Lowercased(swiftstr.TrimWhitespacesAndNewlines(raw))
	n := swiftstr.CharacterCount(s)
	if !strings.Contains(s, "@") || n < 5 || n > 254 {
		return "", errInvalidEmail
	}
	return s, nil
}

// magicStart is EmailMagicLinkController.start. Swift weakness, kept: it mails
// a code to any address, account or not.
func (m *Module) magicStart(c *echo.Context) error {
	body, err := wire.Bind[emailMagicStartRequest](c)
	if err != nil {
		return err
	}
	email, err := NormalizeMagicEmail(body.Email)
	if err != nil {
		return err
	}
	code := m.opt.MagicCodes.Generate()
	id, expiresAt := m.opt.Challenges.Issue("email", email, "magic_link", code)
	if err := m.opt.MagicSender.Send(c.Request().Context(), code, email, "magic_link"); err != nil {
		return err
	}
	slog.Default().With("label", "lv.auth.magic").Info("magic-link OTP issued: email=" + email)
	return writeJSON(c, challengeResponse{ChallengeID: wire.UUID{UUID: id}, ExpiresAt: wire.Time{Time: expiresAt}})
}

// magicVerify is EmailMagicLinkController.verify: every failure, expiry
// included, is the 401. A code proves the address, so an existing password
// account with that email is signed in (and linked).
func (m *Module) magicVerify(c *echo.Context) error {
	body, err := wire.Bind[emailMagicVerifyRequest](c)
	if err != nil {
		return err
	}
	email, err := NormalizeMagicEmail(body.Email)
	if err != nil {
		return err
	}
	if _, _, ok := m.opt.Challenges.Consume(email, body.Code); !ok {
		return errOTPInvalid
	}
	ctx := finish(c)
	u, err := m.opt.Service.UpsertOAuthUser(ctx, "email_magic_link", email, email, true)
	if err != nil {
		return err
	}
	resp, err := m.opt.Service.IssueTokens(ctx, u)
	if err != nil {
		return err
	}
	return writeJSON(c, resp)
}
