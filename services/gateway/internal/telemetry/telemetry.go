// Package telemetry sets up OpenTelemetry tracing. Without OTEL_EXPORTER_OTLP_ENDPOINT tracing is off and costs nothing.
// Sampling follows the standard OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG variables (default: always sample).
package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Setup installs the global tracer provider and the W3C trace context propagator, and returns the shutdown function
// that flushes pending spans.
func Setup(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	// Propagation is always on, so a trace id received from a caller keeps flowing even when this service exports nothing.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}
	exporter, err := otlptracehttp.New(ctx) // endpoint and protocol come from the standard OTEL_* variables
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}
	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(serviceName)), resource.WithFromEnv())
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// SpanName names server spans "METHOD /first/two" so ids in paths (orders, skus) do not explode the cardinality.
func SpanName(_ string, r *http.Request) string {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return r.Method + " /" + strings.Join(parts, "/")
}

// Filter keeps probes and metrics scrapes out of the traces.
func Filter(r *http.Request) bool {
	switch r.URL.Path {
	case "/healthz", "/readyz", "/metrics":
		return false
	}
	return true
}
