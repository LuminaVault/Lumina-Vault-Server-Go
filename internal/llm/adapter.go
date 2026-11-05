package llm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Adapter is ProviderAdapter: one upstream behind the OpenAI wire shape. It
// returns *ProviderError on upstream failure so the transport can classify the
// failover, and never retries across providers itself.
//
// sessionKey is the tenant UUID string, forwarded by Hermes-style adapters.
// sessionID is the optional conversation id; empty means none.
type Adapter interface {
	Kind() ProviderKind
	ChatCompletions(ctx context.Context, payload []byte, sessionKey, sessionID string) (Metadata, error)
	ChatStream(ctx context.Context, payload []byte, sessionKey, sessionID string) Stream
	// AcceptsUserCredentials reports whether this instance can spend a tenant's
	// own key. Surfaced so the wiring is assertable: a provider the UI offers a
	// key field for must have a store behind it.
	AcceptsUserCredentials() bool
}

// BufferedChatStream is the default chatStream for an adapter without native
// streaming: buffer the completion and emit one terminal chunk.
func BufferedChatStream(ctx context.Context, a Adapter, payload []byte, sessionKey, sessionID string) Stream {
	return bufferedStream(func() ([]byte, error) {
		m, err := a.ChatCompletions(ctx, payload, sessionKey, sessionID)
		return m.Data, err
	})
}

// ResolvedCredential is UserCredentialStore.ResolvedCredential. Key nil means
// no secret; BaseURL nil means no override.
type ResolvedCredential struct {
	APIKey  *string
	BaseURL *url.URL
	Label   *string
}

// CredentialSource resolves a tenant's credential for a provider. A nil result
// with a nil error means no row. Implemented by CredentialStore.
type CredentialSource interface {
	Credential(ctx context.Context, provider ProviderKind, tenant uuid.UUID) (*ResolvedCredential, error)
}

// urlSessionTimeout stands in for URLSession.shared's 60 s request timeout. A
// buffered completion sends nothing until it is done, so URLSession's idle timer
// and this whole-request one fire at the same moment in practice.
const urlSessionTimeout = 60 * time.Second

// postBuffered sends one buffered POST and reads the whole response. It keeps
// the timeout running through the body read, so a timeout surfaces as a timeout.
// transportErr is non-nil when no HTTP response was read in full.
func postBuffered(ctx context.Context, client *http.Client, target string, headers [][2]string, payload []byte, timeout time.Duration) (status int, respHeaders map[string]string, body []byte, transportErr error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, nil, err
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	respHeaders = make(map[string]string, len(resp.Header))
	for k, v := range resp.Header {
		respHeaders[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	return resp.StatusCode, respHeaders, body, nil
}

func joinURL(base *url.URL, elem ...string) string { return base.JoinPath(elem...).String() }

func mustParseURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

// parseOptionalURL is URL(string:) over an optional string: nil when absent or
// unparseable.
func parseOptionalURL(s *string) *url.URL {
	if s == nil || *s == "" {
		return nil
	}
	u, err := url.Parse(*s)
	if err != nil {
		return nil
	}
	return u
}

func isByokRequired(err error) bool {
	var byok BYOKKeysRequiredError
	return errors.As(err, &byok)
}
