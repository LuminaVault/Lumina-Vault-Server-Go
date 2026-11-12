package authservice_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"lumina/internal/authservice"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The OAuth provider tests sign id_tokens with a key generated per run and
// serve its public half from an httptest JWKS endpoint, as the Swift suite's
// StubJWKSURLProtocol serves JWTKit's fixture key. Nothing reaches Google or Apple.

const (
	testAudience = "test-client.apps.googleusercontent.com"
	testKID      = "google-test-key"
)

var (
	rsaOnce sync.Once
	rsaKey  *rsa.PrivateKey
)

func signingKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	rsaOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		rsaKey = k
	})
	return rsaKey
}

// jwksStub serves the key under testKID and counts the fetches.
func jwksStub(t *testing.T, key *rsa.PrivateKey) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var fetches atomic.Int32
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
	body := fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":%q,"alg":"RS256","use":"sig","n":%q,"e":%q}]}`, testKID, n, e)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &fetches
}

type claims map[string]any

// googleClaims is signToken's defaults: a valid Google token for testAudience.
func googleClaims() claims {
	return claims{
		"sub": "google-sub-12345", "aud": []string{testAudience}, "iss": "https://accounts.google.com",
		"exp": time.Now().Add(5 * time.Minute).Unix(), "email": "ok@example.com", "email_verified": true,
	}
}

func (c claims) with(key string, v any) claims {
	out := claims{}
	for k, val := range c {
		out[k] = val
	}
	if v == nil {
		delete(out, key)
	} else {
		out[key] = v
	}
	return out
}

func sign(t *testing.T, key *rsa.PrivateKey, kid string, c claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims(c))
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	require.NoError(t, err)
	return s
}

func googleFixture(t *testing.T, audiences ...string) *authservice.GoogleProvider {
	t.Helper()
	srv, _ := jwksStub(t, signingKey(t))
	if len(audiences) == 0 {
		audiences = []string{testAudience}
	}
	aud := map[string]bool{}
	for _, a := range audiences {
		aud[a] = true
	}
	return authservice.NewGoogleProvider(aud, srv.URL, srv.Client())
}

// TestGoogleOAuthProvider ports GoogleOAuthProviderTests
// (Tests/AppTests/Auth/GoogleOAuthProviderTests.swift), all 12 cases: the
// audience, issuer, email, email_verified and expiry gates of
// GoogleOAuthProvider.verify, and parseOAuthAudiences.
func TestGoogleOAuthProvider(t *testing.T) {
	ctx := context.Background()
	key := signingKey(t)

	t.Run("valid token returns identity", func(t *testing.T) {
		info, err := googleFixture(t).Verify(ctx, sign(t, key, testKID, googleClaims()))
		require.NoError(t, err)
		assert.Equal(t, "google-sub-12345", info.ProviderUserID)
		assert.Equal(t, "ok@example.com", info.Email)
		assert.True(t, info.EmailVerified)
	})

	t.Run("wrong audience throws invalid token", func(t *testing.T) {
		_, err := googleFixture(t).Verify(ctx, sign(t, key, testKID, googleClaims().with("aud", []string{"other-client.apps.googleusercontent.com"})))
		assert.Equal(t, authservice.OAuthInvalidToken, err)
	})

	t.Run("a token for either configured audience is accepted", func(t *testing.T) {
		ios, web := "ios-client.apps.googleusercontent.com", "web-client.apps.googleusercontent.com"
		p := googleFixture(t, ios, web)
		for _, a := range []string{ios, web} {
			info, err := p.Verify(ctx, sign(t, key, testKID, googleClaims().with("aud", []string{a})))
			require.NoError(t, err, a)
			assert.Equal(t, "google-sub-12345", info.ProviderUserID)
		}
	})

	t.Run("an audience outside the configured set is still rejected", func(t *testing.T) {
		p := googleFixture(t, "ios-client.apps.googleusercontent.com", "web-client.apps.googleusercontent.com")
		_, err := p.Verify(ctx, sign(t, key, testKID, googleClaims().with("aud", []string{"attacker.apps.googleusercontent.com"})))
		assert.Equal(t, authservice.OAuthInvalidToken, err)
	})

	t.Run("audience parsing splits, trims and drops blanks", func(t *testing.T) {
		set := func(v ...string) map[string]bool {
			out := map[string]bool{}
			for _, s := range v {
				out[s] = true
			}
			return out
		}
		assert.Equal(t, set("a", "b"), authservice.ParseOAuthAudiences("a,b"))
		assert.Equal(t, set("a", "b"), authservice.ParseOAuthAudiences(" a , b "))
		assert.Equal(t, set("a", "b"), authservice.ParseOAuthAudiences("a,,b,"))
		assert.Empty(t, authservice.ParseOAuthAudiences(""))
		assert.Empty(t, authservice.ParseOAuthAudiences(" , "))
		assert.Equal(t, set("only-one"), authservice.ParseOAuthAudiences("only-one"))
		assert.Equal(t, set("a"), authservice.ParseOAuthAudiences("\na\t"))
	})

	t.Run("wrong issuer throws invalid token", func(t *testing.T) {
		_, err := googleFixture(t).Verify(ctx, sign(t, key, testKID, googleClaims().with("iss", "https://impostor.example.com")))
		assert.Equal(t, authservice.OAuthInvalidToken, err)
	})

	t.Run("missing email throws missing claims", func(t *testing.T) {
		_, err := googleFixture(t).Verify(ctx, sign(t, key, testKID, googleClaims().with("email", nil)))
		assert.Equal(t, authservice.OAuthMissingClaims, err)
	})

	t.Run("empty email throws missing claims", func(t *testing.T) {
		_, err := googleFixture(t).Verify(ctx, sign(t, key, testKID, googleClaims().with("email", "")))
		assert.Equal(t, authservice.OAuthMissingClaims, err)
	})

	t.Run("unverified email throws unverified email", func(t *testing.T) {
		_, err := googleFixture(t).Verify(ctx, sign(t, key, testKID, googleClaims().with("email_verified", false)))
		assert.Equal(t, authservice.OAuthUnverifiedEmail, err)
	})

	t.Run("missing email verified claim throws unverified email", func(t *testing.T) {
		_, err := googleFixture(t).Verify(ctx, sign(t, key, testKID, googleClaims().with("email_verified", nil)))
		assert.Equal(t, authservice.OAuthUnverifiedEmail, err)
	})

	t.Run("expired token throws", func(t *testing.T) {
		_, err := googleFixture(t).Verify(ctx, sign(t, key, testKID, googleClaims().with("exp", time.Now().Add(-time.Minute).Unix())))
		// JWTKit's expiry error is caught and becomes invalidToken, the 401.
		assert.Equal(t, authservice.OAuthInvalidToken, err)
	})

	t.Run("valid token accepts canonical issuer", func(t *testing.T) {
		info, err := googleFixture(t).Verify(ctx, sign(t, key, testKID, googleClaims().with("iss", "accounts.google.com")))
		require.NoError(t, err)
		assert.Equal(t, "google-sub-12345", info.ProviderUserID)
	})
}

// TestOAuthTokenShapes covers what JWTKit decides before the gates: the
// signature, the kid, and the Codable claim types. Every one is invalidToken.
func TestOAuthTokenShapes(t *testing.T) {
	ctx := context.Background()
	key := signingKey(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	for name, token := range map[string]string{
		"another key's signature": sign(t, other, testKID, googleClaims()),
		"unknown kid":             sign(t, key, "rotated", googleClaims()),
		"not a jwt":               "not-a-jwt",
		"numeric aud":             sign(t, key, testKID, googleClaims().with("aud", 7)),
		"no sub":                  sign(t, key, testKID, googleClaims().with("sub", nil)),
		"numeric email":           sign(t, key, testKID, googleClaims().with("email", 7)),
		"string email_verified":   sign(t, key, testKID, googleClaims().with("email_verified", "true")),
		"string exp":              sign(t, key, testKID, googleClaims().with("exp", "2100")),
		"no exp":                  sign(t, key, testKID, googleClaims().with("exp", nil)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := googleFixture(t).Verify(ctx, token)
			assert.Equal(t, authservice.OAuthInvalidToken, err)
		})
	}

	t.Run("no kid uses the first key", func(t *testing.T) {
		_, err := googleFixture(t).Verify(ctx, sign(t, key, "", googleClaims()))
		assert.NoError(t, err)
	})
	t.Run("a single string aud is accepted", func(t *testing.T) {
		_, err := googleFixture(t).Verify(ctx, sign(t, key, testKID, googleClaims().with("aud", testAudience)))
		assert.NoError(t, err)
	})
	t.Run("nbf in the future plays no part", func(t *testing.T) {
		_, err := googleFixture(t).Verify(ctx, sign(t, key, testKID, googleClaims().with("nbf", time.Now().Add(time.Hour).Unix())))
		assert.NoError(t, err)
	})
}

// TestJWKSCache: a JWKS endpoint that can't be reached, or answers with
// something that isn't a key set, is a plain error (the route's 500), and a
// good set is fetched once and kept.
func TestJWKSCache(t *testing.T) {
	ctx := context.Background()
	key := signingKey(t)

	t.Run("fetched once", func(t *testing.T) {
		srv, fetches := jwksStub(t, key)
		p := authservice.NewGoogleProvider(map[string]bool{testAudience: true}, srv.URL, srv.Client())
		for range 3 {
			_, err := p.Verify(ctx, sign(t, key, testKID, googleClaims()))
			require.NoError(t, err)
		}
		assert.Equal(t, int32(1), fetches.Load())
	})

	t.Run("refetched after twelve hours", func(t *testing.T) {
		srv, fetches := jwksStub(t, key)
		p := authservice.NewGoogleProvider(map[string]bool{testAudience: true}, srv.URL, srv.Client())
		now := time.Now()
		p.JWKS.Now = func() time.Time { return now }
		_, err := p.Verify(ctx, sign(t, key, testKID, googleClaims()))
		require.NoError(t, err)
		now = now.Add(12 * time.Hour)
		_, err = p.Verify(ctx, sign(t, key, testKID, googleClaims().with("exp", now.Add(time.Minute).Unix())))
		require.NoError(t, err)
		assert.Equal(t, int32(2), fetches.Load())
	})

	t.Run("unreachable is not an OAuthError", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		p := authservice.NewGoogleProvider(map[string]bool{testAudience: true}, url, nil)
		_, err := p.Verify(ctx, sign(t, key, testKID, googleClaims()))
		require.Error(t, err)
		var oauthErr authservice.OAuthError
		assert.False(t, errors.As(err, &oauthErr))
	})

	t.Run("a body that is not a key set is not an OAuthError", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("<html>"))
		}))
		t.Cleanup(srv.Close)
		p := authservice.NewGoogleProvider(map[string]bool{testAudience: true}, srv.URL, srv.Client())
		_, err := p.Verify(ctx, sign(t, key, testKID, googleClaims()))
		require.Error(t, err)
		var oauthErr authservice.OAuthError
		assert.False(t, errors.As(err, &oauthErr))
	})
}

// TestAppleOAuthProvider: AppleOAuthProvider's gates, which have no Swift test
// of their own: one issuer, and email_verified as a Bool or a "true"/"false"
// string in any case.
func TestAppleOAuthProvider(t *testing.T) {
	ctx := context.Background()
	key := signingKey(t)
	srv, _ := jwksStub(t, key)
	p := authservice.NewAppleProvider(map[string]bool{"com.luminavault.app": true, "com.luminavault.web": true}, srv.URL, srv.Client())
	apple := func() claims {
		return claims{
			"sub": "001234.apple", "aud": "com.luminavault.web", "iss": "https://appleid.apple.com",
			"exp": time.Now().Add(5 * time.Minute).Unix(), "email": "Relay@PrivateRelay.AppleID.com", "email_verified": "true",
		}
	}
	info, err := p.Verify(ctx, sign(t, key, testKID, apple()))
	require.NoError(t, err)
	assert.Equal(t, authservice.OAuthIdentityInfo{ProviderUserID: "001234.apple", Email: "Relay@PrivateRelay.AppleID.com", EmailVerified: true}, *info)
	assert.Equal(t, "apple", p.Name())

	for name, tc := range map[string]struct {
		c    claims
		want error
	}{
		"bool true":           {apple().with("email_verified", true), nil},
		"TRUE":                {apple().with("email_verified", "TRUE"), nil},
		"string false":        {apple().with("email_verified", "false"), authservice.OAuthUnverifiedEmail},
		"yes":                 {apple().with("email_verified", "yes"), authservice.OAuthUnverifiedEmail},
		"number":              {apple().with("email_verified", 1), authservice.OAuthUnverifiedEmail},
		"absent":              {apple().with("email_verified", nil), authservice.OAuthUnverifiedEmail},
		"google issuer":       {apple().with("iss", "https://accounts.google.com"), authservice.OAuthInvalidToken},
		"native audience":     {apple().with("aud", "com.luminavault.app"), nil},
		"foreign audience":    {apple().with("aud", "com.other"), authservice.OAuthInvalidToken},
		"no email":            {apple().with("email", nil), authservice.OAuthMissingClaims},
		"expired":             {apple().with("exp", time.Now().Add(-time.Second).Unix()), authservice.OAuthInvalidToken},
		"fractional exp ok":   {apple().with("exp", float64(time.Now().Add(time.Minute).UnixMilli())/1000), nil},
		"no email unverified": {apple().with("email", nil).with("email_verified", false), authservice.OAuthMissingClaims},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.Verify(ctx, sign(t, key, testKID, tc.c))
			assert.Equal(t, tc.want, err)
		})
	}
}

// TestJWKSECKey: JWTKit's JWKS loader takes EC keys too; an ES256 token signed
// by one verifies, and an RS256 header on that key does not.
func TestJWKSECKey(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pt, err := ec.PublicKey.Bytes()
	require.NoError(t, err)
	body := fmt.Sprintf(`{"keys":[{"kty":"EC","kid":"ec1","crv":"P-256","x":%q,"y":%q}]}`,
		base64.RawURLEncoding.EncodeToString(pt[1:33]), base64.RawURLEncoding.EncodeToString(pt[33:]))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(srv.Close)
	p := authservice.NewGoogleProvider(map[string]bool{testAudience: true}, srv.URL, srv.Client())

	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims(googleClaims()))
	tok.Header["kid"] = "ec1"
	s, err := tok.SignedString(ec)
	require.NoError(t, err)
	_, err = p.Verify(context.Background(), s)
	require.NoError(t, err)

	_, err = p.Verify(context.Background(), sign(t, signingKey(t), "ec1", googleClaims()))
	assert.Equal(t, authservice.OAuthInvalidToken, err)
}

// Swift's JWKSCache actor is re-entrant across the fetch, so a stalled fetch
// never blocks another caller's own fetch. Go must not hold its lock over HTTP.
func TestJWKSCacheStalledFetchDoesNotBlockOthers(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			<-release
		}
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	p := authservice.NewGoogleProvider(map[string]bool{testAudience: true}, srv.URL, srv.Client())
	key := signingKey(t)
	go func() { _, _ = p.Verify(context.Background(), sign(t, key, testKID, googleClaims())) }()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	done := make(chan struct{})
	go func() {
		_, _ = p.Verify(context.Background(), sign(t, key, testKID, googleClaims()))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second sign-in waited behind the stalled JWKS fetch")
	}
}
