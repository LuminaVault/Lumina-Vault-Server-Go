package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"lumina/internal/swiftstr"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

const (
	openAIDefaultBaseURL = "https://api.openai.com"
	openAIDefaultModel   = "text-embedding-3-small"
	openRouterBaseURL    = "https://openrouter.ai/api"
	openRouterModel      = "openai/text-embedding-3-small"
	nomicDefaultBaseURL  = "https://api-atlas.nomic.ai"
	nomicDefaultModel    = "nomic-embed-text-v1.5"
	hermesDefaultModel   = "nomic-embed-text-v1.5"

	// defaultMaxInputCharacters caps an OpenAI-wire input: text-embedding-3-small
	// rejects over 8191 tokens with a 400 and whole-document embeds aren't chunked.
	defaultMaxInputCharacters = 16000

	// maxResponseBytes bounds a provider answer. A real one is a few tens of KB;
	// anything cut off fails to decode, which is the honest result.
	maxResponseBytes = 16 << 20
)

// defaultHTTPClient stands in for URLSession.shared; 60 s is its request timeout.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second}
}

// parseBaseURL is `URL(string: raw) ?? fallback` on a trimmed value.
func parseBaseURL(raw, fallback string) *url.URL {
	raw = swiftstr.TrimWhitespacesAndNewlines(raw)
	if raw != "" {
		if u, err := url.Parse(raw); err == nil {
			return u
		}
	}
	u, err := url.Parse(fallback)
	if err != nil {
		panic(err) // fallback is one of the constants above
	}
	return u
}

// vector decodes as Swift's [Float] does: a null, a null element or a number
// that doesn't fit fails, where plain []float32 would quietly store a zero. A
// missing key leaves it nil, so callers can fail that too.
type vector []float32

func (v *vector) UnmarshalJSON(b []byte) error {
	var raw []*float32
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if raw == nil {
		return errors.New("embedding is null")
	}
	out := make(vector, len(raw))
	for i, f := range raw {
		if f == nil {
			return errors.New("embedding element is null")
		}
		out[i] = *f
	}
	*v = out
	return nil
}

// postJSON sends the request shared by every adapter. A transport failure, or
// a body that can't be read, is .network, as URLSession.data throwing is.
func postJSON(ctx context.Context, client *http.Client, target, apiKey, label string, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, &Error{Kind: FailureNetwork, Reason: fmt.Sprintf("%s: %v", label, err)}
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req) //nolint:gosec // G704: the target is operator config or the tenant's own container, never request input
	if err != nil {
		return 0, nil, &Error{Kind: FailureNetwork, Reason: fmt.Sprintf("%s: %v", label, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, &Error{Kind: FailureNetwork, Reason: fmt.Sprintf("%s: %v", label, err)}
	}
	return resp.StatusCode, data, nil
}

var errDecodeFailed = &Error{Kind: FailurePermanent, Permanent: ReasonDecodeFailed}

type openAIItem struct {
	Embedding vector `json:"embedding"`
}

// decodeOpenAIData decodes the `data` array and returns the first embedding. A
// missing or empty array, or any item without an embedding, is decodeFailed.
func decodeOpenAIData(items []openAIItem) (vector, error) {
	if len(items) == 0 {
		return nil, errDecodeFailed
	}
	for _, item := range items {
		if item.Embedding == nil {
			return nil, errDecodeFailed
		}
	}
	return items[0].Embedding, nil
}

// openAIService is OpenAIEmbeddingService, which also backs the openRouter kind:
// OpenRouter serves the same wire format, so only the base URL, the model slug and
// the label differ.
type openAIService struct {
	apiKey   string
	baseURL  *url.URL
	model    string
	label    string
	maxInput int
	client   *http.Client
	// usage receives the response's usage.total_tokens. The registry leaves it
	// nil, as Swift's does.
	usage func(ctx context.Context, tenant uuid.UUID, tokens int64)
}

func (s *openAIService) Embed(ctx context.Context, text string, tenant uuid.UUID) ([]float32, error) {
	if s.apiKey == "" {
		return nil, &Error{Kind: FailurePermanent, Permanent: ReasonMissingAPIKey}
	}
	input := text
	if swiftstr.Count(text) > s.maxInput {
		input = swiftstr.Prefix(text, s.maxInput)
	}
	status, data, err := postJSON(ctx, s.client, s.baseURL.JoinPath("v1/embeddings").String(), s.apiKey, s.label, struct {
		Model          string `json:"model"`
		Input          string `json:"input"`
		Dimensions     int    `json:"dimensions"`
		EncodingFormat string `json:"encoding_format"`
	}{s.model, input, Dimensions, "float"})
	if err != nil {
		return nil, err
	}
	if e := errorFromHTTP(status, data, s.label); e != nil {
		return nil, e
	}
	var decoded struct {
		Data  []openAIItem `json:"data"`
		Usage *struct {
			TotalTokens *int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &decoded) != nil || (decoded.Usage != nil && decoded.Usage.TotalTokens == nil) {
		return nil, errDecodeFailed
	}
	first, err := decodeOpenAIData(decoded.Data)
	if err != nil {
		return nil, err
	}
	if len(first) != Dimensions {
		return nil, &Error{Kind: FailureDimMismatch, Expected: Dimensions, Got: len(first)}
	}
	if s.usage != nil && decoded.Usage != nil {
		s.usage(ctx, tenant, *decoded.Usage.TotalTokens)
	}
	return first, nil
}

// nomicService is NomicEmbeddingService. nomic-embed-text-v1.5 answers 768 dims,
// zero-padded so the vector fits the column.
type nomicService struct {
	apiKey  string
	baseURL *url.URL
	model   string
	client  *http.Client
}

func (s *nomicService) Embed(ctx context.Context, text string, _ uuid.UUID) ([]float32, error) {
	if s.apiKey == "" {
		return nil, &Error{Kind: FailurePermanent, Permanent: ReasonMissingAPIKey}
	}
	status, data, err := postJSON(ctx, s.client, s.baseURL.JoinPath("v1/embedding/text").String(), s.apiKey, "nomic", struct {
		Model string   `json:"model"`
		Texts []string `json:"texts"`
	}{s.model, []string{text}})
	if err != nil {
		return nil, err
	}
	if e := errorFromHTTP(status, data, "nomic"); e != nil {
		return nil, e
	}
	var decoded struct {
		Embeddings []vector `json:"embeddings"`
	}
	if json.Unmarshal(data, &decoded) != nil || len(decoded.Embeddings) == 0 {
		return nil, errDecodeFailed
	}
	return padToTarget(decoded.Embeddings[0]), nil
}

// padToTarget is NomicEmbeddingService.padToTarget: zero-pad up to Dimensions,
// pass an exact width through. An oversize vector is truncated, whatever the
// Swift doc comment says about dimMismatch; Swift's test pins the truncation.
func padToTarget(v []float32) []float32 {
	if len(v) >= Dimensions {
		return v[:Dimensions]
	}
	padded := make([]float32, Dimensions)
	copy(padded, v)
	return padded
}

// HermesHandle is what LocalHermesEmbeddingService reads from a
// HermesContainerHandle.
type HermesHandle struct {
	BaseURL      string
	APIServerKey string
}

// HandleResolver finds the tenant's running Hermes container, read-only. A nil
// handle means no container yet.
type HandleResolver func(ctx context.Context, tenant uuid.UUID) (*HermesHandle, error)

// hermesLocalService is LocalHermesEmbeddingService: the tenant container's
// OpenAI-shaped endpoint. Unlike Nomic it refuses an oversize vector.
type hermesLocalService struct {
	resolve HandleResolver
	model   string
	client  *http.Client
}

func (s *hermesLocalService) Embed(ctx context.Context, text string, tenant uuid.UUID) ([]float32, error) {
	endpointMissing := &Error{Kind: FailurePermanent, Permanent: ReasonEndpointMissing}
	if s.resolve == nil {
		return nil, endpointMissing
	}
	handle, err := s.resolve(ctx, tenant)
	if err != nil {
		return nil, &Error{Kind: FailureNetwork, Reason: fmt.Sprintf("hermesLocal handle: %v", err)}
	}
	if handle == nil {
		return nil, endpointMissing
	}
	target := handle.BaseURL + "/v1/embeddings"
	if _, err := url.Parse(target); err != nil {
		return nil, endpointMissing
	}
	status, data, err := postJSON(ctx, s.client, target, handle.APIServerKey, "hermesLocal", struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}{s.model, text})
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, &Error{Kind: FailurePermanent, Permanent: ReasonEndpointMissing, Response: newProviderResponse(status, data)}
	}
	if e := errorFromHTTP(status, data, "hermesLocal"); e != nil {
		return nil, e
	}
	var decoded struct {
		Data []openAIItem `json:"data"`
	}
	if json.Unmarshal(data, &decoded) != nil {
		return nil, errDecodeFailed
	}
	first, err := decodeOpenAIData(decoded.Data)
	if err != nil {
		return nil, err
	}
	if len(first) > Dimensions {
		return nil, &Error{Kind: FailureDimMismatch, Expected: Dimensions, Got: len(first)}
	}
	return padToTarget(first), nil
}
