package githubapp_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"lumina/internal/githubapp"
	"lumina/internal/githubapp/githubapptest"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func stubWithRepo(t *testing.T) *githubapptest.Server {
	t.Helper()
	s := githubapptest.New(t)
	s.InstallationRepos[7] = []int64{456}
	s.Repos[456] = githubapp.Repository{ID: 456, FullName: "octocat/notes", Private: true, DefaultBranch: "main", Size: 12}
	return s
}

// E4.6 done-when: mint an installation token against the stub and read a
// repository with it.
func TestInstallationTokenMintsCachesAndReads(t *testing.T) {
	s := stubWithRepo(t)
	c := s.Client(t)
	now := time.Now()
	c.SetClock(func() time.Time { return now })
	ctx := context.Background()

	tok, err := c.InstallationToken(ctx, 7, 456)
	require.NoError(t, err)
	mints := s.Mints()
	require.Len(t, mints, 1)
	require.Equal(t, int64(7), mints[0].Installation)
	require.Equal(t, []int64{456}, mints[0].RepositoryIDs)
	require.Equal(t, map[string]string{"contents": "write", "metadata": "read"}, mints[0].Permissions)

	repo, err := c.RepositoryByID(ctx, tok, 456)
	require.NoError(t, err)
	require.Equal(t, "octocat/notes", repo.FullName)

	again, err := c.InstallationToken(ctx, 7, 456)
	require.NoError(t, err)
	require.Equal(t, tok, again, "cached")
	require.Len(t, s.Mints(), 1)

	now = now.Add(56 * time.Minute) // under 5 minutes left
	fresh, err := c.InstallationToken(ctx, 7, 456)
	require.NoError(t, err)
	require.NotEqual(t, tok, fresh)
	require.Len(t, s.Mints(), 2)

	require.NoError(t, c.RevokeInstallationToken(ctx, 7, 456))
	require.Equal(t, []string{fresh}, s.RevokedInstallations())
	require.NoError(t, c.RevokeInstallationToken(ctx, 7, 456), "nothing cached: nothing to do")
	require.Len(t, s.RevokedInstallations(), 1)
}

func TestInstallationTokenErrors(t *testing.T) {
	s := stubWithRepo(t)
	c := s.Client(t)
	_, err := c.InstallationToken(context.Background(), 99, 456)
	require.ErrorIs(t, err, githubapp.ErrNotFound, "an uninstalled App")

	// A JWT signed by another key is refused.
	other := githubapptest.New(t)
	wrong, err := githubapp.New(githubapp.Config{
		AppID: githubapptest.AppID, ClientID: githubapptest.ClientID, ClientSecret: githubapptest.ClientSecret,
		PrivateKey: other.Key, APIBaseURL: s.URL, WebBaseURL: s.URL, HTTP: s.Server.Client(),
	})
	require.NoError(t, err)
	_, err = wrong.InstallationToken(context.Background(), 7, 456)
	require.ErrorIs(t, err, githubapp.ErrUnauthorized)
}

func TestUserTokenFlow(t *testing.T) {
	s := stubWithRepo(t)
	s.Codes["code-1"] = "ghu_user"
	s.Users["ghu_user"] = "octocat"
	s.UserInstallations["ghu_user"] = []githubapp.Installation{{ID: 7, Account: githubapp.Account{Login: "octocat", Type: "User"}, RepositorySelection: "selected"}}
	repo := s.Repos[456]
	repo.Permissions.Push = true
	s.UserRepos["ghu_user"] = map[int64][]githubapp.Repository{7: {repo}}
	c := s.Client(t)
	ctx := context.Background()

	tok, err := c.ExchangeCode(ctx, "code-1", "https://api.example/cb")
	require.NoError(t, err)
	require.Equal(t, "ghu_user", tok.Token)
	require.NotNil(t, tok.ExpiresAt)
	_, err = c.ExchangeCode(ctx, "code-1", "https://api.example/cb")
	require.ErrorIs(t, err, githubapp.ErrBadCode, "a code works once")

	u, err := c.User(ctx, tok.Token)
	require.NoError(t, err)
	require.Equal(t, "octocat", u.Login)
	inst, err := c.UserInstallations(ctx, tok.Token)
	require.NoError(t, err)
	require.Len(t, inst, 1)
	require.Equal(t, "User", inst[0].Account.Type)
	repos, more, err := c.UserInstallationRepos(ctx, tok.Token, 7, 1, 50)
	require.NoError(t, err)
	require.False(t, more)
	require.True(t, repos[0].Permissions.Push)
	_, _, err = c.UserInstallationRepos(ctx, tok.Token, 8, 1, 50)
	require.ErrorIs(t, err, githubapp.ErrNotFound)

	require.NoError(t, c.RevokeUserToken(ctx, tok.Token))
	require.True(t, s.Revoked(tok.Token))
	_, err = c.User(ctx, tok.Token)
	require.ErrorIs(t, err, githubapp.ErrUnauthorized)
	require.NoError(t, c.RevokeUserToken(ctx, tok.Token), "an unknown token counts as revoked")
}

func TestAuthorizeURL(t *testing.T) {
	s := githubapptest.New(t)
	u := s.Client(t).AuthorizeURL("st", "https://api.example/v1/github-sync/callback")
	require.True(t, strings.HasPrefix(u, s.URL+"/login/oauth/authorize?"))
	require.Contains(t, u, "client_id="+githubapptest.ClientID)
	require.Contains(t, u, "state=st")
	require.NotContains(t, u, githubapptest.ClientSecret)
}

// No error text carries a token, the client secret or the key.
func TestErrorsCarryNoSecrets(t *testing.T) {
	s := githubapptest.New(t)
	c := s.Client(t)
	ctx := context.Background()
	var errs []error
	_, err := c.ExchangeCode(ctx, "nope", "https://x/cb")
	errs = append(errs, err)
	_, err = c.User(ctx, "ghu_secret_value")
	errs = append(errs, err)
	_, err = c.RepositoryByID(ctx, "ghs_secret_value", 1)
	errs = append(errs, err)
	s.Close()
	_, err = c.InstallationToken(ctx, 1, 1)
	errs = append(errs, err)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(s.Key)}))
	for _, err := range errs {
		require.Error(t, err)
		for _, secret := range []string{"ghu_secret_value", "ghs_secret_value", githubapptest.ClientSecret, "nope", keyPEM[40:80]} {
			require.NotContains(t, err.Error(), secret)
		}
	}
}

func TestStatusErrorKinds(t *testing.T) {
	for status, want := range map[int]error{
		http.StatusNotFound: githubapp.ErrNotFound, http.StatusUnauthorized: githubapp.ErrUnauthorized,
		http.StatusForbidden: githubapp.ErrForbidden, http.StatusBadGateway: githubapp.ErrUnavailable,
	} {
		require.ErrorIs(t, &githubapp.StatusError{Status: status}, want)
	}
}

func TestParsePrivateKey(t *testing.T) {
	s := githubapptest.New(t)
	raw := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(s.Key)}))
	k, err := githubapp.ParsePrivateKey(raw)
	require.NoError(t, err)
	require.True(t, k.Equal(s.Key))
	k, err = githubapp.ParsePrivateKey(strings.ReplaceAll(strings.TrimSpace(raw), "\n", `\n`))
	require.NoError(t, err, "literal \\n newlines")
	require.True(t, k.Equal(s.Key))
	_, err = githubapp.ParsePrivateKey("-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "AAAA")
}

func TestNewRequiresEveryField(t *testing.T) {
	s := githubapptest.New(t)
	base := githubapp.Config{
		AppID: githubapptest.AppID, ClientID: githubapptest.ClientID, ClientSecret: githubapptest.ClientSecret,
		PrivateKey: s.Key, APIBaseURL: s.URL, WebBaseURL: s.URL, HTTP: http.DefaultClient,
	}
	_, err := githubapp.New(base)
	require.NoError(t, err)
	for _, change := range []func(*githubapp.Config){
		func(c *githubapp.Config) { c.APIBaseURL = "" },
		func(c *githubapp.Config) { c.WebBaseURL = "" },
		func(c *githubapp.Config) { c.HTTP = nil },
		func(c *githubapp.Config) { c.PrivateKey = nil },
		func(c *githubapp.Config) { c.AppID = 0 },
		func(c *githubapp.Config) { c.ClientSecret = "" },
	} {
		cfg := base
		change(&cfg)
		_, err := githubapp.New(cfg)
		require.Error(t, err)
	}
}
