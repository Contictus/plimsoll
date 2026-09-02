package obs

import (
	"context"
	"fmt"
	"os"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// SetupTracing installs the global tracer provider and returns its shutdown function,
// which flushes any batched spans. The caller must defer it: an unflushed batch is the
// usual reason a trace of the very request being debugged is missing.
//
// With OTEL_EXPORTER_OTLP_ENDPOINT unset, tracing is off and the returned shutdown is a
// no-op. That is deliberate -- `go run` and the unit suite have no collector, and an API
// that refused to start without one would get tracing switched off permanently.
func SetupTracing(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}

	// The exporter reads the endpoint, headers and TLS mode from the OTEL_* environment
	// variables, so the deployment configures them in one place (compose) rather than
	// having them duplicated in Go.
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("obs: otlp exporter: %w", err)
	}

	// Built from detectors rather than merged onto resource.Default(). Merging is what
	// produced "conflicting Schema URL" at container startup: the SDK's default resource
	// carries whatever semconv version it was built against, and pinning a different one
	// here breaks on the next SDK upgrade. WithAttributes carries no schema of its own, so
	// it composes with any of them.
	res, err := resource.New(ctx,
		resource.WithFromEnv(),      // OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES
		resource.WithTelemetrySDK(), // sdk name, language and version
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("obs: trace resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)

	// W3C trace context so a span started in the API continues into the worker once M2
	// gives them work to hand each other.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	return once(provider.Shutdown), nil
}

// once makes a shutdown safe to call twice. cmd/api both defers it and calls it on the
// error path; the second call must not be what surfaces as the process's failure.
func once(shutdown func(context.Context) error) func(context.Context) error {
	var (
		guard sync.Once
		err   error
	)
	return func(ctx context.Context) error {
		guard.Do(func() { err = shutdown(ctx) })
		return err
	}
}
