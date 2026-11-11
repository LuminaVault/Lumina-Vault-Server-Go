// Package telemetry sets up structured logging and OpenTelemetry. Exporter
// endpoints come from the standard OTEL_EXPORTER_OTLP_* variables, which the
// OTel SDK reads itself; the api values files already set them for Swift.
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/config"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// ImplAttribute separates Go from Swift series that share a service name
// (api-go-* inherits OTEL_SERVICE_NAME from api-*). Cutover gate queries
// filter on it.
var ImplAttribute = attribute.String("lv.impl", "go")

// Shutdown flushes and stops whatever Setup started.
type Shutdown func(context.Context) error

// Setup installs the JSON slog default logger at LOG_LEVEL and, when
// OTEL_ENABLED=true, the OTLP gRPC trace and metric providers.
func Setup(ctx context.Context, cfg config.Config) (Shutdown, error) {
	slog.SetDefault(NewLogger(cfg.LogLevel))
	if !cfg.OTelEnabled {
		return func(context.Context) error { return nil }, nil
	}

	res, err := buildResource(cfg)
	if err != nil {
		return nil, err
	}
	traceExp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, err
	}
	metricExp, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		return nil, errors.Join(err, traceExp.Shutdown(ctx))
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)), sdkmetric.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}

// buildResource adds service.instance.id, the pod's hostname, which Swift's resource
// doesn't carry. Both servers inherit OTEL_SERVICE_NAME, so Alloy gives their series
// the same job, and the Swift-named metrics (P5) would otherwise land on the same
// series as Swift's with instance="" too: two writers on one counter read as
// resets. The instance label keeps Go's series apart, and a sum() over job, which
// every alert rule takes, still adds both servers up.
func buildResource(cfg config.Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.OTelServiceName), ImplAttribute}
	if host, err := os.Hostname(); err == nil && host != "" {
		attrs = append(attrs, semconv.ServiceInstanceID(host))
	}
	return resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL, attrs...))
}

// NewLogger returns a JSON logger on stdout. Unknown levels fall back to info,
// matching swift-log's tolerance of an unset LOG_LEVEL.
func NewLogger(level string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(level)}))
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace", "debug":
		return slog.LevelDebug
	case "warning", "warn":
		return slog.LevelWarn
	case "error", "critical":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
