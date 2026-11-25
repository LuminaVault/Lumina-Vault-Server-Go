// Package githubapptest is a stub GitHub for tests (spec D17): one httptest
// server that plays both api.github.com and github.com for the calls
// githubapp makes. It checks the App JWT against its own key, the client
// secret on the OAuth endpoints, and the token on every other call, and it
// records what it was asked so tests can prove what never happened.
package githubapptest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"lumina/internal/githubapp"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Fixed credentials. They are test values, and the leak tests look for them.
const (
	AppID        = int64(4242)
	ClientID     = "Iv1.stubclientid"
	ClientSecret = "stub-client-secret-0123456789abcdef" //nolint:gosec // a test stub's own value
)

// Mint is one installation token request.
type Mint struct {
	Installation  int64
	RepositoryIDs []int64
	Permissions   map[string]string
	Token         string
}

// Server is the stub. Set its maps before use; every field is guarded by
// Lock/Unlock when a test changes it while requests run.
type Server struct {
	*httptest.Server
	Key *rsa.PrivateKey
	// TokenTTL is how long a minted installation token lives (default 1h).
	TokenTTL time.Duration
	Now      func() time.Time

	mu sync.Mutex
	// Codes maps an authorization code to the user token it exchanges for.
	Codes map[string]string
	// Users maps a user token to its GitHub login.
	Users map[string]string
	// UserInstallations maps a user token to the installations it can see.
	UserInstallations map[string][]githubapp.Installation
	// UserRepos maps a user token and installation id to the repositories the
	// user can see there.
	UserRepos map[string]map[int64][]githubapp.Repository
	// InstallationRepos maps an installation id to the repository ids the App
	// is installed on.
	InstallationRepos map[int64][]int64
	// Repos is every repository by id.
	Repos map[int64]githubapp.Repository
	// Branches maps a repository full name to its branches.
	Branches map[string][]githubapp.Branch
	// Trees maps a commit sha to its recursive tree.
	Trees map[string]githubapp.Tree
	// Renamed maps a repository's old full name to its id. The stub answers
	// the old name as GitHub does, 301 to /repositories/{id}/..., and counts
	// in Followed any request that went there: the client must not follow.
	Renamed  map[string]int64
	Followed int
	// Intercept, when set, sees every request first; true means it answered.
	Intercept func(w http.ResponseWriter, r *http.Request) bool

	Exchanges                 []string
	Minted                    []Mint
	RevokedUserTokens         []string
	RevokedInstallationTokens []string
	installTokens             map[string]Mint
}

// New starts a stub with a fresh 2048-bit key. It stops with the test.
func New(t testing.TB) *Server {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Key: key, TokenTTL: time.Hour, Now: time.Now,
		Codes: map[string]string{}, Users: map[string]string{},
		UserInstallations: map[string][]githubapp.Installation{},
		UserRepos:         map[string]map[int64][]githubapp.Repository{},
		InstallationRepos: map[int64][]int64{}, Repos: map[int64]githubapp.Repository{},
		Branches: map[string][]githubapp.Branch{}, Trees: map[string]githubapp.Tree{},
		Renamed:       map[string]int64{},
		installTokens: map[string]Mint{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", s.exchange)
	mux.HandleFunc("DELETE /applications/{clientID}/token", s.revokeUser)
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", s.mint)
	mux.HandleFunc("DELETE /installation/token", s.revokeInstallation)
	mux.HandleFunc("GET /user", s.user)
	mux.HandleFunc("GET /user/installations", s.installations)
	mux.HandleFunc("GET /user/installations/{id}/repositories", s.installationRepos)
	mux.HandleFunc("GET /repositories/{id}", s.repository)
	mux.HandleFunc("GET /repos/{owner}/{name}/branches", s.branches)
	mux.HandleFunc("GET /repos/{owner}/{name}/branches/{branch...}", s.branch)
	mux.HandleFunc("GET /repos/{owner}/{name}/git/trees/{sha}", s.tree)
	mux.HandleFunc("GET /repositories/{id}/{rest...}", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.Followed++
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	})
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		intercept := s.Intercept
		s.mu.Unlock()
		if intercept != nil && intercept(w, r) {
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// Client is a githubapp.Client pointed at the stub for both base URLs.
func (s *Server) Client(t testing.TB) *githubapp.Client {
	t.Helper()
	c, err := githubapp.New(githubapp.Config{
		AppID: AppID, ClientID: ClientID, ClientSecret: ClientSecret, PrivateKey: s.Key,
		APIBaseURL: s.URL, WebBaseURL: s.URL, HTTP: s.Server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (s *Server) Lock()   { s.mu.Lock() }
func (s *Server) Unlock() { s.mu.Unlock() }

// ExchangeCount is how many code exchanges reached the stub.
func (s *Server) ExchangeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Exchanges)
}

// Revoked reports whether a user token was revoked.
func (s *Server) Revoked(userToken string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.RevokedUserTokens, userToken)
}

// Mints returns a copy of the installation token requests.
func (s *Server) Mints() []Mint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.Minted)
}

// RevokedInstallations returns a copy of the revoked installation tokens.
func (s *Server) RevokedInstallations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.RevokedInstallationTokens)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (s *Server) exchange(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.URL.Query().Get("code") != "" || r.URL.Query().Get("client_secret") != "" {
		http.Error(w, "secrets belong in the body", http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("client_id") != ClientID || r.PostForm.Get("client_secret") != ClientSecret {
		writeJSON(w, http.StatusOK, map[string]string{"error": "incorrect_client_credentials"})
		return
	}
	s.mu.Lock()
	code := r.PostForm.Get("code")
	s.Exchanges = append(s.Exchanges, code)
	token, ok := s.Codes[code]
	delete(s.Codes, code)
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]string{"error": "bad_verification_code"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": token, "token_type": "bearer", "expires_in": 28800})
}

func (s *Server) revokeUser(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok || id != ClientID || secret != ClientSecret || r.PathValue("clientID") != ClientID {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	s.mu.Lock()
	_, known := s.Users[body.AccessToken]
	s.RevokedUserTokens = append(s.RevokedUserTokens, body.AccessToken)
	delete(s.Users, body.AccessToken)
	s.mu.Unlock()
	if !known {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// appJWT checks the App JWT: RS256 under the stub's key, iss = AppID. The
// caller holds the lock, so a test may swap Key.
func (s *Server) appJWT(r *http.Request) bool {
	tok, err := jwt.ParseWithClaims(bearer(r), &jwt.RegisteredClaims{}, func(*jwt.Token) (any, error) {
		return &s.Key.PublicKey, nil
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(strconv.FormatInt(AppID, 10)), jwt.WithTimeFunc(s.Now))
	if err != nil || !tok.Valid {
		return false
	}
	claims := tok.Claims.(*jwt.RegisteredClaims)
	return claims.ExpiresAt != nil && claims.IssuedAt != nil && claims.ExpiresAt.Sub(claims.IssuedAt.Time) <= 10*time.Minute
}

func (s *Server) mint(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.appJWT(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	inst, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	repos, ok := s.InstallationRepos[inst]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	for _, id := range body.RepositoryIDs {
		if !slices.Contains(repos, id) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
	}
	raw := make([]byte, 30)
	_, _ = rand.Read(raw)
	m := Mint{
		Installation: inst, RepositoryIDs: body.RepositoryIDs, Permissions: body.Permissions,
		Token: "ghs_" + base64.RawURLEncoding.EncodeToString(raw),
	}
	s.Minted = append(s.Minted, m)
	s.installTokens[m.Token] = m
	writeJSON(w, http.StatusCreated, map[string]any{"token": m.Token, "expires_at": s.Now().Add(s.TokenTTL).UTC().Format(time.RFC3339)})
}

func (s *Server) revokeInstallation(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok := bearer(r)
	if _, ok := s.installTokens[tok]; !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	delete(s.installTokens, tok)
	s.RevokedInstallationTokens = append(s.RevokedInstallationTokens, tok)
	w.WriteHeader(http.StatusNoContent)
}

// userLogin is the login of a user token, or "".
func (s *Server) userLogin(r *http.Request) (string, string) {
	tok := bearer(r)
	return tok, s.Users[tok]
}

func (s *Server) user(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, login := s.userLogin(r)
	if login == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"login": login})
}

func (s *Server) installations(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, login := s.userLogin(r)
	if login == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	list := s.UserInstallations[tok]
	if list == nil {
		list = []githubapp.Installation{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(list), "installations": list})
}

func (s *Server) installationRepos(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, login := s.userLogin(r)
	if login == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	inst, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if !slices.ContainsFunc(s.UserInstallations[tok], func(i githubapp.Installation) bool { return i.ID == inst }) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	all := s.UserRepos[tok][inst]
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if per <= 0 {
		per = 30
	}
	if page <= 0 {
		page = 1
	}
	start := min((page-1)*per, len(all))
	end := min(start+per, len(all))
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(all), "repositories": all[start:end]})
}

// readable reports whether the request's token may read repo: an
// installation token minted for it, or a user token that lists it.
func (s *Server) readable(r *http.Request, repo githubapp.Repository) bool {
	tok := bearer(r)
	if m, ok := s.installTokens[tok]; ok {
		return slices.Contains(m.RepositoryIDs, repo.ID)
	}
	for _, repos := range s.UserRepos[tok] {
		if slices.ContainsFunc(repos, func(x githubapp.Repository) bool { return x.ID == repo.ID }) {
			return true
		}
	}
	return false
}

// moved answers a request for a renamed repository's old name with GitHub's
// 301. The caller holds the lock.
func (s *Server) moved(w http.ResponseWriter, r *http.Request) bool {
	old := r.PathValue("owner") + "/" + r.PathValue("name")
	id, ok := s.Renamed[old]
	if !ok {
		return false
	}
	rest := strings.TrimPrefix(r.URL.Path, "/repos/"+old)
	w.Header().Set("Location", s.URL+"/repositories/"+strconv.FormatInt(id, 10)+rest)
	w.WriteHeader(http.StatusMovedPermanently)
	return true
}

func (s *Server) byName(r *http.Request) (githubapp.Repository, bool) {
	full := r.PathValue("owner") + "/" + r.PathValue("name")
	for _, repo := range s.Repos {
		if repo.FullName == full {
			return repo, s.readable(r, repo)
		}
	}
	return githubapp.Repository{}, false
}

func (s *Server) repository(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	repo, ok := s.Repos[id]
	if !ok || !s.readable(r, repo) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, repo)
}

func (s *Server) branches(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.moved(w, r) {
		return
	}
	repo, ok := s.byName(r)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	list := s.Branches[repo.FullName]
	if list == nil {
		list = []githubapp.Branch{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) branch(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.moved(w, r) {
		return
	}
	repo, ok := s.byName(r)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	for _, b := range s.Branches[repo.FullName] {
		if b.Name == r.PathValue("branch") {
			writeJSON(w, http.StatusOK, b)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

func (s *Server) tree(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.moved(w, r) {
		return
	}
	if _, ok := s.byName(r); !ok || r.URL.Query().Get("recursive") != "1" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	tree, ok := s.Trees[r.PathValue("sha")]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, tree)
}
