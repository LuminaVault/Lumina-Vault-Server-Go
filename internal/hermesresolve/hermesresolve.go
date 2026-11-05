// Package hermesresolve is the Go port of Swift's HermesEndpointResolver and
// HermesResolutionMiddleware (Sources/App/LLM/HermesEndpointResolver.swift,
// Middleware/HermesResolutionMiddleware.swift): which Hermes gateway serves a
// tenant, resolved once per request.
//
// A user_hermes_config row with a non-blank base_url overrides the managed
// default. Failures never fall back to the default, because that would surprise
// a user who deliberately routed traffic away from the managed gateway.
//
// The middleware sits on routes that never dial Hermes too (/v1/memory/upsert),
// so a tenant with a broken gateway config gets a 502 there as well. That is
// Swift's behaviour and it is kept.
package hermesresolve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/ids"
	"lumina/internal/secretbox"
	"lumina/internal/ssrfguard"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	ctxResolution = "lv.hermes.resolution"
	reasonCode    = "hermes_unreachable"
)

// Resolution is the endpoint for one request. AuthHeader is plaintext: never log
// it or put it in a span.
type Resolution struct {
	BaseURL        *url.URL
	AuthHeader     *string
	IsUserOverride bool
}

// ResolutionKind is a HermesEndpointResolver.ResolutionError case.
type ResolutionKind int

const (
	DecryptFailed ResolutionKind = iota
	SSRFRejected
	MalformedRow
)

// ResolutionError is a failure a user can fix in Settings.
type ResolutionError struct {
	Kind ResolutionKind
	// Reason is the rejection's description, for SSRFRejected.
	Reason string
}

func (e *ResolutionError) Error() string {
	switch e.Kind {
	case SSRFRejected:
		return "ssrfRejected: " + e.Reason
	case DecryptFailed:
		return "decryptFailed"
	default:
		return "malformedRow"
	}
}

// Message is the user-facing text of the 502: each failure has a different fix.
func (e *ResolutionError) Message() string {
	switch e.Kind {
	case SSRFRejected:
		return "Your Hermes address could not be used (" + e.Reason + "). Check the URL in Settings — a hostname that only resolves on your own network will not resolve from here; the tailnet IP does."
	case DecryptFailed:
		return "Your saved Hermes credentials could not be read. Re-enter the auth header in Settings."
	default:
		return "Your Hermes configuration is incomplete. Re-save it in Settings."
	}
}

// Resolver looks tenants up. In Swift it exists only when LV_SECRET_MASTER_KEY
// is usable, and without it the middleware isn't installed at all; a nil
// *Resolver's Middleware passes everything through to the same effect.
type Resolver struct {
	pool       *pgxpool.Pool
	box        *secretbox.Box
	guard      ssrfguard.Guard
	defaultURL *url.URL
}

func New(pool *pgxpool.Pool, box *secretbox.Box, guard ssrfguard.Guard, defaultBaseURL *url.URL) *Resolver {
	return &Resolver{pool: pool, box: box, guard: guard, defaultURL: defaultBaseURL}
}

// Resolve is lookup, SSRF re-validation (every call, for DNS rebinding), then
// decrypt. A *ResolutionError means the user's config is unusable; anything else
// is a server failure.
func (r *Resolver) Resolve(ctx context.Context, tenant uuid.UUID) (Resolution, error) {
	var (
		baseURL           string
		ciphertext, nonce []byte
	)
	err := r.pool.QueryRow(ctx, `SELECT base_url, auth_header_ciphertext, auth_header_nonce
		FROM user_hermes_config WHERE tenant_id = $1 LIMIT 1`, tenant).Scan(&baseURL, &ciphertext, &nonce)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Resolution{}, err
	}
	// A row may hold cron-dashboard config only (blank base_url): no override,
	// so cron config never breaks chat routing.
	if err != nil || trimWhitespaces(baseURL) == "" {
		return Resolution{BaseURL: r.defaultURL}, nil
	}

	validated, err := r.guard.Validate(ctx, baseURL)
	if err != nil {
		var rejection *ssrfguard.Rejection
		if !errors.As(err, &rejection) {
			return Resolution{}, err
		}
		slog.WarnContext(ctx, "ssrf rejection during resolve", "tenant", ids.Upper(tenant), "rejection", rejection.Error())
		return Resolution{}, &ResolutionError{Kind: SSRFRejected, Reason: rejection.Error()}
	}

	var authHeader *string
	switch {
	case ciphertext != nil && nonce != nil:
		plain, err := r.box.Open(secretbox.Sealed{Ciphertext: ciphertext, Nonce: nonce}, tenant)
		if err != nil {
			slog.ErrorContext(ctx, "auth header decrypt failed", "tenant", ids.Upper(tenant))
			return Resolution{}, &ResolutionError{Kind: DecryptFailed}
		}
		authHeader = &plain
	case ciphertext != nil || nonce != nil:
		// One column populated and not the other: a corrupt row.
		return Resolution{}, &ResolutionError{Kind: MalformedRow}
	}
	return Resolution{BaseURL: validated, AuthHeader: authHeader, IsUserOverride: true}, nil
}

// Middleware resolves once per request and stashes the result for FromContext.
// Apply it after auth. A request with no identity passes through, so routes
// return their own 401; a *ResolutionError answers 502 hermes_unreachable; any
// other failure is returned as usual.
func (r *Resolver) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		if r == nil {
			return next
		}
		return func(c *echo.Context) error {
			tenant, ok := auth.Identity(c)
			if !ok {
				return next(c)
			}
			ctx := c.Request().Context()
			resolution, err := r.Resolve(ctx, tenant)
			if err != nil {
				var failure *ResolutionError
				if !errors.As(err, &failure) {
					return err
				}
				slog.WarnContext(ctx, "hermes endpoint resolution failed", "tenant", ids.Upper(tenant), "error", failure.Error())
				return writeUnreachable(c, failure)
			}
			c.Set(ctxResolution, resolution)
			return next(c)
		}
	}
}

// FromContext returns what Middleware resolved. The bool is false on a route
// without it, or when the middleware passed a request with no identity through.
func FromContext(c *echo.Context) (Resolution, bool) {
	res, ok := c.Get(ctxResolution).(Resolution)
	return res, ok
}

// writeUnreachable is UpstreamErrorResponse: a 502 with
// {"error":{"code":…,"message":…}} as JSONSerialization writes it with
// .sortedKeys, so "/" is escaped and non-ASCII is not.
func writeUnreachable(c *echo.Context, failure *ResolutionError) error {
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	payload.Error.Code = reasonCode
	payload.Error.Message = failure.Message()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return err
	}
	body := bytes.ReplaceAll(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), []byte("/"), []byte(`\/`))
	return c.Blob(http.StatusBadGateway, "application/json", body)
}

// trimWhitespaces is trimmingCharacters(in: .whitespaces): spaces and tabs, not
// newlines.
func trimWhitespaces(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r == '\t' || unicode.Is(unicode.Zs, r) })
}

// IsUserOverride says this request resolved to the caller's own Hermes, which
// EntitlementMiddleware treats as BYOK: the capture gate lets it through.
func IsUserOverride(c *echo.Context) bool {
	r, ok := FromContext(c)
	return ok && r.IsUserOverride
}
