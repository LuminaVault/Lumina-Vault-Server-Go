package embedding

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// MARK: OpenAIEmbeddingServiceTests

func openAIAt(t *testing.T, status int, body string) (*openAIService, *captured) {
	t.Helper()
	base, c := stub(t, status, body)
	return &openAIService{
		apiKey: "sk-test", baseURL: base, model: openAIDefaultModel, label: "openai",
		maxInput: defaultMaxInputCharacters, client: defaultHTTPClient(),
	}, c
}

func TestOpenAIRequestShape(t *testing.T) {
	svc, c := openAIAt(t, http.StatusOK, okPayload(Dimensions, 7))
	v, err := svc.Embed(context.Background(), "hello", uuid.New())
	require.NoError(t, err)
	require.Len(t, v, Dimensions)
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/v1/embeddings", c.path)
	require.Equal(t, "Bearer sk-test", c.auth)
	require.Equal(t, "application/json", c.contentType)
	require.Equal(t, map[string]any{
		"model": "text-embedding-3-small", "input": "hello", "dimensions": float64(1536), "encoding_format": "float",
	}, c.body)
}

func TestOpenAIInputOverCapIsTruncated(t *testing.T) {
	svc, c := openAIAt(t, http.StatusOK, okPayload(Dimensions, 7))
	svc.maxInput = 10
	_, err := svc.Embed(context.Background(), strings.Repeat("a", 25), uuid.New())
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("a", 10), c.body["input"])

	// The cap counts characters: a combining sequence stays whole.
	_, err = svc.Embed(context.Background(), strings.Repeat("e\u0301", 11), uuid.New())
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("e\u0301", 10), c.body["input"])
}

func TestOpenAIMissingKey(t *testing.T) {
	svc, c := openAIAt(t, http.StatusOK, okPayload(Dimensions, 7))
	svc.apiKey = ""
	_, err := svc.Embed(context.Background(), "x", uuid.New())
	require.Equal(t, ReasonMissingAPIKey, asError(t, err).Permanent)
	require.Zero(t, c.calls, "no request without a key")
}

func TestOpenAI429IsRecoverable(t *testing.T) {
	svc, _ := openAIAt(t, http.StatusTooManyRequests, "")
	_, err := svc.Embed(context.Background(), "x", uuid.New())
	require.True(t, asError(t, err).IsRecoverable())
}

func TestOpenAI401IsPermanent(t *testing.T) {
	svc, _ := openAIAt(t, http.StatusUnauthorized, "")
	_, err := svc.Embed(context.Background(), "x", uuid.New())
	e := asError(t, err)
	require.False(t, e.IsRecoverable())
	require.Equal(t, ReasonAuthRejected, e.Permanent)
}

func TestOpenAIUsageCallback(t *testing.T) {
	svc, _ := openAIAt(t, http.StatusOK, okPayload(Dimensions, 42))
	var gotTenant uuid.UUID
	var gotTokens int64
	svc.usage = func(_ context.Context, tenant uuid.UUID, tokens int64) { gotTenant, gotTokens = tenant, tokens }
	tenant := uuid.New()
	_, err := svc.Embed(context.Background(), "x", tenant)
	require.NoError(t, err)
	require.Equal(t, tenant, gotTenant)
	require.Equal(t, int64(42), gotTokens)
}

func TestOpenAI402IsPaymentRequired(t *testing.T) {
	svc, _ := openAIAt(t, http.StatusPaymentRequired, `{"error":{"message":"Insufficient credits. Add more using https://openrouter.ai/settings/credits","code":402}}`)
	_, err := svc.Embed(context.Background(), "x", uuid.New())
	e := asError(t, err)
	require.Equal(t, FailurePermanent, e.Kind)
	require.Equal(t, ReasonPaymentRequired, e.Permanent)
	require.Equal(t, 402, e.Response.Status)
	require.True(t, strings.HasPrefix(e.Response.Message, "Insufficient credits."))
	require.False(t, e.IsRecoverable())
	require.True(t, e.IsProviderUnavailable())
	require.Contains(t, e.Error(), `reason=paymentRequired http=402 msg="Insufficient credits.`)
}

func TestOpenAI400RedactsAndTruncates(t *testing.T) {
	svc, _ := openAIAt(t, http.StatusBadRequest, `{"error":{"message":"Bad key sk-or-v1-abc123 `+strings.Repeat("too long ", 60)+`"}}`)
	_, err := svc.Embed(context.Background(), "x", uuid.New())
	e := asError(t, err)
	require.Equal(t, ReasonRequestRejected, e.Permanent)
	require.Equal(t, 400, e.Response.Status)
	require.NotContains(t, e.Response.Message, "sk-or")
	require.Contains(t, e.Response.Message, "[redacted]")
	require.LessOrEqual(t, len([]rune(e.Response.Message)), maxMessageLength+1)
	require.False(t, e.IsProviderUnavailable(), "a bad input is not a provider outage")
}

func TestOpenAI503IsTransient(t *testing.T) {
	svc, _ := openAIAt(t, http.StatusServiceUnavailable, "upstream unavailable")
	_, err := svc.Embed(context.Background(), "x", uuid.New())
	e := asError(t, err)
	require.Equal(t, FailureTransient, e.Kind)
	require.Equal(t, 503, e.Response.Status)
	require.Equal(t, "upstream unavailable", e.Response.Message)
	require.True(t, e.IsRecoverable())
	require.True(t, e.IsProviderUnavailable())
}

// Every shape Swift's Decodable refuses is decodeFailed, not a stored zero.
func TestOpenAIDecodeIsAsStrictAsSwift(t *testing.T) {
	for _, body := range []string{
		``, `not json`, `{}`, `{"data":null}`, `{"data":[]}`,
		`{"data":[{}]}`, `{"data":[{"embedding":null}]}`, `{"data":[{"embedding":[0.1,null]}]}`,
		`{"data":[{"embedding":[1e99]}]}`, `{"data":[{"embedding":["x"]}]}`,
		`{"data":[{"embedding":[0.1]},{}]}`,
		`{"data":[{"embedding":[` + floats(Dimensions, "0.01") + `]}],"usage":{}}`,
	} {
		svc, _ := openAIAt(t, http.StatusOK, body)
		_, err := svc.Embed(context.Background(), "x", uuid.New())
		require.Equal(t, ReasonDecodeFailed, asError(t, err).Permanent, body)
	}
	svc, _ := openAIAt(t, http.StatusOK, `{"data":[{"embedding":[`+floats(Dimensions, "0.01")+`]}],"usage":null}`)
	_, err := svc.Embed(context.Background(), "x", uuid.New())
	require.NoError(t, err)
}

func TestOpenAIWrongWidthAndNetwork(t *testing.T) {
	svc, _ := openAIAt(t, http.StatusOK, `{"data":[{"embedding":[]}]}`)
	_, err := svc.Embed(context.Background(), "x", uuid.New())
	require.Equal(t, &Error{Kind: FailureDimMismatch, Expected: Dimensions, Got: 0}, asError(t, err))

	svc, _ = openAIAt(t, http.StatusOK, "")
	svc.baseURL = parseBaseURL("http://127.0.0.1:9", "")
	_, err = svc.Embed(context.Background(), "x", uuid.New())
	e := asError(t, err)
	require.Equal(t, FailureNetwork, e.Kind)
	require.True(t, strings.HasPrefix(e.Reason, "openai: "))
}

func TestBaseURLKeepsItsPathAndSlash(t *testing.T) {
	for raw, want := range map[string]string{
		"https://gateway.example.com/openrouter":  "https://gateway.example.com/openrouter/v1/embeddings",
		"https://gateway.example.com/openrouter/": "https://gateway.example.com/openrouter/v1/embeddings",
		"  https://api.example.com \n":            "https://api.example.com/v1/embeddings",
		"":                                        "https://api.openai.com/v1/embeddings",
		"://bad":                                  "https://api.openai.com/v1/embeddings",
	} {
		require.Equal(t, want, parseBaseURL(raw, openAIDefaultBaseURL).JoinPath("v1/embeddings").String(), raw)
	}
}

// MARK: NomicEmbeddingServiceTests

func TestPadToTarget(t *testing.T) {
	fill := func(n int, v float32) []float32 {
		out := make([]float32, n)
		for i := range out {
			out[i] = v
		}
		return out
	}
	padded := padToTarget(fill(768, 0.7))
	require.Len(t, padded, Dimensions)
	require.Equal(t, fill(768, 0.7), padded[:768])
	require.Equal(t, fill(768, 0), padded[768:])

	exact := fill(Dimensions, 0.1)
	require.Equal(t, exact, padToTarget(exact))

	require.Equal(t, fill(Dimensions, 0.5), padToTarget(fill(2000, 0.5)), "oversize is truncated")
}

func TestNomicWire(t *testing.T) {
	base, c := stub(t, http.StatusOK, `{"embeddings":[[`+floats(768, "0.3")+`]]}`)
	svc := &nomicService{apiKey: "nk-test", baseURL: base, model: nomicDefaultModel, client: defaultHTTPClient()}
	v, err := svc.Embed(context.Background(), "hi", uuid.New())
	require.NoError(t, err)
	require.Len(t, v, Dimensions)
	require.Equal(t, float32(0.3), v[767])
	require.Zero(t, v[768])
	require.Equal(t, "/v1/embedding/text", c.path)
	require.Equal(t, "Bearer nk-test", c.auth)
	require.Equal(t, map[string]any{"model": "nomic-embed-text-v1.5", "texts": []any{"hi"}}, c.body)

	svc.apiKey = ""
	_, err = svc.Embed(context.Background(), "hi", uuid.New())
	require.Equal(t, ReasonMissingAPIKey, asError(t, err).Permanent)

	base, _ = stub(t, http.StatusOK, `{"embeddings":[]}`)
	svc = &nomicService{apiKey: "nk-test", baseURL: base, model: nomicDefaultModel, client: defaultHTTPClient()}
	_, err = svc.Embed(context.Background(), "hi", uuid.New())
	require.Equal(t, ReasonDecodeFailed, asError(t, err).Permanent)
}

// MARK: LocalHermesEmbeddingServiceTests

func hermesAt(t *testing.T, status int, body, key string) (*hermesLocalService, *captured) {
	t.Helper()
	base, c := stub(t, status, body)
	return &hermesLocalService{
		resolve: func(context.Context, uuid.UUID) (*HermesHandle, error) {
			return &HermesHandle{BaseURL: base.String(), APIServerKey: key}, nil
		},
		model: hermesDefaultModel, client: defaultHTTPClient(),
	}, c
}

func TestHermesNoContainerIsEndpointMissing(t *testing.T) {
	for _, resolve := range []HandleResolver{nil, func(context.Context, uuid.UUID) (*HermesHandle, error) { return nil, nil }} {
		svc := &hermesLocalService{resolve: resolve, client: defaultHTTPClient()}
		_, err := svc.Embed(context.Background(), "x", uuid.New())
		e := asError(t, err)
		require.False(t, e.IsRecoverable())
		require.Equal(t, ReasonEndpointMissing, e.Permanent)
	}
}

func TestHermes404IsEndpointMissing(t *testing.T) {
	svc, _ := hermesAt(t, http.StatusNotFound, "", "key")
	_, err := svc.Embed(context.Background(), "x", uuid.New())
	e := asError(t, err)
	require.Equal(t, ReasonEndpointMissing, e.Permanent)
	require.Equal(t, 404, e.Response.Status)
}

func TestHermesPadsAndChecksWidth(t *testing.T) {
	svc, c := hermesAt(t, http.StatusOK, `{"data":[{"embedding":[`+floats(768, "0.3")+`]}]}`, "key")
	v, err := svc.Embed(context.Background(), "x", uuid.New())
	require.NoError(t, err)
	require.Len(t, v, Dimensions)
	require.Equal(t, float32(0.3), v[0])
	require.Zero(t, v[Dimensions-1])
	require.Equal(t, "/v1/embeddings", c.path)
	require.Equal(t, "Bearer key", c.auth)
	require.Equal(t, map[string]any{"model": "nomic-embed-text-v1.5", "input": "x"}, c.body)

	svc, c = hermesAt(t, http.StatusOK, `{"data":[{"embedding":[`+floats(Dimensions+1, "0.3")+`]}]}`, "")
	_, err = svc.Embed(context.Background(), "x", uuid.New())
	require.Equal(t, &Error{Kind: FailureDimMismatch, Expected: Dimensions, Got: Dimensions + 1}, asError(t, err))
	require.Empty(t, c.auth, "no header without a key")

	failed := &hermesLocalService{
		resolve: func(context.Context, uuid.UUID) (*HermesHandle, error) { return nil, context.DeadlineExceeded },
		client:  defaultHTTPClient(),
	}
	_, err = failed.Embed(context.Background(), "x", uuid.New())
	require.Equal(t, FailureNetwork, asError(t, err).Kind)
}
