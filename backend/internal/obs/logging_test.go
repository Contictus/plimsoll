package obs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/obs"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

// L13: a secret must not reach a log even when a careless caller passes it directly.
// This is the test that makes the rule enforceable instead of aspirational.
func TestLoggerRedactsSecretValues(t *testing.T) {
	var buf bytes.Buffer
	log := obs.NewLogger(&buf, slog.LevelInfo)

	log.Info("connecting integration",
		"integration_id", "int-123",
		"api_key", auth.Secret("BINANCE-REAL-KEY-abc123"),
	)

	out := buf.String()
	require.Contains(t, out, "int-123", "non-secret identifiers must survive for debugging")
	require.Contains(t, out, "REDACTED")
	require.NotContains(t, out, "BINANCE-REAL-KEY")
	require.NotContains(t, out, "abc123")
}

// auth.Secret only helps a caller who remembered to use it. The far likelier mistake is a
// plain string under an obviously sensitive key, so the handler redacts by key as well.
func TestLoggerRedactsSensitiveKeysEvenForPlainStrings(t *testing.T) {
	for _, key := range []string{
		"password", "api_key", "API_KEY", "apiSecret", "authorization",
		"session_token", "PLIMSOLL_APP_DSN", "private_key", "credential",
	} {
		var buf bytes.Buffer
		obs.NewLogger(&buf, slog.LevelInfo).Info("attempt", key, "leaked-value-xyz")
		require.NotContains(t, buf.String(), "leaked-value-xyz", "key %q was not redacted", key)
		require.Contains(t, buf.String(), "REDACTED", "key %q", key)
	}
}

// Redaction must reach inside groups: a credential nested one level down is still a
// credential.
func TestLoggerRedactsInsideGroups(t *testing.T) {
	var buf bytes.Buffer
	obs.NewLogger(&buf, slog.LevelInfo).Info("connect",
		slog.Group("integration", "id", "int-9", "api_key", "leaked-value-xyz"))

	require.NotContains(t, buf.String(), "leaked-value-xyz")
	require.Contains(t, buf.String(), "int-9")
}

// Ordinary fields must not be swept up: over-redaction costs the debugging information
// the log exists for.
func TestLoggerKeepsOrdinaryFields(t *testing.T) {
	var buf bytes.Buffer
	obs.NewLogger(&buf, slog.LevelInfo).Info("fill",
		"instrument", "BTCUSDT", "venue_event_id", "spot:trade:BTCUSDT:12345678")

	require.Contains(t, buf.String(), "BTCUSDT")
	require.Contains(t, buf.String(), "spot:trade:BTCUSDT:12345678")
	require.NotContains(t, buf.String(), "REDACTED")
}

func TestLoggerEmitsOneJSONObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	log := obs.NewLogger(&buf, slog.LevelInfo)
	log.Info("first", "k", "v")
	log.Info("second", "k", "v")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 2)
	for _, line := range lines {
		var into map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &into), "line %q is not JSON", line)
	}
}

func TestLoggerHonoursItsLevel(t *testing.T) {
	var buf bytes.Buffer
	obs.NewLogger(&buf, slog.LevelInfo).Debug("should not appear")
	require.Empty(t, buf.String())
}

// A log line is only useful next to the trace it belongs to. When the record carries a
// span, its ids ride along, so an operator can pivot from one to the other.
func TestLoggerCorrelatesWithTheActiveSpan(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(
		trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled},
	))

	var buf bytes.Buffer
	obs.NewLogger(&buf, slog.LevelInfo).InfoContext(ctx, "handled request")

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", line["trace_id"])
	require.Equal(t, "00f067aa0ba902b7", line["span_id"])
}

// Without a span there is nothing to correlate, and an empty or zero trace_id would be a
// field an operator could waste time searching for.
func TestLoggerOmitsTraceIDsWhenThereIsNoSpan(t *testing.T) {
	var buf bytes.Buffer
	obs.NewLogger(&buf, slog.LevelInfo).Info("startup")

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.NotContains(t, line, "trace_id")
	require.NotContains(t, line, "span_id")
}

// slog.With returns a logger built from the embedded handler. If WithAttrs did not
// rewrap, correlation and redaction would both vanish the moment anyone bound a field to
// a logger -- which is exactly what a request-scoped logger does.
func TestDerivedLoggersKeepCorrelationAndRedaction(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(
		trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled},
	))

	var buf bytes.Buffer
	derived := obs.NewLogger(&buf, slog.LevelInfo).With("integration_id", "int-7")
	derived.InfoContext(ctx, "connect", "api_key", "leaked-value-xyz")

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", line["trace_id"])
	require.Equal(t, "int-7", line["integration_id"])
	require.Equal(t, "REDACTED", line["api_key"])
	require.NotContains(t, buf.String(), "leaked-value-xyz")
}
