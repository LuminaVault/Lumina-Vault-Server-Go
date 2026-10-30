package auth

import (
	"context"
	"errors"
	"lumina/internal/ids"
	"lumina/internal/wire"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// Claims is the Swift SessionToken payload (Sources/App/Auth/JWT/SessionToken.swift).
type Claims struct {
	jwt.RegisteredClaims
	HPID string `json:"hpid,omitempty"`
	SCP  string `json:"scp,omitempty"`
}

// UserLoader reports whether the token's user still exists. Swift's
// JWTAuthenticator loads the User row and treats a missing row as
// unauthenticated, so a deleted account's live token gets 401.
type UserLoader interface {
	Exists(ctx context.Context, id uuid.UUID) (bool, error)
}

const (
	ctxUserID = "lv.auth.userID"
	ctxHPID   = "lv.auth.hpid"
)

// ErrUnauthorized is what protected routes return: Swift's exact 401 body.
var ErrUnauthorized = wire.ErrUnauthenticated

// Middleware authenticates "Authorization: Bearer <jwt>". Every failure
// (missing or malformed header, bad signature, expired, scoped token, unknown
// user) is a 401, as in Swift where the authenticator returns nil and the
// route's requireIdentity() throws.
func Middleware(keys Keyset, users UserLoader) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			claims, err := Verify(keys, c.Request().Header.Get("Authorization"))
			if err != nil {
				return ErrUnauthorized
			}
			id, err := ids.Parse(claims.Subject)
			if err != nil {
				return ErrUnauthorized
			}
			ok, err := users.Exists(c.Request().Context(), id)
			if err != nil {
				return err
			}
			if !ok {
				return ErrUnauthorized
			}
			c.Set(ctxUserID, id)
			c.Set(ctxHPID, claims.HPID)
			return next(c)
		}
	}
}

var errNoBearer = errors.New("auth: no bearer token")

// Verify checks the header value and returns the claims of a general session
// token. Scoped tokens (scp set) are machine credentials for one route group
// and never pass here. opts are extra parser options (tests pin the clock).
func Verify(keys Keyset, header string, opts ...jwt.ParserOption) (*Claims, error) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return nil, errNoBearer
	}
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		key, ok := keys.lookup(kid)
		if !ok {
			return nil, errors.New("auth: unknown kid")
		}
		return key.Secret, nil
	}, append([]jwt.ParserOption{jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired()}, opts...)...)
	if err != nil {
		return nil, err
	}
	if claims.SCP != "" {
		return nil, errors.New("auth: scoped token")
	}
	return claims, nil
}

// UserID returns the authenticated user. It panics if the route is not behind
// Middleware, which is a wiring bug, not a request error.
func UserID(c *echo.Context) uuid.UUID {
	return c.Get(ctxUserID).(uuid.UUID)
}

// HPID returns the token's Hermes profile id claim, or "".
func HPID(c *echo.Context) string {
	s, _ := c.Get(ctxHPID).(string)
	return s
}
