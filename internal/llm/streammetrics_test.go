package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// telemetry.Global's instruments delegate to the first global MeterProvider set, so
// the package's tests share one reader and compare cumulative counts before and after.
var (
	globalReaderOnce sync.Once
	globalReader     *sdkmetric.ManualReader
)

func counterValue(t *testing.T, name string) int64 {
	t.Helper()
	globalReaderOnce.Do(func() {
		globalReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(globalReader)))
	})
	var rm metricdata.ResourceMetrics
	require.NoError(t, globalReader.Collect(context.Background(), &rm))
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				n += dp.Value
			}
		}
	}
	return n
}

// HermesLLMStreamService: .stream.success for a stream that ran to its end,
// .stream.failure for a non-2xx head.
func TestGatewayStreamCountsSuccessAndFailure(t *testing.T) {
	success, failure := "luminavault.llm.chat.stream.success", "luminavault.llm.chat.stream.failure"
	s0, f0 := counterValue(t, success), counterValue(t, failure)

	base, _ := gatewayStub(t, gatewayStubBody)
	s := &GatewayStreamService{BaseURL: base, DefaultModel: "hermes-3"}
	_, err := collect(s.ChatStream(t.Context(), "tenant", "", ChatRequest{}))
	require.NoError(t, err)
	assert.Equal(t, s0+1, counterValue(t, success))
	assert.Equal(t, f0, counterValue(t, failure))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	bad, err := url.Parse(srv.URL)
	require.NoError(t, err)
	s = &GatewayStreamService{BaseURL: bad, DefaultModel: "hermes-3"}
	_, err = collect(s.ChatStream(t.Context(), "tenant", "", ChatRequest{}))
	require.Error(t, err)
	assert.Equal(t, s0+1, counterValue(t, success))
	assert.Equal(t, f0+1, counterValue(t, failure))
}

// RoutedHermesLLMStreamService: .stream.byok when the BYOK branch is taken, and
// .stream.byok.failure for any error the routed stream throws, BYOK or not.
func TestRoutedStreamCountsBYOKAndEveryFailure(t *testing.T) {
	byok, byokFailure := "luminavault.llm.chat.stream.byok", "luminavault.llm.chat.stream.byok.failure"
	b0, f0 := counterValue(t, byok), counterValue(t, byokFailure)
	req := ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}}

	s := &RoutedStreamService{
		Fallback: &recordingFallback{}, Transport: &recordingTransport{},
		Preferences: stubPreferences{pref: &Preference{Mode: ModeBYOK, PrimaryProvider: OpenAI, PrimaryModel: "gpt-4o"}},
	}
	_, err := collect(s.ChatStream(t.Context(), uuid.NewString(), "", req))
	require.NoError(t, err)
	assert.Equal(t, b0+1, counterValue(t, byok))
	assert.Equal(t, f0, counterValue(t, byokFailure))

	lane := Decision{Primary: Route{Provider: OpenRouter, ModelID: "some/free:free"}, Cerberus: &CerberusMetadata{IsFreeLane: true}}
	s = &RoutedStreamService{Fallback: &recordingFallback{}, Transport: &recordingTransport{}, Router: fixedRouter{decision: lane}}
	_, err = collect(s.ChatStream(t.Context(), uuid.NewString(), "", req))
	require.Error(t, err)
	assert.Equal(t, b0+1, counterValue(t, byok), "a managed turn is not BYOK")
	assert.Equal(t, f0+1, counterValue(t, byokFailure), "but its failure is counted under byok.failure, as in Swift")
}
