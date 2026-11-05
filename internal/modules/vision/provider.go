package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/swiftstr"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// VisionEmbedProviderKind raw values. Swift's fifth, hermesVision, can't be
// selected: the configured name is lowercased before it is compared.
const (
	kindCohere = "cohere"
	kindOpenAI = "openai"
	kindRepl   = "replicate"
	kindStub   = "stub"
)

const defaultCohereBaseURL = "https://api.cohere.com"

// cohereTimeout is URLSession.shared's request timeout.
const cohereTimeout = 60 * time.Second

// upstreamResult is VisionEmbedUpstreamResult.
type upstreamResult struct {
	embedding []float32
	model     string
	// 0 when the provider doesn't report it (`?? 0` in VisionEmbedService).
	sourceWidth, sourceHeight int
}

// adapter is VisionEmbedProviderAdapter. Failures are *providerError.
type adapter interface {
	embed(ctx context.Context, image []byte, mime string) (upstreamResult, error)
}

// providerError is VisionEmbedProviderError. Only .permanent has its own answer:
// transient, network and decode failures read the same to the caller.
type providerError struct {
	permanent bool
	detail    string
}

func (e *providerError) Error() string { return "vision embed provider: " + e.detail }

// Config is the vision.embed.* keys. App+build.swift registers Cohere only with
// a non-empty key, and the stub only when it is the configured provider.
type Config struct {
	Provider         string
	CohereAPIKey     string
	CohereBaseURL    string
	CohereModel      string
	StubDim          int
	StubFill         float64
	StubModel        string
	StubSourceWidth  int
	StubSourceHeight int
}

// registry is VisionEmbedProviderRegistry: one active adapter per boot, or none,
// which is the 503. The active kind and the registered adapters are independent:
// a Cohere key under VISION_EMBED_PROVIDER=openai still has no active adapter.
type registry struct {
	kind   string
	active adapter
}

func newRegistry(cfg Config, log *slog.Logger) registry {
	name := strings.ToLower(swiftstr.TrimWhitespacesAndNewlines(cfg.Provider))
	kind := name
	switch name {
	case kindCohere, kindOpenAI, kindRepl, kindStub:
	default:
		kind = kindCohere
	}
	reg := registry{kind: kind}
	switch {
	case kind == kindStub:
		reg.active = stubAdapter{
			dim: cfg.StubDim, fill: float32(cfg.StubFill), model: cfg.StubModel,
			sourceWidth: cfg.StubSourceWidth, sourceHeight: cfg.StubSourceHeight,
		}
	case kind == kindCohere && cfg.CohereAPIKey != "":
		reg.active = newCohereAdapter(cfg, log)
	}
	return reg
}

// stubAdapter is StubVisionEmbedAdapter: a fixed vector with no network.
type stubAdapter struct {
	dim                       int
	fill                      float32
	model                     string
	sourceWidth, sourceHeight int
}

func (a stubAdapter) embed(context.Context, []byte, string) (upstreamResult, error) {
	v := make([]float32, a.dim)
	for i := range v {
		v[i] = a.fill
	}
	return upstreamResult{embedding: v, model: a.model, sourceWidth: a.sourceWidth, sourceHeight: a.sourceHeight}, nil
}

// cohereAdapter is CohereImageEmbedAdapter: POST {base}/v2/embed with the image
// as a base64 data URI, one embedding back.
type cohereAdapter struct {
	apiKey   string
	endpoint string
	model    string
	client   *http.Client
	log      *slog.Logger
}

func newCohereAdapter(cfg Config, log *slog.Logger) cohereAdapter {
	base := cfg.CohereBaseURL
	if _, err := url.Parse(base); base == "" || err != nil {
		base = defaultCohereBaseURL
	}
	return cohereAdapter{
		apiKey:   cfg.CohereAPIKey,
		endpoint: strings.TrimRight(base, "/") + "/v2/embed",
		model:    cfg.CohereModel,
		client:   &http.Client{Timeout: cohereTimeout},
		log:      log,
	}
}

type cohereRequest struct {
	Model          string   `json:"model"`
	InputType      string   `json:"input_type"`
	EmbeddingTypes []string `json:"embedding_types"`
	Images         []string `json:"images"`
}

// cohereResponse uses pointers because Swift's [[Float]] refuses a missing key
// or a null, where encoding/json would read 0.
type cohereResponse struct {
	Embeddings *struct {
		Float *[][]*float32 `json:"float"`
	} `json:"embeddings"`
}

func (a cohereAdapter) embed(ctx context.Context, image []byte, mime string) (upstreamResult, error) {
	payload, err := json.Marshal(cohereRequest{
		Model: a.model, InputType: "image", EmbeddingTypes: []string{"float"},
		Images: []string{"data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image)},
	})
	if err != nil {
		return upstreamResult{}, &providerError{detail: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(payload))
	if err != nil {
		return upstreamResult{}, &providerError{detail: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return upstreamResult{}, &providerError{detail: "network: " + err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return upstreamResult{}, &providerError{detail: "network: " + err.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return upstreamResult{}, a.statusError(resp.StatusCode, data)
	}
	vec, err := decodeCohere(data)
	if err != nil {
		return upstreamResult{}, &providerError{detail: "decode: " + err.Error()}
	}
	return upstreamResult{embedding: vec, model: a.model}, nil
}

func (a cohereAdapter) statusError(status int, data []byte) *providerError {
	preview := data[:min(len(data), 512)]
	if !utf8.Valid(preview) {
		preview = []byte("<binary>")
	}
	if status == http.StatusTooManyRequests || (status >= 500 && status < 600) {
		a.log.Error("cohere embed transient", "status", status, "body", string(preview))
		return &providerError{detail: fmt.Sprintf("transient %d", status)}
	}
	a.log.Error("cohere embed permanent", "status", status, "body", string(preview))
	return &providerError{permanent: true, detail: fmt.Sprintf("permanent %d", status)}
}

// decodeCohere reads every vector, as Swift's Decodable does, and answers with
// the first.
func decodeCohere(data []byte) ([]float32, error) {
	var out cohereResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if out.Embeddings == nil || out.Embeddings.Float == nil {
		return nil, errors.New("no embeddings.float")
	}
	vectors := *out.Embeddings.Float
	for _, vec := range vectors {
		if vec == nil {
			return nil, errors.New("null vector")
		}
		for _, x := range vec {
			if x == nil {
				return nil, errors.New("null in vector")
			}
		}
	}
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return nil, errors.New("empty embedding")
	}
	first := make([]float32, len(vectors[0]))
	for i, x := range vectors[0] {
		first[i] = *x
	}
	return first, nil
}
