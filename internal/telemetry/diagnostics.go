package telemetry

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
)

// ConfigureSDKDiagnostics installs process-global diagnostics once at startup,
// before creating providers. It is intentionally separate from exporter metrics:
// the same export error can also be reported to this global SDK handler.
func ConfigureSDKDiagnostics(logger *slog.Logger) {
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		if err == nil {
			return
		}
		OTelSDKErrors.Inc()
		logger.Error("OpenTelemetry SDK diagnostic", "component", "otel_sdk", "operation", "diagnostic", "error", SafeError(err))
	}))
	// Queue-full warnings use SDK logging, not the error handler. Preserve these
	// diagnostics without parsing their messages into unstable custom metrics.
	otel.SetLogger(logr.FromSlogHandler(safeSDKHandler{logger.With("component", "otel_sdk", "operation", "diagnostic").Handler()}))
}

type safeSDKHandler struct{ slog.Handler }

// OpenTelemetry documents SDK warnings at logr verbosity 1. The slog bridge
// maps that to level -1, which a default INFO handler would otherwise discard.
func (h safeSDKHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level == -1 {
		level = slog.LevelWarn
	}
	return h.Handler.Enabled(ctx, level)
}
func (h safeSDKHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level == -1 {
		record.Level = slog.LevelWarn
	}
	safe := slog.NewRecord(record.Time, record.Level, redact(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool { safe.AddAttrs(safeAttr(attr)); return true })
	return h.Handler.Handle(ctx, safe)
}
func (h safeSDKHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = safeAttr(a)
	}
	return safeSDKHandler{h.Handler.WithAttrs(clean)}
}
func (h safeSDKHandler) WithGroup(name string) slog.Handler {
	return safeSDKHandler{h.Handler.WithGroup(name)}
}
func safeAttr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	switch a.Value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(redact(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			a.Value = slog.StringValue(SafeError(err))
		}
	case slog.KindGroup:
		attrs := a.Value.Group()
		clean := make([]slog.Attr, len(attrs))
		for i, attr := range attrs {
			clean[i] = safeAttr(attr)
		}
		a.Value = slog.GroupValue(clean...)
	}
	return a
}

var diagnosticURL = regexp.MustCompile(`https?://[^\s"<>]+`)

// SafeError retains useful failure context while masking configured secrets and
// URL credentials/query strings. Payloads and authentication headers are never
// deliberately attached to logs. Arbitrary backend error text is not trusted.
func SafeError(err error) string {
	if err == nil {
		return ""
	}
	return redact(err.Error())
}
func redact(message string) string {
	// Mask both general and logs-specific headers, even when one overrides the other.
	for _, name := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS"} {
		if raw := os.Getenv(name); raw != "" {
			message = strings.ReplaceAll(message, raw, "[REDACTED]")
			if headers, err := parseHeaderMap(raw); err == nil {
				for _, value := range headers {
					if value != "" {
						message = strings.ReplaceAll(message, value, "[REDACTED]")
					}
				}
			}
		}
	}
	if password := os.Getenv("REDIS_PASSWORD"); password != "" {
		message = strings.ReplaceAll(message, password, "[REDACTED]")
	}
	return diagnosticURL.ReplaceAllStringFunc(message, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "[REDACTED URL]"
		}
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		return u.String()
	})
}
