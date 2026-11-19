package connectedapps

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"lumina/internal/ids"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrBadState means the OAuth state is unknown, reused or expired.
	ErrBadState = errors.New("connectedapps: bad oauth state")
	// ErrNotOAuth means the app's auth mode does not match the operation.
	ErrNotOAuth = errors.New("connectedapps: wrong auth mode for app")
	// ErrBadPAT means a pasted token lacks the app's prefix.
	ErrBadPAT = errors.New("connectedapps: bad token format")
	// ErrUnknownApp means the app id is not in the registry.
	ErrUnknownApp = errors.New("connectedapps: unknown app")
	// ErrUpstream marks a failure on the remote app's side while starting a
	// flow: discovery, client registration, a transport error or metadata the
	// guard rejects. Anything Start returns without it failed locally.
	ErrUpstream = errors.New("connectedapps: upstream failure")
	// ErrNoCode means the callback carried no code: the user declined, or the
	// authorization server reported an error.
	ErrNoCode = errors.New("connectedapps: no authorization code")
)

const (
	sessionTTL    = 600 * time.Second
	refreshAhead  = 120 * time.Second
	revokeTimeout = 5 * time.Second
)

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// upstream tags err with ErrUpstream, keeping err for errors.Is.
func upstream(err error) error { return fmt.Errorf("%w: %w", ErrUpstream, err) }

type oauthSession struct {
	user      uuid.UUID
	appID     string
	verifier  string
	clientID  string
	tokenURL  string
	returnTo  *string
	startedAt time.Time
}

// sessionStore mirrors calendar's: in-memory, per process, one-shot take,
// pruned by sessionTTL.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]oauthSession
	now      func() time.Time
}

func (s *sessionStore) put(state string, sess oauthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	s.sessions[state] = sess
}

func (s *sessionStore) take(state string) (oauthSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	sess, ok := s.sessions[state]
	delete(s.sessions, state)
	return sess, ok
}

func (s *sessionStore) prune() {
	cutoff := s.now().Add(-sessionTTL)
	for k, v := range s.sessions {
		if v.startedAt.Before(cutoff) {
			delete(s.sessions, k)
		}
	}
}

// OAuth runs the OAuth 2.1 + PKCE public-client flow against first-party apps
// and keeps their tokens fresh. It never logs and never puts tokens, codes or
// verifiers in errors.
type OAuth struct {
	registry    Registry
	store       *Store
	pool        *pgxpool.Pool
	http        *http.Client
	guard       Validator
	sessions    *sessionStore
	callbackURL string
	now         func() time.Time

	lockMu sync.Mutex
	locks  map[string]*sync.Mutex
}

// NewOAuth builds the flow. Whatever httpc does with redirects, the copy kept
// here refuses them.
func NewOAuth(pool *pgxpool.Pool, reg Registry, store *Store, httpc *http.Client, guard Validator, callbackURL string) *OAuth {
	o := &OAuth{
		registry: reg, store: store, pool: pool, http: noRedirects(httpc), guard: guard,
		callbackURL: callbackURL, now: time.Now, locks: map[string]*sync.Mutex{},
	}
	o.sessions = &sessionStore{sessions: map[string]oauthSession{}, now: func() time.Time { return o.now() }}
	return o
}

// lockFor returns the per-(user, app) mutex that serialises refreshes.
func (o *OAuth) lockFor(user uuid.UUID, appID string) *sync.Mutex {
	o.lockMu.Lock()
	defer o.lockMu.Unlock()
	k := user.String() + "/" + appID
	m, ok := o.locks[k]
	if !ok {
		m = &sync.Mutex{}
		o.locks[k] = m
	}
	return m
}

func (o *OAuth) app(appID string) (App, error) {
	a, ok := o.registry.Get(appID)
	if !ok {
		return App{}, ErrUnknownApp
	}
	return a, nil
}

// Start discovers the app's authorization server, registers a client once and
// returns the URL to send the user to. A failure of the remote side matches
// ErrUpstream; any other error (database, randomness) is local.
func (o *OAuth) Start(ctx context.Context, user uuid.UUID, appID string, returnTo *string) (string, error) {
	app, err := o.app(appID)
	if err != nil {
		return "", err
	}
	if app.Auth != AuthOAuth {
		return "", ErrNotOAuth
	}
	meta, err := o.discover(ctx, app)
	if err != nil {
		return "", upstream(err)
	}
	clientID, err := o.clientID(ctx, app, meta) // tags its own upstream failures
	if err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	state := ids.Upper(uuid.New()) + "." + ids.Upper(uuid.New())

	au, err := url.Parse(meta.AuthorizationEndpoint)
	if err != nil || meta.AuthorizationEndpoint == "" {
		return "", upstream(errors.New("connectedapps: bad authorization endpoint"))
	}
	q := au.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", o.callbackURL)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("resource", app.MCPURL)
	au.RawQuery = q.Encode()
	if _, err := o.guard.Validate(ctx, au.String()); err != nil {
		return "", upstream(fmt.Errorf("connectedapps: url rejected: %w", err))
	}
	o.sessions.put(state, oauthSession{
		user: user, appID: appID, verifier: verifier, clientID: clientID,
		tokenURL: meta.TokenEndpoint, returnTo: returnTo, startedAt: o.now(),
	})
	return au.String(), nil
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
}

// tokenRequest POSTs a form to the token endpoint. status is the HTTP status
// (0 on transport failure).
func (o *OAuth) tokenRequest(ctx context.Context, tokenURL string, form url.Values) (int, tokenResp, error) {
	status, b, err := o.send(ctx, http.MethodPost, tokenURL, "application/x-www-form-urlencoded", []byte(form.Encode()))
	if err != nil {
		return 0, tokenResp{}, err
	}
	var tr tokenResp
	_ = json.Unmarshal(b, &tr)
	return status, tr, nil
}

// Finish completes the flow: it exchanges the code and stores the tokens. The
// state is taken first and works once, whatever follows. Once it is known, the
// app id and returnTo come back even with an error, so the caller can send the
// browser to the right place. An empty code (the user declined) is ErrNoCode
// and makes no request.
func (o *OAuth) Finish(ctx context.Context, state, code string) (string, *string, error) {
	sess, ok := o.sessions.take(state)
	if !ok {
		return "", nil, ErrBadState
	}
	app, err := o.app(sess.appID)
	if err != nil {
		return "", nil, err
	}
	if code == "" {
		return app.ID, sess.returnTo, ErrNoCode
	}
	return app.ID, sess.returnTo, o.exchange(ctx, sess, app, code)
}

func (o *OAuth) exchange(ctx context.Context, sess oauthSession, app App, code string) error {
	status, tr, err := o.tokenRequest(ctx, sess.tokenURL, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {o.callbackURL},
		"client_id":     {sess.clientID},
		"code_verifier": {sess.verifier},
		"resource":      {app.MCPURL},
	})
	if err != nil {
		return err
	}
	if status != http.StatusOK || tr.AccessToken == "" {
		return fmt.Errorf("connectedapps: token exchange failed (%d)", status)
	}
	acct := Account{AppID: app.ID, AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken}
	if tr.ExpiresIn > 0 {
		exp := o.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
		acct.ExpiresAt = &exp
	}
	if tr.Scope != "" {
		acct.Scope = &tr.Scope
	}
	return o.store.Save(ctx, sess.user, acct)
}

// Fresh returns a usable access token, refreshing when under 120s remain.
// Refreshes are serialised per (user, app): concurrent callers share one
// refresh because the later ones re-read the row after the lock.
func (o *OAuth) Fresh(ctx context.Context, user uuid.UUID, appID string) (string, error) {
	app, err := o.app(appID)
	if err != nil {
		return "", err
	}
	acct, err := o.store.Get(ctx, user, appID)
	if err != nil {
		return "", err
	}
	if app.Auth == AuthPAT {
		return acct.AccessToken, nil
	}
	if acct.Status != "connected" {
		return "", ErrNotConnected
	}
	if !o.needsRefresh(acct) {
		return acct.AccessToken, nil
	}
	mu := o.lockFor(user, appID)
	mu.Lock()
	defer mu.Unlock()
	if acct, err = o.store.Get(ctx, user, appID); err != nil {
		return "", err
	}
	if acct.Status != "connected" {
		return "", ErrNotConnected
	}
	if !o.needsRefresh(acct) {
		return acct.AccessToken, nil
	}
	return o.refresh(ctx, user, app, acct)
}

// Refresh forces a token refresh after the remote app rejected staleToken
// with a 401. It shares Fresh's per-(user, app) lock and failure semantics. If
// another caller already replaced the token (the stored one differs from
// staleToken), it returns that one without a second POST.
func (o *OAuth) Refresh(ctx context.Context, user uuid.UUID, appID, staleToken string) (string, error) {
	app, err := o.app(appID)
	if err != nil {
		return "", err
	}
	if app.Auth != AuthOAuth {
		return "", ErrNotOAuth
	}
	mu := o.lockFor(user, appID)
	mu.Lock()
	defer mu.Unlock()
	acct, err := o.store.Get(ctx, user, appID)
	if err != nil {
		return "", err
	}
	if acct.Status != "connected" {
		return "", ErrNotConnected
	}
	if acct.AccessToken != staleToken {
		return acct.AccessToken, nil
	}
	return o.refresh(ctx, user, app, acct)
}

func (o *OAuth) needsRefresh(a Account) bool {
	return a.ExpiresAt != nil && a.ExpiresAt.Sub(o.now()) < refreshAhead
}

func (o *OAuth) refresh(ctx context.Context, user uuid.UUID, app App, acct Account) (string, error) {
	if acct.RefreshToken == "" {
		_ = o.store.MarkNeedsReauth(ctx, user, app.ID, "no_refresh_token")
		return "", ErrNotConnected
	}
	var clientID string
	if err := o.pool.QueryRow(ctx, `SELECT client_id FROM connected_app_clients WHERE app_id = $1`, app.ID).Scan(&clientID); err != nil {
		if isNoRows(err) {
			_ = o.store.MarkNeedsReauth(ctx, user, app.ID, "no_client")
			return "", ErrNotConnected
		}
		return "", err
	}
	meta, err := o.discover(ctx, app)
	if err != nil {
		return "", err
	}
	status, tr, err := o.tokenRequest(ctx, meta.TokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {acct.RefreshToken},
		"client_id":     {clientID},
		"resource":      {app.MCPURL},
	})
	if err != nil {
		return "", err
	}
	if status >= 400 && status < 500 {
		code := sanitizeCode(tr.Error)
		_ = o.store.MarkNeedsReauth(ctx, user, app.ID, code)
		return "", ErrNotConnected
	}
	if status != http.StatusOK || tr.AccessToken == "" {
		return "", fmt.Errorf("connectedapps: token refresh failed (%d)", status)
	}
	next := Account{AppID: app.ID, AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken, Scope: acct.Scope}
	if next.RefreshToken == "" {
		next.RefreshToken = acct.RefreshToken
	}
	if tr.Scope != "" {
		next.Scope = &tr.Scope
	}
	if tr.ExpiresIn > 0 {
		exp := o.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
		next.ExpiresAt = &exp
	}
	if err := o.store.Save(ctx, user, next); err != nil {
		return "", err
	}
	return next.AccessToken, nil
}

// sanitizeCode keeps an OAuth error code short and plain before it is stored.
func sanitizeCode(c string) string {
	if c == "" || len(c) > 64 || strings.ContainsFunc(c, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.'
	}) {
		return "invalid_grant"
	}
	return c
}

// SavePAT stores a pasted personal access token for a PAT app.
func (o *OAuth) SavePAT(ctx context.Context, user uuid.UUID, appID, token string) error {
	app, err := o.app(appID)
	if err != nil {
		return err
	}
	if app.Auth != AuthPAT {
		return ErrNotOAuth
	}
	if app.PATPrefix == "" || !strings.HasPrefix(token, app.PATPrefix) || len(token) == len(app.PATPrefix) {
		return ErrBadPAT
	}
	return o.store.Save(ctx, user, Account{AppID: appID, AccessToken: token})
}

// Revoke tells the app's authorization server to drop the user's refresh
// token (RFC 7009) before a disconnect. It is a no-op without an account, a
// refresh token, a registered client or an advertised revocation endpoint,
// and it gives up after revokeTimeout. It shares the refresh lock, so it
// revokes the token a concurrent refresh left behind, not the one it replaced.
func (o *OAuth) Revoke(ctx context.Context, user uuid.UUID, appID string) error {
	app, err := o.app(appID)
	if err != nil {
		return err
	}
	if app.Auth != AuthOAuth {
		return ErrNotOAuth
	}
	ctx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	mu := o.lockFor(user, appID)
	mu.Lock()
	defer mu.Unlock()
	acct, err := o.store.Get(ctx, user, appID)
	if errors.Is(err, ErrNotConnected) {
		return nil
	}
	if err != nil {
		return err
	}
	if acct.RefreshToken == "" {
		return nil
	}
	// The cached client only: a disconnect never registers a new one.
	var clientID string
	if err := o.pool.QueryRow(ctx, `SELECT client_id FROM connected_app_clients WHERE app_id = $1`, app.ID).Scan(&clientID); err != nil {
		if isNoRows(err) {
			return nil
		}
		return err
	}
	meta, err := o.discover(ctx, app)
	if err != nil {
		return err
	}
	if meta.RevocationEndpoint == "" {
		return nil
	}
	form := url.Values{
		"token":           {acct.RefreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {clientID},
	}
	status, _, err := o.send(ctx, http.MethodPost, meta.RevocationEndpoint, "application/x-www-form-urlencoded", []byte(form.Encode()))
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("connectedapps: revocation returned %d", status)
	}
	return nil
}
