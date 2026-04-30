// Package telemetry initialises the global OpenTelemetry instrumentation for
// the agentmesh process.
//
// It sets up a W3C TraceContext + Baggage composite propagator so that trace
// IDs are correctly forwarded to upstream LLM endpoints and any downstream
// services that participate in distributed tracing. Spans are exported via
// OTLP gRPC to a collector whose address is read from the standard
// OTEL_EXPORTER_OTLP_ENDPOINT environment variable (default: localhost:4317).
//
// The package exposes a single entry-point, InitProvider, which is called once
// at process startup. It returns a shutdown function that callers must invoke
// (typically via defer) to flush buffered spans and release resources before
// the process exits.
package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	v1 "agentmesh/api/v1"
)

// InitProvider initialises the global OpenTelemetry TracerProvider and
// TextMapPropagator for the agentmesh service.
//
// It:
//   - configures standard W3C Trace Context + Baggage propagation,
//   - builds a resource tagged with the service name "agentmesh" and the
//     supplied config version,
//   - creates an OTLP gRPC span exporter (endpoint read from the standard
//     OTEL_EXPORTER_OTLP_ENDPOINT env-var, defaulting to localhost:4317),
//   - wires everything into a BatchSpanProcessor, and
//   - registers the resulting TracerProvider as the global OTel provider.
//
// The returned shutdown function must be called (typically via defer in main)
// to flush buffered spans and release resources cleanly.
func InitProvider(ctx context.Context, cfg *v1.Config) (shutdown func(context.Context) error, err error) {
	// --- Propagation -------------------------------------------------------
	// Use W3C TraceContext + Baggage so trace IDs are forwarded correctly
	// to upstream LLM endpoints and any downstream services.
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),
	)

	// --- Resource ----------------------------------------------------------
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("agentmesh"),
			semconv.ServiceVersion(cfg.Version),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("building OTel resource: %w", err)
	}

	// --- Exporter ----------------------------------------------------------
	// otlptracegrpc reads OTEL_EXPORTER_OTLP_ENDPOINT (default: localhost:4317)
	// and OTEL_EXPORTER_OTLP_INSECURE from the environment; no credentials
	// are hard-coded here.
	//
	// A 5-second timeout prevents agentmesh from hanging at startup when the
	// OTLP collector is not yet available (e.g. sidecar not yet ready in
	// Kubernetes). The SDK will continue to buffer and retry spans after the
	// connection is established.
	exporterCtx, exporterCancel := context.WithTimeout(ctx, 5*time.Second)
	defer exporterCancel()
	exporter, err := otlptracegrpc.New(exporterCtx)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP gRPC exporter: %w", err)
	}

	// --- Tracer Provider ---------------------------------------------------
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)

	// Return a shutdown closure that flushes pending spans and stops the
	// exporter gracefully.
	shutdown = func(ctx context.Context) error {
		if err := tp.Shutdown(ctx); err != nil {
			return fmt.Errorf("shutting down TracerProvider: %w", err)
		}
		return nil
	}
	return shutdown, nil
}
