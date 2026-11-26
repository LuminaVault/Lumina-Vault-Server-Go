package githubapp_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"lumina/internal/githubapp"
	"lumina/internal/githubapp/githubapptest"
	"net/http"
	"strconv"
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
		http.StatusTooManyRequests: githubapp.ErrRateLimited, http.StatusMovedPermanently: githubapp.ErrMoved,
		http.StatusFound: githubapp.ErrMoved, http.StatusTemporaryRedirect: githubapp.ErrMoved,
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

// A renamed or transferred repository: GitHub answers the old name with a
// 301. The client never follows it; it reads the new name by id and asks once
// more (spec: "a repository rename on the stub is followed through
// GET /repositories/{id}").
func TestRenamedRepositoryIsReadByID(t *testing.T) {
	s := stubWithRepo(t)
	c := s.Client(t)
	ctx := context.Background()
	tok, err := c.InstallationToken(ctx, 7, 456)
	require.NoError(t, err)

	b := githubapp.Branch{Name: "main"}
	b.Commit.Commit.Tree.SHA = "7ree"
	s.Lock()
	renamed := s.Repos[456]
	renamed.FullName = "hubot/journal"
	s.Repos[456] = renamed
	s.Renamed["octocat/notes"] = 456
	s.Branches["hubot/journal"] = []githubapp.Branch{b}
	s.Trees["7ree"] = githubapp.Tree{Entries: []githubapp.TreeEntry{{Path: "a.md", Mode: "100644", Type: "blob", SHA: "1", Size: 1}}}
	s.Unlock()

	stale := githubapp.RepoRef{ID: 456, FullName: "octocat/notes"}
	list, err := c.Branches(ctx, tok, stale)
	require.NoError(t, err)
	require.Len(t, list, 1)
	got, err := c.Branch(ctx, tok, stale, "main")
	require.NoError(t, err)
	require.Equal(t, "7ree", got.TreeSHA())
	tree, err := c.Tree(ctx, tok, stale, "7ree")
	require.NoError(t, err)
	require.Len(t, tree.Entries, 1)

	// Without an id there is nothing to ask: the redirect is ErrMoved.
	_, err = c.Branches(ctx, tok, githubapp.RepoRef{FullName: "octocat/notes"})
	require.ErrorIs(t, err, githubapp.ErrMoved)
	require.NotErrorIs(t, err, githubapp.ErrUnavailable)

	s.Lock()
	defer s.Unlock()
	require.Zero(t, s.Followed, "a redirect is never followed")
}

// GitHub's rate limits are ErrRateLimited, not "forbidden", and the error
// says when to come back.
func TestRateLimited(t *testing.T) {
	s := stubWithRepo(t)
	c := s.Client(t)
	now := time.Unix(1_800_000_000, 0)
	c.SetClock(func() time.Time { return now })
	ctx := context.Background()
	answer := func(status int, headers map[string]string) {
		s.Lock()
		defer s.Unlock()
		s.Intercept = func(w http.ResponseWriter, _ *http.Request) bool {
			for k, v := range headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for installation ID 7."}`))
			return true
		}
	}
	reset := now.Add(20 * time.Minute)

	t.Run("primary: 403 with no requests remaining", func(t *testing.T) {
		answer(http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)})
		_, err := c.User(ctx, "ghu_x")
		require.ErrorIs(t, err, githubapp.ErrRateLimited)
		require.NotErrorIs(t, err, githubapp.ErrForbidden)
		var se *githubapp.StatusError
		require.ErrorAs(t, err, &se)
		require.Equal(t, http.StatusForbidden, se.Status)
		require.NotNil(t, se.RateLimitRemaining)
		require.Zero(t, *se.RateLimitRemaining)
		require.True(t, se.RateLimitReset.Equal(reset))
		require.True(t, se.RetryAt(now).Equal(reset))
		require.NotContains(t, err.Error(), "installation ID", "no body text in the error")
	})

	t.Run("secondary: Retry-After, on a 403 and on a 429", func(t *testing.T) {
		for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
			answer(status, map[string]string{"Retry-After": "90", "X-RateLimit-Remaining": "4000"})
			_, err := c.User(ctx, "ghu_x")
			require.ErrorIs(t, err, githubapp.ErrRateLimited)
			var se *githubapp.StatusError
			require.ErrorAs(t, err, &se)
			require.Equal(t, 90*time.Second, se.RetryAfter)
			require.True(t, se.RetryAt(now).Equal(now.Add(90*time.Second)))
		}
		answer(http.StatusTooManyRequests, nil)
		_, err := c.User(ctx, "ghu_x")
		require.ErrorIs(t, err, githubapp.ErrRateLimited)
		var se *githubapp.StatusError
		require.ErrorAs(t, err, &se)
		require.True(t, se.RetryAt(now).IsZero(), "GitHub said nothing about when")
	})

	t.Run("a plain 403 stays forbidden", func(t *testing.T) {
		answer(http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "4999"})
		_, err := c.User(ctx, "ghu_x")
		require.ErrorIs(t, err, githubapp.ErrForbidden)
		require.NotErrorIs(t, err, githubapp.ErrRateLimited)
	})

	t.Run("the token mint and the code exchange keep the headers too", func(t *testing.T) {
		answer(http.StatusTooManyRequests, map[string]string{"Retry-After": "7"})
		_, err := c.InstallationToken(ctx, 7, 456)
		require.ErrorIs(t, err, githubapp.ErrRateLimited)
		_, err = c.ExchangeCode(ctx, "code", "https://x/cb")
		require.ErrorIs(t, err, githubapp.ErrRateLimited)
		var se *githubapp.StatusError
		require.ErrorAs(t, err, &se)
		require.Equal(t, 7*time.Second, se.RetryAfter)
	})
}

// 422 minting a token: the installation no longer covers the repository.
func TestInstallationTokenRepoAccessRemoved(t *testing.T) {
	s := stubWithRepo(t)
	c := s.Client(t)
	_, err := c.InstallationToken(context.Background(), 7, 999)
	require.ErrorIs(t, err, githubapp.ErrRepoAccessRemoved)
	require.NotErrorIs(t, err, githubapp.ErrUnavailable)
	var se *githubapp.StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusUnprocessableEntity, se.Status)
	// Elsewhere a 422 means nothing in particular.
	require.ErrorIs(t, &githubapp.StatusError{Status: http.StatusUnprocessableEntity}, githubapp.ErrUnavailable)
}

// An answer over the read limit is ErrTooLarge, not a body that "did not
// parse"; one under it that GitHub cut short says so itself.
func TestTreeTooLargeAndTruncated(t *testing.T) {
	s := stubWithRepo(t)
	c := s.Client(t)
	ctx := context.Background()
	tok, err := c.InstallationToken(ctx, 7, 456)
	require.NoError(t, err)
	repo := githubapp.RepoRef{ID: 456, FullName: "octocat/notes"}

	entries := make([]githubapp.TreeEntry, 0, 70_000)
	for i := range 70_000 { // about 9 MB of JSON
		entries = append(entries, githubapp.TreeEntry{
			Path: "notes/" + strings.Repeat("d", 40) + "/" + strconv.Itoa(i) + ".md", Mode: "100644", Type: "blob",
			SHA: strings.Repeat("a", 40), Size: 10,
		})
	}
	s.Lock()
	s.Trees["big"] = githubapp.Tree{Entries: entries}
	s.Trees["cut"] = githubapp.Tree{Entries: entries[:10], Truncated: true}
	s.Unlock()

	_, err = c.Tree(ctx, tok, repo, "big")
	require.ErrorIs(t, err, githubapp.ErrTooLarge)
	require.NotErrorIs(t, err, githubapp.ErrUnavailable)

	tree, err := c.Tree(ctx, tok, repo, "cut")
	require.NoError(t, err)
	require.True(t, tree.Truncated)
}

// E4.9 review item 9: what GitHub says about an installation now. It is
// asked as the App (a JWT, no installation token, which a suspended or
// removed installation cannot mint).
func TestInstallationStatus(t *testing.T) {
	s := stubWithRepo(t)
	c := s.Client(t)
	ctx := context.Background()
	st, err := c.InstallationStatus(ctx, 7)
	require.NoError(t, err)
	require.False(t, st.Suspended)
	require.Empty(t, s.Mints(), "no installation token was minted for the question")

	s.Lock()
	s.Suspended[7] = true
	s.Unlock()
	st, err = c.InstallationStatus(ctx, 7)
	require.NoError(t, err)
	require.True(t, st.Suspended)

	_, err = c.InstallationStatus(ctx, 8)
	require.ErrorIs(t, err, githubapp.ErrNotFound, "uninstalled, or never installed")

	s.Lock()
	s.Intercept = func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusBadGateway)
		return true
	}
	s.Unlock()
	_, err = c.InstallationStatus(ctx, 7)
	require.ErrorIs(t, err, githubapp.ErrUnavailable, "GitHub not answering is not an answer")
	require.NotErrorIs(t, err, githubapp.ErrNotFound)
}
