package webauthnroutes_test

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
	"lumina/internal/modules/webauthnroutes"
	"lumina/internal/passkey"
	"lumina/internal/passkey/passkeytest"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultpaths"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var keys = auth.NewKeyset(auth.Key{Kid: "test-kid", Secret: []byte("test-secret-do-not-use-in-prod-32chars")})

const (
	password = "CorrectHorseBatteryStaple1!"
	rpID     = "luminavault.test"
	origin   = "https://luminavault.test"
	jsonCT   = "application/json; charset=utf-8"
)

type nopSender struct{}

func (nopSender) Send(context.Context, string, string, string) error { return nil }

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	srv   http.Handler
	clock *clock
}

type setupOpts struct {
	disabled bool
	cfg      *passkey.Config
}

// setup serves 8A (for /v1/auth/register) and 8C on a fresh database, configured
// as the Swift suites configure it: webauthn.enabled true, RP id luminavault.test,
// origin https://luminavault.test.
func setup(t *testing.T, o setupOpts) env {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	svc := authservice.New(pool, authservice.Options{
		Keys:      keys,
		Profiles:  hermesprofile.New(pool, hermesprofile.LoggingGateway{}, vaultpaths.Paths{Root: t.TempDir()}),
		MFASender: nopSender{}, ResetSender: nopSender{}, VerifySender: nopSender{},
		PasswordCost: 4,
	})
	cfg := passkey.Config{RPID: rpID, RPName: "LuminaVault Test", Origins: passkey.ParseOrigins(origin)}
	if o.cfg != nil {
		cfg = *o.cfg
	}
	clk := &clock{now: time.Now()}
	limits := ratelimit.NewStore()
	web := webauthnroutes.New(pool, webauthnroutes.Options{
		Enabled: !o.disabled, Config: cfg, Service: svc, Limits: limits,
		Identify: auth.Identify(keys, auth.PGUsers(pool)),
		Store:    passkey.NewStoreWithClock(clk.Now),
		Now:      clk.Now,
	})
	sessions := authroutes.New(pool, authroutes.Options{Service: svc, Limits: limits})
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: []httpserver.Module{sessions, web}})
	return env{t: t, pool: pool, srv: e, clock: clk}
}

type resp struct {
	code int
	ct   string
	body string
}

var ipCounter struct {
	sync.Mutex
	n int
}

func freshIP() string {
	ipCounter.Lock()
	defer ipCounter.Unlock()
	ipCounter.n++
	return fmt.Sprintf("10.%d.%d.%d", ipCounter.n>>16&255, ipCounter.n>>8&255, ipCounter.n&255)
}

func (v env) do(method, path, body, token string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Forwarded-For", freshIP())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	v.srv.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.String()}
}

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

type user struct {
	id, username, token string
}

// makeUser registers through /v1/auth/register, as the Swift suites do.
func (v env) makeUser() user {
	v.t.Helper()
	s := strings.ToLower(uuid.NewString()[:8])
	r := v.do(http.MethodPost, "/v1/auth/register", fmt.Sprintf(`{"email":"wae-%s@test.luminavault","username":"wae-%s","password":%q}`, s, s, password), "")
	require.Equal(v.t, http.StatusOK, r.code, r.body)
	var out struct {
		UserID      string `json:"userId"`
		AccessToken string `json:"accessToken"`
	}
	require.NoError(v.t, json.Unmarshal([]byte(r.body), &out))
	return user{id: out.UserID, username: "wae-" + s, token: out.AccessToken}
}

func beginBody(username string) string { return fmt.Sprintf(`{"username":%q}`, username) }

func errorBody(message string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"message": message}})
	return string(b)
}

func challengeOf(t *testing.T, body string) string {
	t.Helper()
	var out struct {
		Options struct {
			Challenge string `json:"challenge"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out), body)
	require.NotEmpty(t, out.Options.Challenge)
	return out.Options.Challenge
}

func credentialIDs(t *testing.T, body string) []string {
	t.Helper()
	var out struct {
		Credentials []struct {
			ID string `json:"id"`
		} `json:"credentials"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out), body)
	ids := []string{}
	for _, c := range out.Credentials {
		ids = append(ids, c.ID)
	}
	return ids
}

// TestWebAuthnAntiEnumeration ports WebAuthnAntiEnumerationTests
// (Tests/AppTests/Auth/WebAuthnAntiEnumerationTests.swift), its 1 case: sign-in options
// for an unknown username are a 200 with a challenge, like a known one's.
func TestWebAuthnAntiEnumeration(t *testing.T) {
	v := setup(t, setupOpts{})
	t.Run("begin authentication returns 200 for unknown username", func(t *testing.T) {
		unknown := "ghost-" + strings.ToLower(uuid.NewString()[:6])
		r := v.do(http.MethodPost, "/v1/auth/webauthn/authenticate/options", beginBody(unknown), "")
		assert.Equal(t, http.StatusOK, r.code)
		assert.Contains(t, r.body, "challenge")

		known := v.makeUser()
		k := v.do(http.MethodPost, "/v1/auth/webauthn/authenticate/options", beginBody(known.username), "")
		assert.Equal(t, http.StatusOK, k.code)
		shape := regexp.MustCompile(`"challenge":"[A-Za-z0-9_-]{43}"`)
		assert.Equal(t, shape.ReplaceAllString(k.body, ""), shape.ReplaceAllString(r.body, ""), "same bytes but the challenge")
	})
}

// TestWebAuthnEnrolmentAuth ports WebAuthnEnrolmentAuthTests
// (Tests/AppTests/Auth/WebAuthnEnrolmentAuthTests.swift), all 8 cases: enrolment needs
// a bearer (both begin paths and finish, before the body is read), acts only on the
// token's account, and sign-in stays public.
func TestWebAuthnEnrolmentAuth(t *testing.T) {
	v := setup(t, setupOpts{})

	t.Run("begin registration without a bearer is rejected", func(t *testing.T) {
		victim := v.makeUser()
		r := v.do(http.MethodPost, "/v1/auth/webauthn/register/begin", beginBody(victim.username), "")
		assert.Equal(t, http.StatusUnauthorized, r.code)
		assert.Equal(t, errorBody("missing identity"), r.body)
	})

	t.Run("deprecated register options alias also requires a bearer", func(t *testing.T) {
		victim := v.makeUser()
		r := v.do(http.MethodPost, "/v1/auth/webauthn/register/options", beginBody(victim.username), "")
		assert.Equal(t, http.StatusUnauthorized, r.code)
	})

	t.Run("finish registration without a bearer is rejected", func(t *testing.T) {
		victim := v.makeUser()
		r := v.do(http.MethodPost, "/v1/auth/webauthn/register/finish", beginBody(victim.username), "")
		assert.Equal(t, http.StatusUnauthorized, r.code)
	})

	t.Run("begin registration with an undecodable body is still 401", func(t *testing.T) {
		r := v.do(http.MethodPost, "/v1/auth/webauthn/register/begin", `{"not-a-field":123}`, "")
		assert.Equal(t, http.StatusUnauthorized, r.code)
	})

	t.Run("begin registration naming another user is forbidden", func(t *testing.T) {
		victim := v.makeUser()
		attacker := v.makeUser()
		r := v.do(http.MethodPost, "/v1/auth/webauthn/register/begin", beginBody(victim.username), attacker.token)
		assert.Equal(t, http.StatusForbidden, r.code)
		assert.Equal(t, errorBody("username does not match the authenticated user"), r.body)

		r = v.do(http.MethodGet, "/v1/auth/webauthn/credentials", "", victim.token)
		assert.Equal(t, http.StatusOK, r.code)
		assert.Empty(t, credentialIDs(t, r.body))
	})

	t.Run("begin registration for the authenticated user issues a challenge", func(t *testing.T) {
		u := v.makeUser()
		r := v.do(http.MethodPost, "/v1/auth/webauthn/register/begin", beginBody(u.username), u.token)
		assert.Equal(t, http.StatusOK, r.code)
		assert.Contains(t, r.body, "challenge")
	})

	t.Run("begin registration without a username uses the token's account", func(t *testing.T) {
		u := v.makeUser()
		r := v.do(http.MethodPost, "/v1/auth/webauthn/register/begin", `{"username":""}`, u.token)
		assert.Equal(t, http.StatusOK, r.code)
		assert.Contains(t, r.body, u.username)
	})

	t.Run("authenticate begin remains reachable without a bearer", func(t *testing.T) {
		u := v.makeUser()
		r := v.do(http.MethodPost, "/v1/auth/webauthn/authenticate/begin", beginBody(u.username), "")
		assert.Equal(t, http.StatusOK, r.code)
		assert.Contains(t, r.body, "challenge")
	})
}

// enrol runs register/begin and register/finish with a software authenticator.
func (v env) enrol(u user, a *passkeytest.Authenticator) resp {
	v.t.Helper()
	r := v.do(http.MethodPost, "/v1/auth/webauthn/register/begin", beginBody(u.username), u.token)
	require.Equal(v.t, http.StatusOK, r.code, r.body)
	body := jsonBody(v.t, map[string]any{"username": u.username, "credentialCreationData": a.Registration(challengeOf(v.t, r.body))})
	return v.do(http.MethodPost, "/v1/auth/webauthn/register/finish", body, u.token)
}

// signIn runs authenticate/begin and authenticate/finish, sending id as the credential id.
func (v env) signIn(username string, a *passkeytest.Authenticator, id string) resp {
	v.t.Helper()
	r := v.do(http.MethodPost, "/v1/auth/webauthn/authenticate/begin", beginBody(username), "")
	require.Equal(v.t, http.StatusOK, r.code, r.body)
	body := jsonBody(v.t, map[string]any{"username": username, "credential": a.Assertion(challengeOf(v.t, r.body), id)})
	return v.do(http.MethodPost, "/v1/auth/webauthn/authenticate/finish", body, "")
}

type row struct {
	tenant    uuid.UUID
	credID    string
	publicKey []byte
	signCount int64
	created   *time.Time
	updated   *time.Time
}

func (v env) rows(tenant string) []row {
	v.t.Helper()
	rs, err := v.pool.Query(context.Background(), `SELECT tenant_id, credential_id, public_key, sign_count, created_at, updated_at FROM webauthn_credentials WHERE tenant_id = $1`, uuid.MustParse(tenant))
	require.NoError(v.t, err)
	defer rs.Close()
	var out []row
	for rs.Next() {
		var r row
		require.NoError(v.t, rs.Scan(&r.tenant, &r.credID, &r.publicKey, &r.signCount, &r.created, &r.updated))
		out = append(out, r)
	}
	return out
}

// TestCeremonies runs both ceremonies end to end with an ES256 software authenticator,
// and pins the row Go writes to Fluent's encoding (WebAuthnCredential, M11): the id as
// padded standard base64 text, the COSE_Key bytes in bytea, the counter as bigint,
// created_at and updated_at both stamped at creation.
func TestCeremonies(t *testing.T) {
	v := setup(t, setupOpts{})
	u := v.makeUser()
	a := passkeytest.New(rpID, origin)
	a.CredentialID = []byte("credential-id-24-bytes!!") // 24 bytes: base64 needs no padding and has no - or _
	a.Counter = 3

	r := v.enrol(u, a)
	require.Equal(t, http.StatusOK, r.code, r.body)
	assert.Equal(t, jsonCT, r.ct)
	stored := "Y3JlZGVudGlhbC1pZC0yNC1ieXRlcyEh"
	assert.Equal(t, `{"credentialID":"`+stored+`"}`, r.body)

	rows := v.rows(u.id)
	require.Len(t, rows, 1)
	assert.Equal(t, stored, rows[0].credID)
	assert.Equal(t, a.COSEKey(), rows[0].publicKey)
	assert.Equal(t, int64(3), rows[0].signCount)
	require.NotNil(t, rows[0].created)
	require.NotNil(t, rows[0].updated)
	assert.Equal(t, *rows[0].created, *rows[0].updated)

	r = v.do(http.MethodGet, "/v1/auth/webauthn/credentials", "", u.token)
	require.Equal(t, http.StatusOK, r.code)
	ts := rows[0].created.UTC().Format("2006-01-02T15:04:05Z")
	assert.Equal(t, `{"credentials":[{"id":"`+stored+`","createdAt":"`+ts+`","lastUsedAt":"`+ts+`"}]}`, r.body)

	// The challenge is gone after a successful finish.
	body := jsonBody(t, map[string]any{"username": u.username, "credentialCreationData": a.Registration("x")})
	r = v.do(http.MethodPost, "/v1/auth/webauthn/register/finish", body, u.token)
	assert.Equal(t, http.StatusBadRequest, r.code)
	assert.Equal(t, errorBody("missing or expired registration challenge"), r.body)

	v.clock.advance(time.Minute)
	a.Counter = 4
	r = v.signIn(strings.ToUpper(u.username), a, stored)
	require.Equal(t, http.StatusOK, r.code, r.body)
	assert.Regexp(t, `^\{"userId":"`+u.id+`","email":"wae-[0-9a-f]{8}@test\.luminavault","accessToken":"[^"]+","refreshToken":"[^"]+","expiresIn":3600,"vaultInitialized":(true|false)\}$`, r.body)
	var tokens struct {
		AccessToken string `json:"accessToken"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.body), &tokens))
	assert.Equal(t, http.StatusOK, v.do(http.MethodGet, "/v1/auth/me", "", tokens.AccessToken).code)

	rows = v.rows(u.id)
	assert.Equal(t, int64(4), rows[0].signCount)
	assert.Equal(t, rows[0].created.Add(time.Minute), *rows[0].updated, "updated_at is the sign-in")

	// Replaying the same counter fails: Swift's potentialReplayAttack is a bare 500.
	r = v.signIn(u.username, a, stored)
	assert.Equal(t, http.StatusInternalServerError, r.code)
	assert.Empty(t, r.body)

	// Deleting is 204, and the list is then empty.
	r = v.do(http.MethodDelete, "/v1/auth/webauthn/credentials/"+stored, "", u.token)
	assert.Equal(t, http.StatusNoContent, r.code)
	assert.Empty(t, r.body)
	r = v.do(http.MethodGet, "/v1/auth/webauthn/credentials", "", u.token)
	assert.Equal(t, `{"credentials":[]}`, r.body)
	r = v.do(http.MethodDelete, "/v1/auth/webauthn/credentials/"+stored, "", u.token)
	assert.Equal(t, http.StatusNoContent, r.code, "nothing to delete is still 204")
}

// TestSwiftRowsVerifyInGo seeds a credential exactly as Swift's Fluent model writes it and
// signs in with it, so a passkey enrolled on Swift keeps working on Go.
func TestSwiftRowsVerifyInGo(t *testing.T) {
	v := setup(t, setupOpts{})
	u := v.makeUser()
	a := passkeytest.New(rpID, origin)
	a.CredentialID = []byte{0xfb, 0xef, 0xbe, 0x01, 0x02, 0x03}
	stored := "++++AQID" // Data.base64EncodedString of the id, as Credential.id holds it
	created := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	_, err := v.pool.Exec(context.Background(), `INSERT INTO webauthn_credentials (id, tenant_id, credential_id, public_key, sign_count, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 9, $5, $5)`, uuid.New(), uuid.MustParse(u.id), stored, a.COSEKey(), created)
	require.NoError(t, err)

	r := v.do(http.MethodGet, "/v1/auth/webauthn/credentials", "", u.token)
	assert.Equal(t, `{"credentials":[{"id":"++++AQID","createdAt":"2026-03-01T12:00:00Z","lastUsedAt":"2026-03-01T12:00:00Z"}]}`, r.body)

	a.Counter = 10
	r = v.signIn(u.username, a, stored)
	require.Equal(t, http.StatusOK, r.code, r.body)
	assert.Equal(t, int64(10), v.rows(u.id)[0].signCount)

	// A null created_at lists as now, and a null updated_at is left out.
	_, err = v.pool.Exec(context.Background(), `UPDATE webauthn_credentials SET created_at = NULL, updated_at = NULL`)
	require.NoError(t, err)
	r = v.do(http.MethodGet, "/v1/auth/webauthn/credentials", "", u.token)
	assert.Regexp(t, `^\{"credentials":\[\{"id":"\+\+\+\+AQID","createdAt":"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ"\}\]\}$`, r.body)
}

// Swift: registration stores the padded standard-base64 spelling of the id, but sign-in
// looks the row up by the id exactly as the client sends it. Browsers and the iOS app
// send unpadded base64url, so for most credential ids (any length not a multiple of
// three, or bytes that encode to - or _) passkey sign-in can never find the row.
func TestSwiftWeaknessCredentialIDSpelling(t *testing.T) {
	v := setup(t, setupOpts{})
	u := v.makeUser()
	a := passkeytest.New(rpID, origin)
	a.CredentialID = make([]byte, 20) // 20 bytes, as Apple's passkeys: base64 ends in "="
	r := v.enrol(u, a)
	require.Equal(t, http.StatusOK, r.code, r.body)
	assert.Equal(t, `{"credentialID":"AAAAAAAAAAAAAAAAAAAAAAAAAAA="}`, r.body)

	r = v.signIn(u.username, a, passkeytest.B64URL(a.CredentialID))
	assert.Equal(t, http.StatusUnauthorized, r.code)
	assert.Equal(t, errorBody("credential not registered"), r.body)

	// Sending the stored spelling instead works.
	r = v.signIn(u.username, a, "AAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	assert.Equal(t, http.StatusOK, r.code, r.body)

	// The duplicate check has the same mismatch: enrolling the same credential again
	// is not caught by confirmCredentialIDNotRegisteredYet; the unique index refuses it.
	r = v.enrol(u, a)
	assert.Equal(t, http.StatusInternalServerError, r.code)
	assert.Empty(t, r.body)
	assert.Len(t, v.rows(u.id), 1)
}

// Swift: the duplicate check that does match (an id whose spellings agree) is the
// library's credentialIDAlreadyExists, also a bare 500, and it spans every account.
func TestDuplicateCredentialAcrossAccounts(t *testing.T) {
	v := setup(t, setupOpts{})
	a := passkeytest.New(rpID, origin)
	a.CredentialID = []byte("same-id-for-two-users!!!")
	first, second := v.makeUser(), v.makeUser()
	require.Equal(t, http.StatusOK, v.enrol(first, a).code)
	r := v.enrol(second, a)
	assert.Equal(t, http.StatusInternalServerError, r.code)
	assert.Empty(t, v.rows(second.id))
}

// TestWire pins the refusals' bytes and order: 503 before identity, identity before the
// body, Foundation's decode messages (swift-webauthn's own for base64url members), the
// challenge before the user lookup, one 401 for unknown user and unknown credential.
func TestWire(t *testing.T) {
	v := setup(t, setupOpts{})
	u := v.makeUser()

	t.Run("options bytes", func(t *testing.T) {
		r := v.do(http.MethodPost, "/v1/auth/webauthn/register/options", `{"username":" `+strings.ToUpper(u.username)+`\n","displayName":"Me/You"}`, u.token)
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.Equal(t, jsonCT, r.ct)
		c := challengeOf(t, r.body)
		userHandle := passkey.URLEncode([]byte(strings.ToUpper(u.id)))
		assert.Equal(t, `{"options":{"challenge":"`+c+`","user":{"id":"`+userHandle+`","name":"`+u.username+`","displayName":"Me\/You"},`+
			`"rp":{"id":"luminavault.test","name":"LuminaVault Test"},"pubKeyCredParams":[{"type":"public-key","alg":-7},`+
			`{"type":"public-key","alg":-35},{"type":"public-key","alg":-36},{"type":"public-key","alg":-65535},`+
			`{"type":"public-key","alg":-257},{"type":"public-key","alg":-258},{"type":"public-key","alg":-259},`+
			`{"type":"public-key","alg":-37},{"type":"public-key","alg":-38},{"type":"public-key","alg":-39}],`+
			`"timeout":300000,"attestation":"none"}}`, r.body)

		r = v.do(http.MethodPost, "/v1/auth/webauthn/authenticate/begin", `{"username":"anyone"}`, "")
		require.Equal(t, http.StatusOK, r.code)
		c = challengeOf(t, r.body)
		assert.Equal(t, `{"options":{"challenge":"`+c+`","timeout":60000,"rpId":"luminavault.test","userVerification":"preferred"}}`, r.body)
	})

	t.Run("decode errors", func(t *testing.T) {
		cases := []struct{ path, body, token, want string }{
			{"/v1/auth/webauthn/register/begin", `{}`, u.token, "Coding key `username` not found."},
			{"/v1/auth/webauthn/register/begin", `{"username":"","displayName":5}`, u.token, "Type mismatch for `displayName` key, expected `String` type."},
			{"/v1/auth/webauthn/register/finish", `{"username":""}`, u.token, "Coding key `credentialCreationData` not found."},
			{
				"/v1/auth/webauthn/register/finish", `{"username":"","credentialCreationData":{"id":"a","type":"public-key","rawId":"*","response":{}}}`, u.token,
				"The given data was not valid input: Failed to decode base64url encoded rawID into bytes",
			},
			{
				"/v1/auth/webauthn/register/finish", `{"username":"","credentialCreationData":{"id":"a","type":"public-key","rawId":"AA","response":{"clientDataJSON":"*","attestationObject":"AA"}}}`, u.token,
				`The given data was not valid input: Failed to decode base64url encoded string at CodingKeys(stringValue: "clientDataJSON", intValue: nil) into bytes`,
			},
			{
				"/v1/auth/webauthn/register/finish", `{"username":"","credentialCreationData":{"id":"a","type":"public-key","rawId":7}}`, u.token,
				"Type mismatch for `credentialCreationData.rawId` key, expected `String` type.",
			},
			{"/v1/auth/webauthn/authenticate/begin", `{"user":"x"}`, "", "Coding key `username` not found."},
			{
				"/v1/auth/webauthn/authenticate/finish", `{"username":"x","credential":{"id":"a","rawId":"%","response":{}}}`, "",
				`The given data was not valid input: Failed to decode base64url encoded string at CodingKeys(stringValue: "rawId", intValue: nil) into bytes`,
			},
			{
				"/v1/auth/webauthn/authenticate/finish", `{"username":"x","credential":{"id":"a","rawId":"AA","response":{"clientDataJSON":"AA","authenticatorData":"AA","signature":"AA"}}}`, "",
				"Coding key `type` not found.",
			},
			{
				"/v1/auth/webauthn/authenticate/finish", `{"username":"x","credential":{"id":"a","rawId":"AA","response":{"clientDataJSON":"AA","authenticatorData":"AA","signature":"AA","userHandle":"!"},"type":"public-key"}}`, "",
				`The given data was not valid input: Failed to decode base64url encoded string at CodingKeys(stringValue: "userHandle", intValue: nil) into bytes`,
			},
			{"/v1/auth/webauthn/authenticate/finish", `not json`, "", "The given data was not valid input: The given data was not valid JSON."},
		}
		for _, tc := range cases {
			r := v.do(http.MethodPost, tc.path, tc.body, tc.token)
			assert.Equal(t, http.StatusBadRequest, r.code, tc.body)
			assert.Equal(t, errorBody(tc.want), r.body, tc.body)
		}
	})

	t.Run("challenges", func(t *testing.T) {
		assertion := `{"username":"%s","credential":{"id":"%s","rawId":"AA","response":{"clientDataJSON":"AA","authenticatorData":"AA","signature":"AA"},"type":"public-key"}}`
		r := v.do(http.MethodPost, "/v1/auth/webauthn/authenticate/finish", fmt.Sprintf(assertion, "nobody-began", "x"), "")
		assert.Equal(t, http.StatusBadRequest, r.code)
		assert.Equal(t, errorBody("missing or expired authentication challenge"), r.body)

		for _, name := range []string{"ghost-user", u.username} {
			require.Equal(t, http.StatusOK, v.do(http.MethodPost, "/v1/auth/webauthn/authenticate/begin", beginBody(name), "").code)
			r = v.do(http.MethodPost, "/v1/auth/webauthn/authenticate/finish", fmt.Sprintf(assertion, name, "unknown-id"), "")
			assert.Equal(t, http.StatusUnauthorized, r.code, name)
			assert.Equal(t, errorBody("credential not registered"), r.body)
		}

		// A failed finish leaves the challenge in place; it expires at 300 s.
		r = v.do(http.MethodPost, "/v1/auth/webauthn/authenticate/finish", fmt.Sprintf(assertion, u.username, "unknown-id"), "")
		assert.Equal(t, http.StatusUnauthorized, r.code)
		v.clock.advance(passkey.ChallengeTTL)
		r = v.do(http.MethodPost, "/v1/auth/webauthn/authenticate/finish", fmt.Sprintf(assertion, u.username, "unknown-id"), "")
		assert.Equal(t, http.StatusBadRequest, r.code)

		r = v.do(http.MethodPost, "/v1/auth/webauthn/register/finish", `{"username":"","credentialCreationData":{"id":"a","type":"public-key","rawId":"AA","response":{"clientDataJSON":"AA","attestationObject":"AA"}}}`, u.token)
		assert.Equal(t, http.StatusBadRequest, r.code)
		assert.Equal(t, errorBody("missing or expired registration challenge"), r.body)

		// The claim is checked before the challenge.
		r = v.do(http.MethodPost, "/v1/auth/webauthn/register/finish", `{"username":"someone-else","credentialCreationData":{"id":"a","type":"public-key","rawId":"AA","response":{"clientDataJSON":"AA","attestationObject":"AA"}}}`, u.token)
		assert.Equal(t, http.StatusForbidden, r.code)

		// A registration challenge expires at 300 s too.
		require.Equal(t, http.StatusOK, v.do(http.MethodPost, "/v1/auth/webauthn/register/begin", beginBody(""), u.token).code)
		v.clock.advance(passkey.ChallengeTTL)
		r = v.do(http.MethodPost, "/v1/auth/webauthn/register/finish", `{"username":"","credentialCreationData":{"id":"a","type":"public-key","rawId":"AA","response":{"clientDataJSON":"AA","attestationObject":"AA"}}}`, u.token)
		assert.Equal(t, http.StatusBadRequest, r.code)

		// A finish that fails verification is a bare 500 and keeps the challenge.
		require.Equal(t, http.StatusOK, v.do(http.MethodPost, "/v1/auth/webauthn/register/begin", beginBody(""), u.token).code)
		r = v.do(http.MethodPost, "/v1/auth/webauthn/register/finish", `{"username":"","credentialCreationData":{"id":"a","type":"public-key","rawId":"AA","response":{"clientDataJSON":"AA","attestationObject":"AA"}}}`, u.token)
		assert.Equal(t, http.StatusInternalServerError, r.code)
		assert.Equal(t, "", r.ct)
		assert.Empty(t, r.body)
		a := passkeytest.New(rpID, origin)
		r = v.do(http.MethodPost, "/v1/auth/webauthn/register/begin", beginBody(""), u.token)
		require.Equal(t, http.StatusOK, r.code)
		body := jsonBody(t, map[string]any{"username": "", "credentialCreationData": a.Registration(challengeOf(t, r.body))})
		assert.Equal(t, http.StatusOK, v.do(http.MethodPost, "/v1/auth/webauthn/register/finish", body, u.token).code)
	})

	t.Run("management needs identity", func(t *testing.T) {
		r := v.do(http.MethodGet, "/v1/auth/webauthn/credentials", "", "")
		assert.Equal(t, http.StatusUnauthorized, r.code)
		assert.Equal(t, errorBody("missing identity"), r.body)
		r = v.do(http.MethodDelete, "/v1/auth/webauthn/credentials/x", "", "not.a.jwt")
		assert.Equal(t, errorBody("missing identity"), r.body)
	})

	t.Run("delete is scoped to the caller", func(t *testing.T) {
		owner, other := v.makeUser(), v.makeUser()
		a := passkeytest.New(rpID, origin)
		a.CredentialID = []byte("scoped-credential-id-24!")
		require.Equal(t, http.StatusOK, v.enrol(owner, a).code)
		id := credentialIDs(t, v.do(http.MethodGet, "/v1/auth/webauthn/credentials", "", owner.token).body)[0]
		assert.Equal(t, http.StatusNoContent, v.do(http.MethodDelete, "/v1/auth/webauthn/credentials/"+id, "", other.token).code)
		assert.Len(t, v.rows(owner.id), 1)
	})
}

// Hummingbird routes on the raw path and hands the parameter over undecoded, so a
// percent-encoded id names nothing, and an id holding "/" cannot be named at all.
func TestDeletePathIsRaw(t *testing.T) {
	v := setup(t, setupOpts{})
	u := v.makeUser()
	for _, id := range []string{"++8+AQID", "+/+/AQID", "%41%41"} {
		_, err := v.pool.Exec(context.Background(), `INSERT INTO webauthn_credentials (id, tenant_id, credential_id, public_key, sign_count, created_at, updated_at)
			VALUES ($1, $2, $3, '\x00', 0, now(), now())`, uuid.New(), uuid.MustParse(u.id), id)
		require.NoError(t, err)
	}
	ids := func() []string {
		got := credentialIDs(t, v.do(http.MethodGet, "/v1/auth/webauthn/credentials", "", u.token).body)
		sort.Strings(got)
		return got
	}
	assert.Equal(t, http.StatusNoContent, v.do(http.MethodDelete, "/v1/auth/webauthn/credentials/%2B%2B8%2BAQID", "", u.token).code)
	assert.Equal(t, http.StatusNoContent, v.do(http.MethodDelete, "/v1/auth/webauthn/credentials/+%2F+%2FAQID", "", u.token).code)
	assert.Equal(t, http.StatusNotFound, v.do(http.MethodDelete, "/v1/auth/webauthn/credentials/+/+/AQID", "", u.token).code)
	assert.Equal(t, []string{"%41%41", "++8+AQID", "+/+/AQID"}, ids())
	// The raw text is what matches.
	assert.Equal(t, http.StatusNoContent, v.do(http.MethodDelete, "/v1/auth/webauthn/credentials/%41%41", "", u.token).code)
	assert.Equal(t, http.StatusNoContent, v.do(http.MethodDelete, "/v1/auth/webauthn/credentials/++8+AQID", "", u.token).code)
	assert.Equal(t, []string{"+/+/AQID"}, ids())
}

// With the feature off nothing is mounted; on but without an RP id or origin, the
// ceremonies are 503 "webauthn disabled" (before identity) and management still works.
func TestEnabledAndConfigured(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		v := setup(t, setupOpts{disabled: true})
		for _, p := range []string{"/v1/auth/webauthn/authenticate/begin", "/v1/auth/webauthn/register/begin"} {
			r := v.do(http.MethodPost, p, `{"username":"x"}`, "")
			assert.Equal(t, http.StatusNotFound, r.code)
			assert.Empty(t, r.body)
		}
	})
	t.Run("unconfigured", func(t *testing.T) {
		v := setup(t, setupOpts{cfg: &passkey.Config{RPID: rpID, RPName: "x"}})
		for _, p := range []string{
			"/v1/auth/webauthn/authenticate/begin", "/v1/auth/webauthn/authenticate/options", "/v1/auth/webauthn/authenticate/finish",
			"/v1/auth/webauthn/register/begin", "/v1/auth/webauthn/register/options", "/v1/auth/webauthn/register/finish",
		} {
			r := v.do(http.MethodPost, p, `not json`, "")
			assert.Equal(t, http.StatusServiceUnavailable, r.code, p)
			assert.Equal(t, errorBody("webauthn disabled"), r.body, p)
		}
		u := v.makeUser()
		r := v.do(http.MethodGet, "/v1/auth/webauthn/credentials", "", u.token)
		assert.Equal(t, `{"credentials":[]}`, r.body)
	})
}

// Sign-in is limited per IP (webAuthnAuthenticateByIP, 20 a minute), enrolment per user
// (webAuthnEnrolByUser).
func TestRateLimits(t *testing.T) {
	v := setup(t, setupOpts{})
	send := func(path, body, token, ip string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("X-Forwarded-For", ip)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		v.srv.ServeHTTP(rec, req)
		return rec.Code
	}
	for range 20 {
		require.Equal(t, http.StatusOK, send("/v1/auth/webauthn/authenticate/begin", `{"username":"x"}`, "", "192.0.2.50"))
	}
	assert.Equal(t, http.StatusTooManyRequests, send("/v1/auth/webauthn/authenticate/options", `{"username":"x"}`, "", "192.0.2.50"))
	assert.Equal(t, http.StatusOK, send("/v1/auth/webauthn/authenticate/begin", `{"username":"x"}`, "", "192.0.2.51"))

	u := v.makeUser()
	for i := range 20 {
		require.Equal(t, http.StatusOK, send("/v1/auth/webauthn/register/begin", `{"username":""}`, u.token, fmt.Sprintf("192.0.2.%d", 100+i)))
	}
	assert.Equal(t, http.StatusTooManyRequests, send("/v1/auth/webauthn/register/begin", `{"username":""}`, u.token, "192.0.2.200"))
}
