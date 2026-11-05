package knowledge_test

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/entitlement"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/migrations"
	"lumina/internal/modules/knowledge"
	"lumina/internal/ratelimit"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	reasonKim = uuid.MustParse("6C000000-0000-4000-8000-0000000000A1")
	reasonNia = uuid.MustParse("6C000000-0000-4000-8000-0000000000A2")
)

// reasonTransport answers the synthesis call, or fails it.
type reasonTransport struct {
	answer string
	fail   error
	calls  int
}

func (s *reasonTransport) ChatCompletions(context.Context, []byte, string, string) (llm.Metadata, error) {
	s.calls++
	if s.fail != nil {
		return llm.Metadata{}, s.fail
	}
	data, err := json.Marshal(map[string]any{
		"id": "t", "model": "t",
		"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": s.answer}}},
	})
	return llm.Metadata{Data: data}, err
}

func (s *reasonTransport) ChatStream(context.Context, []byte, string, string) llm.Stream {
	return func(yield func(llm.Chunk, error) bool) {
		s.calls++
		if s.fail != nil {
			yield(llm.Chunk{}, s.fail)
			return
		}
		yield(llm.Chunk{Delta: s.answer}, nil)
	}
}

func setupReason(t *testing.T, transport llm.ChatTransport) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "knowledge-reason.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	users := auth.PGUsers(pool)
	base := knowledge.New(pool, auth.Identify(keys, users), entitlement.Middleware(pool, entitlement.MemoryQuery, false, nil), ratelimit.NewStore())
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth: auth.Middleware(keys, users), Modules: []httpserver.Module{knowledge.NewReason(base, transport, "test-model")},
	})
	return env{t: t, e: e, pool: pool}
}

func TestReasonGroundsAndSynthesizes(t *testing.T) {
	transport := &reasonTransport{answer: "Atlas supports Beacon."}
	v := setupReason(t, transport)
	rec := v.do(reasonKim, http.MethodPost, "/v1/knowledge/reason", `{"query":"How does Atlas relate to Beacon?"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body knowledge.ReasoningQueryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, 1, transport.calls)
	assert.NotEmpty(t, body.Paths)
	assert.Contains(t, rec.Body.String(), "Atlas supports Beacon.")
}

func TestReasonWithNothingMatchedCostsNoModelCall(t *testing.T) {
	transport := &reasonTransport{answer: "unused"}
	v := setupReason(t, transport)
	require.Equal(t, http.StatusOK, v.do(reasonNia, http.MethodPost, "/v1/knowledge/reason", `{"query":"Atlas"}`).Code, "another tenant's graph is not searched")
	require.Equal(t, http.StatusOK, v.do(reasonKim, http.MethodPost, "/v1/knowledge/reason", `{"query":"unmatched zzqx"}`).Code)
	assert.Zero(t, transport.calls)
}

func TestReasonStreamEndsWithItsTerminalFrame(t *testing.T) {
	v := setupReason(t, &reasonTransport{answer: "Atlas supports Beacon."})
	rec := v.do(reasonKim, http.MethodPost, "/v1/knowledge/reason/stream", `{"query":"How does Atlas relate to Beacon?"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, strings.HasPrefix(rec.Body.String(), "data: "), rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"type":"token"`)
}

// A failed synthesis is not an error frame: the grounded answer stands in for the tokens.
func TestReasonStreamFailureFallsBackToTheGroundedAnswer(t *testing.T) {
	v := setupReason(t, &reasonTransport{fail: errors.New("upstream died")})
	rec := v.do(reasonKim, http.MethodPost, "/v1/knowledge/reason/stream", `{"query":"How does Atlas relate to Beacon?"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"type":"error"`)
	assert.Contains(t, rec.Body.String(), `"type":"token"`)
	assert.Contains(t, rec.Body.String(), `"type":"done"`)
}

func TestReasonResponseConformsToSpec(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)
	v := setupReason(t, &reasonTransport{answer: "Atlas supports Beacon."})
	body := `{"query":"How does Atlas relate to Beacon?"}`
	rec := v.do(reasonKim, http.MethodPost, "/v1/knowledge/reason", body)
	req := httptest.NewRequest(http.MethodPost, "/v1/knowledge/reason", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	route, params, err := router.FindRoute(req)
	require.NoError(t, err)
	out := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		},
		Status: rec.Code, Header: http.Header{"Content-Type": {rec.Header().Get("Content-Type")}},
	}
	out.SetBodyBytes(rec.Body.Bytes())
	require.NoError(t, openapi3filter.ValidateResponse(ctx, out), rec.Body.String())
}
