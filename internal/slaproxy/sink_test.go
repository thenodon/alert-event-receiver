package slaproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("sensitive-handler-secret") }

func TestLogSinkRecordAndWriteErrors(t *testing.T) {
	var output bytes.Buffer
	sink := LogSink{Handler: slog.NewJSONHandler(&output, nil)}
	at := time.Date(2026, 11, 13, 7, 43, 0, 123456789, time.UTC)
	e := DeliveryEvent{IntentID: "stable-intent", OccurrenceID: "occurrence", BindingID: "binding", SinkID: "default", Action: "firing", Policy: PolicyRef{"sla_id", "bronze"}, PolicyVersion: "version", StartsAt: at, EligibleAt: at, EventAt: at, Attempt: 2}
	e.Notification.Alert.Labels = map[string]string{"secret": "never-log-me"}
	e.Notification.Alert.Annotations = map[string]string{"token": "never-log-me"}
	if err := sink.Deliver(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"msg": "SLA alert delivered", "component": "sla_sink", "sink_type": "log", "sink_id": "default", "action": "firing", "intent_id": "stable-intent", "occurrence_id": "occurrence", "binding_id": "binding", "sla_label": "sla_id", "sla_value": "bronze", "policy_version": "version", "delivery_attempt": float64(2), "time": at.Format(time.RFC3339Nano), "starts_at": at.Format(time.RFC3339Nano), "eligible_at": at.Format(time.RFC3339Nano)} {
		if record[key] != want {
			t.Fatal(key, record[key], want)
		}
	}
	if strings.Contains(output.String(), "never-log-me") {
		t.Fatal("payload leaked")
	}
	sink.Handler = slog.NewJSONHandler(errorWriter{}, nil)
	if err := sink.Deliver(context.Background(), e); err == nil {
		t.Fatal("handler write error discarded")
	}
	sink.Handler = slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelError})
	if err := sink.Deliver(context.Background(), e); err == nil {
		t.Fatal("filtered record treated as delivery")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sink.Deliver(ctx, e); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestDeliveryConfigurationDefaultsAndValidation(t *testing.T) {
	c := testConfig(t)
	if c.Delivery.Enabled {
		t.Fatal("delivery implicitly enabled")
	}
	c.Delivery.Enabled = true
	if err := c.Validate(); err == nil {
		t.Fatal("stateless delivery accepted")
	}
	c.Durable.Enabled = true
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Delivery.Dispatcher.ClaimLease != 30*time.Second || c.Delivery.Dispatcher.DeliveryTimeout != 10*time.Second {
		t.Fatal(c.Delivery)
	}
	for name, mutate := range map[string]func(*DeliveryConfig){
		"http":    func(d *DeliveryConfig) { d.Sink.Type = "http" },
		"workers": func(d *DeliveryConfig) { d.Dispatcher.Workers = 0 },
		"batch":   func(d *DeliveryConfig) { d.Dispatcher.BatchSize = 0 },
		"poll":    func(d *DeliveryConfig) { d.Dispatcher.PollInterval = 0 },
		"lease":   func(d *DeliveryConfig) { d.Dispatcher.ClaimLease = d.Dispatcher.DeliveryTimeout },
		"timeout": func(d *DeliveryConfig) { d.Dispatcher.DeliveryTimeout = 0 },
		"backoff": func(d *DeliveryConfig) { d.Dispatcher.RetryMax = time.Millisecond },
	} {
		t.Run(name, func(t *testing.T) {
			bad := c
			mutate(&bad.Delivery)
			if err := bad.Validate(); err == nil {
				t.Fatal("invalid delivery config accepted")
			}
		})
	}
	if _, err := LoadConfig(strings.NewReader(exampleConfig + "delivery:\n  enabled: false\n  sink: {unsupported: value}\n")); err == nil {
		t.Fatal("unknown key accepted")
	}
}

func TestLoggingDeliveryConfigurationExample(t *testing.T) {
	f, err := os.Open("../../docs/examples/alert-sla-proxy-delivery-log.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := LoadConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Delivery.Enabled || cfg.Delivery.Sink.Type != "log" {
		t.Fatal(cfg.Delivery)
	}
}

func TestDeliveryBackoffBoundsAndCapJitter(t *testing.T) {
	for _, tc := range []struct {
		attempt uint64
		jitter  float64
		want    time.Duration
	}{
		{1, 0, 5 * time.Second}, {1, 1, 6250 * time.Millisecond},
		{2, 0, 10 * time.Second}, {100, 0, 4 * time.Minute}, {100, 1, 5 * time.Minute},
	} {
		if got := deliveryRetryDelay(5*time.Second, 5*time.Minute, tc.attempt, tc.jitter); got != tc.want {
			t.Fatal(tc, got)
		}
	}
}
