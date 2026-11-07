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
