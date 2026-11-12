package authservice

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"lumina/internal/swiftstr"
	"math"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// OAuthIdentityInfo is what a provider's verify returns for the upsert.
type OAuthIdentityInfo struct {
	ProviderUserID string
	Email          string
	EmailVerified  bool
}

// OAuthError is Swift's OAuthError. AuthController.oauthExchange turns any of
// them into 401 "invalid id_token"; every other error escapes as a 500.
type OAuthError string

const (
	OAuthInvalidToken    OAuthError = "invalidToken"
	OAuthUnverifiedEmail OAuthError = "unverifiedEmail"
	OAuthMissingClaims   OAuthError = "missingClaims"
	OAuthJWKSUnavailable OAuthError = "jwksUnavailable"
)

func (e OAuthError) Error() string { return "oauth: " + string(e) }

// OAuthProvider is Swift's OAuthProvider: verify a provider-issued id_token.
// Verify returns an OAuthError, and only that, for anything wrong with the token.
type OAuthProvider interface {
	Name() string
	Verify(ctx context.Context, idToken string) (*OAuthIdentityInfo, error)
}

// ParseOAuthAudiences is parseOAuthAudiences: a comma-separated client-id value
// split, trimmed of whitespace and newlines, blanks dropped.
func ParseOAuthAudiences(raw string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		if s := swiftstr.TrimWhitespacesAndNewlines(part); s != "" {
			out[s] = true
		}
	}
	return out
}

// Provider JWKS URLs, as Swift hard-codes them.
const (
	GoogleJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"
	AppleJWKSURL  = "https://appleid.apple.com/auth/keys"
)

// jwksTTL is JWKSCache's 12 hours. The cache never refetches early for an
// unknown kid, as in Swift: a rotated key fails until the TTL runs out.
const jwksTTL = 12 * time.Hour

// fetchTimeout is URLSession.shared's default request timeout.
const fetchTimeout = 60 * time.Second

// JWKSCache is Swift's JWKSCache actor: the provider's key set, fetched on
// first use and again once 12 hours old. The HTTP status is not checked, as in
// Swift: whatever body comes back is parsed as a JWKS, and a body that isn't
// one is a plain error (a 500), not an OAuthError.
type JWKSCache struct {
	URL    string
	Client *http.Client
	Now    func() time.Time

	mu        sync.Mutex
	keys      *jwkSet
	fetchedAt time.Time
}

// NewJWKSCache returns a cache for url. A nil client gets the 60-second default.
func NewJWKSCache(url string, client *http.Client) *JWKSCache {
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	return &JWKSCache{URL: url, Client: client, Now: time.Now}
}

func (c *JWKSCache) current(ctx context.Context) (*jwkSet, error) {
	// Swift's actor is re-entrant across the fetch's await: a stale cache sends
	// each caller to fetch on its own and the last result wins. The lock guards
	// only the cache fields, never the HTTP call, so one stalled fetch does not
	// queue every other sign-in behind it.
	c.mu.Lock()
	if c.keys != nil && c.Now().Sub(c.fetchedAt) < jwksTTL {
		keys := c.keys
		c.mu.Unlock()
		return keys, nil
	}
	c.mu.Unlock()
	set, err := c.fetch(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.keys, c.fetchedAt = set, c.Now()
	c.mu.Unlock()
	return set, nil
}

func (c *JWKSCache) fetch(ctx context.Context) (*jwkSet, error) {
	// URLSession is not cancelled when the client goes away.
	req, err := http.NewRequestWithContext(context.WithoutCancel(ctx), http.MethodGet, c.URL, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parseJWKS(body)
}

// jwkSet is JWTKeyCollection after add(jwksJSON:): keys by kid, and the first
// key as the default for a token with no kid.
type jwkSet struct {
	byKID map[string]jwk
	first *jwk
}

type jwk struct {
	alg string // the JWS alg this key verifies
	key crypto.PublicKey
}

type rawJWK struct {
	KTY string `json:"kty"`
	KID string `json:"kid"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func parseJWKS(body []byte) (*jwkSet, error) {
	var doc struct {
		Keys []rawJWK `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	if doc.Keys == nil {
		return nil, errors.New("jwks: no keys")
	}
	set := &jwkSet{byKID: map[string]jwk{}}
	for _, r := range doc.Keys {
		k, err := r.parse()
		if err != nil {
			return nil, err
		}
		if set.first == nil {
			set.first = &k
		}
		if r.KID != "" {
			set.byKID[r.KID] = k
		}
	}
	return set, nil
}

func b64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func (r rawJWK) parse() (jwk, error) {
	switch r.KTY {
	case "RSA":
		n, err := b64(r.N)
		if err != nil || len(n) == 0 {
			return jwk{}, errors.New("jwks: bad RSA modulus")
		}
		e, err := b64(r.E)
		if err != nil || len(e) == 0 || len(e) > 8 {
			return jwk{}, errors.New("jwks: bad RSA exponent")
		}
		exp := new(big.Int).SetBytes(e)
		if !exp.IsInt64() || exp.Int64() > math.MaxInt32 {
			return jwk{}, errors.New("jwks: bad RSA exponent")
		}
		alg := r.Alg
		if alg == "" {
			alg = "RS256"
		}
		return jwk{alg: alg, key: &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp.Int64())}}, nil
	case "EC":
		var curve elliptic.Curve
		alg := r.Alg
		switch r.Crv {
		case "P-256":
			curve, alg = elliptic.P256(), orDefault(alg, "ES256")
		case "P-384":
			curve, alg = elliptic.P384(), orDefault(alg, "ES384")
		case "P-521":
			curve, alg = elliptic.P521(), orDefault(alg, "ES512")
		default:
			return jwk{}, errors.New("jwks: unsupported curve")
		}
		x, errX := b64(r.X)
		y, errY := b64(r.Y)
		if errX != nil || errY != nil {
			return jwk{}, errors.New("jwks: bad EC point")
		}
		size := (curve.Params().BitSize + 7) / 8
		if len(x) > size || len(y) > size {
			return jwk{}, errors.New("jwks: bad EC point")
		}
		point := make([]byte, 1+2*size)
		point[0] = 4
		copy(point[1+size-len(x):], x)
		copy(point[1+2*size-len(y):], y)
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, point)
		if err != nil {
			return jwk{}, fmt.Errorf("jwks: bad EC point: %w", err)
		}
		return jwk{alg: alg, key: pub}, nil
	case "OKP":
		x, err := b64(r.X)
		if r.Crv != "Ed25519" || err != nil || len(x) != ed25519.PublicKeySize {
			return jwk{}, errors.New("jwks: bad OKP key")
		}
		return jwk{alg: "EdDSA", key: ed25519.PublicKey(x)}, nil
	}
	return jwk{}, fmt.Errorf("jwks: unsupported kty %q", r.KTY)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// verify checks the signature with the key the header names (the first key
// when it names none) and returns the raw payload. Claims are not validated
// here: the provider decodes them as JWTKit's JWTPayload does and checks exp
// itself, so a nbf or iat claim plays no part, as in Swift.
func (s *jwkSet) verify(token string) ([]byte, error) {
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		k := s.first
		if kid, ok := t.Header["kid"].(string); ok {
			found, ok := s.byKID[kid]
			if !ok {
				return nil, errors.New("unknown kid")
			}
			k = &found
		}
		if k == nil {
			return nil, errors.New("no key")
		}
		if t.Method.Alg() != k.alg {
			return nil, errors.New("alg mismatch")
		}
		return k.key, nil
	}, jwt.WithoutClaimsValidation())
	if err != nil || !parsed.Valid {
		return nil, OAuthInvalidToken
	}
	parts := strings.Split(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, OAuthInvalidToken
	}
	return payload, nil
}

// idClaims is GoogleIDClaims and AppleIDClaims: sub, aud, iss and exp required,
// email optional. EmailVerified is decoded by the provider.
type idClaims struct {
	Sub           string
	Aud           []string
	Iss           string
	Exp           float64
	Email         *string
	EmailVerified json.RawMessage
}

// decodeIDClaims decodes the payload as the Codable claims do: a missing or
// mistyped required claim fails, an optional one may be absent or null.
func decodeIDClaims(payload []byte, now time.Time) (*idClaims, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, OAuthInvalidToken
	}
	var c idClaims
	str := func(key string, dst *string) bool {
		v, ok := raw[key]
		return ok && json.Unmarshal(v, dst) == nil && v[0] == '"'
	}
	if !str("sub", &c.Sub) || !str("iss", &c.Iss) {
		return nil, OAuthInvalidToken
	}
	aud, ok := raw["aud"]
	if !ok {
		return nil, OAuthInvalidToken
	}
	// AudienceClaim: one string or an array of strings.
	var one string
	if aud[0] == '"' && json.Unmarshal(aud, &one) == nil {
		c.Aud = []string{one}
	} else {
		var list []json.RawMessage
		if aud[0] != '[' || json.Unmarshal(aud, &list) != nil {
			return nil, OAuthInvalidToken
		}
		for _, el := range list {
			var a string
			if len(el) == 0 || el[0] != '"' || json.Unmarshal(el, &a) != nil {
				return nil, OAuthInvalidToken
			}
			c.Aud = append(c.Aud, a)
		}
	}
	// ExpirationClaim: seconds since 1970 as a JSON number.
	exp, ok := raw["exp"]
	if !ok || json.Unmarshal(exp, &c.Exp) != nil {
		return nil, OAuthInvalidToken
	}
	if v, ok := raw["email"]; ok && string(v) != "null" {
		var e string
		if v[0] != '"' || json.Unmarshal(v, &e) != nil {
			return nil, OAuthInvalidToken
		}
		c.Email = &e
	}
	c.EmailVerified = raw["email_verified"]
	// verifyNotExpired: expired when exp is before now.
	if c.Exp < float64(now.UnixNano())/1e9 {
		return nil, OAuthInvalidToken
	}
	return &c, nil
}

func (c *idClaims) hasAudience(set map[string]bool) bool {
	for _, a := range c.Aud {
		if set[a] {
			return true
		}
	}
	return false
}

// identity checks email presence and verification in Swift's order.
func (c *idClaims) identity(verified bool) (*OAuthIdentityInfo, error) {
	if c.Email == nil || *c.Email == "" {
		return nil, OAuthMissingClaims
	}
	if !verified {
		return nil, OAuthUnverifiedEmail
	}
	return &OAuthIdentityInfo{ProviderUserID: c.Sub, Email: *c.Email, EmailVerified: true}, nil
}

// GoogleProvider is GoogleOAuthProvider.
type GoogleProvider struct {
	Audiences map[string]bool
	JWKS      *JWKSCache
}

// NewGoogleProvider uses Google's JWKS URL when jwksURL is "".
func NewGoogleProvider(audiences map[string]bool, jwksURL string, client *http.Client) *GoogleProvider {
	return &GoogleProvider{Audiences: audiences, JWKS: NewJWKSCache(orDefault(jwksURL, GoogleJWKSURL), client)}
}

func (p *GoogleProvider) Name() string { return "google" }

var googleIssuers = map[string]bool{"https://accounts.google.com": true, "accounts.google.com": true}

// Verify is GoogleOAuthProvider.verify. A JWKS that can't be fetched keeps its
// own error (a 500): availability, not a bad token.
func (p *GoogleProvider) Verify(ctx context.Context, idToken string) (*OAuthIdentityInfo, error) {
	keys, err := p.JWKS.current(ctx)
	if err != nil {
		return nil, err
	}
	payload, err := keys.verify(idToken)
	if err != nil {
		return nil, err
	}
	c, err := decodeIDClaims(payload, p.JWKS.Now())
	if err != nil {
		return nil, err
	}
	// email_verified is Bool?: any other JSON type fails the whole decode.
	verified := false
	if v := c.EmailVerified; len(v) > 0 && string(v) != "null" {
		if json.Unmarshal(v, &verified) != nil {
			return nil, OAuthInvalidToken
		}
	}
	if !googleIssuers[c.Iss] || !c.hasAudience(p.Audiences) {
		return nil, OAuthInvalidToken
	}
	return c.identity(verified)
}

// AppleProvider is AppleOAuthProvider.
type AppleProvider struct {
	Audiences map[string]bool
	JWKS      *JWKSCache
}

// NewAppleProvider uses Apple's JWKS URL when jwksURL is "".
func NewAppleProvider(audiences map[string]bool, jwksURL string, client *http.Client) *AppleProvider {
	return &AppleProvider{Audiences: audiences, JWKS: NewJWKSCache(orDefault(jwksURL, AppleJWKSURL), client)}
}

func (p *AppleProvider) Name() string { return "apple" }

const appleIssuer = "https://appleid.apple.com"

// Verify is AppleOAuthProvider.verify.
func (p *AppleProvider) Verify(ctx context.Context, idToken string) (*OAuthIdentityInfo, error) {
	keys, err := p.JWKS.current(ctx)
	if err != nil {
		return nil, err
	}
	payload, err := keys.verify(idToken)
	if err != nil {
		return nil, err
	}
	c, err := decodeIDClaims(payload, p.JWKS.Now())
	if err != nil {
		return nil, err
	}
	if c.Iss != appleIssuer || !c.hasAudience(p.Audiences) {
		return nil, OAuthInvalidToken
	}
	return c.identity(boolOrString(c.EmailVerified))
}

// boolOrString is BoolOrStringClaim: a Bool, or a string that is "true" in any
// case; anything else (and absent) is false.
func boolOrString(v json.RawMessage) bool {
	if len(v) == 0 {
		return false
	}
	var b bool
	if json.Unmarshal(v, &b) == nil {
		return b
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return swiftstr.Lowercased(s) == "true"
	}
	return false
}

// ExchangeOAuth is DefaultAuthService.exchangeOAuth: verify, upsert, issue.
func (s *Service) ExchangeOAuth(ctx context.Context, p OAuthProvider, idToken string) (*AuthResponse, error) {
	info, err := p.Verify(ctx, idToken)
	if err != nil {
		return nil, err
	}
	u, err := s.UpsertOAuthUser(ctx, p.Name(), info.ProviderUserID, info.Email, info.EmailVerified)
	if err != nil {
		return nil, err
	}
	return s.IssueTokens(ctx, u)
}
