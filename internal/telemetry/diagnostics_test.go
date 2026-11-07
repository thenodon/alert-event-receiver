package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
)

func TestSDKDiagnosticsAreSeparateFromExporterMetrics(t *testing.T) {
	var logs lockedLogBuffer
	prior := otel.GetErrorHandler()
	ConfigureSDKDiagnostics(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer otel.SetErrorHandler(prior)
	defer otel.SetLogger(logr.Discard())
	before := testutil.ToFloat64(OTelSDKErrors)
	exportBefore := testutil.ToFloat64(OTLPExportRequests.WithLabelValues("grpc", "error"))
	otel.Handle(errors.New("SDK diagnostic"))
	if testutil.ToFloat64(OTelSDKErrors)-before != 1 || testutil.ToFloat64(OTLPExportRequests.WithLabelValues("grpc", "error")) != exportBefore {
		t.Fatal("SDK diagnostic counted as export failure")
	}
	if !strings.Contains(logs.String(), `"component":"otel_sdk"`) || strings.Contains(logs.String(), "event_id") {
		t.Fatal("incorrect diagnostic context", logs.String())
	}
}

func TestSafeDiagnostics(t *testing.T) {
	t.Setenv("REDIS_PASSWORD", "valkey-secret")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer secret-token")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_HEADERS", "X-Api-Key=secret-api-key")
	message := "connection failed https://user:password@example.test/logs?token=query-secret Authorization=Bearer secret-token secret-api-key valkey-secret"
	safe := SafeError(errors.New(message))
	for _, secret := range []string{"user:password", "query-secret", "secret-token", "secret-api-key", "valkey-secret"} {
		if strings.Contains(safe, secret) {
			t.Fatalf("secret exposed: %s", safe)
		}
	}
	if !strings.Contains(safe, "connection failed") || !strings.Contains(safe, "example.test/logs") {
		t.Fatal("lost useful error context", safe)
	}
	var logs lockedLogBuffer
	h := safeSDKHandler{slog.NewJSONHandler(&logs, nil)}
	logger := slog.New(h).With("authorization", "Bearer secret-token")
	logger.ErrorContext(context.Background(), "SDK failure", slog.Group("context", slog.Any("error", errors.New(message))))
	if strings.Contains(logs.String(), "secret-token") || strings.Contains(logs.String(), "valkey-secret") {
		t.Fatal("SDK diagnostic leaked secret", logs.String())
	}
}

func TestInvalidHeaderDoesNotEchoValue(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_HEADERS", "private-credential")
	_, err := parseOTLPHeadersFromEnv()
	if err == nil || strings.Contains(err.Error(), "private-credential") {
		t.Fatal("header parse error leaked input", err)
	}
}

// These methods must never be invoked by the diagnostic handler or JSON sink.
// Arbitrary object serialization may disclose fields or have side effects.
type opaqueDiagnostic struct{ Secret string }

func (opaqueDiagnostic) String() string { panic("unexpected diagnostic String call") }
func (opaqueDiagnostic) MarshalJSON() ([]byte, error) {
	panic("unexpected diagnostic MarshalJSON call")
}

type diagnosticLogValuer struct{ value slog.Value }

func (v diagnosticLogValuer) LogValue() slog.Value { return v.value }

func TestOpaqueDiagnosticValuesAreOmitted(t *testing.T) {
	t.Setenv("REDIS_PASSWORD", "configured-secret")
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"map", map[string]any{"password": "unconfigured-secret"}},
		{"slice", []string{"unconfigured-secret"}},
		{"struct", opaqueDiagnostic{Secret: "unconfigured-secret"}},
		{"pointer", &opaqueDiagnostic{Secret: "unconfigured-secret"}},
		{"log valuer returning opaque", diagnosticLogValuer{value: slog.AnyValue(map[string]string{"password": "unconfigured-secret"})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs lockedLogBuffer
			logger := slog.New(safeSDKHandler{slog.NewJSONHandler(&logs, nil)})
			logger.With("bound", tc.value).Error("SDK diagnostic", slog.Any("object", tc.value), slog.Group("nested", slog.Any("object", tc.value)))
			if strings.Contains(logs.String(), "unconfigured-secret") || strings.Count(logs.String(), "[omitted: unsupported diagnostic value]") != 3 {
				t.Fatalf("opaque value not omitted: %s", logs.String())
			}
		})
	}
	// Resolved LogValuers and normal groups still undergo best-effort text masking.
	attr := safeAttr(slog.Any("dynamic", diagnosticLogValuer{value: slog.GroupValue(slog.String("password", "configured-secret"))}))
	if attr.Value.Kind() != slog.KindGroup || attr.Value.Group()[0].Value.String() != "[REDACTED]" {
		t.Fatalf("resolved group not redacted: %v", attr)
	}
}

func TestTextRedactionIsBestEffort(t *testing.T) {
	t.Setenv("REDIS_PASSWORD", "configured-secret")
	attr := safeAttr(slog.String("message", "configured-secret and unknown-sensitive-text"))
	if attr.Value.String() != "[REDACTED] and unknown-sensitive-text" {
		t.Fatalf("unexpected text handling: %v", attr)
	}
	errAttr := safeAttr(slog.Any("error", errors.New("configured-secret: connection failed")))
	if errAttr.Value.Kind() != slog.KindString || errAttr.Value.String() != "[REDACTED]: connection failed" {
		t.Fatalf("lost sanitized error context: %v", errAttr)
	}
}
