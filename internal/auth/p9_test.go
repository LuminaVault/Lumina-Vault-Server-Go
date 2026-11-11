package auth

import (
	"encoding/base64"
	"encoding/json"
	"lumina/internal/config"
	"lumina/internal/ids"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// P9 ports: JWTKeyLoaderTests, SessionTokenTests, JWTAuthenticatorE2ETests and
// PasswordHasherTests, against Go's minter.

var (
	loaderA = strings.Repeat("a", 32)
	loaderB = strings.Repeat("b", 32)
)

func keysFrom(t *testing.T, csv string) Keyset {
	t.Helper()
	k, err := LoadKeys(config.Config{JWTHMACSecrets: csv})
	require.NoError(t, err)
	return k
}

// JWTKeyLoaderTests: `parses csv into ordered kid/secret pairs`, `tolerates
// whitespace around entries`, `empty input yields empty list`.
func TestJWTKeyLoaderParsing(t *testing.T) {
	k := keysFrom(t, "kid1:"+loaderA+",kid2:"+loaderB)
	require.Equal(t, []Key{{Kid: "kid1", Secret: []byte(loaderA)}, {Kid: "kid2", Secret: []byte(loaderB)}}, k.keys)
	k = keysFrom(t, " kid1 : "+loaderA+" , kid2 : "+loaderB+" ")
	require.Equal(t, "kid1", k.keys[0].Kid)
	require.Equal(t, "kid2", k.keys[1].Kid)
	for _, empty := range []string{"", "   "} {
		parsed, err := parseSecrets(empty)
		require.NoError(t, err)
		require.Empty(t, parsed)
	}
	// rejects entry without colon, empty kid, short secret, duplicate kids
	for _, bad := range []string{"noColonHere" + loaderA, ":" + loaderA, "kid1:tooShort", "kid1:" + loaderA + ",kid1:" + loaderB} {
		_, err := parseSecrets(bad)
		require.Error(t, err, bad)
	}
}

// The legacy single key signs as "lv-default" when JWT_KID is unset, as App+build.swift.
func TestLegacyKeyKid(t *testing.T) {
	k, err := LoadKeys(config.Config{JWTHMACSecret: loaderA})
	require.NoError(t, err)
	require.Equal(t, "lv-default", k.Active().Kid)
	k, err = LoadKeys(config.Config{JWTHMACSecret: loaderA, JWTKID: "mine"})
	require.NoError(t, err)
	require.Equal(t, "mine", k.Active().Kid)
}

// JWTKeyLoaderTests: `loaded collection verifies token signed by active kid`,
// `tokens signed by retired kid still verify after rotation`, `tokens signed by
// fully removed kid fail verification`.
func TestJWTKeyLoaderRotation(t *testing.T) {
	user := uuid.New()
	now := time.Now()
	active := keysFrom(t, "activeKid:"+loaderA+",oldKid:"+loaderB)
	tok, err := active.MintAccess(user, now, "")
	require.NoError(t, err)
	c, err := Verify(active, "Bearer "+tok)
	require.NoError(t, err)
	require.Equal(t, ids.Upper(user), c.Subject)

	old, err := keysFrom(t, "kid1:"+loaderA+",kid2:"+loaderB).MintAccess(user, now, "")
	require.NoError(t, err)
	_, err = Verify(keysFrom(t, "kid2:"+loaderB+",kid1:"+loaderA), "Bearer "+old)
	require.NoError(t, err)
	_, err = Verify(keysFrom(t, "kid2:"+loaderB), "Bearer "+old)
	require.Error(t, err)
}

func decodePart(t *testing.T, part string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(part)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

// The token shape AuthService.issueTokens signs: HS256 with the active kid;
// sub and jti uppercase; exp an hour after iat, both seconds with a fraction;
// hpid only when given; never scp.
func TestMintedAccessTokenShape(t *testing.T) {
	keys := keysFrom(t, "live:"+loaderA+",old:"+loaderB)
	user := uuid.New()
	now := time.Date(2026, 11, 11, 12, 0, 0, 123456000, time.UTC)
	tok, err := keys.MintAccess(user, now, "")
	require.NoError(t, err)
	parts := strings.Split(tok, ".")
	require.Len(t, parts, 3)
	require.Equal(t, map[string]any{"alg": "HS256", "typ": "JWT", "kid": "live"}, decodePart(t, parts[0]))
	p := decodePart(t, parts[1])
	require.Equal(t, ids.Upper(user), p["sub"])
	require.InDelta(t, 1794398400.123456, p["iat"], 1e-6)
	require.InDelta(t, 1794402000.123456, p["exp"], 1e-6)
	jti, _ := p["jti"].(string)
	require.Equal(t, strings.ToUpper(jti), jti)
	require.NotContains(t, p, "hpid")
	require.NotContains(t, p, "scp")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(raw), `{"sub":`), string(raw))

	// The signature is plain HS256 over the first two parts.
	parsed, err := jwt.Parse(tok, func(*jwt.Token) (any, error) { return []byte(loaderA), nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithTimeFunc(func() time.Time { return now }))
	require.NoError(t, err)
	require.True(t, parsed.Valid)
}

// SessionTokenTests: `round trips through HMAC`, `rejects expired token`, `hpid claim round trips`.
func TestSessionTokenRoundTrips(t *testing.T) {
	keys := NewKeyset(Key{Kid: "test", Secret: []byte("secret-secret-secret-32chars-x")})
	user := uuid.New()
	tok, err := keys.MintAccess(user, time.Now(), "")
	require.NoError(t, err)
	c, err := Verify(keys, "Bearer "+tok)
	require.NoError(t, err)
	require.Equal(t, ids.Upper(user), c.Subject)
	require.Empty(t, c.HPID)

	expired, err := keys.MintSession(user, time.Now(), -time.Second, MintOptions{})
	require.NoError(t, err)
	_, err = Verify(keys, "Bearer "+expired)
	require.Error(t, err)

	tok, err = keys.MintAccess(user, time.Now(), "hermes-alice")
	require.NoError(t, err)
	c, err = Verify(keys, "Bearer "+tok)
	require.NoError(t, err)
	require.Equal(t, "hermes-alice", c.HPID)
}

// A scoped mint passes only where scp is allowed (SessionTokenScopeTests' rule).
func TestScopedMintIsNotASession(t *testing.T) {
	keys := NewKeyset(Key{Kid: "k", Secret: []byte(loaderA)})
	tok, err := keys.MintSession(uuid.New(), time.Now(), time.Hour, MintOptions{Scope: ScopeAudio})
	require.NoError(t, err)
	require.Equal(t, "audio", decodePart(t, strings.Split(tok, ".")[1])["scp"])
	_, err = Verify(keys, "Bearer "+tok)
	require.Error(t, err)
}

// JWTAuthenticatorE2ETests: valid bearer, missing and malformed header, expired,
// wrong signature, unknown user, and the issued token's hpid, through Middleware
// with tokens Go minted.
func TestJWTAuthenticatorE2E(t *testing.T) {
	keys := NewKeyset(Key{Kid: "e2e", Secret: []byte(loaderA)})
	user := uuid.New()
	users := fakeUsers{user: true}
	tok, err := keys.MintAccess(user, time.Now(), "hermes-e2e")
	require.NoError(t, err)

	code, seen := serve(t, keys, users, tok)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, user, seen)

	code, _ = serve(t, keys, users, "")
	require.Equal(t, http.StatusUnauthorized, code)

	for _, header := range []string{"Basic abc", "Bearer", "Bearer not.a.jwt", tok} {
		e := echo.New()
		e.GET("/p", func(c *echo.Context) error { return c.NoContent(http.StatusOK) }, Middleware(keys, users))
		req := httptest.NewRequest(http.MethodGet, "/p", nil)
		req.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code, header)
	}

	expired, err := keys.MintSession(user, time.Now().Add(-2*time.Hour), time.Hour, MintOptions{})
	require.NoError(t, err)
	code, _ = serve(t, keys, users, expired)
	require.Equal(t, http.StatusUnauthorized, code)

	forged, err := NewKeyset(Key{Kid: "e2e", Secret: []byte(loaderB)}).MintAccess(user, time.Now(), "")
	require.NoError(t, err)
	code, _ = serve(t, keys, users, forged)
	require.Equal(t, http.StatusUnauthorized, code)

	stranger, err := keys.MintAccess(uuid.New(), time.Now(), "")
	require.NoError(t, err)
	code, _ = serve(t, keys, users, stranger)
	require.Equal(t, http.StatusUnauthorized, code)

	c, err := Verify(keys, "Bearer "+tok)
	require.NoError(t, err)
	require.Equal(t, "hermes-e2e", c.HPID)
}

// PasswordHasherTests: `hash and verify round trip`, `different inputs produce different hashes`.
func TestPasswordHasher(t *testing.T) {
	h, err := HashPassword("hunter2hunter2", 4)
	require.NoError(t, err)
	require.True(t, VerifyPassword("hunter2hunter2", h))
	require.False(t, VerifyPassword("wrongpassword", h))
	h1, err := HashPassword("password1", 4)
	require.NoError(t, err)
	h2, err := HashPassword("password2", 4)
	require.NoError(t, err)
	require.NotEqual(t, h1, h2)
	require.False(t, VerifyPassword("x", "not a hash"))

	h, err = HashPassword("cost twelve", PasswordCost)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(h, "$2a$12$"), h)
}

// Swift writes $2b$ and Go $2a$: the same algorithm under two prefixes, so each
// server verifies the other's. The C hashes below (crypt_blowfish, as Swift's
// OpenBSD-derived bcrypt) also pin the 72-byte cut: a longer password hashes as
// its first 72 bytes on both sides.
func TestBcryptPrefixesCrossVerify(t *testing.T) {
	h, err := HashPassword("swap me", 4)
	require.NoError(t, err)
	require.True(t, VerifyPassword("swap me", "$2b$"+h[4:]))

	long := strings.Repeat("x", 80)
	c80 := "$2b$" + "04$TatLwnFGXZ2KeUGDo3o.XOrW2f8u/QALz0N8MrHYxOmT6Gkh5QCsq"
	c72 := "$2b$" + "04$2M5ZZ7oNwshYBZxDDK5GJODCBfzjms0Zq/erO8SSCrVbD3k.jmwJS"
	require.True(t, VerifyPassword(long, c80))
	require.True(t, VerifyPassword(long[:72], c80))
	require.True(t, VerifyPassword(long, c72))
	require.False(t, VerifyPassword(long[:71], c72))

	g, err := HashPassword(long, 4)
	require.NoError(t, err)
	require.True(t, VerifyPassword(long[:72]+"tail", g))
}
