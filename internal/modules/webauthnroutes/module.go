// Package webauthnroutes serves wave 8C: WebAuthnService's eight passkey routes
// (Sources/App/Auth/WebAuthnService.swift). The ceremonies are internal/passkey.
//
// Mounting follows Swift. Nothing mounts unless WEBAUTHN_ENABLED is "true" (any case),
// so by default every path is a 404. Sign-in (authenticate/begin, its /options alias,
// authenticate/finish) is AuthController's public group with the per-IP
// webAuthnAuthenticateByIP limit. Enrolment and management (register/begin, its
// /options alias, register/finish, credentials, credentials/:credentialId) are
// App+build's group: JWTAuthenticator, which never rejects, then webAuthnEnrolByUser
// keyed by user or IP, then the handler, which answers 401 "missing identity" itself.
// With the feature on but no relying-party id or origin, the ceremonies answer 503
// "webauthn disabled" and listing and deleting still work.
package webauthnroutes

import (
	"context"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/authservice"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/passkey"
	"lumina/internal/ratelimit"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Options holds the module's collaborators.
type Options struct {
	Enabled bool
	Config  passkey.Config
	Service *authservice.Service // findUser(byUsername:) and issueTokens
	Limits  *ratelimit.Store     // the process-wide store
	// Identify is JWTAuthenticator: attaches the user, never rejects.
	Identify echo.MiddlewareFunc
	// Store holds pending challenges; nil makes a fresh one.
	Store *passkey.Store
	// Now stamps created_at and updated_at; nil is time.Now.
	Now func() time.Time
}

type Module struct {
	pool  *pgxpool.Pool
	opt   Options
	store *passkey.Store
}

func New(pool *pgxpool.Pool, opt Options) *Module {
	store := opt.Store
	if store == nil {
		store = passkey.NewStore()
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Module{pool: pool, opt: opt, store: store}
}

// Register mounts nothing unless enabled, as every addRoutes in Swift opens with
// `guard enabled`.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	if !m.opt.Enabled {
		return
	}
	byIP := ratelimit.Middleware(m.opt.Limits, ratelimit.WebAuthnAuthenticateByIP, ratelimit.IPOnlyKey)
	r.POST("/v1/auth/webauthn/authenticate/begin", m.beginAuthentication, byIP)
	r.POST("/v1/auth/webauthn/authenticate/options", m.beginAuthentication, byIP)
	r.POST("/v1/auth/webauthn/authenticate/finish", m.finishAuthentication, byIP)

	byUser := ratelimit.Middleware(m.opt.Limits, ratelimit.WebAuthnEnrolByUser, ratelimit.UserOrIPKey)
	r.POST("/v1/auth/webauthn/register/begin", m.beginRegistration, m.opt.Identify, byUser)
	r.POST("/v1/auth/webauthn/register/options", m.beginRegistration, m.opt.Identify, byUser)
	r.POST("/v1/auth/webauthn/register/finish", m.finishRegistration, m.opt.Identify, byUser)
	r.GET("/v1/auth/webauthn/credentials", m.listCredentials, m.opt.Identify, byUser)
	r.DELETE("/v1/auth/webauthn/credentials/:credentialId", m.deleteCredential, m.opt.Identify, byUser)
}

var (
	errDisabled        = wire.NewError(http.StatusServiceUnavailable, "webauthn disabled")
	errMissingIdentity = wire.NewError(http.StatusUnauthorized, "missing identity")
	errClaimMismatch   = wire.NewError(http.StatusForbidden, "username does not match the authenticated user")
	errNoRegistration  = wire.NewError(http.StatusBadRequest, "missing or expired registration challenge")
	errNoAuthChallenge = wire.NewError(http.StatusBadRequest, "missing or expired authentication challenge")
	errNotRegistered   = wire.NewError(http.StatusUnauthorized, "credential not registered")
	// errDuplicate is credentialIDAlreadyExists: a library error, so a bare 500.
	errDuplicate = errors.New("webauthn: credential id already registered")
)

// writeJSON encodes as Foundation's JSONEncoder: a standard-base64 credential id
// holds "/".
func writeJSON(c *echo.Context, v any) error {
	b, err := wire.MarshalFoundation(v)
	if err != nil {
		return err
	}
	return c.Blob(http.StatusOK, wire.ContentTypeJSON, b)
}

type enrollingUser struct {
	id       uuid.UUID
	username string
}

// enrollingUser is WebAuthnService.enrollingUser: the identity JWTAuthenticator
// loaded, checked before the body is decoded, so an unauthenticated caller gets 401
// whatever it sent.
func (m *Module) enrollingUser(c *echo.Context) (enrollingUser, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return enrollingUser{}, errMissingIdentity
	}
	u := enrollingUser{id: id}
	if err := m.pool.QueryRow(c.Request().Context(), `SELECT username FROM users WHERE id = $1`, id).Scan(&u.username); err != nil {
		return enrollingUser{}, err
	}
	return u, nil
}

// requireClaimMatches: the body's username is advisory. Blank (after trimming) is the
// token's account; anything else must match it ignoring case.
func requireClaimMatches(claimed, username string) error {
	claimed = swiftstr.TrimWhitespacesAndNewlines(claimed)
	if claimed != "" && swiftstr.Lowercased(claimed) != swiftstr.Lowercased(username) {
		return errClaimMismatch
	}
	return nil
}

type beginRegistrationRequest struct {
	Username    string  `json:"username"`
	DisplayName *string `json:"displayName,omitempty"`
}

type creationResponse struct {
	Options passkey.CreationOptions `json:"options"`
}

func (m *Module) beginRegistration(c *echo.Context) error {
	if !m.opt.Config.Configured() {
		return errDisabled
	}
	user, err := m.enrollingUser(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[beginRegistrationRequest](c)
	if err != nil {
		return err
	}
	if err := requireClaimMatches(body.Username, user.username); err != nil {
		return err
	}
	displayName := user.username
	if body.DisplayName != nil {
		displayName = *body.DisplayName
	}
	// The user handle is the uuidString's UTF-8 bytes, not the 16 raw bytes.
	options, challenge := m.opt.Config.BeginRegistration([]byte(ids.Upper(user.id)), user.username, displayName)
	m.store.StoreRegistration(user.id, challenge)
	return writeJSON(c, creationResponse{Options: options})
}

type finishRegistrationRequest struct {
	Username               string                 `json:"username"`
	CredentialCreationData registrationCredential `json:"credentialCreationData"`
}

type finishRegistrationResponse struct {
	CredentialID string `json:"credentialID"`
}

func (m *Module) finishRegistration(c *echo.Context) error {
	if !m.opt.Config.Configured() {
		return errDisabled
	}
	user, err := m.enrollingUser(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[finishRegistrationRequest](c)
	if err != nil {
		return err
	}
	if err := requireClaimMatches(body.Username, user.username); err != nil {
		return err
	}
	challenge, ok := m.store.Registration(user.id)
	if !ok {
		return errNoRegistration
	}
	cred := body.CredentialCreationData
	reg, err := m.opt.Config.VerifyRegistration(challenge, passkey.RegistrationResponse{
		ID: cred.ID, Type: cred.Type, RawID: cred.RawID,
		ClientDataJSON: cred.Response.ClientDataJSON, AttestationObject: cred.Response.AttestationObject,
	})
	if err != nil {
		return err
	}
	// Hummingbird finishes a handler whose client has gone; so does this one.
	ctx := context.WithoutCancel(c.Request().Context())
	// confirmCredentialIDNotRegisteredYet. Swift: it looks up the id as the client sent
	// it (usually unpadded base64url), while rows hold the padded standard-base64 form,
	// so it rarely finds a duplicate and the unique index refuses the insert instead (a
	// 500 either way). A failed query counts as "not registered" (`try?`).
	var exists bool
	if err := m.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM webauthn_credentials WHERE credential_id = $1)`, cred.ID).Scan(&exists); err == nil && exists {
		return errDuplicate
	}
	now := m.opt.Now()
	if _, err := m.pool.Exec(ctx, `INSERT INTO webauthn_credentials (id, tenant_id, credential_id, public_key, sign_count, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`, uuid.New(), user.id, reg.ID, reg.PublicKey, int64(reg.SignCount), now); err != nil {
		return err
	}
	m.store.ClearRegistration(user.id)
	return writeJSON(c, finishRegistrationResponse{CredentialID: reg.ID})
}

type beginAuthenticationRequest struct {
	Username string `json:"username"`
}

type requestResponse struct {
	Options passkey.RequestOptions `json:"options"`
}

// beginAuthentication issues options for any username, registered or not
// (anti-enumeration): only finish looks the user up.
func (m *Module) beginAuthentication(c *echo.Context) error {
	if !m.opt.Config.Configured() {
		return errDisabled
	}
	body, err := wire.Bind[beginAuthenticationRequest](c)
	if err != nil {
		return err
	}
	options, challenge := m.opt.Config.BeginAuthentication()
	m.store.StoreAuthentication(body.Username, challenge)
	return writeJSON(c, requestResponse{Options: options})
}

type finishAuthenticationRequest struct {
	Username   string                   `json:"username"`
	Credential authenticationCredential `json:"credential"`
}

func (m *Module) finishAuthentication(c *echo.Context) error {
	if !m.opt.Config.Configured() {
		return errDisabled
	}
	body, err := wire.Bind[finishAuthenticationRequest](c)
	if err != nil {
		return err
	}
	challenge, ok := m.store.Authentication(body.Username)
	if !ok {
		return errNoAuthChallenge
	}
	ctx := c.Request().Context()
	user, err := m.opt.Service.FindUserByUsername(ctx, body.Username)
	if err != nil {
		return err
	}
	if user == nil {
		return errNotRegistered
	}
	// Swift: the row is matched on the id string exactly as sent. Rows written at
	// registration hold padded standard base64, so an unpadded base64url id (what
	// browsers and the iOS app send) only matches when the two spellings agree.
	var (
		rowID     uuid.UUID
		publicKey []byte
		signCount int64
	)
	err = m.pool.QueryRow(ctx, `SELECT id, public_key, sign_count FROM webauthn_credentials WHERE tenant_id = $1 AND credential_id = $2 LIMIT 1`,
		user.ID, body.Credential.ID).Scan(&rowID, &publicKey, &signCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotRegistered
	}
	if err != nil {
		return err
	}
	if signCount < 0 || signCount > 1<<32-1 {
		// UInt32(row.signCount) traps in Swift.
		return errors.New("webauthn: stored sign count out of range")
	}
	cred := body.Credential
	newCount, err := m.opt.Config.VerifyAssertion(challenge, passkey.AssertionResponse{
		ID: cred.ID, Type: cred.Type, ClientDataJSON: cred.Response.ClientDataJSON,
		AuthenticatorData: cred.Response.AuthenticatorData, Signature: cred.Response.Signature,
	}, publicKey, uint32(signCount))
	if err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	if _, err := m.pool.Exec(ctx, `UPDATE webauthn_credentials SET sign_count = $1, updated_at = $2 WHERE id = $3`,
		int64(newCount), m.opt.Now(), rowID); err != nil {
		return err
	}
	m.store.ClearAuthentication(body.Username)
	resp, err := m.opt.Service.IssueTokens(ctx, user)
	if err != nil {
		return err
	}
	return writeJSON(c, resp)
}

// credentialSummary is WebAuthnCredentialSummaryDTO. nickname is always nil, so never
// written; lastUsedAt is updated_at, which Fluent also sets at creation.
type credentialSummary struct {
	ID         string     `json:"id"`
	CreatedAt  wire.Time  `json:"createdAt"`
	LastUsedAt *wire.Time `json:"lastUsedAt,omitempty"`
}

type credentialList struct {
	Credentials []credentialSummary `json:"credentials"`
}

func (m *Module) listCredentials(c *echo.Context) error {
	id, ok := auth.Identity(c)
	if !ok {
		return errMissingIdentity
	}
	rows, err := m.pool.Query(c.Request().Context(), `SELECT credential_id, created_at, updated_at FROM webauthn_credentials WHERE tenant_id = $1`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := credentialList{Credentials: []credentialSummary{}}
	for rows.Next() {
		var (
			s                credentialSummary
			created, updated *time.Time
		)
		if err := rows.Scan(&s.ID, &created, &updated); err != nil {
			return err
		}
		if created == nil {
			now := time.Now() // `createdAt ?? Date()`
			created = &now
		}
		s.CreatedAt = wire.Time{Time: *created}
		if updated != nil {
			s.LastUsedAt = &wire.Time{Time: *updated}
		}
		out.Credentials = append(out.Credentials, s)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return writeJSON(c, out)
}

// deleteCredential is 204 whether or not a row matched. Swift: an id holding "/" (most
// padded standard-base64 ids are "/"-free, but not all) cannot be named in the path.
func (m *Module) deleteCredential(c *echo.Context) error {
	id, ok := auth.Identity(c)
	if !ok {
		return errMissingIdentity
	}
	if _, err := m.pool.Exec(context.WithoutCancel(c.Request().Context()),
		`DELETE FROM webauthn_credentials WHERE tenant_id = $1 AND credential_id = $2`, id, c.Param("credentialId")); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
