package llm

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// ContainerHandle is the part of Swift's HermesContainerHandle the xAI OAuth
// path reads.
type ContainerHandle struct {
	BaseURL        string
	APIServerKey   string
	XAIConnectedAt *time.Time
}

// XAIContainerResolver finds a tenant's Hermes container for the SuperGrok
// linked-account path. Nil result means none. The container manager is wave 6,
// so wiring without one leaves that path off.
type XAIContainerResolver func(ctx context.Context, tenant uuid.UUID) *ContainerHandle

// OpenAICompatibleAdapter covers every provider speaking the OpenAI API:
// Together, Groq, Fireworks, DeepInfra, DeepSeek-direct, xAI, NVIDIA NIM, OpenAI,
// OpenRouter, Nous and custom endpoints.
type OpenAICompatibleAdapter struct {
	ProviderKind ProviderKind
	APIKey       string
	BaseURL      *url.URL
	Client       *http.Client
	// Credentials is optional. Without it only the deployment key is spent.
	Credentials CredentialSource
	XAIResolver XAIContainerResolver
}

var _ Adapter = (*OpenAICompatibleAdapter)(nil)

func (a *OpenAICompatibleAdapter) Kind() ProviderKind           { return a.ProviderKind }
func (a *OpenAICompatibleAdapter) AcceptsUserCredentials() bool { return a.Credentials != nil }

func (a *OpenAICompatibleAdapter) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

// ChatCompletions never sets stream; the payload passes through untouched.
func (a *OpenAICompatibleAdapter) ChatCompletions(ctx context.Context, payload []byte, _, _ string) (Metadata, error) {
	key, base, err := a.resolveCredentials(ctx)
	if err != nil {
		return Metadata{}, err
	}
	headers := [][2]string{{"Content-Type", "application/json"}}
	if key != "" {
		headers = append(headers, [2]string{"Authorization", "Bearer " + key})
	}
	headers = append(headers, a.identityHeaders()...)

	status, respHeaders, body, err := postBuffered(ctx, a.client(), Endpoint(a.ProviderKind, base), headers, payload, urlSessionTimeout)
	if err != nil {
		return Metadata{}, Network(a.ProviderKind, err)
	}
	if status >= 200 && status < 300 {
		return Metadata{Data: body, Headers: respHeaders}, nil
	}
	perr := Classify(a.ProviderKind, status, body)
	slog.ErrorContext(ctx, "llm upstream failed", "provider", a.ProviderKind, "reason", perr.ReasonCode(), "status", status)
	return Metadata{}, perr
}

// ChatStream is the native per-token SSE path: the same endpoint and auth with
// stream forced on.
func (a *OpenAICompatibleAdapter) ChatStream(ctx context.Context, payload []byte, _, _ string) Stream {
	return runStream(ctx, a.client(), a.ProviderKind, streamHeadTimeout, framingSSE,
		func(ctx context.Context) (streamRequest, error) {
			key, base, err := a.resolveCredentials(ctx)
			if err != nil {
				return streamRequest{}, err
			}
			headers := [][2]string{{"Accept", "text/event-stream"}}
			if key != "" {
				headers = append(headers, [2]string{"Authorization", "Bearer " + key})
			}
			headers = append(headers, a.identityHeaders()...)
			return streamRequest{url: Endpoint(a.ProviderKind, base), headers: headers, body: WithStreamFlag(payload)}, nil
		},
		func(record string, yield func(Chunk)) (bool, error) {
			return ProcessOpenAIRecord(record, a.ProviderKind, yield)
		})
}

// identityHeaders: OpenRouter attributes usage by these on shared keys.
func (a *OpenAICompatibleAdapter) identityHeaders() [][2]string {
	if a.ProviderKind != OpenRouter {
		return nil
	}
	return [][2]string{{"HTTP-Referer", "https://luminavault.app"}, {"X-Title", "LuminaVault"}}
}

// resolveCredentials: managed mode spends the platform key, BYOK spends the
// tenant's key or nothing at all. A BYOK path that cannot produce a tenant key
// fails closed rather than falling back to the deployment key, which was the
// cost leak. An undeclared mode keeps managed semantics.
func (a *OpenAICompatibleAdapter) resolveCredentials(ctx context.Context) (string, *url.URL, error) {
	values := RoutingValues(ctx)
	mode := values.CredentialMode
	if mode == ModeManaged {
		return a.APIKey, a.BaseURL, nil
	}
	if a.Credentials == nil || values.CurrentUser == nil {
		if mode == ModeBYOK {
			slog.ErrorContext(ctx, "byok request has no resolvable tenant; failing closed", "provider", a.ProviderKind)
			return "", nil, BYOKKeysRequiredError{}
		}
		return a.APIKey, a.BaseURL, nil
	}
	tenant := values.CurrentUser.ID
	creds, err := a.Credentials.Credential(ctx, a.ProviderKind, tenant)
	if err != nil {
		// Unreadable is not the same as absent, but the money decision is.
		slog.ErrorContext(ctx, "user credential lookup failed", "provider", a.ProviderKind, "error", err)
		if mode == ModeBYOK {
			return "", nil, BYOKKeysRequiredError{}
		}
		return a.APIKey, a.BaseURL, nil
	}
	if a.ProviderKind == XAI && a.XAIResolver != nil && (creds == nil || creds.APIKey == nil || *creds.APIKey == "") {
		if handle := a.XAIResolver(ctx, tenant); handle != nil && handle.XAIConnectedAt != nil {
			if base := parseOptionalURL(&handle.BaseURL); base != nil {
				return handle.APIServerKey, base, nil
			}
		}
	}
	if creds != nil && creds.APIKey != nil && *creds.APIKey != "" {
		if creds.BaseURL != nil {
			return *creds.APIKey, creds.BaseURL, nil
		}
		return *creds.APIKey, a.BaseURL, nil
	}
	if mode == ModeBYOK {
		slog.ErrorContext(ctx, "byok request has no usable credential; failing closed", "provider", a.ProviderKind)
		return "", nil, BYOKKeysRequiredError{}
	}
	return a.APIKey, a.BaseURL, nil
}

// Endpoint routes the chat-completions URL per provider. DeepInfra's default
// base URL already carries /v1/openai.
func Endpoint(kind ProviderKind, base *url.URL) string {
	if kind == DeepInfra {
		return joinURL(base, "chat", "completions")
	}
	return joinURL(base, "v1", "chat", "completions")
}

// DefaultBaseURL is OpenAICompatibleAdapter.defaultBaseURL.
func DefaultBaseURL(kind ProviderKind) *url.URL {
	switch kind {
	case Together:
		return mustParseURL("https://api.together.xyz")
	case Groq:
		return mustParseURL("https://api.groq.com")
	case Fireworks:
		return mustParseURL("https://api.fireworks.ai/inference")
	case DeepInfra:
		return mustParseURL("https://api.deepinfra.com/v1/openai")
	case DeepseekDirect:
		return mustParseURL("https://api.deepseek.com")
	case XAI:
		return mustParseURL("https://api.x.ai")
	case Nvidia:
		return mustParseURL("https://integrate.api.nvidia.com")
	case OpenAI:
		return mustParseURL("https://api.openai.com")
	case OpenRouter:
		return mustParseURL("https://openrouter.ai/api")
	case Nous:
		return mustParseURL("https://inference-api.nousresearch.com")
	}
	// .custom has no default: the tenant's base URL always resolves first.
	return mustParseURL("https://invalid.local")
}
