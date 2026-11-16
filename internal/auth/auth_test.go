package auth

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/config"
	"lumina/internal/ids"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

const (
	secretA = "golden-secret-golden-secret-0000"
	secretB = "second-secret-second-secret-1111"
)

type fakeUsers map[uuid.UUID]bool

func (f fakeUsers) Exists(_ context.Context, id uuid.UUID) (bool, error) { return f[id], nil }

func mint(t *testing.T, kid, secret string, claims Claims, alg jwt.SigningMethod) string {
	t.Helper()
	tok := jwt.NewWithClaims(alg, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	var key any = []byte(secret)
	if alg == jwt.SigningMethodNone {
		key = jwt.UnsafeAllowNoneSignatureType
	}
	s, err := tok.SignedString(key)
	require.NoError(t, err)
	return s
}

func session(user uuid.UUID) Claims {
	return Claims{RegisteredClaims: jwt.RegisteredClaims{
		Subject:   ids.Upper(user),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		ID:        ids.Upper(uuid.New()),
	}}
}

// serve runs one GET through Middleware and returns the status code.
func serve(t *testing.T, keys Keyset, users UserLoader, token string) (int, uuid.UUID) {
	t.Helper()
	e := echo.New()
	var seen uuid.UUID
	e.GET("/p", func(c *echo.Context) error {
		seen = UserID(c)
		return c.NoContent(http.StatusOK)
	}, Middleware(keys, users))
	req := httptest.NewRequest(http.MethodGet, "/p", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Code, seen
}

func TestGoldenTokenVerifies(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/golden/jwt.json")
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("golden fixture not captured yet (Phase 0 Task 0.3)")
	}
	require.NoError(t, err)
	var g struct {
		HMACSecrets string          `json:"hmacSecrets"`
		Token       string          `json:"token"`
		Claims      json.RawMessage `json:"claims"`
	}
	require.NoError(t, json.Unmarshal(raw, &g))
	keys, err := LoadKeys(config.Config{JWTHMACSecrets: g.HMACSecrets})
	require.NoError(t, err)
	var want struct {
		Sub string  `json:"sub"`
		Iat float64 `json:"iat"`
	}
	require.NoError(t, json.Unmarshal(g.Claims, &want))
	// The captured token expires an hour after capture; verify it at capture time.
	capturedAt := time.Unix(int64(want.Iat), 0)
	claims, err := Verify(keys, "Bearer "+g.Token, jwt.WithTimeFunc(func() time.Time { return capturedAt }))
	require.NoError(t, err)
	require.Equal(t, want.Sub, claims.Subject)
}

func TestValidSessionPasses(t *testing.T) {
	user := uuid.New()
	keys := NewKeyset(Key{Kid: "a", Secret: []byte(secretA)})
	code, seen := serve(t, keys, fakeUsers{user: true}, mint(t, "a", secretA, session(user), jwt.SigningMethodHS256))
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, user, seen)
}

// A token signed with the active secret but naming a kid no key has is refused;
// the same token with no kid passes (post-archive fix).
func TestTokenNamingAnUnknownKidRejected(t *testing.T) {
	user := uuid.New()
	keys := NewKeyset(Key{Kid: "a", Secret: []byte(secretA)})
	code, _ := serve(t, keys, fakeUsers{user: true}, mint(t, "nope", secretA, session(user), jwt.SigningMethodHS256))
	require.Equal(t, http.StatusUnauthorized, code)
	code, seen := serve(t, keys, fakeUsers{user: true}, mint(t, "", secretA, session(user), jwt.SigningMethodHS256))
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, user, seen)
}

func TestScopedTokenRejected(t *testing.T) {
	user := uuid.New()
	keys := NewKeyset(Key{Kid: "a", Secret: []byte(secretA)})
	c := session(user)
	c.SCP = "audio"
	code, _ := serve(t, keys, fakeUsers{user: true}, mint(t, "a", secretA, c, jwt.SigningMethodHS256))
	require.Equal(t, http.StatusUnauthorized, code)
}

func TestMissingUserRejected(t *testing.T) {
	keys := NewKeyset(Key{Kid: "a", Secret: []byte(secretA)})
	code, _ := serve(t, keys, fakeUsers{}, mint(t, "a", secretA, session(uuid.New()), jwt.SigningMethodHS256))
	require.Equal(t, http.StatusUnauthorized, code)
}

func TestRetiredKidStillVerifies(t *testing.T) {
	user := uuid.New()
	keys, err := LoadKeys(config.Config{JWTHMACSecrets: "new:" + secretB + ", old:" + secretA})
	require.NoError(t, err)
	code, _ := serve(t, keys, fakeUsers{user: true}, mint(t, "old", secretA, session(user), jwt.SigningMethodHS256))
	require.Equal(t, http.StatusOK, code)
}

func TestNoKidUsesActiveKey(t *testing.T) {
	user := uuid.New()
	keys, _ := LoadKeys(config.Config{JWTHMACSecrets: "new:" + secretB + ",old:" + secretA})
	code, _ := serve(t, keys, fakeUsers{user: true}, mint(t, "", secretB, session(user), jwt.SigningMethodHS256))
	require.Equal(t, http.StatusOK, code)
	code, _ = serve(t, keys, fakeUsers{user: true}, mint(t, "", secretA, session(user), jwt.SigningMethodHS256))
	require.Equal(t, http.StatusUnauthorized, code)
}

func TestNoneAlgExpiredAndMissingRejected(t *testing.T) {
	user := uuid.New()
	keys := NewKeyset(Key{Kid: "a", Secret: []byte(secretA)})
	users := fakeUsers{user: true}
	code, _ := serve(t, keys, users, mint(t, "a", "", session(user), jwt.SigningMethodNone))
	require.Equal(t, http.StatusUnauthorized, code)
	expired := session(user)
	expired.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	code, _ = serve(t, keys, users, mint(t, "a", secretA, expired, jwt.SigningMethodHS256))
	require.Equal(t, http.StatusUnauthorized, code)
	code, _ = serve(t, keys, users, "")
	require.Equal(t, http.StatusUnauthorized, code)
}

func TestLowercaseSubjectAccepted(t *testing.T) {
	// Swift's UUID(uuidString:) parses either case; so must Go.
	user := uuid.New()
	keys := NewKeyset(Key{Kid: "a", Secret: []byte(secretA)})
	c := session(user)
	c.Subject = strings.ToLower(c.Subject)
	code, _ := serve(t, keys, fakeUsers{user: true}, mint(t, "a", secretA, c, jwt.SigningMethodHS256))
	require.Equal(t, http.StatusOK, code)
}

func TestNonCanonicalSubjectsRejected(t *testing.T) {
	// Swift's UUID(uuidString:) takes only the 36-character form.
	user := uuid.New()
	keys := NewKeyset(Key{Kid: "a", Secret: []byte(secretA)})
	for _, sub := range []string{
		strings.ReplaceAll(user.String(), "-", ""),
		"{" + user.String() + "}",
		"urn:uuid:" + user.String(),
	} {
		c := session(user)
		c.Subject = sub
		code, _ := serve(t, keys, fakeUsers{user: true}, mint(t, "a", secretA, c, jwt.SigningMethodHS256))
		require.Equal(t, http.StatusUnauthorized, code, sub)
	}
}

func TestLoadKeysErrors(t *testing.T) {
	for csv, want := range map[string]string{
		"k:short":                        "minimum 32",
		"nocolon":                        "expected kid:secret",
		":" + secretA:                    "empty kid",
		"a:" + secretA + ",a:" + secretB: "duplicate kid",
	} {
		_, err := LoadKeys(config.Config{JWTHMACSecrets: csv})
		require.ErrorContains(t, err, want, csv)
	}
	_, err := LoadKeys(config.Config{})
	require.Error(t, err)
}

// A token with no kid verifies with the active key; a known kid with its own key;
// an unknown kid is refused (post-archive fix). Swift's JWTKeyCollection.getSigner
// fell back to the default signer for an unknown kid.
func TestUnknownKidIsRefused(t *testing.T) {
	ks := Keyset{keys: []Key{{Kid: "active", Secret: []byte("a")}, {Kid: "old", Secret: []byte("b")}}}
	for kid, want := range map[string]string{"": "active", "active": "active", "old": "old"} {
		key, ok := ks.lookup(kid)
		if !ok || key.Kid != want {
			t.Fatalf("lookup(%q) = %q, %v; want %q", kid, key.Kid, ok, want)
		}
	}
	if _, ok := ks.lookup("nope"); ok {
		t.Fatal("an unknown kid must verify nothing")
	}
	if _, ok := (Keyset{}).lookup("nope"); ok {
		t.Fatal("empty keyset must verify nothing")
	}
}
