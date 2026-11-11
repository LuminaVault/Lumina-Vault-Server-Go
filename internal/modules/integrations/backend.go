package integrations

import (
	"context"
	"errors"
	"fmt"
	"lumina/internal/hermescontainers"
	"net/url"
	"strings"
	"sync"
	"time"
)

// hermesBinaryPath is the in-container hermes CLI (the Python venv install).
const hermesBinaryPath = "/opt/hermes/.venv/bin/hermes"

// Errors of XaiOAuthError and NousOAuthError that the controllers map.
var (
	ErrSessionNotFound          = errors.New("sessionNotFound")
	ErrAuthorizeURLMissing      = errors.New("authorizeURLMissingFromStdout")
	ErrVerifyURLMissing         = errors.New("verifyURLMissingFromStdout")
	ErrBackendNotYetImplemented = errors.New("notYetImplemented")
)

// BackendFailedError is backendFailed(reason:).
type BackendFailedError struct{ Reason string }

func (e *BackendFailedError) Error() string {
	return fmt.Sprintf("backendFailed(reason: %q)", e.Reason)
}

// CallbackForwardError is XaiOAuthError.callbackForwardFailed: an unmapped error,
// so a 500.
type CallbackForwardError struct{ Stderr string }

func (e *CallbackForwardError) Error() string {
	return fmt.Sprintf("callbackForwardFailed(stderr: %q)", e.Stderr)
}

// processRegistry is XaiOAuthProcessRegistry / NousOAuthProcessRegistry: the
// parked `hermes auth add` processes, by session, for ten minutes. It is in-memory,
// so start and complete must reach the same process (flip-blockers.md).
type processRegistry struct {
	mu      sync.Mutex
	entries map[string]registryEntry
	ttl     time.Duration
	now     func() time.Time
}

type registryEntry struct {
	handle    hermescontainers.Streaming
	createdAt time.Time
}

func newProcessRegistry() *processRegistry {
	return &processRegistry{entries: map[string]registryEntry{}, ttl: 10 * time.Minute, now: time.Now}
}

func (r *processRegistry) put(sessionID string, h hermescontainers.Streaming) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweep()
	r.entries[sessionID] = registryEntry{handle: h, createdAt: r.now()}
}

func (r *processRegistry) take(sessionID string) (registryEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[sessionID]
	delete(r.entries, sessionID)
	return e, ok
}

func (r *processRegistry) cancel(sessionID string) {
	if e, ok := r.take(sessionID); ok {
		e.handle.Cancel()
	}
}

func (r *processRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweep()
	return len(r.entries)
}

func (r *processRegistry) sweep() {
	cutoff := r.now().Add(-r.ttl)
	for id, e := range r.entries {
		if e.createdAt.Before(cutoff) {
			e.handle.Cancel()
			delete(r.entries, id)
		}
	}
}

// withTimeout runs op, and on timeout cancels h and answers backendFailed or
// timeoutErr, as Swift's withTimeout does.
func withTimeout[R any](ctx context.Context, d time.Duration, h hermescontainers.Streaming, timeoutErr error, op func(context.Context) (R, error)) (R, error) {
	cx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	r, err := op(cx)
	if err != nil && errors.Is(cx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		h.Cancel()
		var zero R
		return zero, timeoutErr
	}
	return r, err
}

// XaiBackend is XaiOAuthBackend.
type XaiBackend interface {
	RequestAuthorizeURL(ctx context.Context, h *hermescontainers.Handle, sessionID string) (string, error)
	SubmitCallback(ctx context.Context, h *hermescontainers.Handle, sessionID, callbackURL string) (bool, error)
	Cancel(sessionID string)
	Revoke(ctx context.Context, h *hermescontainers.Handle) (bool, error)
}

// LiveXaiBackend is LiveXaiOAuthBackend: `hermes auth add xai-oauth --no-browser`
// in the tenant container.
type LiveXaiBackend struct {
	Docker          hermescontainers.Docker
	registry        *processRegistry
	loopbackURL     string
	startTimeout    time.Duration
	completeTimeout time.Duration
}

func NewLiveXaiBackend(docker hermescontainers.Docker) *LiveXaiBackend {
	return &LiveXaiBackend{
		Docker: docker, registry: newProcessRegistry(), loopbackURL: "http://127.0.0.1:56121/callback",
		startTimeout: 30 * time.Second, completeTimeout: 60 * time.Second,
	}
}

var authorizeURLLinePrefixes = []string{"Authorize at:", "Visit:", "Open this URL:"}

func (b *LiveXaiBackend) RequestAuthorizeURL(ctx context.Context, h *hermescontainers.Handle, sessionID string) (string, error) {
	s, err := b.Docker.ExecStreaming(ctx, h.ContainerName, []string{hermesBinaryPath, "auth", "add", "xai-oauth", "--no-browser"})
	if err != nil {
		return "", err
	}
	u, err := withTimeout(ctx, b.startTimeout, s, ErrAuthorizeURLMissing, func(cx context.Context) (string, error) {
		return consumeAuthorizeURL(cx, s.Lines())
	})
	if err != nil {
		return "", err
	}
	b.registry.put(sessionID, s)
	return u, nil
}

func (b *LiveXaiBackend) SubmitCallback(ctx context.Context, h *hermescontainers.Handle, sessionID, callbackURL string) (bool, error) {
	entry, ok := b.registry.take(sessionID)
	if !ok {
		return false, ErrSessionNotFound
	}
	res, err := b.Docker.Exec(ctx, h.ContainerName, []string{"curl", "--silent", "--show-error", "--max-time", "10", forwardURL(b.loopbackURL, callbackURL)}, nil)
	if err != nil {
		return false, err
	}
	if !res.OK() {
		entry.handle.Cancel()
		return false, &CallbackForwardError{Stderr: res.Stderr}
	}
	code, err := withTimeout(ctx, b.completeTimeout, entry.handle,
		&BackendFailedError{Reason: "hermes CLI did not exit within timeout"},
		func(cx context.Context) (int, error) { return entry.handle.Wait(cx) })
	if err != nil {
		return false, err
	}
	return code == 0, nil
}

func (b *LiveXaiBackend) Cancel(sessionID string) { b.registry.cancel(sessionID) }

func (b *LiveXaiBackend) Revoke(ctx context.Context, h *hermescontainers.Handle) (bool, error) {
	res, err := b.Docker.Exec(ctx, h.ContainerName, []string{hermesBinaryPath, "auth", "remove", "xai-oauth"}, nil)
	if err != nil {
		return false, err
	}
	return res.OK(), nil
}

// consumeAuthorizeURL reads lines until one names the authorize URL.
func consumeAuthorizeURL(ctx context.Context, lines <-chan string) (string, error) {
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case line, ok := <-lines:
			if !ok {
				return "", ErrAuthorizeURLMissing
			}
			for _, prefix := range authorizeURLLinePrefixes {
				if strings.Contains(line, prefix) {
					if t := strings.TrimSpace(strings.ReplaceAll(line, prefix, "")); t != "" {
						return t, nil
					}
				}
			}
			if strings.HasPrefix(line, "https://accounts.x.ai/") || strings.HasPrefix(line, "https://x.ai/") {
				return strings.TrimSpace(line), nil
			}
		}
	}
}

// forwardURL appends the captured callback's query to the loopback URL.
func forwardURL(loopback, captured string) string {
	parsed, err := url.Parse(captured)
	if err != nil || parsed.RawQuery == "" {
		return loopback
	}
	sep := "?"
	if strings.Contains(loopback, "?") {
		sep = "&"
	}
	return loopback + sep + parsed.RawQuery
}

// NousBackend is NousOAuthBackend.
type NousBackend interface {
	RequestVerification(ctx context.Context, h *hermescontainers.Handle, sessionID string) (verifyURL string, userCode *string, err error)
	AwaitCompletion(ctx context.Context, h *hermescontainers.Handle, sessionID string) (bool, error)
	Cancel(sessionID string)
	Revoke(ctx context.Context, h *hermescontainers.Handle) (bool, error)
	SubscriptionPlan(ctx context.Context, h *hermescontainers.Handle) *string
}

// LiveNousBackend is LiveNousOAuthBackend: `hermes auth add nous --type oauth`.
type LiveNousBackend struct {
	Docker          hermescontainers.Docker
	registry        *processRegistry
	startTimeout    time.Duration
	completeTimeout time.Duration
}

func NewLiveNousBackend(docker hermescontainers.Docker) *LiveNousBackend {
	return &LiveNousBackend{Docker: docker, registry: newProcessRegistry(), startTimeout: 30 * time.Second, completeTimeout: 300 * time.Second}
}

var (
	verifyURLLinePrefixes = []string{"Visit:", "Open this URL:", "Authorize at:", "To authorize, visit", "Go to"}
	userCodeLinePrefixes  = []string{"Enter code:", "User code:", "user_code:", "Code:", "and enter code"}
)

type verification struct {
	url  string
	code *string
}

func (b *LiveNousBackend) RequestVerification(ctx context.Context, h *hermescontainers.Handle, sessionID string) (string, *string, error) {
	s, err := b.Docker.ExecStreaming(ctx, h.ContainerName, []string{hermesBinaryPath, "auth", "add", "nous", "--type", "oauth"})
	if err != nil {
		return "", nil, err
	}
	v, err := withTimeout(ctx, b.startTimeout, s, ErrVerifyURLMissing, func(cx context.Context) (verification, error) {
		return consumeVerification(cx, s.Lines())
	})
	if err != nil {
		return "", nil, err
	}
	b.registry.put(sessionID, s)
	return v.url, v.code, nil
}

func (b *LiveNousBackend) AwaitCompletion(ctx context.Context, _ *hermescontainers.Handle, sessionID string) (bool, error) {
	entry, ok := b.registry.take(sessionID)
	if !ok {
		return false, ErrSessionNotFound
	}
	code, err := withTimeout(ctx, b.completeTimeout, entry.handle,
		&BackendFailedError{Reason: "hermes CLI did not exit within timeout"},
		func(cx context.Context) (int, error) { return entry.handle.Wait(cx) })
	if err != nil {
		return false, err
	}
	return code == 0, nil
}

func (b *LiveNousBackend) Cancel(sessionID string) { b.registry.cancel(sessionID) }

func (b *LiveNousBackend) Revoke(ctx context.Context, h *hermescontainers.Handle) (bool, error) {
	res, err := b.Docker.Exec(ctx, h.ContainerName, []string{hermesBinaryPath, "auth", "remove", "nous"}, nil)
	if err != nil {
		return false, err
	}
	return res.OK(), nil
}

func (b *LiveNousBackend) SubscriptionPlan(ctx context.Context, h *hermescontainers.Handle) *string {
	res, err := b.Docker.Exec(ctx, h.ContainerName, []string{hermesBinaryPath, "portal", "status"}, nil)
	if err != nil || !res.OK() {
		return nil
	}
	return extractPlan(res.Stdout)
}

// extractPlan is LiveNousOAuthBackend.extractPlan.
func extractPlan(stdout string) *string {
	for raw := range strings.SplitSeq(stdout, "\n") {
		line := strings.Trim(raw, " ")
		lower := strings.ToLower(line)
		for _, kw := range []string{"plan", "tier", "subscription"} {
			if !strings.Contains(lower, kw) {
				continue
			}
			if i := strings.IndexByte(line, ':'); i >= 0 {
				if v := strings.Trim(line[i+1:], " "); v != "" {
					return &v
				}
			}
			if line == "" {
				return nil
			}
			return &line
		}
	}
	return nil
}

func consumeVerification(ctx context.Context, lines <-chan string) (verification, error) {
	var foundURL, foundCode *string
	for {
		select {
		case <-ctx.Done():
			return verification{}, ctx.Err()
		case line, ok := <-lines:
			if !ok {
				return verification{}, ErrVerifyURLMissing
			}
			if foundCode == nil {
				foundCode = extractUserCode(line)
			}
			if foundURL == nil {
				foundURL = extractVerifyURL(line)
			}
			if foundURL != nil {
				code := foundCode
				if code == nil {
					code = embeddedUserCode(*foundURL)
				}
				return verification{url: *foundURL, code: code}, nil
			}
		}
	}
}

func extractVerifyURL(line string) *string {
	for _, p := range verifyURLLinePrefixes {
		if strings.Contains(line, p) {
			if u := firstHTTPSToken(line); u != nil {
				return u
			}
		}
	}
	if u := firstHTTPSToken(line); u != nil && strings.Contains(*u, "nousresearch.com") {
		return u
	}
	return nil
}

func extractUserCode(line string) *string {
	for _, p := range userCodeLinePrefixes {
		if !strings.Contains(line, p) {
			continue
		}
		parts := strings.Split(line, p)
		fields := strings.Split(strings.TrimSpace(parts[len(parts)-1]), " ")
		token := strings.Trim(fields[0], ".,")
		if token != "" {
			return &token
		}
	}
	return nil
}

func embeddedUserCode(raw string) *string {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	if v, ok := u.Query()["user_code"]; ok && len(v) > 0 {
		return &v[0]
	}
	return nil
}

func firstHTTPSToken(line string) *string {
	for _, f := range strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' }) {
		if strings.HasPrefix(f, "https://") {
			t := strings.Trim(f, "\"'<>")
			return &t
		}
	}
	return nil
}
