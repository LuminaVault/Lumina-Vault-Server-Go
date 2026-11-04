package llm

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// AnthropicAPIVersion pins the Messages API version.
const AnthropicAPIVersion = "2023-06-01"

// AnthropicAdapter translates an OpenAI chat-completions payload to Anthropic's
// Messages API and the reply back. See anthropic_translate.go.
//
// Managed trial: with Trial set, a request spending the platform key gets the
// trial's model, effort and thinking settings, and every failure is reshaped so
// the transport fails over rather than stopping (ManagedFailure). When the
// deployment has no registry key of its own, the trial key is reserved for the
// trial model: any other Anthropic model on the platform key fails exactly as it
// did before the trial, when that key was empty.
type AnthropicAdapter struct {
	APIKey      string
	BaseURL     *url.URL // nil means https://api.anthropic.com
	Client      *http.Client
	Credentials CredentialSource
	Trial       *TrialConfig
}

var _ Adapter = (*AnthropicAdapter)(nil)

func (a *AnthropicAdapter) Kind() ProviderKind           { return Anthropic }
func (a *AnthropicAdapter) AcceptsUserCredentials() bool { return a.Credentials != nil }

func (a *AnthropicAdapter) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

func (a *AnthropicAdapter) baseURL() *url.URL {
	if a.BaseURL != nil {
		return a.BaseURL
	}
	return DefaultAnthropicBaseURL()
}

func (a *AnthropicAdapter) ChatCompletions(ctx context.Context, payload []byte, _, _ string) (Metadata, error) {
	// Credentials first: whether the platform key is spent decides the model
	// default, thinking and effort, and the error mapping.
	creds, err := a.resolveCredentials(ctx)
	if err != nil {
		return Metadata{}, err
	}
	options := a.requestOptions(creds)
	translated, err := TranslateRequest(payload, false, options)
	if err != nil {
		return Metadata{}, err
	}
	if err := a.guardTrialKeyModel(translated.Model, creds); err != nil {
		return Metadata{}, err
	}
	bodyData, err := encodeJSON(translated.Body)
	if err != nil {
		return Metadata{}, Permanent(Anthropic, 400, strPtr("failed to serialize anthropic payload"))
	}

	headers := [][2]string{
		{"x-api-key", creds.key},
		{"anthropic-version", AnthropicAPIVersion},
		{"Content-Type", "application/json"},
	}
	status, respHeaders, body, err := postBuffered(ctx, a.client(), joinURL(creds.baseURL, "v1", "messages"), headers, bodyData, urlSessionTimeout)
	if err != nil {
		return Metadata{}, Network(Anthropic, err)
	}
	if status >= 200 && status < 300 {
		response, err := TranslateResponse(body, translated.Model, translated.StripJSONFences)
		if err != nil {
			return Metadata{}, err
		}
		logAnthropicUsage(ctx, response.Usage, translated.Model, options.Managed, false)
		data, err := encodeJSON(response.OpenAI)
		if err != nil {
			return Metadata{}, Transient(Anthropic, status, strPtr("failed to encode OpenAI response"))
		}
		return Metadata{Data: data, Headers: respHeaders}, nil
	}
	var perr *ProviderError
	if options.Managed {
		perr = ManagedFailure(status, body)
	} else {
		perr = Classify(Anthropic, status, body)
	}
	slog.ErrorContext(ctx, "anthropic upstream failed", "reason", perr.ReasonCode(), "status", status)
	return Metadata{}, perr
}

// ChatStream runs the Messages SSE protocol. The transport does not meter
// streams, so usage is logged here as anthropic_usage.
func (a *AnthropicAdapter) ChatStream(ctx context.Context, payload []byte, _, _ string) Stream {
	var state AnthropicStreamState
	managed := false
	upstream := runStream(ctx, a.client(), Anthropic, streamHeadTimeout, framingSSE,
		func(ctx context.Context) (streamRequest, error) {
			creds, err := a.resolveCredentials(ctx)
			if err != nil {
				return streamRequest{}, err
			}
			options := a.requestOptions(creds)
			managed = options.Managed
			translated, err := TranslateRequest(payload, true, options)
			if err != nil {
				return streamRequest{}, err
			}
			if err := a.guardTrialKeyModel(translated.Model, creds); err != nil {
				return streamRequest{}, err
			}
			state.Model = translated.Model
			bodyData, err := encodeJSON(translated.Body)
			if err != nil {
				return streamRequest{}, err
			}
			return streamRequest{
				url: joinURL(creds.baseURL, "v1", "messages"),
				headers: [][2]string{
					{"Accept", "text/event-stream"},
					{"x-api-key", creds.key},
					{"anthropic-version", AnthropicAPIVersion},
				},
				body: bodyData,
			}, nil
		},
		func(record string, yield func(Chunk)) (bool, error) {
			done, err := ProcessAnthropicRecord(record, &state, yield)
			if err == nil && done {
				logAnthropicUsage(ctx, &state.Usage, state.Model, managed, true)
			}
			return done, err
		})
	return func(yield func(Chunk, error) bool) {
		for chunk, err := range upstream {
			var perr *ProviderError
			if err != nil && managed && errors.As(err, &perr) {
				err = RemapManaged(perr)
			}
			if !yield(chunk, err) || err != nil {
				return
			}
		}
	}
}

type anthropicCredentials struct {
	key        string
	baseURL    *url.URL
	isPlatform bool // the deployment key is being spent
	isTrialKey bool // that key is the trial's own (no registry Anthropic key)
}

// resolveCredentials is the same rule as OpenAICompatibleAdapter's: managed
// spends the platform key, BYOK spends the tenant's or fails closed.
func (a *AnthropicAdapter) resolveCredentials(ctx context.Context) (anthropicCredentials, error) {
	values := RoutingValues(ctx)
	mode := values.CredentialMode
	if mode == ModeManaged {
		return a.platformCredentials(), nil
	}
	if a.Credentials == nil || values.CurrentUser == nil {
		if mode == ModeBYOK {
			slog.ErrorContext(ctx, "byok request for anthropic has no resolvable tenant; failing closed")
			return anthropicCredentials{}, BYOKKeysRequiredError{}
		}
		return a.platformCredentials(), nil
	}
	creds, err := a.Credentials.Credential(ctx, Anthropic, values.CurrentUser.ID)
	if err != nil {
		slog.ErrorContext(ctx, "user credential lookup failed for anthropic", "error", err)
		if mode == ModeBYOK {
			return anthropicCredentials{}, BYOKKeysRequiredError{}
		}
		return a.platformCredentials(), nil
	}
	if creds != nil && creds.APIKey != nil && *creds.APIKey != "" {
		base := creds.BaseURL
		if base == nil {
			base = a.baseURL()
		}
		return anthropicCredentials{key: *creds.APIKey, baseURL: base}, nil
	}
	if mode == ModeBYOK {
		slog.ErrorContext(ctx, "byok request for anthropic has no usable credential; failing closed")
		return anthropicCredentials{}, BYOKKeysRequiredError{}
	}
	return a.platformCredentials(), nil
}

// platformCredentials: the registry key when the deployment has one, otherwise
// the trial key.
func (a *AnthropicAdapter) platformCredentials() anthropicCredentials {
	if a.APIKey == "" && a.Trial != nil {
		return anthropicCredentials{key: a.Trial.APIKey, baseURL: a.baseURL(), isPlatform: true, isTrialKey: true}
	}
	return anthropicCredentials{key: a.APIKey, baseURL: a.baseURL(), isPlatform: true}
}

func (a *AnthropicAdapter) requestOptions(creds anthropicCredentials) AnthropicRequestOptions {
	if !creds.isPlatform || a.Trial == nil {
		return AnthropicRequestOptions{}
	}
	return AnthropicRequestOptions{Managed: true, Trial: a.Trial}
}

// guardTrialKeyModel: before the trial the platform key was empty, so any managed
// Anthropic route got a 401. The trial key must not quietly start paying for
// those at Sonnet and Opus rates; they keep failing the same way.
func (a *AnthropicAdapter) guardTrialKeyModel(model string, creds anthropicCredentials) error {
	if !creds.isTrialKey || a.Trial == nil || model == a.Trial.Model {
		return nil
	}
	return Permanent(Anthropic, 401, strPtr("anthropic platform key is reserved for the managed trial model"))
}

// ManagedFailure is the failover rule for platform-paid traffic: every failure
// hands the request to the next candidate. 401, 402, 403 and a 400 naming the
// usage limit or credit balance (how a workspace spend cap surfaces) are billing
// or auth. Everything else, including 400, 404, 429, 5xx and 529, is transient.
// BYOK keeps Classify.
func ManagedFailure(status int, body []byte) *ProviderError {
	preview := bodyPreview(body)
	lower := ""
	if preview != nil {
		lower = strings.ToLower(*preview)
	}
	switch {
	case status == 401, status == 402, status == 403:
		return CreditExhausted(Anthropic, status, preview)
	case status == 400 && (strings.Contains(lower, "usage limit") || strings.Contains(lower, "credit balance")):
		return CreditExhausted(Anthropic, status, preview)
	}
	return Transient(Anthropic, status, preview)
}

// RemapManaged is the streaming counterpart: runStream has already used the
// shared classifier, so re-map the non-recoverable ones.
func RemapManaged(err *ProviderError) *ProviderError {
	if err.Kind != KindPermanent {
		return err
	}
	var body []byte
	if err.Body != nil {
		body = []byte(*err.Body)
	}
	return ManagedFailure(err.Status, body)
}

func logAnthropicUsage(ctx context.Context, usage *AnthropicUsage, model string, managed, streamed bool) {
	if usage == nil {
		return
	}
	slog.InfoContext(ctx, "anthropic_usage",
		"event", "anthropic_usage",
		"model", model,
		"managed", managed,
		"streamed", streamed,
		"input_tokens", usage.InputTokens,
		"output_tokens", usage.OutputTokens,
		"cache_read_input_tokens", usage.CacheReadInputTokens,
		"cache_creation_input_tokens", usage.CacheCreationInputTokens,
		"est_cost_usd_micros", usage.EstimatedCostUsdMicros(),
	)
}
