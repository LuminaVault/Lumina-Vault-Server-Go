package llm

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

const (
	// HermesRequestTimeout is the per-request timeout for gateway completions.
	HermesRequestTimeout = 90 * time.Second
	hermesRetryDelay     = 2 * time.Second
)

// NoRedirectClient answers a redirect with the 3xx itself, as BYOHTTP.session
// does: a BYO endpoint must not bounce the call to an internal host after the
// endpoint was validated.
func NoRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// HermesGatewayAdapter wraps the in-VPS Hermes gateway. A user override in
// Values.CurrentResolution sends the call to the tenant's own gateway instead,
// with its Authorization header.
type HermesGatewayAdapter struct {
	BaseURL *url.URL
	// DefaultAuthHeader is the managed gateway's full Authorization value.
	DefaultAuthHeader string
	// Client is used for buffered calls; nil means NoRedirectClient.
	Client *http.Client
	// StreamClient is used for streams; nil means http.DefaultClient. Swift
	// streams on HTTPClient.shared, which follows redirects, so unlike the
	// buffered path the stream has no redirect guard. Reproduced as is.
	StreamClient *http.Client
	// RequestTimeout and RetryDelay default to 90 s and 2 s.
	RequestTimeout time.Duration
	RetryDelay     time.Duration
}

var _ Adapter = (*HermesGatewayAdapter)(nil)

func (a *HermesGatewayAdapter) Kind() ProviderKind           { return HermesGateway }
func (a *HermesGatewayAdapter) AcceptsUserCredentials() bool { return false }

func (a *HermesGatewayAdapter) dispatchTarget(ctx context.Context) (*url.URL, string) {
	if r := RoutingValues(ctx).CurrentResolution; r != nil && r.IsUserOverride {
		if r.AuthHeader == nil {
			return r.BaseURL, ""
		}
		return r.BaseURL, *r.AuthHeader
	}
	return a.BaseURL, a.DefaultAuthHeader
}

// ChatCompletions retries once on a timeout for a non-streamed payload, with 2 s
// between attempts. A streamed payload never retries: partial output may have
// shipped. This is on top of the transport's failover, so a timed-out request
// can make up to two attempts per candidate.
func (a *HermesGatewayAdapter) ChatCompletions(ctx context.Context, payload []byte, sessionKey, sessionID string) (Metadata, error) {
	base, auth := a.dispatchTarget(ctx)
	m, err := a.dispatch(ctx, payload, sessionKey, sessionID, base, auth)
	var perr *ProviderError
	if !errors.As(err, &perr) || perr.Kind != KindNetwork || networkKindOf(perr.Underlying) != netTimeout || isStreamPayload(payload) {
		return m, err
	}
	slog.WarnContext(ctx, "hermes upstream timed out; retrying once")
	delay := a.RetryDelay
	if delay == 0 {
		delay = hermesRetryDelay
	}
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return Metadata{}, ctx.Err()
	}
	return a.dispatch(ctx, payload, sessionKey, sessionID, base, auth)
}

func (a *HermesGatewayAdapter) dispatch(ctx context.Context, payload []byte, sessionKey, sessionID string, base *url.URL, auth string) (Metadata, error) {
	headers := [][2]string{{"Content-Type", "application/json"}, {"X-Hermes-Session-Key", sessionKey}}
	if sessionID != "" {
		headers = append(headers, [2]string{"X-Hermes-Session-Id", sessionID})
	}
	if auth != "" {
		headers = append(headers, [2]string{"Authorization", auth})
	}
	client := a.Client
	if client == nil {
		client = NoRedirectClient()
	}
	timeout := a.RequestTimeout
	if timeout == 0 {
		timeout = HermesRequestTimeout
	}
	status, respHeaders, body, err := postBuffered(ctx, client, joinURL(base, "v1", "chat", "completions"), headers, payload, timeout)
	if err != nil {
		return Metadata{}, Network(HermesGateway, err)
	}
	if status >= 200 && status < 300 {
		return Metadata{Data: body, Headers: respHeaders}, nil
	}
	perr := Classify(HermesGateway, status, body)
	slog.ErrorContext(ctx, "hermes upstream failed", "reason", perr.ReasonCode(), "status", status)
	return Metadata{}, perr
}

// ChatStream honours the same BYO override as the buffered path. No retry.
func (a *HermesGatewayAdapter) ChatStream(ctx context.Context, payload []byte, sessionKey, sessionID string) Stream {
	client := a.StreamClient
	if client == nil {
		client = http.DefaultClient
	}
	return runStream(ctx, client, HermesGateway, streamHeadTimeout, framingSSE,
		func(ctx context.Context) (streamRequest, error) {
			base, auth := a.dispatchTarget(ctx)
			headers := [][2]string{{"Accept", "text/event-stream"}, {"X-Hermes-Session-Key", sessionKey}}
			if sessionID != "" {
				headers = append(headers, [2]string{"X-Hermes-Session-Id", sessionID})
			}
			if auth != "" {
				headers = append(headers, [2]string{"Authorization", auth})
			}
			return streamRequest{url: joinURL(base, "v1", "chat", "completions"), headers: headers, body: WithStreamFlag(payload)}, nil
		},
		func(record string, yield func(Chunk)) (bool, error) {
			return ProcessOpenAIRecord(record, HermesGateway, yield)
		})
}

func isStreamPayload(payload []byte) bool {
	dict, ok := decodeObject(payload)
	if !ok {
		return false
	}
	stream, _ := dict["stream"].(bool)
	return stream
}
