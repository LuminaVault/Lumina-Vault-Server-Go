// Package githubsync serves /v1/vault/github-sync and the GitHub sign-in
// callback: a vault owner connects one GitHub repository and branch to a
// vault (spec 2026-11-19-e4-github-sync, E4.7 server side, D3, D4, D13, D14).
// The work lives in lumina/internal/githubsync and lumina/internal/githubapp.
//
// Binding (D3, the #181 class): the server never reads installation_id from a
// redirect. The unauthenticated callback only parks the code; the signed-in
// client completes with the same account and vault that started the flow,
// and only then is the code exchanged. Which installations a Lumina user may
// use is whatever GitHub lists for that user's own token. Every managing route
// reads the vault's owner fresh; members may read the status only.
//
// No response, redirect or log line carries a token, a code, a completion
// handle or a picker session id other than to the client it belongs to.
//
// Every error body of these routes carries a stable code (D14), also
// the ones that come from outside this package (auth, vault access, the rate
// limit, a body that does not decode).
package githubsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/githubapp"
	"lumina/internal/githubsync"
	"lumina/internal/httpserver"
	"lumina/internal/modules/calendar"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultgit"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

const (
	basePath     = "/v1/vault/github-sync"
	callbackPath = "/v1/github-sync/callback"
	// appCallback is where the callback sends the iOS app.
	appCallback = "luminavault://github-sync"
	// webIntegrationsPath is the web app's page for this feature, where the
	// callback sends a browser whose sign-in it no longer knows.
	webIntegrationsPath = "/settings/integrations"
	// A state names where its sign-in started, so the callback can send even
	// an expired one back to the right place. The prefix chooses between two
	// fixed destinations and nothing else.
	stateWeb = "w."
	stateApp = "a."

	// maxFlowsPerUser and maxFlows bound the started sign-ins held in memory.
	maxFlowsPerUser = 5
	maxFlows        = 10_000

	stateTTL      = 10 * time.Minute
	completionTTL = 60 * time.Second
	pickerTTL     = 10 * time.Minute
	pageSize      = 50
	// maxRepoPages bounds the search for one repository in a user's list
	// (100 per page).
	maxRepoPages  = 50
	githubTimeout = 30 * time.Second
	revokeTimeout = 5 * time.Second
	sampleLimit   = 20
)

// Options are the module's dependencies.
type Options struct {
	Client *githubapp.Client
	Store  *githubsync.Store
	Access *vaultaccess.Service
	Paths  vaultpaths.Paths
	// CallbackURL is GITHUB_SYNC_CALLBACK_URL, the redirect_uri of the sign-in.
	CallbackURL string
	// Slug is the App's slug, for the install link.
	Slug string
	// MaxRepoBytes and MaxFiles are D11's limits.
	MaxRepoBytes int64
	MaxFiles     int
	// MaxFileSize is the history limit a remote file must fit to be
	// materialized (D8). Zero is vaultgit.DefaultMaxFileSize.
	MaxFileSize int64
	// WebReturnOrigins is the returnTo allowlist (CORS_ALLOWED_ORIGINS).
	WebReturnOrigins []string
	// Limits is the process's rate-limit store. Nil gives the module its own.
	Limits *ratelimit.Store
	Log    *slog.Logger
}

type Module struct {
	client       *githubapp.Client
	store        *githubsync.Store
	access       *vaultaccess.Service
	paths        vaultpaths.Paths
	callbackURL  string
	slug         string
	maxRepoBytes int64
	maxFiles     int
	maxFileSize  int64
	webReturn    []string
	limits       *ratelimit.Store
	log          *slog.Logger
	now          func() time.Time

	flows   *onceStore[flow]
	parked  *onceStore[parked]
	pickers *pickers
}

func New(opts Options) *Module {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	maxFile := opts.MaxFileSize
	if maxFile <= 0 {
		maxFile = vaultgit.DefaultMaxFileSize
	}
	limits := opts.Limits
	if limits == nil {
		limits = ratelimit.NewStore()
	}
	m := &Module{
		limits: limits,
		client: opts.Client, store: opts.Store, access: opts.Access, paths: opts.Paths,
		callbackURL: opts.CallbackURL, slug: opts.Slug, maxRepoBytes: opts.MaxRepoBytes, maxFiles: opts.MaxFiles,
		maxFileSize: maxFile, webReturn: opts.WebReturnOrigins, log: log, now: time.Now,
	}
	clock := func() time.Time { return m.now() }
	m.flows = newOnceStore[flow](stateTTL, clock, maxFlowsPerUser, maxFlows)
	// A parked code exists only for a state that was just taken, so the
	// flows' bounds already bound these.
	m.parked = newOnceStore[parked](completionTTL, clock, maxFlowsPerUser, maxFlows)
	m.pickers = newPickers(pickerTTL, clock, m.revokeUserToken)
	return m
}

// PendingSignIns is how many started sign-ins are held, for tests of the bound.
func (m *Module) PendingSignIns() int { return m.flows.len() }

// SetClock replaces the clock of the stores, for tests of expiry.
func (m *Module) SetClock(now func() time.Time) { m.now = now }

// Register mounts the routes. Every signed-in route is rate-limited per user,
// after authMW so the key is the account; the status read has its own, looser
// policy because clients poll it, and the two routes that can cost dozens of
// GitHub calls have a tighter one on top. coded is outermost, so the 401 and
// the 429 carry a code like every other error here.
func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	limit := func(p ratelimit.Policy) echo.MiddlewareFunc {
		return ratelimit.Middleware(m.limits, p, ratelimit.UserOrIPKey)
	}
	status := []echo.MiddlewareFunc{m.coded, authMW, limit(ratelimit.GitHubSyncStatusByUser)}
	manage := []echo.MiddlewareFunc{m.coded, authMW, limit(ratelimit.GitHubSyncByUser)}
	heavy := append(slices.Clone(manage), limit(ratelimit.GitHubSyncHeavyByUser))
	r.GET(basePath, m.status, status...)
	r.POST(basePath+"/connect", m.connect, manage...)
	r.POST(basePath+"/complete", m.complete, manage...)
	r.GET(basePath+"/installations", m.installations, manage...)
	r.GET(basePath+"/repositories", m.repositories, manage...)
	r.GET(basePath+"/branches", m.branches, heavy...)
	r.PUT(basePath+"/connection", m.setConnection, heavy...)
	r.DELETE(basePath+"/connection", m.disconnect, manage...)
	r.GET(callbackPath, m.callback, m.coded, ratelimit.Middleware(m.limits, ratelimit.GitHubSyncCallbackByIP, ratelimit.IPOnlyKey))
}

// MARK: errors

// apiError is D14's error body: wire.Error's envelope plus a stable code, and
// for remote_not_empty the summary beside it.
type apiError struct {
	status  int
	code    string
	message string
	summary *githubsync.Summary
	// retryAfter, in seconds, is sent as Retry-After when positive.
	retryAfter int
}

func (e *apiError) Error() string   { return e.message }
func (e *apiError) StatusCode() int { return e.status }

func (e *apiError) WriteResponse(c *echo.Context) error {
	type body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	out := struct {
		Error   body                `json:"error"`
		Summary *githubsync.Summary `json:"summary,omitempty"`
	}{Error: body{Code: e.code, Message: e.message}, Summary: e.summary}
	if e.retryAfter > 0 {
		c.Response().Header().Set("Retry-After", strconv.Itoa(e.retryAfter))
	}
	return wire.JSON(c, e.status, out)
}

func newErr(status int, code, message string) *apiError {
	return &apiError{status: status, code: code, message: message}
}

var (
	errOwnerRequired     = newErr(http.StatusForbidden, "owner_required", "Only the vault owner can manage GitHub sync.")
	errSessionExpired    = newErr(http.StatusBadRequest, "session_expired", "This GitHub sign-in expired. Connect GitHub again.")
	errWrongAccount      = newErr(http.StatusForbidden, "wrong_account", "This GitHub sign-in was started from another LuminaVault account or vault. Connect GitHub again from here.")
	errRepoInUse         = newErr(http.StatusConflict, "repo_in_use", "Another vault already syncs with this repository.")
	errRepoTooLarge      = newErr(http.StatusRequestEntityTooLarge, "repo_too_large", "This repository is larger than LuminaVault can sync.")
	errRepoNotAccessible = newErr(http.StatusNotFound, "repo_not_accessible", "That repository is not available to you through LuminaVault on GitHub.")
	errInvalidBranch     = newErr(http.StatusBadRequest, "invalid_branch", "That is not a valid branch name.")
	errInvalidReturn     = newErr(http.StatusBadRequest, "invalid_return_to", "invalid_return_to")
	errAlreadyConnected  = newErr(http.StatusConflict, "already_connected", "This vault is already connected to GitHub. Disconnect it first.")
	errNotConnected      = newErr(http.StatusNotFound, "not_connected", "This vault is not connected to GitHub.")
	errGitHubUnavailable = newErr(http.StatusBadGateway, "github_unavailable", "GitHub could not be reached. Try again later.")
	errRepoArchived      = newErr(http.StatusConflict, "repo_archived", "That repository is archived on GitHub, so nothing can be pushed to it. Unarchive it or pick another.")
	errBadRequest        = newErr(http.StatusBadRequest, "invalid_request", "invalid request")
)

// errGitHubRateLimited is GitHub's rate limit on the App or the user's token:
// 503 with Retry-After when GitHub said when to come back.
func errGitHubRateLimited(retryAfter int) *apiError {
	e := newErr(http.StatusServiceUnavailable, "github_rate_limited", "GitHub is limiting requests right now. Try again in a few minutes.")
	e.retryAfter = retryAfter
	return e
}

// codeFor is the code of an error that came from outside this package.
func codeFor(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "unauthenticated"
	case status == http.StatusForbidden:
		return "forbidden"
	case status == http.StatusNotFound:
		return "not_found"
	case status == http.StatusTooManyRequests:
		return "rate_limited"
	case status >= http.StatusInternalServerError:
		return "internal_error"
	}
	return "invalid_request"
}

// coded gives every error of a route D14's body. An error this package made
// already has its code; anything else (auth, vault access, the rate limit,
// wire's decoding) keeps its status and message and gets the code of its
// status. An error with no status is a 500: logged here, never shown.
func (m *Module) coded(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		err := next(c)
		if err == nil {
			return nil
		}
		var own *apiError
		if errors.As(err, &own) {
			return err
		}
		var we *wire.Error
		if errors.As(err, &we) {
			return newErr(we.Status, codeFor(we.Status), we.Message)
		}
		status := echo.StatusCode(err)
		if status == 0 {
			status = http.StatusInternalServerError
		}
		if status >= http.StatusInternalServerError {
			m.log.ErrorContext(c.Request().Context(), "github sync: request failed", "error", err, "path", c.Path())
			return newErr(status, codeFor(status), "Something went wrong. Try again later.")
		}
		return newErr(status, codeFor(status), http.StatusText(status))
	}
}

func errRemoteNotEmpty(s githubsync.Summary) *apiError {
	e := newErr(http.StatusConflict, "remote_not_empty", "This branch already has files. Confirm to merge them with the vault.")
	e.summary = &s
	return e
}

// appOps are the calls made as the App (its JWT or an installation token).
// A 401 there is the server's own credential failing (a bad key, a skewed
// clock), not the user's sign-in expiring.
var appOps = map[string]bool{"installation_token": true, "repository": true, "branch": true, "tree": true}

// githubErr maps a GitHub failure to D14's codes. The error's own text is
// never shown or logged beyond its kind.
func (m *Module) githubErr(ctx context.Context, op string, err error) error {
	switch {
	case errors.Is(err, githubapp.ErrRateLimited):
		retry := 0
		var se *githubapp.StatusError
		if errors.As(err, &se) {
			if at := se.RetryAt(m.now()); !at.IsZero() {
				retry = max(1, int(at.Sub(m.now()).Round(time.Second)/time.Second))
			}
		}
		m.log.WarnContext(ctx, "github sync: github rate limit", "op", op, "kind", kind(err), "retry_after_s", retry)
		return errGitHubRateLimited(retry)
	case errors.Is(err, githubapp.ErrNotFound), errors.Is(err, githubapp.ErrForbidden), errors.Is(err, githubapp.ErrRepoAccessRemoved):
		// ErrRepoAccessRemoved: the installation no longer covers the repository.
		return errRepoNotAccessible
	case errors.Is(err, githubapp.ErrUnauthorized) && !appOps[op]:
		return errSessionExpired
	case errors.Is(err, githubapp.ErrTooLarge) && op == "tree":
		// A listing too big to read is a repository too big to plan.
		return errRepoTooLarge
	}
	m.log.WarnContext(ctx, "github sync: github call failed", "op", op, "kind", kind(err))
	return errGitHubUnavailable
}

func kind(err error) string {
	var se *githubapp.StatusError
	switch {
	case errors.Is(err, githubapp.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, githubapp.ErrTooLarge):
		return "too_large"
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return "timeout"
	case errors.As(err, &se):
		return "status_" + strconv.Itoa(se.Status)
	}
	return "failed"
}

// MARK: access

func userOf(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, wire.ErrUnauthenticated
	}
	return id, nil
}

// owner resolves the vault (X-Vault-ID, or the personal vault) and requires
// the caller to be its owner, read fresh from the database.
func (m *Module) owner(c *echo.Context) (user, vault uuid.UUID, err error) {
	user, err = userOf(c)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	owner, err := m.store.VaultOwner(c.Request().Context(), a.VaultID)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if owner != user {
		return uuid.Nil, uuid.Nil, errOwnerRequired
	}
	return user, a.VaultID, nil
}

func (m *Module) session(c *echo.Context, user, vault uuid.UUID, id string) (string, picker, error) {
	if id == "" {
		return "", picker{}, errSessionExpired
	}
	s, ok := m.pickers.get(id, user, vault)
	if !ok {
		return "", picker{}, errSessionExpired
	}
	return id, s, nil
}

func (m *Module) revokeUserToken(token string) {
	ctx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
	defer cancel()
	if err := m.client.RevokeUserToken(ctx, token); err != nil {
		m.log.WarnContext(ctx, "github sync: user token revoke failed", "kind", kind(err))
	}
}

func githubCtx(c *echo.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request().Context(), githubTimeout)
}

// MARK: links

func (m *Module) installURL() string {
	return m.client.WebBaseURL() + "/apps/" + m.slug + "/installations/new"
}

// manageURL is the installation's settings page on GitHub.
func (m *Module) manageURL(accountType, login string, installation int64) string {
	id := strconv.FormatInt(installation, 10)
	if accountType == "Organization" {
		return m.client.WebBaseURL() + "/organizations/" + login + "/settings/installations/" + id
	}
	return m.client.WebBaseURL() + "/settings/installations/" + id
}

// MARK: GET /v1/vault/github-sync

type statusDTO struct {
	// Available says the server has the GitHub App configured. It is always
	// true in an answer: a server without the App does not mount these routes
	// at all, and the client reads that 404 as "not available". The field
	// stays so one decoder serves both.
	Available  bool           `json:"available"`
	CanManage  bool           `json:"canManage"`
	Connection *connectionDTO `json:"connection"`
}

type progressDTO struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

type errorDTO struct {
	Code     string    `json:"code"`
	At       wire.Time `json:"at"`
	Retrying bool      `json:"retrying"`
}

type conflictDTO struct {
	Path         string    `json:"path"`
	ConflictPath string    `json:"conflictPath"`
	At           wire.Time `json:"at"`
}

type heldDTO struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type connectionDTO struct {
	State            string        `json:"state"`
	AccountLogin     string        `json:"accountLogin"`
	AccountType      string        `json:"accountType"`
	RepoFullName     string        `json:"repoFullName"`
	RepoURL          string        `json:"repoUrl"`
	RepoPrivate      bool          `json:"repoPrivate"`
	Branch           string        `json:"branch"`
	ConnectedAt      wire.Time     `json:"connectedAt"`
	ConnectedByLogin string        `json:"connectedByLogin"`
	LastPushAt       *wire.Time    `json:"lastPushAt"`
	LastPullAt       *wire.Time    `json:"lastPullAt"`
	PendingPush      bool          `json:"pendingPush"`
	Syncing          bool          `json:"syncing"`
	Progress         *progressDTO  `json:"progress"`
	Error            *errorDTO     `json:"error"`
	Conflicts        []conflictDTO `json:"conflicts"`
	HeldCount        int           `json:"heldCount"`
	Held             []heldDTO     `json:"held"`
	ManageURL        string        `json:"manageUrl"`
}

// status answers any member of the vault; nothing in it is secret.
func (m *Module) status(c *echo.Context) error {
	user, err := userOf(c)
	if err != nil {
		return err
	}
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	owner, err := m.store.VaultOwner(ctx, a.VaultID)
	if err != nil {
		return err
	}
	out, err := m.statusOf(ctx, a.VaultID, owner == user)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) statusOf(ctx context.Context, vault uuid.UUID, canManage bool) (statusDTO, error) {
	out := statusDTO{Available: true, CanManage: canManage}
	conn, err := m.store.Get(ctx, vault)
	if err != nil || conn == nil {
		return out, err
	}
	dto := &connectionDTO{
		State: conn.State, AccountLogin: conn.AccountLogin, AccountType: conn.AccountType,
		RepoFullName: conn.RepoFullName, RepoURL: m.client.WebBaseURL() + "/" + conn.RepoFullName,
		RepoPrivate: conn.RepoPrivate, Branch: conn.Branch,
		ConnectedAt: wire.Time{Time: conn.CreatedAt}, ConnectedByLogin: conn.ConnectedGitHubLogin,
		LastPushAt: wireTime(conn.LastPushAt), LastPullAt: wireTime(conn.LastPullAt),
		PendingPush: m.pendingPush(vault, conn.RemoteSHA),
		Conflicts:   []conflictDTO{}, Held: []heldDTO{}, HeldCount: conn.Report.HeldCount,
		ManageURL: m.manageURL(conn.AccountType, conn.AccountLogin, conn.InstallationID),
	}
	if conn.ErrorCode != nil {
		e := &errorDTO{Code: *conn.ErrorCode, Retrying: conn.State == githubsync.StateActive}
		if conn.ErrorAt != nil {
			e.At = wire.Time{Time: *conn.ErrorAt}
		}
		dto.Error = e
	}
	for _, cf := range conn.Report.Conflicts[:min(len(conn.Report.Conflicts), sampleLimit)] {
		dto.Conflicts = append(dto.Conflicts, conflictDTO{Path: cf.Path, ConflictPath: cf.ConflictPath, At: wire.Time{Time: cf.At}})
	}
	for _, h := range conn.Report.Held[:min(len(conn.Report.Held), sampleLimit)] {
		dto.Held = append(dto.Held, heldDTO{Path: h.Path, Reason: string(h.Reason)})
	}
	out.Connection = dto
	return out, nil
}

// pendingPush: the vault's history has a commit GitHub has not seen.
func (m *Module) pendingPush(vault uuid.UUID, remote *string) bool {
	repo, err := vaultgit.Open(m.paths.Raw(vault))
	if err != nil {
		return false
	}
	head, err := repo.Head()
	if err != nil || head == "" {
		return false
	}
	return remote == nil || *remote != head
}

func wireTime(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}

// MARK: POST /v1/vault/github-sync/connect

type connectRequest struct {
	ReturnTo *string `json:"returnTo,omitempty"`
}

type connectResponse struct {
	AuthorizeURL string `json:"authorizeUrl"`
}

// connect starts the sign-in (D3 step 1): a single-use state bound to the
// caller and the vault, and the GitHub URL to open.
func (m *Module) connect(c *echo.Context) error {
	user, vault, err := m.owner(c)
	if err != nil {
		return err
	}
	raw, err := wire.ReadBody(c)
	if err != nil {
		return err
	}
	req := &connectRequest{}
	if len(bytes.TrimSpace(raw)) > 0 { // iOS sends no body
		if req, err = wire.Decode[connectRequest](raw); err != nil {
			return err
		}
	}
	var returnTo *string
	if req.ReturnTo != nil {
		valid, ok := calendar.ValidReturn(*req.ReturnTo, m.webReturn)
		if !ok {
			return errInvalidReturn
		}
		returnTo = &valid
	}
	state, err := randomHandle()
	if err != nil {
		return err
	}
	if returnTo != nil {
		state = stateWeb + state
	} else {
		state = stateApp + state
	}
	m.flows.put(state, user.String(), flow{user: user, vault: vault, returnTo: returnTo})
	return wire.JSON(c, http.StatusOK, connectResponse{AuthorizeURL: m.client.AuthorizeURL(state, m.callbackURL)})
}

// MARK: GET /v1/github-sync/callback

// callback is unauthenticated, so it stores nothing but the parked code (D3
// step 2): it takes the state once, parks the code under a fresh completion
// handle and sends the browser back. installation_id and setup_action are
// never read.
//
// A state it does not know (expired, used, forged, or started on a process
// that has restarted) has no flow to say where the browser came from. One
// that names the app goes to the app's scheme; any other goes to the web
// app's integrations page, where a browser can show the error. The app's
// scheme is the last resort, when no web origin is configured.
func (m *Module) callback(c *echo.Context) error {
	state, _ := wire.QueryParam(c, "state")
	f, ok := m.flows.take(state)
	if state == "" || !ok {
		return c.Redirect(http.StatusFound, m.unknownFlowRedirect(state))
	}
	code, _ := wire.QueryParam(c, "code")
	if code == "" { // the user declined
		return c.Redirect(http.StatusFound, redirect(f.returnTo, m.webReturn, "error", ""))
	}
	handle, err := randomHandle()
	if err != nil {
		return c.Redirect(http.StatusFound, redirect(f.returnTo, m.webReturn, "error", ""))
	}
	m.parked.put(handle, f.user.String(), parked{flow: f, code: code})
	return c.Redirect(http.StatusFound, redirect(f.returnTo, m.webReturn, "authorized", handle))
}

// unknownFlowRedirect is where a callback with no known flow goes.
func (m *Module) unknownFlowRedirect(state string) string {
	const q = "?status=error"
	if strings.HasPrefix(state, stateApp) {
		return appCallback + q
	}
	if page, ok := webErrorPage(m.webReturn); ok {
		return page + q
	}
	return appCallback + q
}

// webErrorPage is the integrations page of the first allowed web origin, an
// https one when there is one (the list may start with a development
// origin). It passes the same check as a client's returnTo.
func webErrorPage(allowed []string) (string, bool) {
	var fallback string
	for _, origin := range allowed {
		page, ok := calendar.ValidReturn(strings.TrimRight(strings.TrimSpace(origin), "/")+webIntegrationsPath, allowed)
		if !ok {
			continue
		}
		if strings.HasPrefix(strings.ToLower(page), "https://") {
			return page, true
		}
		if fallback == "" {
			fallback = page
		}
	}
	return fallback, fallback != ""
}

// redirect is the validated returnTo (checked again here) or the iOS deep
// link, with status= and completion= when there is a handle.
func redirect(returnTo *string, allowed []string, status, handle string) string {
	q := "status=" + status
	if handle != "" {
		q += "&completion=" + handle // base64url: needs no escaping
	}
	if returnTo != nil {
		if valid, ok := calendar.ValidReturn(*returnTo, allowed); ok {
			sep := "?"
			if strings.Contains(valid, "?") {
				sep = "&"
			}
			return valid + sep + q
		}
	}
	return appCallback + "?" + q
}

// MARK: POST /v1/vault/github-sync/complete

type completeRequest struct {
	Completion string `json:"completion"`
}

type installationDTO struct {
	ID                  int64  `json:"id"`
	AccountLogin        string `json:"accountLogin"`
	AccountType         string `json:"accountType"`
	RepositorySelection string `json:"repositorySelection"`
}

type completeResponse struct {
	Session       string            `json:"session"`
	ExpiresAt     wire.Time         `json:"expiresAt"`
	GitHubLogin   string            `json:"githubLogin"`
	Installations []installationDTO `json:"installations"`
	InstallURL    string            `json:"installUrl"`
}

// complete is D3 steps 3 and 4. The handle is spent on arrival. A flow
// started by another account or for another vault is refused before the code
// is exchanged, so GitHub never mints that user token here.
func (m *Module) complete(c *echo.Context) error {
	user, err := userOf(c)
	if err != nil {
		return err
	}
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	req, err := wire.Bind[completeRequest](c)
	if err != nil {
		return err
	}
	if req.Completion == "" {
		return errSessionExpired
	}
	p, ok := m.parked.take(req.Completion)
	if !ok {
		return errSessionExpired
	}
	ctx := context.WithoutCancel(c.Request().Context())
	if p.flow.user != user || p.flow.vault != a.VaultID {
		m.log.WarnContext(ctx, "github sync: completion from another account or vault refused")
		return errWrongAccount
	}
	if owner, err := m.store.VaultOwner(ctx, a.VaultID); err != nil {
		return err
	} else if owner != user {
		return errOwnerRequired
	}
	ctx, cancel := context.WithTimeout(ctx, githubTimeout)
	defer cancel()
	tok, err := m.client.ExchangeCode(ctx, p.code, m.callbackURL)
	if errors.Is(err, githubapp.ErrBadCode) {
		return errSessionExpired
	}
	if err != nil {
		return m.githubErr(ctx, "exchange", err)
	}
	ghUser, err := m.client.User(ctx, tok.Token)
	if err != nil {
		m.revokeUserToken(tok.Token)
		return m.githubErr(ctx, "user", err)
	}
	inst, err := m.client.UserInstallations(ctx, tok.Token)
	if err != nil {
		m.revokeUserToken(tok.Token)
		return m.githubErr(ctx, "installations", err)
	}
	id, expires, err := m.pickers.put(picker{user: user, vault: a.VaultID, token: tok.Token, login: ghUser.Login})
	if err != nil {
		m.revokeUserToken(tok.Token)
		return err
	}
	return wire.JSON(c, http.StatusOK, completeResponse{
		Session: id, ExpiresAt: wire.Time{Time: expires}, GitHubLogin: ghUser.Login,
		Installations: installationDTOs(inst), InstallURL: m.installURL(),
	})
}

func installationDTOs(list []githubapp.Installation) []installationDTO {
	out := make([]installationDTO, 0, len(list))
	for _, i := range list {
		out = append(out, installationDTO{ID: i.ID, AccountLogin: i.Account.Login, AccountType: i.Account.Type, RepositorySelection: i.RepositorySelection})
	}
	return out
}

// MARK: GET /v1/vault/github-sync/installations

type installationsResponse struct {
	Installations []installationDTO `json:"installations"`
	InstallURL    string            `json:"installUrl"`
}

func (m *Module) installations(c *echo.Context) error {
	user, vault, err := m.owner(c)
	if err != nil {
		return err
	}
	_, s, err := m.session(c, user, vault, wire.Query(c, "session"))
	if err != nil {
		return err
	}
	ctx, cancel := githubCtx(c)
	defer cancel()
	list, err := m.client.UserInstallations(ctx, s.token)
	if err != nil {
		return m.githubErr(ctx, "installations", err)
	}
	return wire.JSON(c, http.StatusOK, installationsResponse{Installations: installationDTOs(list), InstallURL: m.installURL()})
}

// userInstallation finds id in the user's own installation list (D3 step 4).
func (m *Module) userInstallation(ctx context.Context, token string, id int64) (githubapp.Installation, error) {
	list, err := m.client.UserInstallations(ctx, token)
	if err != nil {
		return githubapp.Installation{}, m.githubErr(ctx, "installations", err)
	}
	for _, i := range list {
		if i.ID == id {
			return i, nil
		}
	}
	return githubapp.Installation{}, errRepoNotAccessible
}

// userRepo finds a repository in what the user's token lists for an
// installation (D4). Not there is repo_not_accessible.
func (m *Module) userRepo(ctx context.Context, token string, installation, repoID int64) (githubapp.Repository, error) {
	for page := 1; page <= maxRepoPages; page++ {
		repos, more, err := m.client.UserInstallationRepos(ctx, token, installation, page, 100)
		if err != nil {
			return githubapp.Repository{}, m.githubErr(ctx, "repositories", err)
		}
		for _, r := range repos {
			if r.ID == repoID {
				return r, nil
			}
		}
		if !more {
			break
		}
	}
	return githubapp.Repository{}, errRepoNotAccessible
}

func queryInt64(c *echo.Context, key string) (int64, error) {
	v, err := strconv.ParseInt(wire.Query(c, key), 10, 64)
	if err != nil || v <= 0 {
		return 0, errBadRequest
	}
	return v, nil
}

// MARK: GET /v1/vault/github-sync/repositories

type repositoryDTO struct {
	ID            int64  `json:"id"`
	FullName      string `json:"fullName"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"defaultBranch"`
	Empty         bool   `json:"empty"`
	SizeKB        int64  `json:"sizeKb"`
	CanPush       bool   `json:"canPush"`
	InUse         bool   `json:"inUse"`
}

type repositoriesResponse struct {
	Repositories       []repositoryDTO `json:"repositories"`
	NextPage           *int            `json:"nextPage"`
	AddRepositoriesURL string          `json:"addRepositoriesUrl"`
	NewRepositoryURL   string          `json:"newRepositoryUrl"`
}

// repositories lists, with the user's token, the repositories of one of the
// user's installations that the user can push to (D4).
func (m *Module) repositories(c *echo.Context) error {
	user, vault, err := m.owner(c)
	if err != nil {
		return err
	}
	_, s, err := m.session(c, user, vault, wire.Query(c, "session"))
	if err != nil {
		return err
	}
	installation, err := queryInt64(c, "installationId")
	if err != nil {
		return err
	}
	page := 1
	if p, ok := wire.QueryInt(c, "page"); ok && p > 0 {
		page = p
	}
	ctx, cancel := githubCtx(c)
	defer cancel()
	inst, err := m.userInstallation(ctx, s.token, installation)
	if err != nil {
		return err
	}
	repos, more, err := m.client.UserInstallationRepos(ctx, s.token, installation, page, pageSize)
	if err != nil {
		return m.githubErr(ctx, "repositories", err)
	}
	ids := make([]int64, 0, len(repos))
	for _, r := range repos {
		ids = append(ids, r.ID)
	}
	inUse, err := m.store.ReposInUse(ctx, ids, vault)
	if err != nil {
		return err
	}
	out := repositoriesResponse{
		Repositories:       []repositoryDTO{},
		AddRepositoriesURL: m.manageURL(inst.Account.Type, inst.Account.Login, inst.ID),
		NewRepositoryURL:   m.client.WebBaseURL() + "/new",
	}
	for _, r := range repos {
		if !r.Permissions.Push {
			continue
		}
		out.Repositories = append(out.Repositories, repositoryDTO{
			ID: r.ID, FullName: r.FullName, Private: r.Private, Archived: r.Archived, DefaultBranch: r.DefaultBranch,
			// GitHub's repository object has no emptiness flag; a size of 0 is
			// the cheap sign. Connect checks the branch itself.
			Empty: r.Size == 0, SizeKB: r.Size, CanPush: true, InUse: inUse[r.ID],
		})
	}
	if more {
		next := page + 1
		out.NextPage = &next
	}
	return wire.JSON(c, http.StatusOK, out)
}

// MARK: GET /v1/vault/github-sync/branches

type branchesResponse struct {
	Branches      []string `json:"branches"`
	DefaultBranch string   `json:"defaultBranch"`
}

func (m *Module) branches(c *echo.Context) error {
	user, vault, err := m.owner(c)
	if err != nil {
		return err
	}
	_, s, err := m.session(c, user, vault, wire.Query(c, "session"))
	if err != nil {
		return err
	}
	installation, err := queryInt64(c, "installationId")
	if err != nil {
		return err
	}
	repoID, err := queryInt64(c, "repositoryId")
	if err != nil {
		return err
	}
	ctx, cancel := githubCtx(c)
	defer cancel()
	if _, err := m.userInstallation(ctx, s.token, installation); err != nil {
		return err
	}
	repo, err := m.userRepo(ctx, s.token, installation, repoID)
	if err != nil {
		return err
	}
	list, err := m.client.Branches(ctx, s.token, githubapp.RepoRef{ID: repo.ID, FullName: repo.FullName})
	if err != nil {
		return m.githubErr(ctx, "branches", err)
	}
	out := branchesResponse{Branches: make([]string, 0, len(list)), DefaultBranch: repo.DefaultBranch}
	for _, b := range list {
		out.Branches = append(out.Branches, b.Name)
	}
	if out.DefaultBranch == "" {
		out.DefaultBranch = "main"
	}
	return wire.JSON(c, http.StatusOK, out)
}

// MARK: PUT /v1/vault/github-sync/connection

type connectionRequest struct {
	Session        string `json:"session"`
	InstallationID int64  `json:"installationId"`
	RepositoryID   int64  `json:"repositoryId"`
	Branch         string `json:"branch,omitempty"`
	MergeExisting  bool   `json:"mergeExisting,omitempty"`
}

// setConnection connects the vault (D4, D11). Everything the client names is
// checked again with the user's own token before the App's token is used: an
// installation or repository outside the user's lists is 404 even though the
// App could reach it.
func (m *Module) setConnection(c *echo.Context) error {
	user, vault, err := m.owner(c)
	if err != nil {
		return err
	}
	req, err := wire.Bind[connectionRequest](c)
	if err != nil {
		return err
	}
	sessionID, s, err := m.session(c, user, vault, req.Session)
	if err != nil {
		return err
	}
	if req.InstallationID <= 0 || req.RepositoryID <= 0 {
		return errBadRequest
	}
	ctx, cancel := githubCtx(c)
	defer cancel()
	existing, err := m.store.Get(ctx, vault)
	if err != nil {
		return err
	}
	if existing != nil {
		return errAlreadyConnected
	}
	inst, err := m.userInstallation(ctx, s.token, req.InstallationID)
	if err != nil {
		return err
	}
	userRepo, err := m.userRepo(ctx, s.token, req.InstallationID, req.RepositoryID)
	if err != nil {
		return err
	}
	if !userRepo.Permissions.Push {
		return errRepoNotAccessible
	}
	inUse, err := m.store.ReposInUse(ctx, []int64{req.RepositoryID}, vault)
	if err != nil {
		return err
	}
	if inUse[req.RepositoryID] {
		return errRepoInUse
	}
	// Now the App's side: it must be able to read the repository itself.
	token, err := m.client.InstallationToken(ctx, req.InstallationID, req.RepositoryID)
	if err != nil {
		return m.githubErr(ctx, "installation_token", err)
	}
	repo, err := m.client.RepositoryByID(ctx, token, req.RepositoryID)
	if err != nil {
		return m.githubErr(ctx, "repository", err)
	}
	if repo.Archived {
		return errRepoArchived
	}
	if repo.Size*1024 > m.maxRepoBytes {
		return errRepoTooLarge
	}
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = repo.DefaultBranch
	}
	if branch == "" {
		branch = "main"
	}
	if !vaultgit.ValidBranchName(branch) {
		return errInvalidBranch
	}
	if err := m.checkRemote(ctx, token, githubapp.RepoRef{ID: repo.ID, FullName: repo.FullName}, branch, vault, req.MergeExisting); err != nil {
		return err
	}
	err = m.store.Insert(ctx, githubsync.Connection{
		VaultID: vault, InstallationID: inst.ID, AccountLogin: inst.Account.Login, AccountType: inst.Account.Type,
		RepoID: repo.ID, RepoFullName: repo.FullName, RepoPrivate: repo.Private, Branch: branch,
		// Active with no remote_sha: the push and pull workers (E4.8, E4.9)
		// take it from here, a first push or a merge with the remote.
		State:             githubsync.StateActive,
		ConnectedByUserID: &user, ConnectedGitHubLogin: s.login,
	})
	switch {
	case errors.Is(err, githubsync.ErrAlreadyConnected):
		return errAlreadyConnected
	case errors.Is(err, githubsync.ErrRepoInUse):
		return errRepoInUse
	case err != nil:
		return err
	}
	// The picker is over: its user token is revoked now (D4).
	m.pickers.drop(sessionID)
	m.log.InfoContext(ctx, "github sync: vault connected", "vault", vault.String(), "installation", inst.ID, "repo", repo.ID)
	out, err := m.statusOf(ctx, vault, true)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusAccepted, out)
}

// checkRemote is D11's first-connect rule. A branch with no commits needs
// nothing. Otherwise the remote is counted against the vault, refused when it
// is over the limits, and answered with a summary unless the owner already
// confirmed the merge.
func (m *Module) checkRemote(ctx context.Context, token string, repo githubapp.RepoRef, branch string, vault uuid.UUID, mergeExisting bool) error {
	b, err := m.client.Branch(ctx, token, repo, branch)
	if errors.Is(err, githubapp.ErrNotFound) {
		return nil // empty repository, or a new branch the first push creates
	}
	if err != nil {
		return m.githubErr(ctx, "branch", err)
	}
	tree, err := m.client.Tree(ctx, token, repo, b.TreeSHA())
	if err != nil {
		return m.githubErr(ctx, "tree", err)
	}
	if tree.Truncated {
		return errRepoTooLarge
	}
	entries := make([]githubsync.RemoteEntry, 0, len(tree.Entries))
	for _, e := range tree.Entries {
		entries = append(entries, githubsync.RemoteEntry{Path: e.Path, Mode: e.Mode, Type: e.Type, SHA: e.SHA, Size: e.Size})
	}
	summary, err := githubsync.Summarize(ctx, entries, m.paths.Raw(vault), m.maxFileSize)
	if err != nil {
		if ctx.Err() != nil {
			return m.githubErr(ctx, "tree", fmt.Errorf("%w: %w", githubapp.ErrUnavailable, ctx.Err()))
		}
		return err
	}
	if summary.Files > m.maxFiles {
		return errRepoTooLarge
	}
	if !mergeExisting && slices.ContainsFunc(entries, func(e githubsync.RemoteEntry) bool { return e.Type != "tree" }) {
		return errRemoteNotEmpty(summary)
	}
	return nil
}

// MARK: DELETE /v1/vault/github-sync/connection

// disconnect deletes the row and revokes a cached installation token (D13).
// Vault files and local history stay; nothing on GitHub changes. The App
// stays installed; the status's manageUrl is where the user removes it.
func (m *Module) disconnect(c *echo.Context) error {
	_, vault, err := m.owner(c)
	if err != nil {
		return err
	}
	ctx := context.WithoutCancel(c.Request().Context())
	conn, err := m.store.Delete(ctx, vault)
	if err != nil {
		return err
	}
	if conn == nil {
		return errNotConnected
	}
	rctx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	if err := m.client.RevokeInstallationToken(rctx, conn.InstallationID, conn.RepoID); err != nil {
		m.log.WarnContext(ctx, "github sync: installation token revoke failed", "kind", kind(err))
	}
	m.log.InfoContext(ctx, "github sync: vault disconnected", "vault", vault.String())
	return c.NoContent(http.StatusNoContent)
}
