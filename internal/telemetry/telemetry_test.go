package telemetry

import (
	"context"
	"log/slog"
	"lumina/internal/config"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

func TestSetupDisabledInstallsNoExporter(t *testing.T) {
	shutdown, err := Setup(context.Background(), config.Config{OTelEnabled: false, LogLevel: "info"})
	require.NoError(t, err)
	require.NoError(t, shutdown(context.Background()))
	_, isSDK := otel.GetTracerProvider().(*sdktrace.TracerProvider)
	require.False(t, isSDK)
}

func TestResourceHasImplAttribute(t *testing.T) {
	res, err := buildResource(config.Config{OTelServiceName: "luminavault-api-production"})
	require.NoError(t, err)
	attrs := map[string]string{}
	for _, kv := range res.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	require.Equal(t, "go", attrs["lv.impl"])
	require.Equal(t, "luminavault-api-production", attrs[string(semconv.ServiceNameKey)])
}

func TestParseLevel(t *testing.T) {
	require.Equal(t, slog.LevelDebug, parseLevel("trace"))
	require.Equal(t, slog.LevelWarn, parseLevel("WARNING"))
	require.Equal(t, slog.LevelError, parseLevel("critical"))
	require.Equal(t, slog.LevelInfo, parseLevel(""))
}
