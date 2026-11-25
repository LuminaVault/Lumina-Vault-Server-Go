// Package githubapp is LuminaVault's client for its GitHub App (spec
// 2026-11-19-e4-github-sync D1, D3, D4, D5): the App JWT, installation tokens
// and their in-memory cache, the user sign-in used only to list a user's
// installations and repositories, and the few REST calls sync needs.
//
// Only this package and the GitHub sync packages may import it. No token,
// code, client secret or key ever appears in an error, a log line or a value
// this package returns other than the token methods themselves; errors carry
// a status and a fixed text, never a response body.
package githubapp

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// APIVersion is the REST API version every request names.
const APIVersion = "2026-03-10"

const (
	jwtLifetime = 9 * time.Minute
	// jwtBackdate covers a clock that runs a little ahead of GitHub's.
	jwtBackdate = 60 * time.Second
	// refreshAhead: a cached installation token is replaced this long before
	// it expires (D1).
	refreshAhead = 5 * time.Minute
	maxBody      = 4 << 20
)

var (
	// ErrNotFound is a 404: the installation, repository or branch does not
	// exist or this token cannot see it.
	ErrNotFound = errors.New("githubapp: not found")
	// ErrUnauthorized is a 401: the token or code is no longer valid.
	ErrUnauthorized = errors.New("githubapp: unauthorized")
	// ErrForbidden is a 403, including a suspended installation.
	ErrForbidden = errors.New("githubapp: forbidden")
	// ErrUnavailable is everything else GitHub or the network did: a 5xx, a
	// timeout, a body that does not parse, a redirect.
	ErrUnavailable = errors.New("githubapp: github unavailable")
	// ErrBadCode is a code exchange GitHub refused (expired, used, wrong
	// redirect URI).
	ErrBadCode = errors.New("githubapp: authorization code refused")
)

// Config is what New needs. Every field is required: a test that forgets a
// base URL must fail rather than reach github.com.
type Config struct {
	AppID        int64
	ClientID     string
	ClientSecret string
	PrivateKey   *rsa.PrivateKey
	// APIBaseURL is https://api.github.com in production.
	APIBaseURL string
	// WebBaseURL is https://github.com: the OAuth authorize and token
	// endpoints, and the links shown to users.
	WebBaseURL string
	// HTTP is the client every request goes through. The copy kept here
	// refuses redirects.
	HTTP *http.Client
}

// Client talks to GitHub as the App. It is safe for concurrent use.
type Client struct {
	appID        int64
	clientID     string
	clientSecret string
	key          *rsa.PrivateKey
	api          string
	web          string
	http         *http.Client
	now          func() time.Time

	mu     sync.Mutex
	tokens map[tokenKey]cachedToken
}

type tokenKey struct{ installation, repo int64 }

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// New checks cfg and builds a Client.
func New(cfg Config) (*Client, error) {
	switch {
	case cfg.AppID <= 0:
		return nil, errors.New("githubapp: missing app id")
	case cfg.ClientID == "" || cfg.ClientSecret == "":
		return nil, errors.New("githubapp: missing client id or secret")
	case cfg.PrivateKey == nil:
		return nil, errors.New("githubapp: missing private key")
	case cfg.HTTP == nil:
		return nil, errors.New("githubapp: missing http client")
	}
	api, err := baseURL(cfg.APIBaseURL)
	if err != nil {
		return nil, err
	}
	web, err := baseURL(cfg.WebBaseURL)
	if err != nil {
		return nil, err
	}
	httpc := *cfg.HTTP
	httpc.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	return &Client{
		appID: cfg.AppID, clientID: cfg.ClientID, clientSecret: cfg.ClientSecret, key: cfg.PrivateKey,
		api: api, web: web, http: &httpc, now: time.Now, tokens: map[tokenKey]cachedToken{},
	}, nil
}

var errRedirect = errors.New("githubapp: redirects are not followed")

func baseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || raw == "" || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("githubapp: missing or invalid base url")
	}
	return strings.TrimRight(raw, "/"), nil
}

// SetClock replaces the clock, for tests of the token cache.
func (c *Client) SetClock(now func() time.Time) { c.now = now }

// ClientID is the App's OAuth client id, which is public.
func (c *Client) ClientID() string { return c.clientID }

// WebBaseURL is the GitHub web origin, for links.
func (c *Client) WebBaseURL() string { return c.web }

// AuthorizeURL is the GitHub sign-in page for the App's user authorization.
func (c *Client) AuthorizeURL(state, redirectURI string) string {
	q := url.Values{"client_id": {c.clientID}, "state": {state}, "redirect_uri": {redirectURI}}
	return c.web + "/login/oauth/authorize?" + q.Encode()
}

// appJWT is the App's own credential: RS256, iss = App id, 9 minutes.
func (c *Client) appJWT() (string, error) {
	now := c.now()
	claims := jwt.RegisteredClaims{
		Issuer:    strconv.FormatInt(c.appID, 10),
		IssuedAt:  jwt.NewNumericDate(now.Add(-jwtBackdate)),
		ExpiresAt: jwt.NewNumericDate(now.Add(jwtLifetime)),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(c.key)
	if err != nil {
		return "", errors.New("githubapp: could not sign the app jwt")
	}
	return signed, nil
}

// InstallationToken returns a token for one repository of one installation,
// with contents:write and metadata:read only. It is cached in memory until 5
// minutes before it expires and is never written anywhere.
func (c *Client) InstallationToken(ctx context.Context, installation, repo int64) (string, error) {
	key := tokenKey{installation, repo}
	c.mu.Lock()
	if t, ok := c.tokens[key]; ok && c.now().Add(refreshAhead).Before(t.expiresAt) {
		c.mu.Unlock()
		return t.token, nil
	}
	c.mu.Unlock()

	signed, err := c.appJWT()
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{
		"repository_ids": []int64{repo},
		"permissions":    map[string]string{"contents": "write", "metadata": "read"},
	})
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := "/app/installations/" + strconv.FormatInt(installation, 10) + "/access_tokens"
	if err := c.do(ctx, http.MethodPost, c.api+path, "Bearer "+signed, body, http.StatusCreated, &out); err != nil {
		return "", err
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return "", fmt.Errorf("%w: token response incomplete", ErrUnavailable)
	}
	c.mu.Lock()
	c.tokens[key] = cachedToken{token: out.Token, expiresAt: out.ExpiresAt}
	c.mu.Unlock()
	return out.Token, nil
}

// RevokeInstallationToken drops the cached token for (installation, repo)
// and asks GitHub to revoke it. Without a cached token it does nothing.
func (c *Client) RevokeInstallationToken(ctx context.Context, installation, repo int64) error {
	key := tokenKey{installation, repo}
	c.mu.Lock()
	t, ok := c.tokens[key]
	delete(c.tokens, key)
	c.mu.Unlock()
	if !ok || !c.now().Before(t.expiresAt) {
		return nil
	}
	return c.do(ctx, http.MethodDelete, c.api+"/installation/token", "Bearer "+t.token, nil, http.StatusNoContent, nil)
}

// UserToken is a GitHub user's token from the sign-in. It is held in memory
// for the repository picker only (D3, D4).
type UserToken struct {
	Token     string
	ExpiresAt *time.Time
}

// ExchangeCode turns the sign-in's authorization code into a user token. The
// code and the client secret go in the form body, never in a URL.
func (c *Client) ExchangeCode(ctx context.Context, code, redirectURI string) (UserToken, error) {
	form := url.Values{
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.web+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return UserToken{}, fmt.Errorf("%w: bad request", ErrUnavailable)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	status, raw, err := c.send(req)
	if err != nil {
		return UserToken{}, err
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	if status != http.StatusOK || json.Unmarshal(raw, &out) != nil {
		return UserToken{}, statusError(status)
	}
	// GitHub answers a refused code with 200 and an "error" field.
	if out.Error != "" || out.AccessToken == "" {
		return UserToken{}, ErrBadCode
	}
	t := UserToken{Token: out.AccessToken}
	if out.ExpiresIn > 0 {
		at := c.now().Add(time.Duration(out.ExpiresIn) * time.Second)
		t.ExpiresAt = &at
	}
	return t, nil
}

// RevokeUserToken deletes a user token (DELETE /applications/{client_id}/token,
// basic auth with the client id and secret). A token GitHub no longer knows
// (404) counts as revoked.
func (c *Client) RevokeUserToken(ctx context.Context, token string) error {
	body, _ := json.Marshal(map[string]string{"access_token": token})
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.api+"/applications/"+url.PathEscape(c.clientID)+"/token", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: bad request", ErrUnavailable)
	}
	req.SetBasicAuth(c.clientID, c.clientSecret)
	c.headers(req, "")
	req.Header.Set("Content-Type", "application/json")
	status, _, err := c.send(req)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent || status == http.StatusNotFound {
		return nil
	}
	return statusError(status)
}

// Account is the user or organization an installation belongs to.
type Account struct {
	Login string `json:"login"`
	Type  string `json:"type"` // User | Organization
}

// Installation is one installation of the App the signed-in user can access.
type Installation struct {
	ID                  int64   `json:"id"`
	Account             Account `json:"account"`
	RepositorySelection string  `json:"repository_selection"`
}

// Repository is the part of GitHub's repository object sync reads.
type Repository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	// Size is in KiB, as GitHub reports it.
	Size        int64 `json:"size"`
	Permissions struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

// User is the signed-in GitHub user.
type User struct {
	Login string `json:"login"`
}

// User answers GET /user with a user token.
func (c *Client) User(ctx context.Context, userToken string) (User, error) {
	var u User
	if err := c.get(ctx, "/user", "Bearer "+userToken, &u); err != nil {
		return User{}, err
	}
	return u, nil
}

// UserInstallations lists the App's installations this user can access
// (GET /user/installations). That answer, not a redirect parameter, is what
// binds a Lumina user to an installation (D3).
func (c *Client) UserInstallations(ctx context.Context, userToken string) ([]Installation, error) {
	var all []Installation
	for page := 1; page <= 10; page++ {
		var out struct {
			TotalCount    int            `json:"total_count"`
			Installations []Installation `json:"installations"`
		}
		if err := c.get(ctx, "/user/installations?per_page=100&page="+strconv.Itoa(page), "Bearer "+userToken, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Installations...)
		if len(out.Installations) < 100 || len(all) >= out.TotalCount {
			break
		}
	}
	return all, nil
}

// UserInstallationRepos lists one page of the repositories both the App and
// this user can access in an installation (D4). more reports another page.
func (c *Client) UserInstallationRepos(ctx context.Context, userToken string, installation int64, page, perPage int) (repos []Repository, more bool, err error) {
	var out struct {
		TotalCount   int          `json:"total_count"`
		Repositories []Repository `json:"repositories"`
	}
	path := "/user/installations/" + strconv.FormatInt(installation, 10) + "/repositories?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page)
	if err := c.get(ctx, path, "Bearer "+userToken, &out); err != nil {
		return nil, false, err
	}
	return out.Repositories, page*perPage < out.TotalCount, nil
}

// Branch is a branch name and its tip.
type Branch struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

// Branches lists the first 100 branches of a repository with the given token.
// An empty repository has none.
func (c *Client) Branches(ctx context.Context, token, fullName string) ([]Branch, error) {
	var out []Branch
	if err := c.get(ctx, "/repos/"+repoPath(fullName)+"/branches?per_page=100", "Bearer "+token, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Branch reads one branch; a branch that does not exist is ErrNotFound.
func (c *Client) Branch(ctx context.Context, token, fullName, branch string) (Branch, error) {
	var out Branch
	if err := c.get(ctx, "/repos/"+repoPath(fullName)+"/branches/"+segments(branch), "Bearer "+token, &out); err != nil {
		return Branch{}, err
	}
	return out, nil
}

// RepositoryByID reads a repository by its id (GET /repositories/{id}), which
// survives a rename.
func (c *Client) RepositoryByID(ctx context.Context, token string, id int64) (Repository, error) {
	var out Repository
	if err := c.get(ctx, "/repositories/"+strconv.FormatInt(id, 10), "Bearer "+token, &out); err != nil {
		return Repository{}, err
	}
	return out, nil
}

// TreeEntry is one entry of a recursive git tree.
type TreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"` // blob | tree | commit
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

// Tree is a recursive tree listing. Truncated means GitHub cut it short.
type Tree struct {
	Entries   []TreeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

// Tree lists every entry under a commit (GET .../git/trees/{sha}?recursive=1).
func (c *Client) Tree(ctx context.Context, token, fullName, commitSHA string) (Tree, error) {
	var out Tree
	if err := c.get(ctx, "/repos/"+repoPath(fullName)+"/git/trees/"+url.PathEscape(commitSHA)+"?recursive=1", "Bearer "+token, &out); err != nil {
		return Tree{}, err
	}
	return out, nil
}

// repoPath escapes owner/name for a URL path.
func repoPath(fullName string) string {
	owner, name, _ := strings.Cut(fullName, "/")
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// segments escapes each segment of a branch name, keeping its slashes.
func segments(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func (c *Client) get(ctx context.Context, path, authorization string, out any) error {
	return c.do(ctx, http.MethodGet, c.api+path, authorization, nil, http.StatusOK, out)
}

func (c *Client) headers(req *http.Request, authorization string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	req.Header.Set("User-Agent", "LuminaVault")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
}

// do sends one REST request and decodes a want-status answer into out.
func (c *Client) do(ctx context.Context, method, rawURL, authorization string, body []byte, want int, out any) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, r)
	if err != nil {
		return fmt.Errorf("%w: bad request", ErrUnavailable)
	}
	c.headers(req, authorization)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	status, raw, err := c.send(req)
	if err != nil {
		return err
	}
	if status != want {
		return statusError(status)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: response did not parse", ErrUnavailable)
	}
	return nil
}

// send runs req. A transport error becomes ErrUnavailable with the context's
// error kept for errors.Is; the URL (which never holds a secret) is dropped
// anyway, since url.Error prints it.
func (c *Client) send(req *http.Request) (int, []byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return 0, nil, fmt.Errorf("%w: %w", ErrUnavailable, ctxErr)
		}
		return 0, nil, fmt.Errorf("%w: request failed", ErrUnavailable)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: body read failed", ErrUnavailable)
	}
	return resp.StatusCode, raw, nil
}

// StatusError is a non-success answer; its text is the status only.
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return "githubapp: github answered " + strconv.Itoa(e.Status) }

func (e *StatusError) Unwrap() error {
	switch e.Status {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	}
	return ErrUnavailable
}

func statusError(status int) error { return &StatusError{Status: status} }
