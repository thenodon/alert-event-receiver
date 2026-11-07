package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type lockedLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *lockedLogBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

type stubExporter struct {
	export   func(context.Context, []sdklog.Record) error
	flush    func(context.Context) error
	shutdown func(context.Context) error
}

func (e stubExporter) Export(ctx context.Context, r []sdklog.Record) error {
	if e.export != nil {
		return e.export(ctx, r)
	}
	return nil
}
func (e stubExporter) ForceFlush(ctx context.Context) error {
	if e.flush != nil {
		return e.flush(ctx)
	}
	return nil
}
func (e stubExporter) Shutdown(ctx context.Context) error {
	if e.shutdown != nil {
		return e.shutdown(ctx)
	}
	return nil
}

func TestExporterMetrics(t *testing.T) {
	for _, protocol := range []string{"grpc", "http/protobuf"} {
		for _, result := range []string{"success", "error"} {
			t.Run(protocol+"/"+result, func(t *testing.T) {
				var logs lockedLogBuffer
				sentinel := errors.New("export failed (may be partial)")
				var want error
				if result == "error" {
					want = sentinel
				}
				original := []sdklog.Record{{}, {}}
				calls := 0
				ctx := context.WithValue(context.Background(), struct{}{}, "marker")
				exporter := &observedExporter{Exporter: stubExporter{export: func(got context.Context, records []sdklog.Record) error {
					calls++
					if got != ctx || len(records) != 2 || &records[0] != &original[0] {
						t.Error("decorator changed call")
					}
					return want
				}}, protocol: protocol, logger: slog.New(slog.NewJSONHandler(&logs, nil))}
				requests := OTLPExportRequests.WithLabelValues(protocol, result)
				records := OTLPExportRecords.WithLabelValues(protocol, result)
				reqBefore := testutil.ToFloat64(requests)
				recBefore := testutil.ToFloat64(records)
				sdkBefore := testutil.ToFloat64(OTelSDKErrors)
				hist := OTLPExportDuration.WithLabelValues(protocol)
				var histBefore dto.Metric
				hist.(interface{ Write(*dto.Metric) error }).Write(&histBefore)
				if err := exporter.Export(ctx, original); !errors.Is(err, want) {
					t.Fatalf("got %v, want %v", err, want)
				}
				if calls != 1 || testutil.ToFloat64(requests)-reqBefore != 1 || testutil.ToFloat64(records)-recBefore != 2 {
					t.Fatal("export call accounting incorrect")
				}
				var histAfter dto.Metric
				hist.(interface{ Write(*dto.Metric) error }).Write(&histAfter)
				if histAfter.GetHistogram().GetSampleCount()-histBefore.GetHistogram().GetSampleCount() != 1 {
					t.Fatal("duration not observed")
				}
				if testutil.ToFloat64(OTelSDKErrors) != sdkBefore {
					t.Fatal("decorator counted an SDK diagnostic")
				}
				if result == "error" {
					for _, field := range []string{`"component":"otlp_exporter"`, `"operation":"export"`, `"record_count":2`, `"error":`} {
						if !strings.Contains(logs.String(), field) {
							t.Errorf("missing %s: %s", field, logs.String())
						}
					}
					if strings.Contains(logs.String(), "event_id") || strings.Contains(logs.String(), "alert_fingerprint") {
						t.Fatal("batch error attributed to an event")
					}
				} else if logs.String() != "" {
					t.Fatal("unexpected success log")
				}
			})
		}
	}
}

func TestAsyncExportFailure(t *testing.T) {
	var logs lockedLogBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	prior := otel.GetErrorHandler()
	ConfigureSDKDiagnostics(logger)
	t.Cleanup(func() { otel.SetErrorHandler(prior); otel.SetLogger(logr.Discard()) })
	sentinel := errors.New("asynchronous export failure")
	reported := make(chan struct{}, 1)
	// Delegate to the installed diagnostic handler and signal only after it finishes.
	handler := otel.GetErrorHandler()
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { handler.Handle(err); reported <- struct{}{} }))
	exporter := &observedExporter{Exporter: stubExporter{export: func(_ context.Context, r []sdklog.Record) error {
		if len(r) == 0 {
			return nil
		}
		return sentinel
	}}, protocol: "grpc", logger: logger}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter, sdklog.WithExportMaxBatchSize(1))))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	errCounter := OTLPExportRequests.WithLabelValues("grpc", "error")
	before := testutil.ToFloat64(errCounter)
	sdkBefore := testutil.ToFloat64(OTelSDKErrors)
	emitter := &OTelEmitter{logger: provider.Logger("async-test")}
	if err := emitter.Emit(context.Background(), models.LifecycleEvent{Fingerprint: "fp", StartsAt: time.Now(), Transition: "firing"}); err != nil {
		t.Fatal("SDK submission should return nil", err)
	}
	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		t.Fatal("SDK did not report async export failure")
	}
	if testutil.ToFloat64(errCounter)-before != 1 || testutil.ToFloat64(OTelSDKErrors)-sdkBefore != 1 {
		t.Fatal("export and SDK diagnostic accounting incorrect")
	}
	if !strings.Contains(logs.String(), "OTLP export call failed") || !strings.Contains(logs.String(), "OpenTelemetry SDK diagnostic") {
		t.Fatal("missing structured diagnostics", logs.String())
	}
}

func TestShutdownFlushesPendingRecords(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			var logs lockedLogBuffer
			var count int
			flushCalled, shutdownCalled := false, false
			sentinel := errors.New("shutdown export failure")
			exporter := &observedExporter{Exporter: stubExporter{
				export: func(_ context.Context, r []sdklog.Record) error {
					count += len(r)
					if fail && len(r) > 0 {
						return sentinel
					}
					return nil
				},
				flush: func(context.Context) error { flushCalled = true; return nil }, shutdown: func(context.Context) error { shutdownCalled = true; return nil },
			}, protocol: "grpc", logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter, sdklog.WithExportInterval(time.Hour))))
			emitter := &OTelEmitter{logger: provider.Logger("shutdown-test")}
			if err := emitter.Emit(context.Background(), models.LifecycleEvent{}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := provider.Shutdown(ctx)
			if fail && !errors.Is(err, sentinel) || !fail && err != nil {
				t.Fatalf("shutdown error: %v", err)
			}
			if count != 1 || !flushCalled || !shutdownCalled {
				t.Fatalf("pending batch not flushed/closed: %d %v %v", count, flushCalled, shutdownCalled)
			}
		})
	}
}

func TestExporterLifecycleErrors(t *testing.T) {
	var logs lockedLogBuffer
	sentinel := errors.New("lifecycle failure")
	exporter := &observedExporter{Exporter: stubExporter{flush: func(context.Context) error { return sentinel }, shutdown: func(context.Context) error { return sentinel }}, protocol: "grpc", logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	before := testutil.ToFloat64(OTLPExportRequests.WithLabelValues("grpc", "error"))
	if !errors.Is(exporter.ForceFlush(context.Background()), sentinel) || !errors.Is(exporter.Shutdown(context.Background()), sentinel) {
		t.Fatal("lost lifecycle error")
	}
	if testutil.ToFloat64(OTLPExportRequests.WithLabelValues("grpc", "error")) != before {
		t.Fatal("lifecycle error counted as export")
	}
	if !strings.Contains(logs.String(), `"operation":"force_flush"`) || !strings.Contains(logs.String(), `"operation":"shutdown"`) {
		t.Fatal("missing lifecycle logs")
	}
}

func TestQueueDropsAreDiagnosticsNotExporterFailures(t *testing.T) {
	var logs lockedLogBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	prior := otel.GetErrorHandler()
	ConfigureSDKDiagnostics(logger)
	t.Cleanup(func() { otel.SetErrorHandler(prior); otel.SetLogger(logr.Discard()) })
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	exporter := &observedExporter{Exporter: stubExporter{export: func(_ context.Context, r []sdklog.Record) error {
		if len(r) > 0 {
			once.Do(func() { close(started); <-release })
		}
		return nil
	}}, protocol: "grpc", logger: logger}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter, sdklog.WithMaxQueueSize(1), sdklog.WithExportMaxBatchSize(1))))
	defer func() {
		unblock()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := provider.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	emitter := &OTelEmitter{logger: provider.Logger("queue-test")}
	errBefore := testutil.ToFloat64(OTLPExportRequests.WithLabelValues("grpc", "error"))
	sdkErrorsBefore := testutil.ToFloat64(OTelSDKErrors)
	if err := emitter.Emit(context.Background(), models.LifecycleEvent{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("export did not start")
	}
	for range 3 {
		if err := emitter.Emit(context.Background(), models.LifecycleEvent{}); err != nil {
			t.Fatal("submission unexpectedly detected queue loss")
		}
	}
	// Shutdown flushes the pending queue and its dropped-record diagnostic after
	// the first export returns. Wait for shutdown to finish before reading logs.
	unblock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := provider.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "dropped log records") || !strings.Contains(logs.String(), `"component":"otel_sdk"`) {
		t.Fatal("queue-drop warning not routed to structured logs", logs.String())
	}
	if testutil.ToFloat64(OTLPExportRequests.WithLabelValues("grpc", "error")) != errBefore {
		t.Fatal("queue drop mislabeled as export error")
	}
	if testutil.ToFloat64(OTelSDKErrors) != sdkErrorsBefore {
		t.Fatal("queue warning mislabeled as an SDK error-handler diagnostic")
	}
}
