package auth

import (
	"context"
	"lumina/internal/ids"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AudioTokenLifetime is HermesAudioTokenService.lifetime: 90 days.
const AudioTokenLifetime = 90 * 24 * time.Hour

// AudioTokens is HermesAudioTokenService: it mints the ScopeAudio credential
// seeded into a tenant's Hermes container and revokes them all by moving the
// tenant's epoch. It is the one token Go mints before wave 8.
type AudioTokens struct {
	Keys Keyset
	Pool *pgxpool.Pool
	Now  func() time.Time
}

func (a AudioTokens) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// audioTokenClaims is SessionToken as JWTKit writes it: times as fractional
// seconds, so iat stays strictly after an epoch written a moment earlier.
type audioTokenClaims struct {
	Sub string  `json:"sub"`
	Exp float64 `json:"exp"`
	Iat float64 `json:"iat"`
	JTI string  `json:"jti"`
	SCP string  `json:"scp"`
}

func (audioTokenClaims) GetExpirationTime() (*jwt.NumericDate, error) { return nil, nil }
func (audioTokenClaims) GetIssuedAt() (*jwt.NumericDate, error)       { return nil, nil }
func (audioTokenClaims) GetNotBefore() (*jwt.NumericDate, error)      { return nil, nil }
func (audioTokenClaims) GetIssuer() (string, error)                   { return "", nil }
func (c audioTokenClaims) GetSubject() (string, error)                { return c.Sub, nil }
func (audioTokenClaims) GetAudience() (jwt.ClaimStrings, error)       { return nil, nil }

func seconds(t time.Time) float64 { return float64(t.UnixMicro()) / 1e6 }

// Mint is mint: a ScopeAudio token for tenant, signed with the active key.
func (a AudioTokens) Mint(_ context.Context, tenant uuid.UUID) (string, error) {
	now := a.now()
	key := a.Keys.Active()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, audioTokenClaims{
		Sub: ids.Upper(tenant), Exp: seconds(now.Add(AudioTokenLifetime)), Iat: seconds(now),
		JTI: ids.Upper(uuid.New()), SCP: ScopeAudio,
	})
	if key.Kid != "" {
		tok.Header["kid"] = key.Kid
	}
	return tok.SignedString(key.Secret)
}

// Revoke is revoke: every outstanding audio token for tenant stops working. A
// tenant with no users row is a no-op.
func (a AudioTokens) Revoke(ctx context.Context, tenant uuid.UUID) error {
	_, err := a.Pool.Exec(ctx, `UPDATE users SET hermes_audio_token_epoch = $2, updated_at = $2 WHERE id = $1`, tenant, a.now())
	return err
}
