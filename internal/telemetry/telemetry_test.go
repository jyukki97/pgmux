package telemetry

import (
	"context"
	"testing"

	"github.com/jyukki97/pgmux/internal/config"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestInit_Disabled(t *testing.T) {
	shutdown, err := Init(config.TelemetryConfig{Enabled: false})
	if err != nil {
		t.Fatalf("Init disabled: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func ptrFloat64(v float64) *float64 { return &v }

func TestInit_Stdout(t *testing.T) {
	shutdown, err := Init(config.TelemetryConfig{
		Enabled:     true,
		Exporter:    "stdout",
		ServiceName: "pgmux-test",
		SampleRatio: ptrFloat64(1.0),
	})
	if err != nil {
		t.Fatalf("Init stdout: %v", err)
	}
	defer shutdown(context.Background())

	// Verify that a real TracerProvider was set (not the noop)
	tp := otel.GetTracerProvider()
	if _, ok := tp.(*sdktrace.TracerProvider); !ok {
		t.Fatalf("expected *sdktrace.TracerProvider, got %T", tp)
	}
}

func TestInit_UnknownExporter(t *testing.T) {
	_, err := Init(config.TelemetryConfig{
		Enabled:  true,
		Exporter: "unknown",
	})
	if err == nil {
		t.Fatal("expected error for unknown exporter")
	}
}

func TestTracer(t *testing.T) {
	tracer := Tracer()
	if tracer == nil {
		t.Fatal("Tracer() returned nil")
	}
}

func TestInit_SampleRatioZero(t *testing.T) {
	shutdown, err := Init(config.TelemetryConfig{
		Enabled:     true,
		Exporter:    "stdout",
		ServiceName: "pgmux-test",
		SampleRatio: ptrFloat64(0),
	})
	if err != nil {
		t.Fatalf("Init with zero sample ratio: %v", err)
	}
	defer shutdown(context.Background())
}

func TestInit_SampleRatioFractional(t *testing.T) {
	shutdown, err := Init(config.TelemetryConfig{
		Enabled:     true,
		Exporter:    "stdout",
		ServiceName: "pgmux-test",
		SampleRatio: ptrFloat64(0.5),
	})
	if err != nil {
		t.Fatalf("Init with fractional sample ratio: %v", err)
	}
	defer shutdown(context.Background())
}
