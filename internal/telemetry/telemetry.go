package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/jyukki97/pgmux/internal/config"
)

// Version is the pgmux version embedded in trace resources.
const Version = "0.1.0"

// Init initializes the OpenTelemetry TracerProvider based on the given config.
// When telemetry is disabled, a noop tracer is used and shutdown is a no-op.
// Returns a shutdown function that must be called on application exit.
func Init(cfg config.TelemetryConfig) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		slog.Info("telemetry disabled, using noop tracer")
		return func(context.Context) error { return nil }, nil
	}

	ctx := context.Background()

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(Version),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create otel resource: %w", err)
	}

	var exporter sdktrace.SpanExporter
	switch cfg.Exporter {
	case "stdout":
		exporter, err = stdouttrace.New(stdouttrace.WithWriter(os.Stdout))
		if err != nil {
			return nil, fmt.Errorf("create stdout exporter: %w", err)
		}
	case "otlp":
		exporter, err = otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(cfg.Endpoint),
			otlptracegrpc.WithInsecure(),
		)
		if err != nil {
			return nil, fmt.Errorf("create otlp exporter: %w", err)
		}
	default:
		return nil, fmt.Errorf("unknown telemetry exporter: %q", cfg.Exporter)
	}

	ratio := *cfg.SampleRatio
	var sampler sdktrace.Sampler
	if ratio >= 1.0 {
		sampler = sdktrace.AlwaysSample()
	} else if ratio <= 0 {
		sampler = sdktrace.NeverSample()
	} else {
		sampler = sdktrace.TraceIDRatioBased(ratio)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	slog.Info("telemetry initialized",
		"exporter", cfg.Exporter,
		"endpoint", cfg.Endpoint,
		"service", cfg.ServiceName,
		"sample_ratio", *cfg.SampleRatio)

	return tp.Shutdown, nil
}

// Tracer returns a named tracer from the global TracerProvider.
func Tracer() trace.Tracer {
	return otel.Tracer("github.com/jyukki97/pgmux")
}
