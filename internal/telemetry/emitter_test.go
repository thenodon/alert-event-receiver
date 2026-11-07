package telemetry

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type recordExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (e *recordExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, record := range records {
		e.records = append(e.records, record.Clone())
	}
	return nil
}
func (*recordExporter) Shutdown(context.Context) error   { return nil }
func (*recordExporter) ForceFlush(context.Context) error { return nil }

func TestEmitterRecord(t *testing.T) {
	for _, transition := range []string{models.TransitionFiring, models.TransitionResolved} {
		for _, orphan := range []bool{false, true} {
			if orphan && transition == models.TransitionFiring {
				continue
			}
			name := transition
			if orphan {
				name += "/orphan"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				exporter := &recordExporter{}
				provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
				t.Cleanup(func() {
					if err := provider.Shutdown(ctx); err != nil {
						t.Error(err)
					}
				})
				emitter := &OTelEmitter{logger: provider.Logger("emitter-test")}
				start := time.Date(2026, 11, 6, 12, 0, 0, 123456789, time.FixedZone("local", 3600))
				event := models.LifecycleEvent{
					EventKind: "alert_transition", EventVersion: 1, Transition: transition, Status: transition,
					Fingerprint: "abc123", FingerprintSrc: "alertname+severity", Alertname: "HighErrorRate",
					Labels:      map[string]string{"alertname": "HighErrorRate", "severity": "warning", "service": "checkout"},
					Annotations: map[string]string{"summary": "Error rate high", "description": "Investigate checkout"},
					StartsAt:    start, GeneratorURL: "https://prometheus.example/graph", AMGroupKey: "group", AMReceiver: "receiver", AMExternalURL: "https://am.example", StateWriteResult: "opened",
				}
				expectedTimestamp := start
				expected := map[string]attribute.Value{
					"event.id":   attribute.StringValue(models.LifecycleEventID(event.Fingerprint, event.StartsAt, event.Transition)),
					"event.kind": attribute.StringValue("alert_transition"), "event.version": attribute.IntValue(1),
					"alert.transition": attribute.StringValue(transition), "alert.status": attribute.StringValue(transition),
					"alert.fingerprint": attribute.StringValue("abc123"), "alert.fingerprint_source": attribute.StringValue("alertname+severity"),
					"alert.alertname": attribute.StringValue("HighErrorRate"), "alert.starts_at": attribute.StringValue("2026-11-06T11:00:00Z"),
					"alert.duration_seconds": attribute.Float64Value(0), "alert.state_write_result": attribute.StringValue("opened"),
					"alert.generator_url": attribute.StringValue("https://prometheus.example/graph"),
					"am.group_key":        attribute.StringValue("group"), "am.receiver": attribute.StringValue("receiver"), "am.external_url": attribute.StringValue("https://am.example"),
					"alert.label.alertname": attribute.StringValue("HighErrorRate"), "alert.label.severity": attribute.StringValue("warning"), "alert.label.service": attribute.StringValue("checkout"),
					"alert.annotation.summary": attribute.StringValue("Error rate high"), "alert.annotation.description": attribute.StringValue("Investigate checkout"),
				}
				if transition == models.TransitionResolved {
					event.EndsAt = start.Add(90*time.Second + 500*time.Millisecond)
					event.DurationSeconds = 90.5
					event.StateWriteResult = "closed"
					expectedTimestamp = event.EndsAt
					expected["alert.ends_at"] = attribute.StringValue("2026-11-06T11:01:30Z")
					expected["alert.duration_seconds"] = attribute.Float64Value(90.5)
					expected["alert.state_write_result"] = attribute.StringValue("closed")
				}
				if orphan {
					event.OrphanReason = "open_state_missing"
					event.StateWriteResult = "resolved_without_open_state"
					expected["alert.orphan_reason"] = attribute.StringValue("open_state_missing")
					expected["alert.state_write_result"] = attribute.StringValue("resolved_without_open_state")
				}
				if err := emitter.Emit(ctx, event); err != nil {
					t.Fatal(err)
				}
				exporter.mu.Lock()
				defer exporter.mu.Unlock()
				if len(exporter.records) != 1 {
					t.Fatalf("got %d records", len(exporter.records))
				}
				record := exporter.records[0]
				if !record.Timestamp().Equal(expectedTimestamp) {
					t.Errorf("timestamp = %s, want %s", record.Timestamp(), expectedTimestamp)
				}
				if !reflect.DeepEqual(record.Body(), attribute.StringValue("alert lifecycle event")) {
					t.Errorf("body = %v", record.Body())
				}
				if record.Severity() != otellog.SeverityWarn || record.SeverityText() != "warning" {
					t.Errorf("severity = %v %q", record.Severity(), record.SeverityText())
				}
				actual := map[string]attribute.Value{}
				record.WalkAttributes(func(kv attribute.KeyValue) bool { actual[string(kv.Key)] = kv.Value; return true })
				if record.AttributesLen() != len(expected) || !reflect.DeepEqual(actual, expected) {
					t.Errorf("attributes mismatch\ngot: %#v\nwant: %#v", actual, expected)
				}
			})
		}
	}
}

func TestEmitterSeverity(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  otellog.Severity
	}{
		{"critical", otellog.SeverityFatal}, {"error", otellog.SeverityError}, {"warning", otellog.SeverityWarn}, {"warn", otellog.SeverityWarn}, {"info", otellog.SeverityInfo}, {"", otellog.SeverityInfo}, {"custom", otellog.SeverityInfo},
	} {
		t.Run(tc.label, func(t *testing.T) {
			exporter := &recordExporter{}
			provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
			t.Cleanup(func() {
				if err := provider.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
			})
			emitter := &OTelEmitter{logger: provider.Logger("severity-test")}
			if err := emitter.Emit(context.Background(), models.LifecycleEvent{Labels: map[string]string{"severity": tc.label}}); err != nil {
				t.Fatal(err)
			}
			exporter.mu.Lock()
			defer exporter.mu.Unlock()
			if len(exporter.records) != 1 {
				t.Fatalf("got %d records", len(exporter.records))
			}
			record := exporter.records[0]
			if record.Severity() != tc.want || record.SeverityText() != tc.label {
				t.Errorf("got %v %q, want %v %q", record.Severity(), record.SeverityText(), tc.want, tc.label)
			}
		})
	}
}
