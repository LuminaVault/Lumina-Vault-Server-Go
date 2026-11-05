package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"lumina/internal/ids"
	"lumina/internal/wire"
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

type fakeAudioUsers struct {
	epoch *time.Time
	found bool
	err   error
}

func (f fakeAudioUsers) AudioTokenEpoch(context.Context, uuid.UUID) (*time.Time, bool, error) {
	return f.epoch, f.found, f.err
}

// Exists lets one fake stand behind both authenticators.
func (f fakeAudioUsers) Exists(context.Context, uuid.UUID) (bool, error) { return f.found, f.err }

// rawToken signs a payload as Swift's SessionToken would write it. Map claims, so a
// test can leave a claim out or give it an exact value.
func rawToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = "a"
	s, err := tok.SignedString([]byte(secretA))
	require.NoError(t, err)
	return s
}

func payload(user uuid.UUID, extra jwt.MapClaims) jwt.MapClaims {
	claims := jwt.MapClaims{
		"sub": ids.Upper(user),
		"exp": float64(time.Now().Add(time.Hour).Unix()),
		"jti": ids.Upper(uuid.New()),
	}
	for k, v := range extra {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}
	return claims
}

// through sends a token through mw and reports the identity it attached, with
// auth.Require after it, as a mounted route has.
func through(t *testing.T, mw echo.MiddlewareFunc, token string) (int, uuid.UUID) {
	t.Helper()
	e := echo.New()
	e.HTTPErrorHandler = wire.HTTPErrorHandler
	var seen uuid.UUID
	e.GET("/p", func(c *echo.Context) error {
		seen = UserID(c)
		return c.NoContent(http.StatusOK)
	}, mw, Require)
	req := httptest.NewRequest(http.MethodGet, "/p", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Code, seen
}

// The asymmetry AudioJWTAuthenticator's comment calls the whole security model, from
// both sides. A row reading `true` on the session side would turn a compromised
// Hermes container into full account access.
func TestScopeAsymmetryFromBothSides(t *testing.T) {
	user := uuid.New()
	keys := NewKeyset(Key{Kid: "a", Secret: []byte(secretA)})
	users := fakeAudioUsers{found: true}
	session, audio := Identify(keys, users), AudioIdentify(keys, users)
	now := float64(time.Now().Unix())

	cases := []struct {
		name          string
		extra         jwt.MapClaims
		onSession     bool
		onAudioRoutes bool
	}{
		{"ordinary session", jwt.MapClaims{"iat": now}, true, true},
		{"token minted before iat and scp existed", nil, true, true},
		{"scp null decodes as no scope", jwt.MapClaims{"scp": json.RawMessage("null")}, true, true},
		{"audio scope", jwt.MapClaims{"iat": now, "scp": ScopeAudio}, false, true},
		{"future scope", jwt.MapClaims{"iat": now, "scp": "calendar"}, false, false},
		{"empty scope is a scope", jwt.MapClaims{"iat": now, "scp": ""}, false, false},
		{"scope is compared exactly", jwt.MapClaims{"iat": now, "scp": "AUDIO"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := rawToken(t, payload(user, tc.extra))
			for side, want := range map[string]struct {
				mw echo.MiddlewareFunc
				ok bool
			}{"session": {session, tc.onSession}, "audio": {audio, tc.onAudioRoutes}} {
				code, seen := through(t, want.mw, token)
				if want.ok {
					require.Equal(t, http.StatusOK, code, side)
					require.Equal(t, user, seen, side)
				} else {
					require.Equal(t, http.StatusUnauthorized, code, side)
				}
			}
		})
	}
}

func TestAudioEpochRevokesScopedTokensOnly(t *testing.T) {
	user := uuid.New()
	keys := NewKeyset(Key{Kid: "a", Secret: []byte(secretA)})
	epoch := time.Date(2026, 11, 5, 12, 0, 0, 500_000_000, time.UTC) // .5 s
	at := float64(epoch.Unix())
	users := fakeAudioUsers{found: true, epoch: &epoch}
	audio := AudioIdentify(keys, users)

	cases := []struct {
		name  string
		extra jwt.MapClaims
		want  int
	}{
		{"scoped, iat after the epoch", jwt.MapClaims{"scp": ScopeAudio, "iat": at + 1}, http.StatusOK},
		{"scoped, iat a fraction after the epoch", jwt.MapClaims{"scp": ScopeAudio, "iat": at + 0.75}, http.StatusOK},
		{"scoped, iat a fraction before the epoch", jwt.MapClaims{"scp": ScopeAudio, "iat": at + 0.25}, http.StatusUnauthorized},
		{"scoped, iat equal to the epoch: strict", jwt.MapClaims{"scp": ScopeAudio, "iat": at + 0.5}, http.StatusUnauthorized},
		{"scoped, iat before the epoch", jwt.MapClaims{"scp": ScopeAudio, "iat": at - 60}, http.StatusUnauthorized},
		{"scoped, no iat", jwt.MapClaims{"scp": ScopeAudio}, http.StatusUnauthorized},
		{"session older than the epoch", jwt.MapClaims{"iat": at - 60}, http.StatusOK},
		{"session with no iat", nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := through(t, audio, rawToken(t, payload(user, tc.extra)))
			require.Equal(t, tc.want, code)
		})
	}
	t.Run("no epoch: a scoped token needs no iat", func(t *testing.T) {
		code, _ := through(t, AudioIdentify(keys, fakeAudioUsers{found: true}), rawToken(t, payload(user, jwt.MapClaims{"scp": ScopeAudio})))
		require.Equal(t, http.StatusOK, code)
	})
}

func TestAudioIdentifyRefusals(t *testing.T) {
	user := uuid.New()
	keys := NewKeyset(Key{Kid: "a", Secret: []byte(secretA)})
	audio := AudioIdentify(keys, fakeAudioUsers{found: true})
	scoped := jwt.MapClaims{"scp": ScopeAudio}

	for name, token := range map[string]string{
		"expired":      rawToken(t, payload(user, jwt.MapClaims{"scp": ScopeAudio, "exp": float64(time.Now().Add(-time.Minute).Unix())})),
		"no exp":       rawToken(t, payload(user, jwt.MapClaims{"scp": ScopeAudio, "exp": nil})),
		"no jti":       rawToken(t, payload(user, jwt.MapClaims{"scp": ScopeAudio, "jti": nil})),
		"bad sub":      rawToken(t, payload(user, jwt.MapClaims{"scp": ScopeAudio, "sub": "nope"})),
		"scp a number": rawToken(t, payload(user, jwt.MapClaims{"scp": 1})),
		"garbage":      "garbage",
	} {
		t.Run(name, func(t *testing.T) {
			code, _ := through(t, audio, token)
			require.Equal(t, http.StatusUnauthorized, code)
		})
	}
	t.Run("wrong key", func(t *testing.T) {
		other := AudioIdentify(NewKeyset(Key{Kid: "a", Secret: []byte(secretB)}), fakeAudioUsers{found: true})
		code, _ := through(t, other, rawToken(t, payload(user, scoped)))
		require.Equal(t, http.StatusUnauthorized, code)
	})
	t.Run("unknown user", func(t *testing.T) {
		code, _ := through(t, AudioIdentify(keys, fakeAudioUsers{}), rawToken(t, payload(user, scoped)))
		require.Equal(t, http.StatusUnauthorized, code)
	})
	t.Run("a failed read is an error, not a 401", func(t *testing.T) {
		code, _ := through(t, AudioIdentify(keys, fakeAudioUsers{err: errors.New("db down")}), rawToken(t, payload(user, scoped)))
		require.Equal(t, http.StatusInternalServerError, code)
	})
	t.Run("no header passes through unidentified", func(t *testing.T) {
		e := echo.New()
		e.GET("/p", func(c *echo.Context) error {
			_, ok := Identity(c)
			require.False(t, ok)
			return c.NoContent(http.StatusNoContent)
		}, audio)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/p", nil))
		require.Equal(t, http.StatusNoContent, rec.Code)
	})
	t.Run("repeated Authorization lines are joined, so neither is a token", func(t *testing.T) {
		e := echo.New()
		e.GET("/p", func(c *echo.Context) error {
			_, ok := Identity(c)
			require.False(t, ok)
			return c.NoContent(http.StatusNoContent)
		}, audio)
		req := httptest.NewRequest(http.MethodGet, "/p", nil)
		req.Header.Add("Authorization", "Bearer "+rawToken(t, payload(user, scoped)))
		req.Header.Add("Authorization", "Bearer other")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusNoContent, rec.Code)
	})
}

func TestAudioIdentifyAttachesHPID(t *testing.T) {
	keys := NewKeyset(Key{Kid: "a", Secret: []byte(secretA)})
	e := echo.New()
	var hpid string
	e.GET("/p", func(c *echo.Context) error {
		hpid = HPID(c)
		return c.NoContent(http.StatusOK)
	}, AudioIdentify(keys, fakeAudioUsers{found: true}), Require)
	req := httptest.NewRequest(http.MethodGet, "/p", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken(t, payload(uuid.New(), jwt.MapClaims{"scp": ScopeAudio, "hpid": "hermes-alice"})))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "hermes-alice", hpid)
}

// Auth/SessionTokenScopeTests.swift. The rest of that suite is covered by the
// asymmetry table: an ordinary session has no scope, a token with no scp claim passes.
func TestScopeIsEncodedUnderTheExpectedClaimName(t *testing.T) {
	c := session(uuid.New())
	c.SCP = ScopeAudio
	segments := strings.Split(mint(t, "a", secretA, c, jwt.SigningMethodHS256), ".")
	require.Len(t, segments, 3)
	raw, err := base64.RawURLEncoding.DecodeString(segments[1])
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, "audio", decoded["scp"], "renaming the claim would read every scoped token as a session")
}

func TestOrdinarySessionCarriesNoScopeClaim(t *testing.T) {
	segments := strings.Split(mint(t, "a", secretA, session(uuid.New()), jwt.SigningMethodHS256), ".")
	raw, err := base64.RawURLEncoding.DecodeString(segments[1])
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"scp"`)
}

func TestVerifyKeepsIssuedAtForFreshReAuth(t *testing.T) {
	keys := NewKeyset(Key{Kid: "a", Secret: []byte(secretA)})
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	claims, err := Verify(keys, "Bearer "+rawToken(t, payload(uuid.New(), jwt.MapClaims{"iat": float64(at.Unix())})))
	require.NoError(t, err)
	require.NotNil(t, claims.IssuedAt)
	require.True(t, claims.IssuedAt.Equal(at))
}
