// Package idempotency is the Go port of LuminaVaultServer's IdempotencyMiddleware
// (Sources/App/Middleware/IdempotencyMiddleware.swift). Clients attach
// Idempotency-Key: <UUID> to a mutating request they might retry; the response
// of the first execution is stored under (tenant_id, key) and replayed.
//
//   - No key, or one that isn't a UUID: a no-op.
//   - Stored, unexpired, same request hash: the stored response, with
//     Idempotent-Replayed: true.
//   - Stored, unexpired, different hash: 409.
//   - Otherwise the handler runs. A response it returned with status 2xx, 3xx or
//     4xx except 408 is stored, if the body fits.
//
// An error the handler returns is not a response: it passes straight through and
// is never stored, even a 4xx. Swift behaves the same way (a thrown HTTPError
// skips the persistence code), despite its header comment saying 4xx is cached.
//
// The whole response is buffered and only then forwarded, as in Swift, so do
// not put this on a route that streams to the client.
//
// Both servers read and write idempotency_keys while they run side by side, so
// the hash must stay byte-compatible: a retry that spans a flip must replay, not
// 409. Apply it behind auth, because the key is scoped to the tenant.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/ids"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	headerKey      = "Idempotency-Key"
	headerReplayed = "Idempotent-Replayed"

	defaultTTL             = 24 * time.Hour
	defaultMaxRequestBody  = 32 << 20
	defaultMaxResponseBody = 1 << 20
	// persistTimeout bounds the bookkeeping write once the handler has run.
	persistTimeout = 5 * time.Second
)

var errReused = wire.NewError(http.StatusConflict, "Idempotency-Key reused with different request")

// Options overrides the Swift defaults. Tests use it; routes pass the zero value.
type Options struct {
	TTL             time.Duration
	MaxRequestBody  int64
	MaxResponseBody int
	Now             func() time.Time
}

// New returns the middleware. Each route group gets its own instance, but they
// share one table, as in Swift.
func New(pool *pgxpool.Pool, opts Options) echo.MiddlewareFunc {
	if opts.TTL <= 0 {
		opts.TTL = defaultTTL
	}
	if opts.MaxRequestBody <= 0 {
		opts.MaxRequestBody = defaultMaxRequestBody
	}
	if opts.MaxResponseBody <= 0 {
		opts.MaxResponseBody = defaultMaxResponseBody
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	m := &middleware{pool: pool, opts: opts}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error { return m.handle(c, next) }
	}
}

type middleware struct {
	pool *pgxpool.Pool
	opts Options
}

type stored struct {
	id          uuid.UUID
	requestHash string
	status      int
	contentType *string
	body        []byte
	expiresAt   time.Time
}

func (m *middleware) handle(c *echo.Context, next echo.HandlerFunc) error {
	r := c.Request()
	key, ok := parseKey(r.Header.Values(headerKey))
	if !ok {
		return next(c)
	}
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	ctx := r.Context()

	original := r.Body
	body, err := readUpTo(original, m.opts.MaxRequestBody)
	if err != nil {
		// Too large or unreadable: skip idempotency. The retry re-executes, and
		// the handler still gets every byte, whatever was read so far included.
		// Swift hands the handler a stream it has already drained (see the PR).
		slog.DebugContext(ctx, "idempotency: body too large, bypassing", "key", key.String())
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), original), original}
		return next(c)
	}

	requestHash := hashRequest(body, r.Method, rawPath(r))

	existing, err := m.lookup(ctx, tenant, key)
	if err != nil {
		return err
	}
	if existing != nil {
		switch {
		case !existing.expiresAt.After(m.opts.Now()):
			if _, err := m.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE id = $1`, existing.id); err != nil {
				slog.DebugContext(ctx, "idempotency: expired row not deleted", "error", err)
			}
		case existing.requestHash != requestHash:
			slog.WarnContext(ctx, "idempotency: key reused with different body", "tenant", ids.Upper(tenant), "key", ids.Upper(key))
			return errReused
		default:
			slog.DebugContext(ctx, "idempotency: replay", "tenant", ids.Upper(tenant), "key", ids.Upper(key))
			return replay(c.Response(), existing)
		}
	}

	// Re-attach the buffered body so the handler sees it, and buffer the response.
	r.Body = io.NopCloser(bytes.NewReader(body))
	w := c.Response()
	rec := newRecorder(w.Header())
	c.SetResponse(rec)
	defer c.SetResponse(w)
	if err := next(c); err != nil {
		return err
	}
	c.SetResponse(w)

	status := rec.statusCode()
	if status >= 200 && status < 500 && status != http.StatusRequestTimeout && rec.body.Len() <= m.opts.MaxResponseBody {
		m.persist(ctx, tenant, key, requestHash, status, rec)
	}
	return rec.forward(w)
}

// parseKey is `UUID(uuidString:)` on HTTPFields' combined value: exactly the
// 36-character form, either case. ids.Parse also takes bare hex, braces and
// urn:, which Swift treats as no key at all. Repeated headers join with ", ",
// which never parses, so they are no key either.
func parseKey(values []string) (uuid.UUID, bool) {
	raw := strings.Join(values, ", ")
	if len(raw) != 36 {
		return uuid.Nil, false
	}
	key, err := uuid.Parse(raw)
	return key, err == nil
}

var errTooLarge = errors.New("idempotency: request body too large")

// readUpTo reads at most limit bytes. On any failure it returns what it read
// with the error, so the caller can still hand the handler the whole body.
func readUpTo(body io.Reader, limit int64) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return b, err
	}
	if int64(len(b)) > limit {
		return b, errTooLarge
	}
	return b, nil
}

// hashRequest is Swift's request hash: SHA-256 over the body, then the method,
// then the path, with no separators, in lowercase hex.
func hashRequest(body []byte, method, path string) string {
	h := sha256.New()
	h.Write(body)
	h.Write([]byte(method))
	h.Write([]byte(path))
	return hex.EncodeToString(h.Sum(nil))
}

// rawPath is Hummingbird's request.uri.path: the path as sent, without the
// query and with percent escapes intact.
func rawPath(r *http.Request) string {
	if r.RequestURI != "" {
		path, _, _ := strings.Cut(r.RequestURI, "?")
		return path
	}
	return r.URL.EscapedPath()
}

func (m *middleware) lookup(ctx context.Context, tenant, key uuid.UUID) (*stored, error) {
	var s stored
	err := m.pool.QueryRow(ctx, `SELECT id, request_hash, response_status, response_content_type, response_body, expires_at
		FROM idempotency_keys WHERE tenant_id = $1 AND key = $2 LIMIT 1`, tenant, key).
		Scan(&s.id, &s.requestHash, &s.status, &s.contentType, &s.body, &s.expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // no row is the common case, not an error
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// persist stores the response. A concurrent retry may have stored one first, in
// which case the unique index rejects this insert: this request still holds a
// valid response, and later replays hit the other row. A client hanging up
// doesn't stop the write, since the handler's work is done.
func (m *middleware) persist(ctx context.Context, tenant, key uuid.UUID, requestHash string, status int, rec *recorder) {
	var contentType *string
	if ct := rec.header.Get("Content-Type"); ct != "" {
		contentType = &ct
	}
	now := m.opts.Now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	// A non-nil body: the column is NOT NULL and a 204 has none.
	body := append([]byte{}, rec.body.Bytes()...)
	if _, err := m.pool.Exec(ctx, `INSERT INTO idempotency_keys
		(id, tenant_id, key, request_hash, response_status, response_content_type, response_body, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		uuid.New(), tenant, key, requestHash, status, contentType, body, now, now.Add(m.opts.TTL)); err != nil {
		slog.DebugContext(ctx, "idempotency: persistence race, returning live response", "error", err)
	}
}

// replay answers from the stored row with only its Content-Type and the marker.
func replay(w http.ResponseWriter, s *stored) error {
	h := w.Header()
	if s.contentType != nil {
		h.Set("Content-Type", *s.contentType)
	} else {
		h["Content-Type"] = nil // no sniffing: the first response had none
	}
	h.Set(headerReplayed, "true")
	w.WriteHeader(s.status)
	if len(s.body) == 0 {
		return nil
	}
	_, err := w.Write(s.body)
	return err
}

// recorder buffers a handler's response. Its headers start as a copy, so a
// handler that sets some and then returns an error leaves the real ones alone.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newRecorder(h http.Header) *recorder {
	return &recorder{header: h.Clone()}
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

// Flush is a no-op: everything is forwarded at the end.
func (r *recorder) Flush() {}

func (r *recorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func (r *recorder) forward(w http.ResponseWriter) error {
	h := w.Header()
	for k := range h {
		delete(h, k)
	}
	for k, v := range r.header {
		h[k] = v
	}
	w.WriteHeader(r.statusCode())
	if r.body.Len() == 0 {
		return nil
	}
	_, err := w.Write(r.body.Bytes())
	return err
}
