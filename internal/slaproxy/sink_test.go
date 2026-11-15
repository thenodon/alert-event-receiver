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
	deliveryAt := time.Date(2026, 11, 13, 9, 0, 0, 987654321, time.UTC)
	sink := LogSink{Handler: slog.NewJSONHandler(&output, nil), Now: func() time.Time { return deliveryAt }}
	at := time.Date(2026, 11, 13, 7, 30, 0, 123456789, time.UTC)
	e := DeliveryEvent{IntentID: "stable-intent", OccurrenceID: "occurrence", BindingID: "binding", SinkID: "default", Action: "firing", Policy: PolicyRef{"sla_id", "bronze"}, PolicyVersion: "version", StartsAt: at, EligibleAt: at, Attempt: 2}
	e.Notification.Alert.Labels = map[string]string{"secret": "never-log-me"}
	e.Notification.Alert.Annotations = map[string]string{"token": "never-log-me"}
	if err := sink.Deliver(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"msg": "SLA alert delivered", "component": "sla_sink", "sink_type": "log", "sink_id": "default", "action": "firing", "intent_id": "stable-intent", "occurrence_id": "occurrence", "binding_id": "binding", "sla_label": "sla_id", "sla_value": "bronze", "policy_version": "version", "delivery_attempt": float64(2), "time": deliveryAt.Format(time.RFC3339Nano), "delivery_at": deliveryAt.Format(time.RFC3339Nano), "starts_at": at.Format(time.RFC3339Nano), "eligible_at": at.Format(time.RFC3339Nano)} {
		if record[key] != want {
			t.Fatal(key, record[key], want)
		}
	}
	if strings.Contains(output.String(), "never-log-me") {
		t.Fatal("payload leaked")
	}
	output.Reset()
	endsAt := at.Add(30 * time.Minute)
	e.Action = "resolved"
	e.EndsAt = &endsAt
	if err := sink.Deliver(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["time"] != deliveryAt.Format(time.RFC3339Nano) || record["delivery_at"] != deliveryAt.Format(time.RFC3339Nano) || record["starts_at"] != at.Format(time.RFC3339Nano) || record["ends_at"] != endsAt.Format(time.RFC3339Nano) {
		t.Fatal("resolved delivery timestamp changed alert timestamps", record)
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
		"http":    func(d *DeliveryConfig) { d.Sinks[0].Type = "http" },
		"workers": func(d *DeliveryConfig) { d.Dispatcher.Workers = 0 },
		"batch":   func(d *DeliveryConfig) { d.Dispatcher.BatchSize = 0 },
		"poll":    func(d *DeliveryConfig) { d.Dispatcher.PollInterval = 0 },
		"lease":   func(d *DeliveryConfig) { d.Dispatcher.ClaimLease = d.Dispatcher.DeliveryTimeout },
		"timeout": func(d *DeliveryConfig) { d.Dispatcher.DeliveryTimeout = 0 },
		"backoff": func(d *DeliveryConfig) { d.Dispatcher.RetryMax = time.Millisecond },
	} {
		t.Run(name, func(t *testing.T) {
			bad := c
			bad.Delivery.Sinks = append([]SinkConfig(nil), c.Delivery.Sinks...)
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
	if !cfg.Delivery.Enabled || cfg.Delivery.Sinks[0].Type != "log" {
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

func TestMultiSinkConfiguration(t *testing.T) {
	validHTTP := func() SinkConfig {
		return SinkConfig{ID: "servicenow", Type: "http", HTTP: &HTTPSinkConfig{URL: "https://sn.example/api?source=prometheus", Timeout: time.Second}}
	}
	for name, mutate := range map[string]func(*DeliveryConfig){
		"empty":          func(d *DeliveryConfig) { d.Sinks = nil },
		"duplicate":      func(d *DeliveryConfig) { d.Sinks = append(d.Sinks, d.Sinks[0]) },
		"empty ID":       func(d *DeliveryConfig) { d.Sinks[0].ID = "" },
		"unsafe ID":      func(d *DeliveryConfig) { d.Sinks[0].ID = "a:{b}" },
		"unknown":        func(d *DeliveryConfig) { d.Sinks[0].Type = "servicenow" },
		"missing HTTP":   func(d *DeliveryConfig) { d.Sinks[0].HTTP = nil },
		"zero timeout":   func(d *DeliveryConfig) { d.Sinks[0].HTTP.Timeout = 0 },
		"excess timeout": func(d *DeliveryConfig) { d.Sinks[0].HTTP.Timeout = time.Hour },
		"partial auth":   func(d *DeliveryConfig) { d.Sinks[0].HTTP.BasicAuth.UsernameEnv = "USER" },
		"invalid env": func(d *DeliveryConfig) {
			d.Sinks[0].HTTP.BasicAuth.UsernameEnv = "bad-env"
			d.Sinks[0].HTTP.BasicAuth.PasswordEnv = "PASS"
		},
		"log HTTP settings": func(d *DeliveryConfig) { d.Sinks[0].Type = "log" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Durable.Enabled = true
			cfg.Delivery.Enabled = true
			cfg.Delivery.Sinks = []SinkConfig{validHTTP()}
			mutate(&cfg.Delivery)
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid sink accepted")
			}
		})
	}
	for _, url := range []string{"", "://broken", "ftp://example.com", "https://", "https://user:secret@example.com", "https://example.com/#fragment", "http://example.com:0", "https://example.com:65536", "https://example.com:", "https://example.com/a b"} {
		sink := validHTTP()
		sink.HTTP.URL = url
		if err := sink.Validate(10 * time.Second); err == nil {
			t.Errorf("accepted URL %q", url)
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatal("URL leaked")
		}
	}
	cfg := testConfig(t)
	cfg.Durable.Enabled = true
	cfg.Delivery.Enabled = true
	cfg.Delivery.Sinks = []SinkConfig{validHTTP(), {ID: "second-http", Type: "http", HTTP: &HTTPSinkConfig{URL: "http://localhost:9080/hook", Timeout: time.Second}}, {ID: "audit", Type: "log"}, {ID: "other-log", Type: "log"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []string{"durable:\n  sink_id: default\n", "delivery:\n  sink: {type: log}\n"} {
		if _, err := LoadConfig(strings.NewReader(exampleConfig + legacy)); err == nil {
			t.Fatal("legacy schema accepted")
		}
	}
}

func TestHTTPDeliveryConfigurationExample(t *testing.T) {
	f, err := os.Open("../../docs/examples/alert-sla-proxy-delivery-http.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := LoadConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Delivery.Enabled || len(cfg.Delivery.Sinks) != 2 || cfg.Delivery.Sinks[0].Type != "http" || cfg.Delivery.Sinks[0].HTTP.BasicAuth.UsernameEnv != "SERVICENOW_USERNAME" || cfg.Delivery.Sinks[1].Type != "log" {
		t.Fatal("invalid multi-sink example")
	}
}
