package obs_test

import (
	"context"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/obs"
	"github.com/stretchr/testify/require"
)

// With no collector configured the process must still start. Local `go run` and the unit
// suite have no Jaeger, and an API that refuses to boot without one would push every
// developer toward disabling tracing permanently.
func TestSetupTracingIsANoOpWithoutAnEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	shutdown, err := obs.SetupTracing(context.Background(), "plimsoll-test")
	require.NoError(t, err)
	require.NotNil(t, shutdown, "shutdown must be callable even when tracing is off")
	require.NoError(t, shutdown(context.Background()))
}

// Shutdown must tolerate being called twice: cmd/api defers it and also calls it on the
// error path, and a double shutdown must not be the thing that masks the real error.
func TestTracingShutdownIsIdempotent(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	shutdown, err := obs.SetupTracing(context.Background(), "plimsoll-test")
	require.NoError(t, err)
	require.NoError(t, shutdown(context.Background()))
	require.NoError(t, shutdown(context.Background()))
}

// The no-endpoint path returns before any of the SDK is touched, so on its own it proves
// nothing about the exporter, the resource, or the provider. This exercises the real one.
// It caught a schema-URL conflict between resource.Default() and a pinned semconv version
// that only appeared at container startup.
//
// No collector is needed: the gRPC exporter dials lazily, so construction and shutdown
// both complete against an address with nothing behind it.
func TestSetupTracingBuildsARealProvider(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:14317")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	shutdown, err := obs.SetupTracing(ctx, "plimsoll-test")
	require.NoError(t, err)
	require.NoError(t, shutdown(ctx))
}
