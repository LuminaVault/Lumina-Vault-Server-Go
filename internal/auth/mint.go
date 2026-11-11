package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"lumina/internal/ids"
	"time"

	"github.com/google/uuid"
)

// AccessTokenLifetime is AuthService.accessTokenLifetime: the access token
// lives one hour and AuthResponse.expiresIn is this in seconds.
const AccessTokenLifetime = time.Hour

// MintOptions are SessionToken's optional fields.
type MintOptions struct {
	// HPID is set only for a ready Hermes profile (AuthService.issueTokens).
	HPID string
	// Scope marks a machine token. Empty is an ordinary session, which carries no scp claim.
	Scope string
	// NoIssuedAt drops iat. Swift's SessionToken makes it optional; issueTokens always sets it.
	NoIssuedAt bool
	// JTI replaces the random uppercase UUID.
	JTI string
}

// sessionPayload is SessionToken as JWTKit encodes it: CodingKeys order, the
// dates as JSONEncoder writes a Double (seconds since 1970, fractional), and
// nil optionals left out.
type sessionPayload struct {
	Sub  string   `json:"sub"`
	Exp  float64  `json:"exp"`
	Iat  *float64 `json:"iat,omitempty"`
	JTI  string   `json:"jti"`
	HPID *string  `json:"hpid,omitempty"`
	Scp  *string  `json:"scp,omitempty"`
}

// numericDate is Date.timeIntervalSince1970 at microsecond precision, which is
// what Swift's Double carries for a current date.
func numericDate(t time.Time) float64 {
	return float64(t.UnixMicro()) / 1e6
}

// MintSession signs a SessionToken for user with the active key, the way
// AuthService.issueTokens does: HS256, kid header of the active key, sub and
// jti as uppercase UUIDs, exp now + lifetime, iat now.
func (k Keyset) MintSession(user uuid.UUID, now time.Time, lifetime time.Duration, opt MintOptions) (string, error) {
	if len(k.keys) == 0 {
		return "", errors.New("auth: no signing key")
	}
	p := sessionPayload{Sub: ids.Upper(user), Exp: numericDate(now.Add(lifetime)), JTI: opt.JTI}
	if p.JTI == "" {
		p.JTI = ids.Upper(uuid.New())
	}
	if !opt.NoIssuedAt {
		iat := numericDate(now)
		p.Iat = &iat
	}
	if opt.HPID != "" {
		p.HPID = &opt.HPID
	}
	if opt.Scope != "" {
		p.Scp = &opt.Scope
	}
	key := k.Active()
	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	if key.Kid != "" {
		header["kid"] = key.Kid
	}
	return sign(header, p, key.Secret)
}

// MintAccess is the access token of issueTokens: MintSession with a one-hour
// lifetime and an optional hpid.
func (k Keyset) MintAccess(user uuid.UUID, now time.Time, hpid string) (string, error) {
	return k.MintSession(user, now, AccessTokenLifetime, MintOptions{HPID: hpid})
}

func sign(header map[string]string, payload any, secret []byte) (string, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(h) + "." + enc.EncodeToString(b)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signing))
	return signing + "." + enc.EncodeToString(mac.Sum(nil)), nil
}
