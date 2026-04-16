package telemetry

import (
	"context"
	"fmt"
	"sort"

	otellog "go.opentelemetry.io/otel/log"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
)

// Emit writes a LifecycleEvent as an OTel LogRecord.
//
// Field mapping:
//   - Timestamp  ← event.EndsAt (resolved) or event.StartsAt (firing)
//   - Body       ← "alert lifecycle event"
//   - Severity   ← derived from alert labels["severity"]
//   - Attributes ← all other event fields, labels (label.*), annotations (annotation.*)
func (e *OTelEmitter) Emit(ctx context.Context, event models.LifecycleEvent) error {
	var r otellog.Record

	// Use the most relevant time as the log record timestamp.
	if !event.EndsAt.IsZero() {
		r.SetTimestamp(event.EndsAt)
	} else {
		r.SetTimestamp(event.StartsAt)
	}

	r.SetBody(otellog.StringValue("alert lifecycle event"))
	r.SetSeverity(alertSeverityToOTel(event.Labels["severity"]))
	r.SetSeverityText(event.Labels["severity"])

	attrs := []otellog.KeyValue{
		otellog.String("event_kind", event.EventKind),
		otellog.Int("event_version", event.EventVersion),
		otellog.String("transition", event.Transition),
		otellog.String("status", event.Status),
		otellog.String("fingerprint", event.Fingerprint),
		otellog.String("fingerprint_source", event.FingerprintSrc),
		otellog.String("alertname", event.Alertname),
		otellog.String("startsAt", event.StartsAt.UTC().Format("2006-01-02T15:04:05Z")),
		otellog.Float64("duration_seconds", event.DurationSeconds),
		otellog.String("state_write_result", event.StateWriteResult),
		otellog.String("generatorURL", event.GeneratorURL),
		otellog.String("am_group_key", event.AMGroupKey),
		otellog.String("am_receiver", event.AMReceiver),
		otellog.String("am_external_url", event.AMExternalURL),
	}

	if !event.EndsAt.IsZero() {
		attrs = append(attrs, otellog.String("endsAt", event.EndsAt.UTC().Format("2006-01-02T15:04:05Z")))
	}
	if event.OrphanReason != "" {
		attrs = append(attrs, otellog.String("orphan_reason", event.OrphanReason))
	}

	// Flatten labels as label.{key} attributes.
	for _, k := range sortedKeys(event.Labels) {
		attrs = append(attrs, otellog.String(fmt.Sprintf("label.%s", k), event.Labels[k]))
	}

	// Flatten annotations as annotation.{key} attributes.
	for _, k := range sortedKeys(event.Annotations) {
		attrs = append(attrs, otellog.String(fmt.Sprintf("annotation.%s", k), event.Annotations[k]))
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

