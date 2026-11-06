package telemetry

import (
	"context"
	"fmt"
	"sort"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
)

// Emit writes a LifecycleEvent as an OTel LogRecord.
//
// Attribute namespaces:
//   - event.*             event metadata
//   - alert.*             normalized alert lifecycle fields
//   - alert.label.*       labels from the incoming alert payload
//   - alert.annotation.*  annotations from the incoming alert payload
//   - am.*                Alertmanager envelope metadata
//
// Field mapping:
//   - Timestamp  <- event.EndsAt (resolved) or event.StartsAt (firing)
//   - Body       <- "alert lifecycle event"
//   - Severity   <- derived from alert labels["severity"]
func (e *OTelEmitter) Emit(ctx context.Context, event models.LifecycleEvent) error {
	var r otellog.Record

	// Use the most relevant time as the log record timestamp.
	if !event.EndsAt.IsZero() {
		r.SetTimestamp(event.EndsAt)
	} else {
		r.SetTimestamp(event.StartsAt)
	}

	r.SetBody(attribute.StringValue("alert lifecycle event"))
	r.SetSeverity(alertSeverityToOTel(event.Labels["severity"]))
	r.SetSeverityText(event.Labels["severity"])

	attrs := []attribute.KeyValue{
		attribute.String("event.kind", event.EventKind),
		attribute.Int("event.version", event.EventVersion),
		attribute.String("alert.transition", event.Transition),
		attribute.String("alert.status", event.Status),
		attribute.String("alert.fingerprint", event.Fingerprint),
		attribute.String("alert.fingerprint_source", event.FingerprintSrc),
		attribute.String("alert.alertname", event.Alertname),
		attribute.String("alert.starts_at", event.StartsAt.UTC().Format("2006-01-02T15:04:05Z")),
		attribute.Float64("alert.duration_seconds", event.DurationSeconds),
		attribute.String("alert.state_write_result", event.StateWriteResult),
		attribute.String("alert.generator_url", event.GeneratorURL),
		attribute.String("am.group_key", event.AMGroupKey),
		attribute.String("am.receiver", event.AMReceiver),
		attribute.String("am.external_url", event.AMExternalURL),
	}

	if !event.EndsAt.IsZero() {
		attrs = append(attrs, attribute.String("alert.ends_at", event.EndsAt.UTC().Format("2006-01-02T15:04:05Z")))
	}
	if event.OrphanReason != "" {
		attrs = append(attrs, attribute.String("alert.orphan_reason", event.OrphanReason))
	}

	// Flatten labels as alert.label.{key} attributes.
	for _, k := range sortedKeys(event.Labels) {
		attrs = append(attrs, attribute.String(fmt.Sprintf("alert.label.%s", k), event.Labels[k]))
	}

	// Flatten annotations as alert.annotation.{key} attributes.
	for _, k := range sortedKeys(event.Annotations) {
		attrs = append(attrs, attribute.String(fmt.Sprintf("alert.annotation.%s", k), event.Annotations[k]))
	}

	r.AddAttributes(attrs...)
	e.logger.Emit(ctx, r)
	return nil
}

func alertSeverityToOTel(severity string) otellog.Severity {
	switch severity {
	case "critical":
		return otellog.SeverityFatal
	case "error":
		return otellog.SeverityError
	case "warning", "warn":
		return otellog.SeverityWarn
	case "info":
		return otellog.SeverityInfo
	default:
		return otellog.SeverityInfo
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
