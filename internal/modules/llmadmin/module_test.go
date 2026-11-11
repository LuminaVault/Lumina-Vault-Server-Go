package llmadmin_test

import (
	"context"
	"encoding/json"
	"io"
	"lumina/internal/config"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/modules/llmadmin"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type line struct {
	Kind      string  `json:"kind"`
	Region    string  `json:"region"`
	OK        bool    `json:"ok"`
	LatencyMs int     `json:"latency_ms"`
	Error     *string `json:"error"`
}

func setup(t *testing.T, token string) (*httptest.Server, *[]string, func(hdr string) *httptest.ResponseRecorder) {
	t.Helper()
	var auths []string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.URL.Path+" "+r.Header.Get("Authorization"))
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusUnauthorized) // any HTTP status is reachable
	}))
	t.Cleanup(stub.Close)
	up, _ := url.Parse(stub.URL)
	refused, _ := url.Parse("http://127.0.0.1:9")
	registry := llm.NewRegistry(nil,
		&llm.OpenAICompatibleAdapter{ProviderKind: llm.OpenAI, APIKey: "sk-openai", BaseURL: up},
		&llm.OpenAICompatibleAdapter{ProviderKind: llm.Kimi, APIKey: "sk-kimi", BaseURL: refused},
		&llm.AnthropicAdapter{APIKey: "sk-ant"}, // not OpenAI-compatible: not checked
	)
	e := httpserver.New(config.Config{}, httpserver.Deps{Modules: []httpserver.Module{llmadmin.New(registry, token, nil)}})
	do := func(hdr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/admin/llm/health", nil)
		if hdr != "" {
			req.Header.Set("X-Admin-Token", hdr)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}
	return stub, &auths, do
}

func TestHealthIsBehindTheAdminToken(t *testing.T) {
	_, auths, do := setup(t, "secret")
	for _, hdr := range []string{"", "wrong"} {
		rec := do(hdr)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.JSONEq(t, `{"error":{"message":"admin token invalid"}}`, rec.Body.String())
	}
	assert.Empty(t, *auths, "nothing is pinged before the token check")

	_, _, disabled := setup(t, "")
	rec := disabled("secret")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.JSONEq(t, `{"error":{"message":"admin disabled"}}`, rec.Body.String())
}

func TestHealthPingsEveryOpenAICompatibleAdapter(t *testing.T) {
	_, auths, do := setup(t, "secret")
	rec := do("secret")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		CheckedAt string `json:"checked_at"`
		Providers []line `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.NotEmpty(t, out.CheckedAt)
	require.Len(t, out.Providers, 2)

	assert.Equal(t, "kimi", out.Providers[0].Kind, "sorted by kind")
	assert.Equal(t, "cn", out.Providers[0].Region)
	assert.False(t, out.Providers[0].OK)
	assert.NotNil(t, out.Providers[0].Error)

	assert.Equal(t, "openai", out.Providers[1].Kind)
	assert.Equal(t, "us", out.Providers[1].Region)
	assert.True(t, out.Providers[1].OK, "a 401 is still reachable")
	assert.Nil(t, out.Providers[1].Error)
	assert.Equal(t, []string{"/v1/chat/completions Bearer sk-openai"}, *auths)

	// The response matches the spec.
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/admin/llm/health", nil)
	route, params, err := router.FindRoute(req)
	require.NoError(t, err)
	in := &openapi3filter.RequestValidationInput{
		Request: req, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}
	outp := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: rec.Code, Header: rec.Header()}
	outp.SetBodyBytes(rec.Body.Bytes())
	require.NoError(t, openapi3filter.ValidateResponse(context.Background(), outp))
}
