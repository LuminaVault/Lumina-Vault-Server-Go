package authservice_test

import (
	"context"
	"errors"
	"lumina/internal/authservice"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubXClient is the Swift suite's StubXAPIClient: a scripted user or an
// error, recording every access token it is handed.
type stubXClient struct {
	mu       sync.Mutex
	user     *authservice.XUser
	err      error
	observed []string
}

func (s *stubXClient) FetchMe(_ context.Context, token string) (*authservice.XUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observed = append(s.observed, token)
	if s.err != nil {
		return nil, s.err
	}
	return s.user, nil
}

// xEmail is XOAuthController.exchange's email rule.
func xEmail(u authservice.XUser) string {
	if u.Email != nil {
		return swiftstr.Lowercased(*u.Email)
	}
	return u.ID + "@x.luminavault.local"
}

// TestXOAuth ports XOAuthTests (Tests/AppTests/Auth/XOAuthTests.swift), all
// 10 cases: decoding /2/users/me, the stub client, the email fallback, and
// upsertOAuthUser(provider: "x") against the database.
func TestXOAuth(t *testing.T) {
	ctx := context.Background()

	t.Run("decodes users me envelope with email", func(t *testing.T) {
		// The Swift case's body has no email, despite its name.
		u, err := authservice.DecodeXUserResponse([]byte(`{"data":{"id":"1234567890","name":"Ada Lovelace","username":"ada","verified":true}}`))
		require.NoError(t, err)
		assert.Equal(t, "1234567890", u.ID)
		assert.Equal(t, "Ada Lovelace", u.Name)
		assert.Equal(t, "ada", u.Username)
		assert.Nil(t, u.Email)
		require.NotNil(t, u.Verified)
		assert.True(t, *u.Verified)
	})

	t.Run("decodes users me envelope with explicit email", func(t *testing.T) {
		u, err := authservice.DecodeXUserResponse([]byte(`{"data":{"id":"42","name":"Grace Hopper","username":"grace","email":"grace@navy.example","verified":false}}`))
		require.NoError(t, err)
		require.NotNil(t, u.Email)
		assert.Equal(t, "grace@navy.example", *u.Email)
		assert.False(t, *u.Verified)
	})

	t.Run("rejects missing data envelope", func(t *testing.T) {
		_, err := authservice.DecodeXUserResponse([]byte(`{ "id": "42", "name": "no-envelope" }`))
		require.Error(t, err)
		// A plain error, so the route answers 500 as Swift's DecodingError does.
		var we *wire.Error
		assert.False(t, errors.As(err, &we))
	})

	t.Run("stub XAPI client returns scripted user", func(t *testing.T) {
		stub := &stubXClient{user: &authservice.XUser{ID: "stub-user-id", Name: "Stub User", Username: "stubuser"}}
		got, err := stub.FetchMe(ctx, "doesnt-matter-stub")
		require.NoError(t, err)
		assert.Equal(t, "stub-user-id", got.ID)
		assert.Nil(t, got.Email)
		assert.Equal(t, []string{"doesnt-matter-stub"}, stub.observed)
	})

	t.Run("stub XAPI client can throw", func(t *testing.T) {
		stub := &stubXClient{err: errors.New("stub 401")}
		_, err := stub.FetchMe(ctx, "anything")
		assert.Error(t, err)
	})

	t.Run("email fallback produces placeholder for missing X", func(t *testing.T) {
		assert.Equal(t, "9876@x.luminavault.local", xEmail(authservice.XUser{ID: "9876", Name: "n", Username: "u"}))
	})

	t.Run("email fallback uses X provided email when present", func(t *testing.T) {
		e := "Mixed.Case@Example.com"
		assert.Equal(t, "mixed.case@example.com", xEmail(authservice.XUser{ID: "9876", Name: "n", Username: "u", Email: &e}))
	})

	pool := newPool(t)
	h := build(t, pool, harness{})
	identity := func(t *testing.T, provider, providerUserID string) (count int, tenant uuid.UUID, verified bool) {
		t.Helper()
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM oauth_identities WHERE provider = $1 AND provider_user_id = $2`,
			provider, providerUserID).Scan(&count))
		if count > 0 {
			require.NoError(t, pool.QueryRow(ctx, `SELECT tenant_id, email_verified FROM oauth_identities WHERE provider = $1 AND provider_user_id = $2`,
				provider, providerUserID).Scan(&tenant, &verified))
		}
		return count, tenant, verified
	}

	t.Run("upsert creates user with placeholder email", func(t *testing.T) {
		puid := "x-" + randomSuffix()
		email := puid + "@x.luminavault.local"
		u, err := h.svc.UpsertOAuthUser(ctx, "x", puid, email, false)
		require.NoError(t, err)
		assert.Equal(t, email, u.Email)
		assert.True(t, strings.HasPrefix(u.Username, "oauth-"), u.Username)

		n, tenant, verified := identity(t, "x", puid)
		assert.Equal(t, 1, n)
		assert.Equal(t, u.ID, tenant)
		assert.False(t, verified)

		var status string
		require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM hermes_profiles WHERE tenant_id = $1`, u.ID).Scan(&status))
		assert.Equal(t, "ready", status)

		// The new account is a trial with a personal vault, as register's is.
		row := h.user(t, "id = $1", u.ID)
		require.NotNil(t, row)
		assert.Equal(t, "trial", row.tier)
		assert.NotNil(t, row.tierExpires)
		assert.False(t, row.isVerified)
		var vaults int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM vaults WHERE id = $1`, u.ID).Scan(&vaults))
		assert.Equal(t, 1, vaults)
	})

	t.Run("upsert returns same user on second call", func(t *testing.T) {
		puid := "x-" + randomSuffix()
		email := puid + "@x.luminavault.local"
		first, err := h.svc.UpsertOAuthUser(ctx, "x", puid, email, false)
		require.NoError(t, err)
		second, err := h.svc.UpsertOAuthUser(ctx, "x", puid, email, false)
		require.NoError(t, err)
		assert.Equal(t, first.ID, second.ID)
		n, _, _ := identity(t, "x", puid)
		assert.Equal(t, 1, n)
	})

	t.Run("upsert links to existing user by email", func(t *testing.T) {
		email := randomEmail("x-link")
		username := randomUsername("x-link")
		_, err := h.svc.Register(ctx, email, username, password)
		require.NoError(t, err)

		puid := "x-" + randomSuffix()
		u, err := h.svc.UpsertOAuthUser(ctx, "x", puid, email, true)
		require.NoError(t, err)
		assert.Equal(t, email, u.Email)
		assert.Equal(t, username, u.Username)

		var users int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email = $1`, email).Scan(&users))
		assert.Equal(t, 1, users)
		n, tenant, _ := identity(t, "x", puid)
		assert.Equal(t, 1, n)
		assert.Equal(t, u.ID, tenant)
	})

	// Swift weakness, kept: the link ignores emailVerified, so an unverified
	// identity claiming an address signs in to that account.
	t.Run("an unverified identity still links by email", func(t *testing.T) {
		email := randomEmail("x-unverified")
		resp, err := h.svc.Register(ctx, email, randomUsername("x-unv"), password)
		require.NoError(t, err)
		u, err := h.svc.UpsertOAuthUser(ctx, "phone", "+1555"+randomSuffix(), strings.ToUpper(email), false)
		require.NoError(t, err)
		assert.Equal(t, resp.UserID.UUID, u.ID)
	})
}

// TestHTTPXClient is DefaultXAPIClient against an httptest stand-in for
// api.x.com: the request it sends, and how it maps the answers.
func TestHTTPXClient(t *testing.T) {
	ctx := context.Background()
	var got *http.Request
	status, body := http.StatusOK, `{"data":{"id":"42","name":"G","username":"grace","email":"g@x.example"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := &authservice.HTTPXClient{URL: srv.URL + "/2/users/me?user.fields=id,name,username,verified,verified_email", Client: srv.Client()}

	u, err := c.FetchMe(ctx, "tok-1")
	require.NoError(t, err)
	assert.Equal(t, "42", u.ID)
	assert.Equal(t, "Bearer tok-1", got.Header.Get("Authorization"))
	assert.Equal(t, "application/json", got.Header.Get("Accept"))
	assert.Equal(t, http.MethodGet, got.Method)
	assert.Equal(t, "user.fields=id,name,username,verified,verified_email", got.URL.RawQuery)

	status, body = http.StatusUnauthorized, `{"title":"Unauthorized"}`
	_, err = c.FetchMe(ctx, "tok-2")
	assert.Equal(t, wire.NewError(http.StatusUnauthorized, "x access_token rejected"), err)

	status, body = http.StatusOK, `{"data":{"id":"42"}}`
	_, err = c.FetchMe(ctx, "tok-3")
	var we *wire.Error
	require.Error(t, err)
	assert.False(t, errors.As(err, &we), "a missing field is a 500, not a 400")

	assert.Equal(t, authservice.XUsersMeURL, authservice.NewXClient(nil).URL)
}
