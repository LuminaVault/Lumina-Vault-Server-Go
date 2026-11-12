package authroutes_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"lumina/internal/authservice"
	"lumina/internal/modules/authroutes"
	"lumina/internal/wire"
	"math/big"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingSMS is SMSSenderTests' RecordingSMSSender.
type recordingSMS struct {
	mu    sync.Mutex
	codes []string
	to    []string
}

func (r *recordingSMS) Send(_ context.Context, code, phone, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.codes, r.to = append(r.codes, code), append(r.to, phone)
	return nil
}

type failingSMS struct{}

func (failingSMS) Send(context.Context, string, string, string) error {
	return authservice.NewSMSSender(authservice.SMSConfig{Kind: "twilio"}, nil).Send(context.Background(), "1", "+1", "x")
}

func randomE164() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(900_000_000))
	return fmt.Sprintf("+1555%d", n.Int64()+100_000_000)
}

func randomMagicEmail() string { return "magic-" + suffix() + "@example.test" }

func (v env) identity(t *testing.T, userID, provider string) (providerUserID string, ok bool) {
	t.Helper()
	err := v.pool.QueryRow(context.Background(), `SELECT provider_user_id FROM oauth_identities WHERE tenant_id = $1 AND provider = $2`,
		userID, provider).Scan(&providerUserID)
	return providerUserID, err == nil
}

func decodeAuth(t *testing.T, r resp) authBody {
	t.Helper()
	require.Equal(t, http.StatusOK, r.code, r.body)
	var out authBody
	require.NoError(t, json.Unmarshal([]byte(r.body), &out))
	return out
}

// TestPhoneE164Validator ports PhoneE164ValidatorTests, the two cases at the
// end of Tests/AppTests/Auth/PreAuthChallengeStoreTests.swift.
func TestPhoneE164Validator(t *testing.T) {
	t.Run("accepts valid E 164", func(t *testing.T) {
		for raw, want := range map[string]string{
			"+15551234567": "+15551234567", "  +442071838750  ": "+442071838750", "+5511987654321": "+5511987654321",
			// NSRegularExpression's \d is any decimal digit, and trimming takes newlines.
			"+1٢٣٤٥٦٧\n": "+1٢٣٤٥٦٧",
		} {
			got, err := authroutes.ValidateE164(raw)
			require.NoError(t, err, raw)
			assert.Equal(t, want, got)
		}
	})
	t.Run("rejects invalid", func(t *testing.T) {
		for _, raw := range []string{"5551234567", "+0123456", "+12", "+1234567890123456", "+1-555-123-4567", ""} {
			_, err := authroutes.ValidateE164(raw)
			assert.Error(t, err, raw)
		}
	})
}

func phoneEnv(t *testing.T) (env, *recordingSMS) {
	t.Helper()
	sms := &recordingSMS{}
	return setupWith(t, func(o *authroutes.Options) {
		o.SMS = sms
		o.PhoneCodes = authservice.FixedCodeGenerator{Code: "424242"}
	}), sms
}

// TestPhoneAuthFlow ports PhoneAuthFlowTests
// (Tests/AppTests/Auth/PhoneAuthFlowTests.swift), all 5 cases: HER-137's
// POST /v1/auth/phone/start and /phone/verify with the fixed code 424242 and a
// recording SMS sender, so nothing is sent.
func TestPhoneAuthFlow(t *testing.T) {
	v, sms := phoneEnv(t)
	start := func(phone string) resp {
		return v.do(http.MethodPost, "/v1/auth/phone/start", fmt.Sprintf(`{"phone":%q}`, phone), nil)
	}
	verify := func(phone, code string) resp {
		return v.do(http.MethodPost, "/v1/auth/phone/verify", fmt.Sprintf(`{"phone":%q,"code":%q}`, phone, code), nil)
	}

	t.Run("start then verify creates user with phone identity", func(t *testing.T) {
		phone := randomE164()
		r := start(phone)
		require.Equal(t, http.StatusOK, r.code, r.body)
		var s struct {
			ChallengeID string    `json:"challengeId"`
			ExpiresAt   time.Time `json:"expiresAt"`
		}
		require.NoError(t, json.Unmarshal([]byte(r.body), &s))
		assert.True(t, s.ExpiresAt.After(time.Now()))
		assert.Equal(t, phone, sms.to[len(sms.to)-1])

		a := decodeAuth(t, verify(phone, "424242"))
		assert.NotEmpty(t, a.AccessToken)
		assert.NotEmpty(t, a.RefreshToken)
		assert.Equal(t, strings.TrimPrefix(phone, "+")+"@phone.luminavault.local", a.Email)
		puid, ok := v.identity(t, a.UserID, "phone")
		require.True(t, ok, "expected OAuthIdentity(provider: phone) for %s", a.UserID)
		assert.Equal(t, phone, puid)
	})

	t.Run("verify with bad code returns 401", func(t *testing.T) {
		phone := randomE164()
		require.Equal(t, http.StatusOK, start(phone).code)
		assert.Equal(t, http.StatusUnauthorized, verify(phone, "000000").code)
	})

	t.Run("verify without start returns 401", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, verify(randomE164(), "424242").code)
	})

	t.Run("start rejects non E 164", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, start("555-1234").code)
	})

	t.Run("same destination reissue burns old code", func(t *testing.T) {
		phone := randomE164()
		require.Equal(t, http.StatusOK, start(phone).code)
		require.Equal(t, http.StatusOK, start(phone).code)
		assert.Equal(t, http.StatusOK, verify(phone, "424242").code)
		assert.Equal(t, http.StatusUnauthorized, verify(phone, "424242").code)
	})

	t.Run("a second sign-in with the number is the same account", func(t *testing.T) {
		phone := randomE164()
		require.Equal(t, http.StatusOK, start(phone).code)
		first := decodeAuth(t, verify(phone, "424242"))
		require.Equal(t, http.StatusOK, start(phone).code)
		second := decodeAuth(t, verify(phone, "424242"))
		assert.Equal(t, first.UserID, second.UserID)
	})
}

func magicEnv(t *testing.T) (env, *sentMail) {
	t.Helper()
	mail := &sentMail{}
	return setupWith(t, func(o *authroutes.Options) {
		o.MagicSender = mail
		o.MagicCodes = authservice.FixedCodeGenerator{Code: "313131"}
	}), mail
}

type sentMail struct {
	mu                 sync.Mutex
	to, purpose, codes []string
}

func (s *sentMail) Send(_ context.Context, code, to, purpose string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes, s.to, s.purpose = append(s.codes, code), append(s.to, to), append(s.purpose, purpose)
	return nil
}

// TestEmailMagicLinkFlow ports EmailMagicLinkFlowTests
// (Tests/AppTests/Auth/EmailMagicLinkFlowTests.swift), all 5 cases: HER-138's
// POST /v1/auth/email/start and /email/verify with the fixed code 313131 and a
// recording sender.
func TestEmailMagicLinkFlow(t *testing.T) {
	v, mail := magicEnv(t)
	start := func(email string) resp {
		return v.do(http.MethodPost, "/v1/auth/email/start", fmt.Sprintf(`{"email":%q}`, email), nil)
	}
	verify := func(email, code string) resp {
		return v.do(http.MethodPost, "/v1/auth/email/verify", fmt.Sprintf(`{"email":%q,"code":%q}`, email, code), nil)
	}

	t.Run("start then verify creates user with email magic link identity", func(t *testing.T) {
		email := randomMagicEmail()
		r := start(email)
		require.Equal(t, http.StatusOK, r.code, r.body)
		var s struct {
			ExpiresAt time.Time `json:"expiresAt"`
		}
		require.NoError(t, json.Unmarshal([]byte(r.body), &s))
		assert.True(t, s.ExpiresAt.After(time.Now()))
		assert.Equal(t, "magic_link", mail.purpose[len(mail.purpose)-1])

		a := decodeAuth(t, verify(email, "313131"))
		assert.NotEmpty(t, a.AccessToken)
		assert.NotEmpty(t, a.RefreshToken)
		puid, ok := v.identity(t, a.UserID, "email_magic_link")
		require.True(t, ok)
		assert.Equal(t, email, puid)
	})

	t.Run("verify with bad code returns 401", func(t *testing.T) {
		email := randomMagicEmail()
		require.Equal(t, http.StatusOK, start(email).code)
		assert.Equal(t, http.StatusUnauthorized, verify(email, "000000").code)
	})

	t.Run("verify without start returns 401", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, verify(randomMagicEmail(), "313131").code)
	})

	t.Run("start rejects invalid email", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, start("no-at-sign").code)
	})

	t.Run("second start issues fresh challenge and burns old code", func(t *testing.T) {
		email := randomMagicEmail()
		require.Equal(t, http.StatusOK, start(email).code)
		require.Equal(t, http.StatusOK, start(email).code)
		assert.Equal(t, http.StatusOK, verify(email, "313131").code)
		assert.Equal(t, http.StatusUnauthorized, verify(email, "313131").code)
	})

	t.Run("a padded mixed-case address signs in to the password account", func(t *testing.T) {
		s := suffix()
		email := "magic-pw-" + s + "@test.luminavault"
		reg := v.register(email, "magicpw"+s)
		require.Equal(t, http.StatusOK, start("  "+strings.ToUpper(email)+"\n").code)
		a := decodeAuth(t, verify(email, "313131"))
		assert.Equal(t, reg.UserID, a.UserID)
	})
}

// TestPreAuthWire pins the start and verify bytes: key order, the uppercase
// challenge id, whole-second ISO-8601, and every refusal's body.
func TestPreAuthWire(t *testing.T) {
	expired := authservice.NewPreAuthChallengeStore(-time.Second, authservice.PreAuthMaxAttempts)
	v := setupWith(t, func(o *authroutes.Options) {
		o.SMS = &recordingSMS{}
		o.MagicSender = &sentMail{}
		o.Challenges = expired
		o.PhoneCodes = authservice.FixedCodeGenerator{Code: "424242"}
	})
	startShape := regexp.MustCompile(`^\{"challengeId":"[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}","expiresAt":"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ"\}$`)
	for _, tc := range []struct{ path, body string }{
		{"/v1/auth/phone/start", `{"phone":"+15550001111"}`},
		{"/v1/auth/email/start", `{"email":"wire@parity.test"}`},
	} {
		r := v.do(http.MethodPost, tc.path, tc.body, nil)
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.Equal(t, jsonCT, r.ct)
		assert.Regexp(t, startShape, r.body)
	}
	for _, tc := range []struct {
		name, path, body string
		code             int
		msg              string
	}{
		{"phone expired is 410", "/v1/auth/phone/verify", `{"phone":"+15550001111","code":"424242"}`, http.StatusGone, "challenge expired; request a new code"},
		{"magic expired is 401", "/v1/auth/email/verify", `{"email":"wire@parity.test","code":"424242"}`, http.StatusUnauthorized, "invalid or expired code"},
		{"bad phone", "/v1/auth/phone/start", `{"phone":"+0"}`, http.StatusBadRequest, `phone must be E.164 (^\+[1-9]\d{6,14}$)`},
		{"bad phone on verify", "/v1/auth/phone/verify", `{"phone":"1","code":"1"}`, http.StatusBadRequest, `phone must be E.164 (^\+[1-9]\d{6,14}$)`},
		{"short email", "/v1/auth/email/start", `{"email":"a@b"}`, http.StatusBadRequest, "email looks invalid"},
		{"bad email on verify", "/v1/auth/email/verify", `{"email":"nope","code":"1"}`, http.StatusBadRequest, "email looks invalid"},
		{"missing code", "/v1/auth/phone/verify", `{"phone":"+15550001111"}`, http.StatusBadRequest, "Coding key `code` not found."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := v.do(http.MethodPost, tc.path, tc.body, nil)
			assert.Equal(t, tc.code, r.code, r.body)
			assert.Equal(t, errorBody(tc.msg), r.body)
		})
	}

	t.Run("a failed send is the sender's error, after the challenge is issued", func(t *testing.T) {
		store := authservice.NewPreAuthChallengeStore(time.Minute, 5)
		w := setupWith(t, func(o *authroutes.Options) {
			o.SMS = failingSMS{}
			o.Challenges = store
			o.PhoneCodes = authservice.FixedCodeGenerator{Code: "424242"}
		})
		r := w.do(http.MethodPost, "/v1/auth/phone/start", `{"phone":"+15550002222"}`, nil)
		assert.Equal(t, http.StatusInternalServerError, r.code)
		assert.Equal(t, errorBody("twilio not configured"), r.body)
		outcome, _, _ := store.ConsumeTyped("+15550002222", "424242")
		assert.Equal(t, authservice.ConsumeOK, outcome)
	})

	t.Run("start is limited to three a minute per IP", func(t *testing.T) {
		ip := map[string]string{"X-Forwarded-For": "203.0.113.77"}
		for i := range 3 {
			r := v.do(http.MethodPost, "/v1/auth/email/start", fmt.Sprintf(`{"email":"limit%d@parity.test"}`, i), ip)
			require.Equal(t, http.StatusOK, r.code)
		}
		assert.Equal(t, http.StatusTooManyRequests, v.do(http.MethodPost, "/v1/auth/email/start", `{"email":"limit@parity.test"}`, ip).code)
		// The phone bucket is its own, and verify has none.
		assert.Equal(t, http.StatusOK, v.do(http.MethodPost, "/v1/auth/phone/start", `{"phone":"+15550003333"}`, ip).code)
		for range 5 {
			assert.Equal(t, http.StatusUnauthorized, v.do(http.MethodPost, "/v1/auth/email/verify", `{"email":"limit@parity.test","code":"1"}`, ip).code)
		}
	})
}

// pairingEnv fixes the pairing code so the tests can tell a wrong one.
func pairingEnv(t *testing.T) env {
	t.Helper()
	return setupWith(t, nil)
}

type pairingStart struct {
	PairingID string `json:"pairingId"`
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expiresAt"`
}

type pairingPoll struct {
	Approved     bool    `json:"approved"`
	UserID       *string `json:"userId"`
	AccessToken  *string `json:"accessToken"`
	RefreshToken *string `json:"refreshToken"`
}

// TestPairingFlow ports PairingFlowTests
// (Tests/AppTests/Auth/PairingFlowTests.swift), all 5 cases: QR-from-mobile
// web sign-in over /v1/auth/pairing/start, GET /{id} and POST /{id}/approve.
func TestPairingFlow(t *testing.T) {
	v := pairingEnv(t)
	phone := func() authBody {
		s := suffix()
		return v.register("pair-"+s+"@test.luminavault", "pair-"+s)
	}
	start := func(t *testing.T) pairingStart {
		t.Helper()
		r := v.do(http.MethodPost, "/v1/auth/pairing/start", "", nil)
		require.Equal(t, http.StatusOK, r.code, r.body)
		var out pairingStart
		require.NoError(t, json.Unmarshal([]byte(r.body), &out))
		return out
	}
	poll := func(t *testing.T, id string) (resp, pairingPoll) {
		t.Helper()
		r := v.do(http.MethodGet, "/v1/auth/pairing/"+id, "", nil)
		var out pairingPoll
		if r.code == http.StatusOK {
			require.NoError(t, json.Unmarshal([]byte(r.body), &out))
		}
		return r, out
	}
	approve := func(id, code, token string) resp {
		h := map[string]string{}
		if token != "" {
			h = bearer(token)
		}
		return v.do(http.MethodPost, "/v1/auth/pairing/"+id+"/approve", fmt.Sprintf(`{"code":%q}`, code), h)
	}

	t.Run("approved pairing hands the browser tokens for the approving account", func(t *testing.T) {
		p := phone()
		s := start(t)
		assert.NotEmpty(t, s.PairingID)
		assert.Len(t, s.Code, 6)
		assert.Greater(t, s.ExpiresAt, time.Now().UnixMilli())

		r, pending := poll(t, s.PairingID)
		require.Equal(t, http.StatusOK, r.code)
		assert.False(t, pending.Approved)
		assert.Nil(t, pending.AccessToken)
		assert.Equal(t, `{"approved":false}`, r.body)

		assert.Equal(t, http.StatusNoContent, approve(s.PairingID, s.Code, p.AccessToken).code)

		r, done := poll(t, s.PairingID)
		require.Equal(t, http.StatusOK, r.code)
		assert.True(t, done.Approved)
		require.NotNil(t, done.UserID)
		assert.Equal(t, p.UserID, *done.UserID)
		require.NotNil(t, done.AccessToken)
		assert.NotEmpty(t, *done.AccessToken)
		require.NotNil(t, done.RefreshToken)
		assert.NotEmpty(t, *done.RefreshToken)

		me := v.do(http.MethodGet, "/v1/auth/me", "", bearer(*done.AccessToken))
		require.Equal(t, http.StatusOK, me.code)
		var m struct {
			UserID string `json:"userId"`
		}
		require.NoError(t, json.Unmarshal([]byte(me.body), &m))
		assert.Equal(t, p.UserID, m.UserID)
	})

	t.Run("wrong code is rejected and leaves the pairing pending", func(t *testing.T) {
		p := phone()
		s := start(t)
		wrong := "000000"
		if s.Code == wrong {
			wrong = "111111"
		}
		r := approve(s.PairingID, wrong, p.AccessToken)
		assert.Equal(t, http.StatusUnauthorized, r.code)
		assert.Equal(t, errorBody("pairing code mismatch"), r.body)
		r, pending := poll(t, s.PairingID)
		require.Equal(t, http.StatusOK, r.code)
		assert.False(t, pending.Approved)
	})

	t.Run("a pairing can only be approved once", func(t *testing.T) {
		p := phone()
		s := start(t)
		assert.Equal(t, http.StatusNoContent, approve(s.PairingID, s.Code, p.AccessToken).code)
		r := approve(s.PairingID, s.Code, p.AccessToken)
		assert.Equal(t, http.StatusConflict, r.code)
		assert.Equal(t, errorBody("pairing already approved"), r.body)
	})

	t.Run("unknown pairing id is a 404 on both poll and approve", func(t *testing.T) {
		p := phone()
		unknown := strings.ToUpper(suffix()) + "-0000-4000-8000-000000000000"
		r, _ := poll(t, unknown)
		assert.Equal(t, http.StatusNotFound, r.code)
		assert.Equal(t, errorBody("pairing expired or unknown"), r.body)
		r = approve(unknown, "123456", p.AccessToken)
		assert.Equal(t, http.StatusNotFound, r.code)
		assert.Equal(t, errorBody("pairing expired or unknown"), r.body)
	})

	t.Run("approve requires a bearer token", func(t *testing.T) {
		s := start(t)
		r := approve(s.PairingID, s.Code, "")
		assert.Equal(t, http.StatusUnauthorized, r.code)
		assert.Equal(t, errorBody("Authenticated identity is unavailable"), r.body)
	})
}

// TestPairingWire: the start body's shape, the approved poll's key order, and
// Hummingbird's routing: GET on the literal "start" is a bare 404, the id is
// case-sensitive, and approve reads the body before looking the pairing up.
func TestPairingWire(t *testing.T) {
	v := setupWith(t, func(o *authroutes.Options) { o.PairingCodes = authservice.FixedCodeGenerator{Code: "123456"} })
	r := v.do(http.MethodPost, "/v1/auth/pairing/start", "not even json", nil)
	require.Equal(t, http.StatusOK, r.code)
	assert.Regexp(t, `^\{"pairingId":"[0-9A-F-]{36}","code":"123456","expiresAt":\d{13}\}$`, r.body)
	var s pairingStart
	require.NoError(t, json.Unmarshal([]byte(r.body), &s))

	got := v.do(http.MethodGet, "/v1/auth/pairing/start", "", nil)
	assert.Equal(t, http.StatusNotFound, got.code)
	assert.Empty(t, got.body)
	assert.Equal(t, http.StatusNotFound, v.do(http.MethodGet, "/v1/auth/pairing/"+strings.ToLower(s.PairingID), "", nil).code)

	reg := v.register("pair-wire-"+suffix()+"@test.luminavault", "pair-wire-"+suffix())
	bad := v.do(http.MethodPost, "/v1/auth/pairing/unknown/approve", `{}`, bearer(reg.AccessToken))
	assert.Equal(t, http.StatusBadRequest, bad.code, "the body is decoded before the lookup")

	require.Equal(t, http.StatusNoContent, v.do(http.MethodPost, "/v1/auth/pairing/"+s.PairingID+"/approve", `{"code":"123456"}`, bearer(reg.AccessToken)).code)
	r = v.do(http.MethodGet, "/v1/auth/pairing/"+s.PairingID, "", nil)
	assert.Regexp(t, `^\{"approved":true,"userId":"`+reg.UserID+`","email":"pair-wire-[0-9a-f]{8}@test.luminavault","accessToken":"[^"]+","refreshToken":"[^"]+","expiresIn":\d+,"vaultInitialized":false\}$`, r.body)
	// Swift weakness, kept: every poll until expiry hands the tokens out again.
	assert.Equal(t, r.body, v.do(http.MethodGet, "/v1/auth/pairing/"+s.PairingID, "", nil).body)

	// start shares registerByIP's bucket with /v1/auth/register.
	ip := map[string]string{"X-Forwarded-For": "203.0.113.90"}
	for range 3 {
		require.Equal(t, http.StatusOK, v.do(http.MethodPost, "/v1/auth/pairing/start", "", ip).code)
	}
	for range 2 {
		require.Equal(t, http.StatusBadRequest, v.do(http.MethodPost, "/v1/auth/register", `{}`, ip).code)
	}
	assert.Equal(t, http.StatusTooManyRequests, v.do(http.MethodPost, "/v1/auth/pairing/start", "", ip).code)
}

// --- OAuth exchange -----------------------------------------------------------

var (
	rsaOnce sync.Once
	rsaKey  *rsa.PrivateKey
)

func signingKey() *rsa.PrivateKey {
	rsaOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		rsaKey = k
	})
	return rsaKey
}

// jwksServer stands in for Google's certs endpoint.
func jwksServer(t *testing.T) *httptest.Server {
	t.Helper()
	k := signingKey()
	body := fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":"k1","alg":"RS256","n":%q,"e":%q}]}`,
		base64.RawURLEncoding.EncodeToString(k.N.Bytes()), base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(srv.Close)
	return srv
}

func idToken(t *testing.T, sub, email string, verified bool) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": sub, "aud": "web-client", "iss": "https://accounts.google.com",
		"exp": time.Now().Add(time.Minute).Unix(), "email": email, "email_verified": verified,
	})
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(signingKey())
	require.NoError(t, err)
	return s
}

// TestOAuthExchange drives POST /v1/auth/oauth/{provider}/exchange with a
// Google provider on a stub JWKS: GoogleOAuthProviderTests' verifier behind
// the route, and exchangeOAuth's upsert (AuthFlowTests has no OAuth case).
func TestOAuthExchange(t *testing.T) {
	jwks := jwksServer(t)
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	v := setupWith(t, func(o *authroutes.Options) {
		aud := map[string]bool{"web-client": true}
		o.OAuthProviders = map[string]authservice.OAuthProvider{
			"google": authservice.NewGoogleProvider(aud, jwks.URL, jwks.Client()),
			"apple":  authservice.NewAppleProvider(aud, downURL, nil),
		}
	})
	exchange := func(provider, body string) resp {
		return v.do(http.MethodPost, "/v1/auth/oauth/"+provider+"/exchange", body, nil)
	}

	sub := "g-" + suffix()
	email := "G-" + suffix() + "@Example.test"
	a := decodeAuth(t, exchange("google", fmt.Sprintf(`{"idToken":%q}`, idToken(t, sub, email, true))))
	assert.Equal(t, strings.ToLower(email), a.Email)
	puid, ok := v.identity(t, a.UserID, "google")
	require.True(t, ok)
	assert.Equal(t, sub, puid)
	again := decodeAuth(t, exchange("google", fmt.Sprintf(`{"idToken":%q}`, idToken(t, sub, "changed@example.test", true))))
	assert.Equal(t, a.UserID, again.UserID, "the identity wins over the email")

	for _, tc := range []struct {
		name, provider, body string
		code                 int
		body2                string
	}{
		{"unverified email", "google", fmt.Sprintf(`{"idToken":%q}`, idToken(t, "g2", "u@example.test", false)), 401, errorBody("invalid id_token")},
		{"garbage token", "google", `{"idToken":"x.y.z"}`, 401, errorBody("invalid id_token")},
		{"unknown provider before the body", "github", `not json`, 404, errorBody("unsupported provider")},
		{"x is not an id_token provider here", "X", `{}`, 404, errorBody("unsupported provider")},
		{"missing idToken", "google", `{}`, 400, errorBody("Coding key `idToken` not found.")},
		{"JWKS unreachable is a 500", "apple", `{"idToken":"x.y.z"}`, 500, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := exchange(tc.provider, tc.body)
			assert.Equal(t, tc.code, r.code, r.body)
			assert.Equal(t, tc.body2, r.body)
		})
	}

	t.Run("no providers configured", func(t *testing.T) {
		w := setup(t)
		r := w.do(http.MethodPost, "/v1/auth/oauth/google/exchange", `{"idToken":"x"}`, nil)
		assert.Equal(t, http.StatusNotFound, r.code)
	})
}

// TestXExchange drives POST /v1/auth/oauth/x/exchange with the Swift suite's
// stub client (XOAuthTests), which takes the literal route ahead of :provider.
func TestXExchange(t *testing.T) {
	x := &stubX{}
	v := setupWith(t, func(o *authroutes.Options) { o.XClient = x })
	exchange := func(body string) resp { return v.do(http.MethodPost, "/v1/auth/oauth/x/exchange", body, nil) }

	r := exchange(`{"accessToken":""}`)
	assert.Equal(t, http.StatusBadRequest, r.code)
	assert.Equal(t, errorBody("accessToken required"), r.body)
	assert.Empty(t, x.observed, "an empty token never reaches X")

	id := "x" + suffix()
	x.user = &authservice.XUser{ID: id, Name: "N", Username: "u"}
	a := decodeAuth(t, exchange(`{"accessToken":"tok"}`))
	assert.Equal(t, id+"@x.luminavault.local", a.Email)
	assert.Equal(t, []string{"tok"}, x.observed)
	var verified bool
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT email_verified FROM oauth_identities WHERE provider = 'x' AND provider_user_id = $1`, id).Scan(&verified))
	assert.False(t, verified)

	mail := "Mixed." + suffix() + "@Example.com"
	x.user = &authservice.XUser{ID: "y" + suffix(), Name: "N", Username: "u", Email: &mail}
	a = decodeAuth(t, exchange(`{"accessToken":"tok"}`))
	assert.Equal(t, strings.ToLower(mail), a.Email)

	x.user, x.err = nil, errors.New("connection refused")
	assert.Equal(t, http.StatusInternalServerError, exchange(`{"accessToken":"tok"}`).code)
	x.err = wire.NewError(http.StatusUnauthorized, "x access_token rejected")
	r = exchange(`{"accessToken":"tok"}`)
	assert.Equal(t, http.StatusUnauthorized, r.code)
	assert.Equal(t, errorBody("x access_token rejected"), r.body)
}

type stubX struct {
	mu       sync.Mutex
	user     *authservice.XUser
	err      error
	observed []string
}

func (s *stubX) FetchMe(_ context.Context, token string) (*authservice.XUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observed = append(s.observed, token)
	return s.user, s.err
}
