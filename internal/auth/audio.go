package auth

import (
	"context"
	"errors"
	"lumina/internal/ids"
	"lumina/internal/wire"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// ScopeAudio is SessionToken.Scope.audio, the scope of the long-lived credential
// seeded into a tenant's Hermes container.
const ScopeAudio = "audio"

// AudioUserLoader is what AudioIdentify needs from the users table.
type AudioUserLoader interface {
	// AudioTokenEpoch reports whether the user exists and, if so, when their
	// outstanding audio tokens were last revoked (nil if never).
	AudioTokenEpoch(ctx context.Context, id uuid.UUID) (epoch *time.Time, found bool, err error)
}

type pgAudioUsers struct{ pool *pgxpool.Pool }

// PGAudioUsers reads users.hermes_audio_token_epoch.
func PGAudioUsers(pool *pgxpool.Pool) AudioUserLoader { return pgAudioUsers{pool} }

func (u pgAudioUsers) AudioTokenEpoch(ctx context.Context, id uuid.UUID) (*time.Time, bool, error) {
	var epoch *time.Time
	err := u.pool.QueryRow(ctx, `SELECT hermes_audio_token_epoch FROM users WHERE id = $1`, id).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return epoch, true, nil
}

// audioClaims keeps what Swift's SessionToken sees and RegisteredClaims loses: iat
// with its fraction (the parser truncates to seconds, and the comparison below is
// strict), and whether jti, a required field in Swift, is there at all.
type audioClaims struct {
	sessionClaims
	IssuedAtSeconds *float64 `json:"iat"`
	JTI             *string  `json:"jti"`
}

// AudioIdentify is AudioJWTAuthenticator, for the /v1/audio/* routes and nothing
// else: it attaches the user for an ordinary session (no scp) or for a token scoped
// ScopeAudio, and never rejects. Mount auth.Require after it.
//
// Every other route uses Identify or Middleware, which refuse any scope. That
// asymmetry is the security model: a compromised Hermes container holds a credential
// for these routes, not a session. Any other scope is refused here too, so adding a
// scope later can't silently widen this group.
//
// A scoped token is also checked against the tenant's revocation epoch: with one
// set, iat must be strictly later, and a missing iat is refused. An ordinary session
// is not checked against it.
func AudioIdentify(keys Keyset, users AudioUserLoader) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			header, _ := wire.HeaderValue(c.Request(), "Authorization")
			claims := &audioClaims{}
			if err := parse(keys, header, claims); err != nil || claims.JTI == nil {
				return next(c)
			}
			scoped := claims.Scope != nil
			if scoped && *claims.Scope != ScopeAudio {
				return next(c)
			}
			id, err := ids.Parse(claims.Subject)
			if err != nil {
				return next(c)
			}
			epoch, found, err := users.AudioTokenEpoch(c.Request().Context(), id)
			if err != nil {
				return err
			}
			if !found {
				return next(c)
			}
			if scoped && epoch != nil && !issuedAfter(claims.IssuedAtSeconds, *epoch) {
				return next(c)
			}
			c.Set(ctxUserID, id)
			c.Set(ctxHPID, claims.HPID)
			return next(c)
		}
	}
}

// issuedAfter is `issuedAt > epoch`, in seconds since 1970 as Foundation's Date
// compares them. A nil iat is a malformed scoped credential.
func issuedAfter(iat *float64, epoch time.Time) bool {
	return iat != nil && *iat > float64(epoch.UnixMicro())/1e6
}
