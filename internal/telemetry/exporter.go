package telemetry

import (
	"context"
	"log/slog"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// observedExporter observes public exporter calls, not wire attempts or backend
// persistence. The underlying exporter owns retry behavior. A failed call can
// have partially succeeded; its record count must never be read as loss count.
type observedExporter struct {
	sdklog.Exporter
	protocol string
	logger   *slog.Logger
}

func (e *observedExporter) Export(ctx context.Context, records []sdklog.Record) error {
	started := time.Now()
	err := e.Exporter.Export(ctx, records)
	duration := time.Since(started).Seconds()
	result := "success"
	if err != nil {
		result = "error"
		e.logger.ErrorContext(ctx, "OTLP export call failed", "component", "otlp_exporter", "operation", "export",
			"protocol", e.protocol, "record_count", len(records), "error", SafeError(err))
	}
	OTLPExportRequests.WithLabelValues(e.protocol, result).Inc()
	OTLPExportRecords.WithLabelValues(e.protocol, result).Add(float64(len(records)))
	OTLPExportDuration.WithLabelValues(e.protocol).Observe(duration)
	return err
}

func (e *observedExporter) ForceFlush(ctx context.Context) error {
	err := e.Exporter.ForceFlush(ctx)
	if err != nil {
		e.logger.ErrorContext(ctx, "OTLP exporter flush failed", "component", "otlp_exporter", "operation", "force_flush", "protocol", e.protocol, "error", SafeError(err))
	}
	return err
}

func (e *observedExporter) Shutdown(ctx context.Context) error {
	err := e.Exporter.Shutdown(ctx)
	if err != nil {
		e.logger.ErrorContext(ctx, "OTLP exporter shutdown failed", "component", "otlp_exporter", "operation", "shutdown", "protocol", e.protocol, "error", SafeError(err))
	}
	return err
}
