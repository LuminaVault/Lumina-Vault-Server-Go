package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The request-duration metric carries Echo's matched route as http.route, the
// label Swift's hb_requests_total had and the 5xx-by-route panel groups by.
// Echo v5 sets http.Request.Pattern after routing and otelhttp reads it, so no
// labeler is needed; this guards that, since D3's alert and dashboard queries
// rely on the label.
func TestRouteTelemetryLabelsRequestDuration(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	h := otelhttp.NewHandler(newTestServer(), "http.server", otelhttp.WithMeterProvider(mp))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v1/test/items/42", http.NoBody))
	require.Equal(t, http.StatusOK, rec.Code)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var routes []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "request duration is a float64 histogram")
			for _, dp := range hist.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key("http.route")); ok {
					routes = append(routes, v.AsString())
				}
			}
		}
	}
	require.Equal(t, []string{"/v1/test/items/:id"}, routes)
}
