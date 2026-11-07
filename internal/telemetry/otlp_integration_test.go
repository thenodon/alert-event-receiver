package telemetry

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	collogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
)

func TestHTTPExporterResultsIncludingPartialRejection(t *testing.T) {
	for _, mode := range []string{"success", "error", "partial"} {
		t.Run(mode, func(t *testing.T) {
			reply := &collogpb.ExportLogsServiceResponse{}
			if mode == "partial" {
				reply.PartialSuccess = &collogpb.ExportLogsPartialSuccess{RejectedLogRecords: 1, ErrorMessage: "one record rejected"}
			}
			encoded, err := proto.Marshal(reply)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if mode == "error" {
					http.Error(w, "invalid batch", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
				_, _ = w.Write(encoded)
			}))
			defer server.Close()
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http") // canonical metric label is http/protobuf.
			t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", server.URL)
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
			t.Setenv("OTEL_EXPORTER_OTLP_LOGS_HEADERS", "")
			t.Setenv("OTEL_BLRP_SCHEDULE_DELAY", "3600000")
			prior := otel.GetLoggerProvider()
			defer otel.SetLoggerProvider(prior)
			var logs lockedLogBuffer
			shutdown, err := InitOTel(context.Background(), "integration-test", slog.New(slog.NewJSONHandler(&logs, nil)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = shutdown(ctx)
			})
			result := "success"
			if mode != "success" {
				result = "error"
			}
			requests := OTLPExportRequests.WithLabelValues("http/protobuf", result)
			records := OTLPExportRecords.WithLabelValues("http/protobuf", result)
			req0, rec0 := testutil.ToFloat64(requests), testutil.ToFloat64(records)
			emitter := NewOTelEmitter("integration-test", "test")
			// Two records enter a call where the partial response rejects just one.
			for range 2 {
				if err := emitter.Emit(context.Background(), models.LifecycleEvent{EventKind: "alert_transition", Transition: "firing", StartsAt: time.Now(), Fingerprint: "fp"}); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = shutdown(ctx)
			if mode == "success" && err != nil || mode != "success" && err == nil {
				t.Fatalf("mode=%s shutdown error=%v", mode, err)
			}
			if testutil.ToFloat64(requests)-req0 != 1 || testutil.ToFloat64(records)-rec0 != 2 {
				t.Fatal("exporter call result accounting incorrect")
			}
			if mode == "partial" && !strings.Contains(logs.String(), "1 logs rejected") {
				t.Fatal("partial rejection not visible", logs.String())
			}
		})
	}
}

func TestOTelInitializationRejectsInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, protocol, headers string }{
		{"protocol", "unknown", ""}, {"headers", "http/protobuf", "private-credential"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", tc.protocol)
			t.Setenv("OTEL_EXPORTER_OTLP_LOGS_HEADERS", tc.headers)
			shutdown, err := InitOTel(context.Background(), "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err == nil || shutdown != nil {
				t.Fatal("expected initialization error")
			}
			if strings.Contains(err.Error(), "private-credential") {
				t.Fatal("configuration error leaked secret")
			}
		})
	}
}
