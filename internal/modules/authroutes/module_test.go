package authroutes_test

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/authservice"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/hermesprofile"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/authroutes"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultpaths"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var keys = auth.NewKeyset(auth.Key{Kid: "test-kid", Secret: []byte("test-secret-do-not-use-in-prod-32chars")})

const password = "CorrectHorseBatteryStaple1!"

type event struct {
	name, distinct string
	props          map[string]any
}

type events struct {
	mu  sync.Mutex
	got []event
}

func (e *events) Capture(name, distinctID string, props map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, event{name, distinctID, props})
}

func (e *events) named(name string) []event {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []event
	for _, ev := range e.got {
		if ev.name == name {
			out = append(out, ev)
		}
	}
	return out
}

type nopSender struct{}

func (nopSender) Send(context.Context, string, string, string) error { return nil }

type env struct {
	t         *testing.T
	srv       http.Handler
	analytics *events
}

// setup serves the 8A routes on a fresh database with fixed codes and senders
// that send nothing.
func setup(t *testing.T) env {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	analytics := &events{}
	svc := authservice.New(pool, authservice.Options{
		Keys:      keys,
		Profiles:  hermesprofile.New(pool, hermesprofile.LoggingGateway{}, vaultpaths.Paths{Root: t.TempDir()}),
		Analytics: analytics,
		MFASender: nopSender{}, ResetSender: nopSender{}, VerifySender: nopSender{},
		MFACodes:   authservice.FixedCodeGenerator{Code: "123456"},
		ResetCodes: authservice.FixedCodeGenerator{Code: "654321"}, VerifyCodes: authservice.FixedCodeGenerator{Code: "789012"},
		PasswordCost: 4,
	})
	module := authroutes.New(pool, authroutes.Options{Service: svc, Limits: ratelimit.NewStore(), Analytics: analytics})
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: []httpserver.Module{module}})
	return env{t: t, srv: e, analytics: analytics}
}

type resp struct {
	code int
	ct   string
	hdr  http.Header
	body string
}

var ipCounter struct {
	sync.Mutex
	n int
}

// freshIP gives each request its own X-Forwarded-For unless the caller sets
// one, so no per-IP bucket fills by accident.
func freshIP() string {
	ipCounter.Lock()
	defer ipCounter.Unlock()
	ipCounter.n++
	return fmt.Sprintf("10.%d.%d.%d", ipCounter.n>>16&255, ipCounter.n>>8&255, ipCounter.n&255)
}

func (v env) do(method, path, body string, headers map[string]string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Forwarded-For", freshIP())
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	rec := httptest.NewRecorder()
	v.srv.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Header(), rec.Body.String()}
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func suffix() string { return strings.ToLower(uuid.NewString()[:8]) }

type authBody struct {
	UserID         string  `json:"userId"`
	Email          string  `json:"email"`
	AccessToken    string  `json:"accessToken"`
	RefreshToken   string  `json:"refreshToken"`
	ExpiresIn      int     `json:"expiresIn"`
	MFARequired    *bool   `json:"mfaRequired"`
	MFAChallengeID *string `json:"mfaChallengeId"`
}

func (v env) register(email, username string) authBody {
	v.t.Helper()
	r := v.do(http.MethodPost, "/v1/auth/register", fmt.Sprintf(`{"email":%q,"username":%q,"password":%q}`, email, username, password), nil)
	require.Equal(v.t, http.StatusOK, r.code, r.body)
	var out authBody
	require.NoError(v.t, json.Unmarshal([]byte(r.body), &out))
	return out
}

func errorBody(message string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"message": message}})
	return string(b)
}

const jsonCT = "application/json; charset=utf-8"

// TestPrivacyToggle ports PrivacyToggleTests
// (Tests/AppTests/Auth/PrivacyToggleTests.swift), all 3 cases: HER-176's
// PUT /v1/auth/me/privacy and the privacy field of GET /v1/auth/me.
func TestPrivacyToggle(t *testing.T) {
	v := setup(t)
	me := func(r resp) map[string]any {
		t.Helper()
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(r.body), &out), r.body)
		return out
	}

	t.Run("new user defaults to privacy off", func(t *testing.T) {
		s := suffix()
		token := v.register("priv-"+s+"@test.luminavault", "priv"+s).AccessToken
		r := v.do(http.MethodGet, "/v1/auth/me", "", bearer(token))
		require.Equal(t, http.StatusOK, r.code)
		assert.Equal(t, false, me(r)["privacyNoCNOrigin"])
	})

	t.Run("put me privacy flips toggle and persists", func(t *testing.T) {
		s := suffix()
		token := v.register("priv-"+s+"@test.luminavault", "priv"+s).AccessToken
		r := v.do(http.MethodPut, "/v1/auth/me/privacy", `{"privacyNoCNOrigin":true}`, bearer(token))
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.Equal(t, true, me(r)["privacyNoCNOrigin"])
		assert.Equal(t, true, me(v.do(http.MethodGet, "/v1/auth/me", "", bearer(token)))["privacyNoCNOrigin"])
		r = v.do(http.MethodPut, "/v1/auth/me/privacy", `{"privacyNoCNOrigin":false}`, bearer(token))
		assert.Equal(t, false, me(r)["privacyNoCNOrigin"])
	})

	t.Run("put me privacy requires bearer", func(t *testing.T) {
		r := v.do(http.MethodPut, "/v1/auth/me/privacy", `{"privacyNoCNOrigin":true}`, nil)
		assert.Equal(t, http.StatusUnauthorized, r.code)
	})
}

// TestMeWire pins /v1/auth/me's bytes: MeResponse's key order, uppercase id,
// Foundation's "\/", and updatePrivacy writing only the fields present.
func TestMeWire(t *testing.T) {
	v := setup(t)
	s := suffix()
	reg := v.register("me/"+s+"@test.luminavault", "me"+s)

	r := v.do(http.MethodGet, "/v1/auth/me", "", bearer(reg.AccessToken))
	require.Equal(t, http.StatusOK, r.code)
	assert.Equal(t, jsonCT, r.ct)
	assert.Equal(t, `{"userId":"`+reg.UserID+`","email":"me\/`+s+`@test.luminavault","username":"me`+s+
		`","isVerified":false,"privacyNoCNOrigin":false,"contextRouting":false,"autoSaveLinks":true,"mnemosyneEnabled":true,"isAdmin":false}`, r.body)
	assert.Equal(t, strings.ToUpper(reg.UserID), reg.UserID)

	r = v.do(http.MethodPut, "/v1/auth/me/privacy", `{"contextRouting":true,"autoSaveLinks":false,"mnemosyneEnabled":null}`, bearer(reg.AccessToken))
	require.Equal(t, http.StatusOK, r.code, r.body)
	assert.Contains(t, r.body, `"privacyNoCNOrigin":false,"contextRouting":true,"autoSaveLinks":false,"mnemosyneEnabled":true`)

	r = v.do(http.MethodPut, "/v1/auth/me/privacy", `{}`, bearer(reg.AccessToken))
	require.Equal(t, http.StatusOK, r.code)
	assert.Contains(t, r.body, `"contextRouting":true,"autoSaveLinks":false`)

	r = v.do(http.MethodPut, "/v1/auth/me/privacy", `{"contextRouting":"yes"}`, bearer(reg.AccessToken))
	assert.Equal(t, http.StatusBadRequest, r.code)
	assert.Contains(t, r.body, "contextRouting")

	// Identity before the body, as Swift's group authenticator runs first.
	r = v.do(http.MethodPut, "/v1/auth/me/privacy", `not json`, nil)
	assert.Equal(t, http.StatusUnauthorized, r.code)
	assert.Equal(t, errorBody("Authenticated identity is unavailable"), r.body)
	r = v.do(http.MethodGet, "/v1/auth/me", "", bearer("not.a.jwt"))
	assert.Equal(t, http.StatusUnauthorized, r.code)
}

var (
	upperUUID = `[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}`
	jwtRe     = `[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`
	refreshRe = `[A-Za-z0-9_-]{43}`
)

// TestSessionRoutesWire pins AuthController's wire: AuthResponse's bytes, the
// empty 202s and 204s, AuthError's bodies, the per-IP limits and the PostHog events.
func TestSessionRoutesWire(t *testing.T) {
	v := setup(t)
	s := suffix()
	email, username := "wire-"+s+"@test.luminavault", "wire-"+s

	r := v.do(http.MethodPost, "/v1/auth/register", fmt.Sprintf(`{"email":"WIRE-%s@test.luminavault","username":" Wire-%s ","password":%q}`, s, s, password), nil)
	require.Equal(t, http.StatusOK, r.code, r.body)
	assert.Equal(t, jsonCT, r.ct)
	assert.Regexp(t, regexp.MustCompile(`^\{"userId":"`+upperUUID+`","email":"`+regexp.QuoteMeta(email)+`","accessToken":"`+jwtRe+
		`","refreshToken":"`+refreshRe+`","expiresIn":3600,"vaultInitialized":false\}$`), r.body)
	var reg authBody
	require.NoError(t, json.Unmarshal([]byte(r.body), &reg))
	if ev := v.analytics.named("user_registered"); assert.Len(t, ev, 1) {
		assert.Equal(t, reg.UserID, ev[0].distinct)
		assert.Equal(t, map[string]any{"auth_method": "password"}, ev[0].props)
	}
	assert.Len(t, v.analytics.named("$set"), 1)

	t.Run("register errors", func(t *testing.T) {
		r := v.do(http.MethodPost, "/v1/auth/register", fmt.Sprintf(`{"email":%q,"username":"x-%s","password":%q}`, email, suffix(), password), nil)
		assert.Equal(t, http.StatusConflict, r.code)
		assert.Equal(t, errorBody("email already registered"), r.body)
		r = v.do(http.MethodPost, "/v1/auth/register", fmt.Sprintf(`{"email":"x-%s@t.lv","username":%q,"password":%q}`, suffix(), username, password), nil)
		assert.Equal(t, errorBody("username already taken"), r.body)
		r = v.do(http.MethodPost, "/v1/auth/register", `{"email":"a@b.c","username":"ok-name","password":"elevenchars"}`, nil)
		assert.Equal(t, http.StatusBadRequest, r.code)
		assert.Equal(t, errorBody("password too short (min 12)"), r.body)
		// Twelve Characters, more bytes: Swift counts grapheme clusters.
		r = v.do(http.MethodPost, "/v1/auth/register", `{"email":"a@b.c","username":"no","password":"🇵🇹🇵🇹🇵🇹🇵🇹🇵🇹🇵🇹🇵🇹🇵🇹🇵🇹🇵🇹🇵🇹🇵🇹"}`, nil)
		assert.Equal(t, errorBody("username must be 3-31 chars, lowercase a-z/0-9/-, not reserved"), r.body)
		r = v.do(http.MethodPost, "/v1/auth/register", `{"email":"a@b.c","password":"x"}`, nil)
		assert.Equal(t, http.StatusBadRequest, r.code)
		assert.Equal(t, errorBody("Coding key `username` not found."), r.body)
	})

	t.Run("login", func(t *testing.T) {
		r := v.do(http.MethodPost, "/v1/auth/login", fmt.Sprintf(`{"email":%q,"password":"wrong-password-1"}`, email), nil)
		assert.Equal(t, http.StatusUnauthorized, r.code)
		assert.Equal(t, errorBody("invalid credentials"), r.body)

		r = v.do(http.MethodPost, "/v1/auth/login", fmt.Sprintf(`{"email":%q,"password":%q,"mfaCode":7}`, email, password), nil)
		assert.Equal(t, http.StatusBadRequest, r.code, "mfaCode is decoded though unused")

		r = v.do(http.MethodPost, "/v1/auth/login", fmt.Sprintf(`{"email":%q,"password":%q,"mfaCode":null}`, email, password), nil)
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.NotContains(t, r.body, "mfaRequired")

		r = v.do(http.MethodPost, "/v1/auth/login", fmt.Sprintf(`{"email":%q,"password":%q}`, email, password), map[string]string{"MFA-Auth-V1": "TRUE"})
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.Regexp(t, regexp.MustCompile(`^\{"userId":"`+reg.UserID+`","email":"`+regexp.QuoteMeta(email)+
			`","accessToken":"","refreshToken":"","expiresIn":0,"mfaRequired":true,"mfaChallengeId":"`+upperUUID+`","vaultInitialized":false\}$`), r.body)
		var pending authBody
		require.NoError(t, json.Unmarshal([]byte(r.body), &pending))

		logins := v.analytics.named("user_logged_in")
		require.Len(t, logins, 2)
		assert.Equal(t, map[string]any{"auth_method": "password", "mfa_required": false}, logins[0].props)
		assert.Equal(t, map[string]any{"auth_method": "password", "mfa_required": true}, logins[1].props)

		r = v.do(http.MethodPost, "/v1/auth/mfa/verify", `{"challengeId":"not-a-uuid","code":"123456"}`, nil)
		assert.Equal(t, http.StatusBadRequest, r.code)
		r = v.do(http.MethodPost, "/v1/auth/mfa/verify", `{"challengeId":"`+strings.ToLower(*pending.MFAChallengeID)+`","code":"000000"}`, nil)
		assert.Equal(t, errorBody("mfa code invalid"), r.body)
		r = v.do(http.MethodPost, "/v1/auth/mfa/verify", `{"challengeId":"`+*pending.MFAChallengeID+`","code":"123456"}`, nil)
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.Contains(t, r.body, `"expiresIn":3600,"vaultInitialized":false}`)

		r = v.do(http.MethodPost, "/v1/auth/mfa/resend", fmt.Sprintf(`{"email":%q}`, email), nil)
		assert.Equal(t, http.StatusAccepted, r.code)
		assert.Empty(t, r.body)
		assert.Empty(t, r.ct)
		r = v.do(http.MethodPost, "/v1/auth/mfa/resend", `{"email":"nobody@test.luminavault"}`, nil)
		assert.Equal(t, errorBody("invalid credentials"), r.body)
	})

	t.Run("refresh and logout", func(t *testing.T) {
		r := v.do(http.MethodPost, "/v1/auth/refresh", fmt.Sprintf(`{"refreshToken":%q}`, reg.RefreshToken), nil)
		require.Equal(t, http.StatusOK, r.code)
		var next authBody
		require.NoError(t, json.Unmarshal([]byte(r.body), &next))
		r = v.do(http.MethodPost, "/v1/auth/refresh", fmt.Sprintf(`{"refreshToken":%q}`, reg.RefreshToken), nil)
		assert.Equal(t, http.StatusUnauthorized, r.code)
		assert.Equal(t, errorBody("invalid refresh token"), r.body)

		r = v.do(http.MethodPost, "/v1/auth/logout", fmt.Sprintf(`{"refreshToken":%q}`, next.RefreshToken), nil)
		assert.Equal(t, http.StatusNoContent, r.code)
		assert.Empty(t, r.body)
		r = v.do(http.MethodPost, "/v1/auth/logout", `{"refreshToken":"unknown"}`, nil)
		assert.Equal(t, http.StatusNoContent, r.code)
		r = v.do(http.MethodPost, "/v1/auth/refresh", fmt.Sprintf(`{"refreshToken":%q}`, next.RefreshToken), nil)
		assert.Equal(t, http.StatusUnauthorized, r.code)
	})

	t.Run("reset and email verification", func(t *testing.T) {
		for _, path := range []string{"/v1/auth/forgot-password", "/v1/auth/resend-reset", "/v1/auth/email/send-verify"} {
			for _, who := range []string{email, "nobody@test.luminavault"} {
				r := v.do(http.MethodPost, path, fmt.Sprintf(`{"email":%q}`, who), nil)
				assert.Equal(t, http.StatusAccepted, r.code, path)
				assert.Empty(t, r.body, path)
			}
		}
		r := v.do(http.MethodPost, "/v1/auth/reset-password", fmt.Sprintf(`{"email":%q,"code":"000000","newPassword":"NewPasswordCorrectHorse1!"}`, email), nil)
		assert.Equal(t, errorBody("reset code invalid or expired"), r.body)
		r = v.do(http.MethodPost, "/v1/auth/reset-password", fmt.Sprintf(`{"email":%q,"code":"654321","newPassword":"NewPasswordCorrectHorse1!"}`, email), nil)
		require.Equal(t, http.StatusOK, r.code, r.body)

		r = v.do(http.MethodPost, "/v1/auth/email/confirm", fmt.Sprintf(`{"email":%q,"code":"000000"}`, email), nil)
		assert.Equal(t, http.StatusUnauthorized, r.code)
		assert.Equal(t, errorBody("verification code invalid or expired"), r.body)
		r = v.do(http.MethodPost, "/v1/auth/email/confirm", fmt.Sprintf(`{"email":%q,"code":"789012"}`, email), nil)
		assert.Equal(t, http.StatusNoContent, r.code)
		assert.Empty(t, r.body)
		// Verified: any code is a 204.
		r = v.do(http.MethodPost, "/v1/auth/email/confirm", fmt.Sprintf(`{"email":%q,"code":"nope"}`, email), nil)
		assert.Equal(t, http.StatusNoContent, r.code)
	})

	t.Run("per-IP limits; logout has none", func(t *testing.T) {
		ip := map[string]string{"X-Forwarded-For": "203.0.113.9, 10.0.0.1"}
		for i := range 5 {
			r := v.do(http.MethodPost, "/v1/auth/register", `{}`, ip)
			assert.Equal(t, http.StatusBadRequest, r.code, i)
		}
		r := v.do(http.MethodPost, "/v1/auth/register", `{}`, ip)
		assert.Equal(t, http.StatusTooManyRequests, r.code)
		assert.NotEmpty(t, r.hdr.Get("Retry-After"))
		// Each route has its own bucket.
		r = v.do(http.MethodPost, "/v1/auth/login", `{}`, ip)
		assert.Equal(t, http.StatusBadRequest, r.code)
		for range 40 {
			r = v.do(http.MethodPost, "/v1/auth/logout", `{"refreshToken":"x"}`, ip)
			require.Equal(t, http.StatusNoContent, r.code)
		}
	})
}
