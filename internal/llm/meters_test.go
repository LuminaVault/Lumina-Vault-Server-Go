package llm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Ports CostLedgerServiceTests: Swift's ProviderCostRate.usdMicros is the per-side
// floor CatalogCost applies to a route's own rates, so the rate is quoted on a
// Cerberus route for a model the catalogue does not price.
func TestAuditCCostLedgerService(t *testing.T) {
	cost := func(in, out int64, tokensIn, tokensOut int) int64 {
		return CatalogCost(ProviderID(OpenAI), "auditc-unpriced", tokensIn, tokensOut, &CerberusMetadata{Routes: []WireRoute{{
			Provider: ProviderID(OpenAI), Model: "auditc-unpriced", InputPerMillionUsdMicros: &in, OutputPerMillionUsdMicros: &out,
		}}})
	}
	t.Run("cost is per-million-token rate applied to in and out", func(t *testing.T) {
		assert.Equal(t, int64(2_000_000), cost(500_000, 1_500_000, 1_000_000, 1_000_000))
		assert.Equal(t, int64(250_000), cost(500_000, 1_500_000, 500_000, 0))
	})
	t.Run("zero and negative tokens clamp to zero cost", func(t *testing.T) {
		assert.Zero(t, cost(500_000, 500_000, 0, 0))
		assert.Zero(t, cost(500_000, 500_000, -100, -100))
	})
	t.Run("small token counts round down", func(t *testing.T) {
		assert.Zero(t, cost(500_000, 0, 1, 0))
		assert.Equal(t, int64(3), cost(1_000_000, 0, 3, 0))
	})
	t.Run("retry hint is positive and within a day", func(t *testing.T) {
		secs := secondsUntilUTCMidnight(time.Now())
		assert.GreaterOrEqual(t, secs, 60.0)
		assert.LessOrEqual(t, secs, 86400.0)
	})
	t.Run("retry hint from a known instant points at next UTC midnight", func(t *testing.T) {
		assert.Equal(t, 3600.0, secondsUntilUTCMidnight(time.Date(2026, 6, 3, 23, 0, 0, 0, time.UTC)))
	})
}

// Ports UpstreamErrorTelemetryTests: record increments the counter with the code
// and provider dimensions.
func TestAuditCUpstreamErrorTelemetry(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	counter := newUpstreamErrorCounter(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	ctx := context.Background()

	recordUpstreamErrorOn(ctx, counter, "upstream_timeout", "hermesGateway")
	recordUpstreamErrorOn(ctx, counter, "upstream_unreachable", "hermesGateway")

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	var sum metricdata.Sum[int64]
	found := false
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == "luminavault.llm.chat.upstream_error" {
				sum, found = m.Data.(metricdata.Sum[int64])
			}
		}
	}
	require.True(t, found, "the counter is named luminavault.llm.chat.upstream_error")
	counts := map[string]int64{}
	for _, point := range sum.DataPoints {
		code, _ := point.Attributes.Value(attribute.Key("code"))
		provider, _ := point.Attributes.Value(attribute.Key("provider"))
		assert.Equal(t, "hermesGateway", provider.AsString())
		counts[code.AsString()] += point.Value
	}
	assert.Equal(t, map[string]int64{"upstream_timeout": 1, "upstream_unreachable": 1}, counts)
}

// Ports RoutingContextMeterTests: the numbers behind the chat context gauge, and
// the disclosure rule that a window size is a model fingerprint.
func TestAuditCRoutingContextMeter(t *testing.T) {
	user := func(s string) ChatMessage { return ChatMessage{Role: "user", Content: s} }
	t.Run("prompt size grows with the prompt", func(t *testing.T) {
		small := EstimatePromptTokens([]ChatMessage{user("hi")})
		large := EstimatePromptTokens([]ChatMessage{user(strings.Repeat("word ", 1000))})
		assert.Positive(t, small)
		assert.Greater(t, large, small*10)
	})
	t.Run("many short messages cost more than one message of the same text", func(t *testing.T) {
		many := make([]ChatMessage, 50)
		for i := range many {
			many[i] = user("ok")
		}
		assert.Greater(t, EstimatePromptTokens(many), EstimatePromptTokens([]ChatMessage{user(strings.Repeat("ok", 50))}))
	})
	t.Run("an empty prompt estimates to nothing", func(t *testing.T) {
		assert.Zero(t, EstimatePromptTokens(nil))
	})
	t.Run("a known model resolves its context window", func(t *testing.T) {
		window, ok := ContextWindowForModel("gpt-4o-mini")
		assert.True(t, ok)
		assert.Equal(t, 128_000, window)
	})
	t.Run("a provider-prefixed model id still resolves", func(t *testing.T) {
		window, ok := ContextWindowForModel("openai/gpt-4o-mini")
		assert.True(t, ok)
		assert.Equal(t, 128_000, window)
	})
	t.Run("an unknown model yields no window rather than a guess", func(t *testing.T) {
		_, ok := ContextWindowForModel("some-model-nobody-has-heard-of")
		assert.False(t, ok)
		_, ok = ContextWindowForModel("")
		assert.False(t, ok)
	})

	routing := func() CerberusEvent {
		prompt, window := 900, 1_000_000
		return CerberusEvent{Routing: &RoutingEvent{
			ExecutionID: uuid.New(), ProfileID: uuid.New(), ProfileName: "Auto", TaskType: "general", Strategy: "sequential",
			ActiveRoutes: []WireRoute{}, PromptTokens: &prompt, ContextWindowTokens: &window,
		}}
	}
	t.Run("hidden disclosure strips the context window", func(t *testing.T) {
		scrubbed := ScrubCerberusEvent(routing(), DisclosureHidden)
		require.NotNil(t, scrubbed.Routing)
		assert.Nil(t, scrubbed.Routing.ContextWindowTokens)
	})
	t.Run("hidden disclosure keeps the prompt size", func(t *testing.T) {
		scrubbed := ScrubCerberusEvent(routing(), DisclosureHidden)
		require.NotNil(t, scrubbed.Routing)
		require.NotNil(t, scrubbed.Routing.PromptTokens)
		assert.Equal(t, 900, *scrubbed.Routing.PromptTokens)
	})
	t.Run("visible disclosure passes both numbers through", func(t *testing.T) {
		scrubbed := ScrubCerberusEvent(routing(), DisclosureVisible)
		require.NotNil(t, scrubbed.Routing)
		require.NotNil(t, scrubbed.Routing.PromptTokens)
		require.NotNil(t, scrubbed.Routing.ContextWindowTokens)
		assert.Equal(t, 900, *scrubbed.Routing.PromptTokens)
		assert.Equal(t, 1_000_000, *scrubbed.Routing.ContextWindowTokens)
	})
	t.Run("hidden disclosure strips the model from the usage receipt", func(t *testing.T) {
		usage := CerberusEvent{Usage: &UsageEvent{
			ExecutionID: uuid.New(), Provider: ProviderID(OpenAI), Model: "gpt-4o-mini",
			TokensIn: 100, TokensOut: 200, EstimatedCostUsdMicros: 5, LatencyMs: 300,
		}}
		scrubbed := ScrubCerberusEvent(usage, DisclosureHidden)
		require.NotNil(t, scrubbed.Usage)
		assert.Empty(t, scrubbed.Usage.Model)
		assert.Empty(t, scrubbed.Usage.Provider)
		assert.Equal(t, 100, scrubbed.Usage.TokensIn)
		assert.Equal(t, 200, scrubbed.Usage.TokensOut)
	})
}
