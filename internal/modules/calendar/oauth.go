package calendar

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/modules/calendar/db"
	"lumina/internal/secretbox"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Purpose says which "Connect" started a flow. Both share one Google client
// and one callback URL, so the callback reads it from the session.
type Purpose int

const (
	PurposeCalendar Purpose = iota
	PurposeGmail
)

func (p Purpose) scope() string {
	if p == PurposeGmail {
		return scopeGmailConnect
	}
	return scopeCalendarConnect
}

const (
	appCallbackBase      = "luminavault://oauth/google-calendar"
	gmailAppCallbackBase = "luminavault://oauth/google-gmail"
	sessionTTL           = 600 * time.Second
)

// Errors Start returns. Controllers turn them into their own 503 and 400.
var (
	ErrNotConfigured = errors.New("calendar oauth: not configured")
	ErrInvalidReturn = errors.New("calendar oauth: invalid returnTo")
)

type session struct {
	tenant    uuid.UUID
	startedAt time.Time
	returnTo  *string
	purpose   Purpose
}

// sessionStore is CalendarOAuthSessionStore: in-flight OAuth sessions keyed by
// the state Google echoes back. It is per process, in Go as in Swift, so
// connect and the unauthenticated callback must reach the same process
// (coupling.md). take is one-shot; sessions expire after sessionTTL.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]session
	now      func() time.Time
}

func (s *sessionStore) put(state string, sess session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	s.sessions[state] = sess
}

func (s *sessionStore) take(state string) (session, bool) {
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

// Status is GoogleCalendarOAuthService.Status.
type Status struct {
	Connected    bool
	NeedsReauth  bool
	AccountEmail *string
	LastSyncedAt *time.Time
}

// GmailStatus is GoogleCalendarOAuthService.GmailStatus.
type GmailStatus struct {
	Connected         bool
	NeedsReauth       bool
	AccountEmail      *string
	CalendarConnected bool
}

// Google holds the Google side of the module: GoogleCalendarOAuthService with
// the token store and sync service it drives. The mail module shares it,
// because Gmail rides on the same grant and the same calendar_accounts row.
type Google struct {
	q          *db.Queries
	oauth      oauthClient
	tokens     *tokenStore
	sync       *syncService
	sessions   *sessionStore
	configured bool
	webReturn  []string
	now        func() time.Time
}

// GoogleOptions are the Swift config keys the Google side reads.
type GoogleOptions struct {
	Box *secretbox.Box
	// ClientID, ClientSecret and RedirectURI are oauth.googleCalendar.*.
	// Connect answers 503 unless all three are set.
	ClientID, ClientSecret, RedirectURI string
	// WebReturnOrigins is cors.allowedOrigins, the only places ?returnTo= may name.
	WebReturnOrigins []string
	// Endpoints overrides Google's URLs (tests). Zero means DefaultEndpoints.
	Endpoints Endpoints
	// HTTP overrides the client (tests).
	HTTP *http.Client
	// Now overrides the clock (tests).
	Now func() time.Time
}

// NewGoogle builds the Google side. Swift only does this when a SecretBox
// exists; without one the /v1/calendar Google routes and /v1/mail/gmail are
// not registered at all.
func NewGoogle(pool *pgxpool.Pool, opts GoogleOptions) *Google {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	client := opts.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second} // URLSession's default request timeout
	}
	endpoints := opts.Endpoints
	if endpoints == (Endpoints{}) {
		endpoints = DefaultEndpoints
	}
	q := db.New(pool)
	o := oauthClient{clientID: opts.ClientID, clientSecret: opts.ClientSecret, redirectURI: opts.RedirectURI, endpoints: endpoints, http: client}
	tokens := &tokenStore{q: q, box: opts.Box, oauth: o, now: now, inFlight: map[uuid.UUID]*refreshCall{}}
	cal := calendarClient{base: endpoints.CalendarBase, http: client, now: now}
	return &Google{
		q:          q,
		oauth:      o,
		tokens:     tokens,
		sync:       &syncService{q: q, tokens: tokens, client: cal, now: now},
		sessions:   &sessionStore{sessions: map[string]session{}, now: now},
		configured: opts.ClientID != "" && opts.ClientSecret != "" && opts.RedirectURI != "",
		webReturn:  opts.WebReturnOrigins,
		now:        now,
	}
}

func (g *Google) account(ctx context.Context, tenant uuid.UUID) (*db.CalendarAccount, error) {
	a, err := g.q.GoogleAccount(ctx, tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // no row is an answer, not a failure
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// Status is status(tenantID:): the row is the Google grant, shared with Gmail,
// and Calendar is connected only when that grant includes the Calendar scope.
func (g *Google) Status(ctx context.Context, tenant uuid.UUID) (Status, error) {
	a, err := g.account(ctx, tenant)
	if err != nil || a == nil || !grants(&a.Scope, scopeCalendarEvents) {
		return Status{}, err
	}
	return Status{
		Connected:    a.Status == "connected",
		NeedsReauth:  a.Status == "needs_reauth",
		AccountEmail: a.AccountEmail,
		LastSyncedAt: a.LastSyncedAt,
	}, nil
}

// GmailStatus is gmailStatus(tenantID:).
func (g *Google) GmailStatus(ctx context.Context, tenant uuid.UUID) (GmailStatus, error) {
	a, err := g.account(ctx, tenant)
	if err != nil || a == nil {
		return GmailStatus{}, err
	}
	out := GmailStatus{CalendarConnected: grants(&a.Scope, scopeCalendarEvents) && a.Status == "connected"}
	if grants(&a.Scope, scopeGmailReadonly) {
		out.Connected = a.Status == "connected"
		out.NeedsReauth = a.Status == "needs_reauth"
		out.AccountEmail = a.AccountEmail
	}
	return out, nil
}

// DisconnectGmail stops reading mail. When the same grant also serves
// Calendar, Google has no per-scope revoke, so the scope is dropped locally
// and Calendar stays. Otherwise the whole grant is revoked and the row deleted.
func (g *Google) DisconnectGmail(ctx context.Context, tenant uuid.UUID) error {
	a, err := g.account(ctx, tenant)
	if err != nil || a == nil {
		return err
	}
	if grants(&a.Scope, scopeCalendarEvents) {
		return g.q.SetAccountScope(ctx, db.SetAccountScopeParams{TenantID: tenant, Scope: withoutGmail(a.Scope), Now: g.now()})
	}
	g.revokeRemote(ctx, tenant)
	return g.q.DeleteGoogleAccount(ctx, tenant)
}

// Disconnect revokes and forgets, and purges the tenant's cached Google events
// whatever state the grant was in. If the grant also serves Gmail, only the
// Calendar scope goes.
func (g *Google) Disconnect(ctx context.Context, tenant uuid.UUID) error {
	a, err := g.account(ctx, tenant)
	if err != nil {
		return err
	}
	switch {
	case a == nil:
	case grants(&a.Scope, scopeGmailReadonly):
		if err := g.q.SetAccountScopeClearSyncToken(ctx, db.SetAccountScopeClearSyncTokenParams{TenantID: tenant, Scope: withoutCalendar(a.Scope), Now: g.now()}); err != nil {
			return err
		}
	default:
		g.revokeRemote(ctx, tenant)
		if err := g.q.DeleteGoogleAccount(ctx, tenant); err != nil {
			return err
		}
	}
	return g.q.DeleteGoogleEvents(ctx, tenant)
}

// revokeRemote is best-effort. It revokes a usable access token, which may
// mean refreshing first; a grant in needs_reauth has none and Google is not called.
func (g *Google) revokeRemote(ctx context.Context, tenant uuid.UUID) {
	access, err := g.tokens.validAccessToken(ctx, tenant)
	if err != nil {
		return
	}
	if err := g.oauth.revoke(ctx, access); err != nil {
		slog.InfoContext(ctx, "calendar revoke failed", "error", err)
	}
}

// Start mints the state, stores the session and returns the consent URL.
// returnTo (nil for the iOS app) must be on an allowed web origin.
func (g *Google) Start(_ context.Context, tenant uuid.UUID, returnTo *string, purpose Purpose) (string, error) {
	if !g.configured {
		return "", ErrNotConfigured
	}
	var webReturn *string
	if returnTo != nil {
		valid, ok := validatedReturn(*returnTo, g.webReturn)
		if !ok {
			return "", ErrInvalidReturn
		}
		webReturn = &valid
	}
	state := ids.Upper(uuid.New()) + "." + ids.Upper(uuid.New())
	g.sessions.put(state, session{tenant: tenant, startedAt: g.now(), returnTo: webReturn, purpose: purpose})
	return g.oauth.authorizeURL(state, purpose.scope()), nil
}

// handleCallback handles Google's redirect and returns where to send the
// browser. It never fails: every outcome is a redirect.
func (g *Google) handleCallback(ctx context.Context, state string, code, callbackErr *string) string {
	// The session says where to send the browser, so read it first: even a
	// decline has to land a web user back on the web page.
	sess, ok := g.sessions.take(state)
	if !ok {
		reason := "session_not_found"
		return redirectTo(appCallbackBase, "error", &reason)
	}
	base := appCallbackBase
	switch {
	case sess.returnTo != nil:
		base = *sess.returnTo
	case sess.purpose == PurposeGmail:
		base = gmailAppCallbackBase
	}
	if callbackErr != nil {
		slog.InfoContext(ctx, "calendar oauth declined", "error", *callbackErr)
		return redirectTo(base, "error", callbackErr)
	}
	if code == nil {
		reason := "missing_code"
		return redirectTo(base, "error", &reason)
	}
	if err := g.connect(ctx, sess, *code); err != nil {
		slog.ErrorContext(ctx, "calendar oauth exchange failed", "error", err)
		reason := "exchange_failed"
		return redirectTo(base, "error", &reason)
	}
	return redirectTo(base, "ok", nil)
}

func (g *Google) connect(ctx context.Context, sess session, code string) error {
	exchanged, err := g.oauth.exchangeCode(ctx, code)
	if err != nil {
		return err
	}
	// Swift reads the existing row with try?, so a failed read means no row.
	var existing *string
	if a, err := g.account(ctx, sess.tenant); err == nil && a != nil {
		existing = &a.Scope
	}
	exchanged.scope = ptr(effectiveScope(exchanged.scope, existing, sess.purpose.scope()))
	var email *string
	if exchanged.idToken != nil {
		email = emailFromIDToken(*exchanged.idToken)
	}
	if err := g.tokens.storeInitialTokens(ctx, sess.tenant, exchanged, email); err != nil {
		return err
	}
	// Best-effort initial sync; a failure doesn't block the connect, and sync
	// itself skips a grant without the Calendar scope.
	if _, err := g.sync.sync(ctx, sess.tenant); err != nil {
		slog.WarnContext(ctx, "calendar initial sync failed", "tenant", sess.tenant, "error", err)
	}
	return nil
}

// effectiveScope is what to store when Google's token response omits `scope`
// (it normally lists every grant): what was there plus what this flow asked for.
func effectiveScope(returned, existing *string, requested string) string {
	if returned != nil && *returned != "" {
		return *returned
	}
	joined := requested
	if existing != nil {
		joined = *existing + " " + requested
	}
	seen := map[string]bool{}
	var out []string
	for f := range strings.SplitSeq(joined, " ") {
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

// emailFromIDToken reads the `email` claim without verifying the signature:
// the token came straight from Google over TLS, and it is only displayed.
func emailFromIDToken(token string) *string {
	segments := strings.FieldsFunc(token, func(r rune) bool { return r == '.' })
	if len(segments) < 2 {
		return nil
	}
	b64 := strings.NewReplacer("-", "+", "_", "/").Replace(segments[1])
	for len(b64)%4 != 0 {
		b64 += "="
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(data, &claims); err != nil {
		return nil
	}
	return stringField(claims, "email")
}

func ptr[T any](v T) *T { return &v }

// ErrNotConnected and ErrNeedsReauth are CalendarTokenStore.Error.notConnected
// and .needsReauth, for callers outside the package (the mail_inbox_recent tool).
var (
	ErrNotConnected = errNotConnected
	ErrNeedsReauth  = errNeedsReauth
)

// GrantsGmail is GoogleCalendarOAuthClient.grants(grantedScope, gmail.readonly):
// whether the stored Google grant includes Gmail read access.
func (g *Google) GrantsGmail(ctx context.Context, tenant uuid.UUID) bool {
	a, err := g.account(ctx, tenant)
	return err == nil && a != nil && grants(&a.Scope, scopeGmailReadonly)
}

// AccessToken is CalendarTokenStore.validAccessToken, refreshing when needed.
func (g *Google) AccessToken(ctx context.Context, tenant uuid.UUID) (string, error) {
	return g.tokens.validAccessToken(ctx, tenant)
}
