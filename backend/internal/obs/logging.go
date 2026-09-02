// Package obs wires structured logging and tracing. It is the only package that
// configures a global; everything else takes a *slog.Logger as a dependency.
package obs

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// redactedValue is what a secret looks like in a log. It matches auth.Secret's own
// rendering, so a reader cannot tell which of the two defences caught it -- and does not
// need to.
const redactedValue = "REDACTED"

// sensitiveKeyStems are matched case-insensitively as substrings of an attribute key.
// Substring matching over-redacts -- `token_hash` is already a hash and safe to print --
// and that is the intended bias: losing one debugging field costs an inconvenience, and
// printing one credential costs the key (L13).
var sensitiveKeyStems = []string{
	"password",
	"passphrase",
	"secret",
	"token",
	"api_key",
	"apikey",
	"authorization",
	"credential",
	"private_key",
	"privatekey",
	"dsn",
	"kek",
}

// NewLogger returns a JSON logger, one object per line, correlated with the active span.
//
// It carries two independent defences against a credential reaching the log (L13). Values
// implementing slog.LogValuer -- notably auth.Secret -- redact themselves. Attributes
// whose key looks sensitive are redacted whatever their type, which is what catches the
// likelier mistake: a plain string logged under `api_key` by a caller who never reached
// for auth.Secret.
//
// Correlation requires the context-carrying call: InfoContext, not Info. A line logged
// without a context simply has no ids, which is correct for startup and shutdown.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(traceHandler{slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: redactSensitive,
	})})
}

// traceHandler stamps each record with the ids of the span in its context, so an operator
// can pivot between a log line and the trace it came from. The ids are omitted entirely
// when there is no span: a zeroed trace_id is a field someone would search for and never
// find.
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, record slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, record)
}

// WithAttrs and WithGroup rewrap, because the embedded handler returns a bare
// slog.Handler and a logger built with slog.With would otherwise lose correlation.
func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}

// redactSensitive is the handler's ReplaceAttr hook. slog calls it for every non-group
// attribute, including those inside groups, so a credential nested one level down is
// caught too.
func redactSensitive(_ []string, a slog.Attr) slog.Attr {
	if isSensitiveKey(a.Key) {
		return slog.String(a.Key, redactedValue)
	}
	return a
}

func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, stem := range sensitiveKeyStems {
		if strings.Contains(lower, stem) {
			return true
		}
	}
	return false
}
