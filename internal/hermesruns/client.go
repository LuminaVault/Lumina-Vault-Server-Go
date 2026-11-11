package hermesruns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/hermesresolve"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClientErrorKind is the case of HermesRunsClientError.
type ClientErrorKind int

const (
	ErrUnsupported ClientErrorKind = iota
	ErrRunExpired
	ErrApprovalNotPendingUpstream
	ErrUpstream
	ErrInvalidResponse
	ErrResponseTooLarge
	ErrStreamIdle
)

// ClientError is HermesRunsClientError.
type ClientError struct {
	Kind   ClientErrorKind
	Status int
	Code   *string
	Detail string
}

func (e *ClientError) Error() string {
	switch e.Kind {
	case ErrUpstream:
		code := "nil"
		if e.Code != nil {
			code = fmt.Sprintf("%q", *e.Code)
		}
		return fmt.Sprintf("upstream(status: %d, code: %s)", e.Status, code)
	case ErrInvalidResponse:
		return fmt.Sprintf("invalidResponse(%q)", e.Detail)
	}
	return e.StableCode()
}

// StableCode is HermesRunsClientError.stableCode.
func (e *ClientError) StableCode() string {
	switch e.Kind {
	case ErrUnsupported:
		return "hermes_runs_unsupported"
	case ErrRunExpired:
		return "hermes_run_expired"
	case ErrApprovalNotPendingUpstream:
		return "hermes_approval_not_pending"
	case ErrUpstream:
		return "hermes_runs_upstream_error"
	case ErrInvalidResponse:
		return "hermes_runs_invalid_response"
	case ErrResponseTooLarge:
		return "hermes_runs_response_too_large"
	}
	return "hermes_runs_stream_idle"
}

// HTTPStatus is HermesRunsController.status(for: HermesRunsClientError).
func (e *ClientError) HTTPStatus() int {
	switch e.Kind {
	case ErrUnsupported:
		return http.StatusNotImplemented
	case ErrRunExpired:
		return http.StatusGone
	case ErrApprovalNotPendingUpstream:
		return http.StatusConflict
	}
	return http.StatusBadGateway
}

func isClientErr(err error, kind ClientErrorKind) bool {
	var ce *ClientError
	return errors.As(err, &ce) && ce.Kind == kind
}

// Capabilities is HermesRunsCapabilities.
type Capabilities struct {
	ApprovalEvents, RunEventsSSE bool
}

// SupportsRuns needs both: without approval events the phone can't act, without
// SSE the watcher can't follow.
func (c Capabilities) SupportsRuns() bool { return c.ApprovalEvents && c.RunEventsSSE }

// ParseCapabilities is HermesRunsClient.parseCapabilities.
func ParseCapabilities(data []byte) Capabilities {
	v, ok := decodeJSON(data)
	object, isObject := v.(map[string]any)
	if !ok || !isObject {
		return Capabilities{}
	}
	features, ok := object["features"].(map[string]any)
	if !ok {
		features = object
	}
	approval, _ := features["approval_events"].(bool)
	sse, _ := features["run_events_sse"].(bool)
	return Capabilities{ApprovalEvents: approval, RunEventsSSE: sse}
}

// Client is HermesRunsClient: /v1/runs on one resolved Hermes endpoint.
type Client struct {
	BaseURL *url.URL
	// AuthHeader is the full Authorization value; empty sends none.
	AuthHeader string
	// SessionKey is X-Hermes-Session-Key; empty sends none.
	SessionKey string
	HTTP       *http.Client
	// RequestTimeout (30 s), StreamIdleTimeout (120 s) and MaxResponseBytes (4 MiB)
	// default when zero.
	RequestTimeout    time.Duration
	StreamIdleTimeout time.Duration
	MaxResponseBytes  int
	Log               *slog.Logger
	Now               func() time.Time
}

// NoRedirectClient is BYOHTTP.httpClient: a BYO endpoint must never bounce a
// bearer through a redirect.
func NoRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// MakeClient is HermesRunsClient.make: a BYO override carries its own full
// Authorization value; the managed gateway uses the central bearer.
func MakeClient(res hermesresolve.Resolution, managedAPIKey, sessionKey string, httpClient *http.Client) *Client {
	auth := ""
	if res.IsUserOverride {
		if res.AuthHeader != nil {
			auth = *res.AuthHeader
		}
	} else if managedAPIKey != "" {
		auth = "Bearer " + managedAPIKey
	}
	return &Client{BaseURL: res.BaseURL, AuthHeader: auth, SessionKey: sessionKey, HTTP: httpClient}
}

func (c *Client) requestTimeout() time.Duration {
	if c.RequestTimeout > 0 {
		return c.RequestTimeout
	}
	return 30 * time.Second
}

func (c *Client) idleTimeout() time.Duration {
	if c.StreamIdleTimeout > 0 {
		return c.StreamIdleTimeout
	}
	return 120 * time.Second
}

func (c *Client) maxBytes() int {
	if c.MaxResponseBytes > 0 {
		return c.MaxResponseBytes
	}
	return 4 * 1024 * 1024
}

func (c *Client) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return NoRedirectClient()
}

// URLKey is baseURL.absoluteString, the capability cache key.
func (c *Client) URLKey() string { return c.BaseURL.String() }

// endpoint is baseURL.appendingPathComponent(path).
func (c *Client) endpoint(path string) string {
	base := c.BaseURL.String()
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + path
}

// pathRunID escapes a run id as one path component.
func pathRunID(id string) string { return url.PathEscape(id) }

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.AuthHeader != "" {
		req.Header.Set("Authorization", c.AuthHeader)
	}
	if c.SessionKey != "" {
		req.Header.Set("X-Hermes-Session-Key", c.SessionKey)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// send runs one unary call and collects the body, failing past MaxResponseBytes.
func (c *Client) send(ctx context.Context, method, path string, body any) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	limit := c.maxBytes()
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return 0, nil, err
	}
	if len(data) > limit {
		return 0, nil, &ClientError{Kind: ErrResponseTooLarge}
	}
	return resp.StatusCode, data, nil
}

func ok2xx(status int) bool { return status >= 200 && status < 300 }

// Capabilities is GET /v1/capabilities.
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	status, body, err := c.send(ctx, http.MethodGet, "v1/capabilities", nil)
	if err != nil {
		return Capabilities{}, err
	}
	if !ok2xx(status) {
		return Capabilities{}, &ClientError{Kind: ErrUpstream, Status: status}
	}
	return ParseCapabilities(body), nil
}

// RequireRunsSupport throws unsupported unless the gateway can run, approve and stream.
func (c *Client) RequireRunsSupport(ctx context.Context) error {
	caps, err := c.Capabilities(ctx)
	if err != nil {
		return err
	}
	if !caps.SupportsRuns() {
		return &ClientError{Kind: ErrUnsupported}
	}
	return nil
}

// Start is POST /v1/runs → 202 {run_id}.
func (c *Client) Start(ctx context.Context, prompt string, sessionID, model *string) (string, error) {
	body := map[string]any{"input": prompt}
	if sessionID != nil && *sessionID != "" {
		body["session_id"] = *sessionID
	}
	if model != nil && *model != "" {
		body["model"] = *model
	}
	object, err := c.decodeObject(ctx, http.MethodPost, "v1/runs", body, "start", "")
	if err != nil {
		return "", err
	}
	id, _ := object["run_id"].(string)
	if id == "" {
		return "", &ClientError{Kind: ErrInvalidResponse, Detail: "run_id missing from POST /v1/runs"}
	}
	return id, nil
}

// Status is GET /v1/runs/{id}.
func (c *Client) Status(ctx context.Context, runID string) (Snapshot, error) {
	object, err := c.decodeObject(ctx, http.MethodGet, "v1/runs/"+pathRunID(runID), nil, "status", runID)
	if err != nil {
		return Snapshot{}, err
	}
	status, ok := object["status"].(string)
	if !ok {
		return Snapshot{}, &ClientError{Kind: ErrInvalidResponse, Detail: "status missing from GET /v1/runs/" + runID}
	}
	return Snapshot{
		Status: status, LastEvent: strPtr(object["last_event"]), Output: strPtr(object["output"]),
		Error: strPtr(object["error"]), SessionID: strPtr(object["session_id"]),
	}, nil
}

// Approve is POST /v1/runs/{id}/approval {choice}.
func (c *Client) Approve(ctx context.Context, runID string, choice Choice) error {
	_, err := c.decodeObject(ctx, http.MethodPost, "v1/runs/"+pathRunID(runID)+"/approval", map[string]any{"choice": string(choice)}, "approve", runID)
	return err
}

// Stop is POST /v1/runs/{id}/stop. Hermes answers `stopping`; run.cancelled
// follows on the stream.
func (c *Client) Stop(ctx context.Context, runID string) error {
	_, err := c.decodeObject(ctx, http.MethodPost, "v1/runs/"+pathRunID(runID)+"/stop", nil, "stop", runID)
	return err
}

func (c *Client) decodeObject(ctx context.Context, method, path string, body any, operation, runID string) (map[string]any, error) {
	status, data, err := c.send(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if !ok2xx(status) {
		failure := mapFailure(status, data)
		c.log().WarnContext(ctx, "hermes runs "+operation+" failed", "status", status, "code", failure.StableCode())
		return nil, failure
	}
	v, ok := decodeJSON(data)
	object, isObject := v.(map[string]any)
	if !ok || !isObject {
		return nil, &ClientError{Kind: ErrInvalidResponse, Detail: "non-object body from " + operation}
	}
	return object, nil
}

// mapFailure reads Hermes' {"error": {"message", "code"}} envelope.
func mapFailure(status int, body []byte) *ClientError {
	var code *string
	if v, ok := decodeJSON(body); ok {
		if object, ok := v.(map[string]any); ok {
			if inner, ok := object["error"].(map[string]any); ok {
				code = strPtr(inner["code"])
			}
			if code == nil {
				code = strPtr(object["code"])
			}
		}
	}
	c := ""
	if code != nil {
		c = *code
	}
	switch {
	case status == http.StatusNotFound || c == "run_not_found":
		return &ClientError{Kind: ErrRunExpired}
	case status == http.StatusConflict || c == "approval_not_pending" || c == "approval_not_active":
		return &ClientError{Kind: ErrApprovalNotPendingUpstream}
	}
	return &ClientError{Kind: ErrUpstream, Status: status, Code: code}
}

// Events is GET /v1/runs/{id}/events as typed frames, delivered to yield in
// order. It returns nil when Hermes closes the stream, or the stall, transport
// or rejection error. Canceling ctx cancels the request.
func (c *Client) Events(ctx context.Context, runID string, yield func(Frame)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodGet, "v1/runs/"+pathRunID(runID)+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	// The request timeout bounds the head only; the idle watchdog bounds the body.
	headTimer := time.AfterFunc(c.requestTimeout(), cancel)
	resp, err := c.httpClient().Do(req)
	headTimer.Stop()
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if !ok2xx(resp.StatusCode) {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, int64(c.maxBytes())+1))
		return mapFailure(resp.StatusCode, data)
	}

	idle := c.idleTimeout()
	activity := make(chan struct{}, 1)
	stalled := make(chan struct{})
	go func() {
		timer := time.NewTimer(idle)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-activity:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idle)
			case <-timer.C:
				close(stalled)
				cancel()
				return
			}
		}
	}()

	maxBytes := c.maxBytes() * 16
	total := 0
	var parser Parser
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			select {
			case activity <- struct{}{}:
			default:
			}
			total += n
			if total > maxBytes {
				return &ClientError{Kind: ErrResponseTooLarge}
			}
			for _, record := range parser.Feed(string(buf[:n])) {
				if frame, ok := DecodeFrame(record.Event, record.Data, c.now()); ok {
					yield(frame)
				}
			}
		}
		if readErr != nil {
			select {
			case <-stalled:
				return &ClientError{Kind: ErrStreamIdle}
			default:
			}
			if errors.Is(readErr, io.EOF) {
				if record, ok := parser.Flush(); ok {
					if frame, ok := DecodeFrame(record.Event, record.Data, c.now()); ok {
						yield(frame)
					}
				}
				return nil
			}
			return readErr
		}
	}
}
