package slaproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	s, err := New(testConfig(t), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testAlert(status string, labels map[string]string) models.Alert {
	start := time.Date(2026, 11, 11, 8, 0, 0, 123456789, time.FixedZone("CET", 3600))
	a := models.Alert{
		Status: status, Fingerprint: "original-fingerprint", StartsAt: start,
		Labels: labels, Annotations: map[string]string{"summary": "original annotation"},
		GeneratorURL: "http://prometheus.example/graph",
	}
	if status == "resolved" {
		a.EndsAt = start.Add(time.Hour)
	}
	return a
}

func testPayload(t *testing.T, alerts ...models.Alert) []byte {
	t.Helper()
	if alerts == nil {
		alerts = []models.Alert{}
	}
	p := models.WebhookPayload{
		Version: "4", Receiver: "sla-proxy", Status: "firing", GroupKey: "original-group",
		ExternalURL: "http://alertmanager.example", Alerts: alerts,
		GroupLabels:       map[string]string{"job": "demo"},
		CommonLabels:      map[string]string{"team": "ops"},
		CommonAnnotations: map[string]string{"description": "example"},
	}
	if len(alerts) > 0 && alerts[0].Status == "resolved" {
		p.Status = "resolved"
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func post(s *Service, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, s.cfg.WebhookPath, bytes.NewReader(body)))
	return w
}

func counter(t *testing.T, s *Service, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := s.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.Metric {
			got := map[string]string{}
			for _, label := range m.Label {
				got[label.GetName()] = label.GetValue()
			}
			if reflect.DeepEqual(got, labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("missing metric %s%v", name, labels)
	return 0
}

func TestObserveFiringResolvedAndBatch(t *testing.T) {
	for _, statuses := range [][]string{{"firing"}, {"resolved"}, {"firing", "resolved", "firing"}, {}} {
		t.Run(strings.Join(statuses, "+"), func(t *testing.T) {
			s := newTestService(t)
			var alerts []models.Alert
			for _, status := range statuses {
				alerts = append(alerts, testAlert(status, map[string]string{"sla_id": "business"}))
			}
			w := post(s, testPayload(t, alerts...))
			var result Observation
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
				t.Fatalf("%d: %s", w.Code, w.Body)
			}
			if result.Mode != "observe_only" || result.Persisted || result.Forwarded || result.Received != len(alerts) || result.Selected != len(alerts) || result.Skipped != 0 {
				t.Fatalf("unexpected acknowledgement: %+v", result)
			}
			counts := map[string]int{}
			for _, status := range statuses {
				counts[status]++
			}
			for _, status := range []string{"firing", "resolved"} {
				if got := counter(t, s, "sla_proxy_alerts_received_total", map[string]string{"status": status}); got != float64(counts[status]) {
					t.Fatal("wrong received count", got)
				}
				if got := counter(t, s, "sla_proxy_alerts_selected_total", map[string]string{"sla_label": "sla_id", "sla_value": "business", "status": status}); got != float64(counts[status]) {
					t.Fatal("wrong selected count", got)
				}
			}
		})
	}
}

func TestOrderedSelectionAndCountedSkips(t *testing.T) {
	s := newTestService(t)
	alerts := []models.Alert{
		testAlert("firing", map[string]string{"sla_id": "business", "sla_name": "business"}),
		testAlert("resolved", map[string]string{"sla_name": "business"}),
		testAlert("firing", map[string]string{"sla_id": "", "sla_name": "business"}),
		testAlert("firing", map[string]string{"sla_id": "unknown", "sla_name": "business"}),
		testAlert("resolved", map[string]string{"sla_id": ""}),
		testAlert("firing", nil),
	}
	w := post(s, testPayload(t, alerts...))
	var result Observation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if result.Received != 6 || result.Selected != 3 || result.Skipped != 3 {
		t.Fatal(result)
	}
	for reason, want := range map[string]float64{"missing_sla_label": 2, "unknown_sla": 1} {
		if got := counter(t, s, "sla_proxy_alerts_skipped_total", map[string]string{"reason": reason}); got != want {
			t.Fatalf("%s=%v", reason, got)
		}
	}
	if got := counter(t, s, "sla_proxy_alerts_selected_total", map[string]string{"sla_label": "sla_id", "sla_value": "business", "status": "firing"}); got != 1 {
		t.Fatal(got)
	}
	if got := counter(t, s, "sla_proxy_alerts_selected_total", map[string]string{"sla_label": "sla_name", "sla_value": "business", "status": "firing"}); got != 1 {
		t.Fatal(got)
	}
	// The same name in two entries identifies two different policies.
	ref, reason := s.cfg.Select(alerts[1])
	if ref != (PolicyRef{"sla_name", "business"}) || reason != "selected" {
		t.Fatal(ref, reason)
	}
	ref, reason = s.cfg.Select(alerts[3])
	if ref != (PolicyRef{"sla_id", "unknown"}) || reason != "unknown_sla" {
		t.Fatal("unexpected fallback", ref, reason)
	}
}

func TestInvalidWebhookHasNoAlertObservations(t *testing.T) {
	valid := testAlert("firing", map[string]string{"sla_id": "business"})
	base := string(testPayload(t, valid))
	cases := map[string][]byte{
		"invalid json": []byte("{"), "null": []byte("null"), "array": []byte("[]"),
		"missing metadata":    []byte(`{"alerts":[]}`),
		"null alerts":         []byte(`{"version":"4","receiver":"sla-proxy","status":"firing","alerts":null}`),
		"unknown root field":  []byte(strings.Replace(base, `"version":`, `"unexpected":1,"version":`, 1)),
		"unknown alert field": []byte(strings.Replace(base, `"fingerprint":`, `"unexpected":1,"fingerprint":`, 1)),
		"duplicate field":     []byte(strings.Replace(base, `"fingerprint":`, `"fingerprint":"different","fingerprint":`, 1)),
		"duplicate label":     []byte(strings.Replace(base, `"sla_id":"business"`, `"sla_id":"unknown","sla_id":"business"`, 1)),
		"trailing document":   append([]byte(base), []byte(" {}")...),
		"trailing garbage":    append([]byte(base), []byte(" !")...),
		"malformed timestamp": []byte(strings.Replace(base, valid.StartsAt.Format(time.RFC3339Nano), "yesterday", 1)),
	}
	resolved := testAlert("resolved", map[string]string{"sla_id": "business"})
	cases["malformed endsAt"] = []byte(strings.Replace(string(testPayload(t, resolved)), resolved.EndsAt.Format(time.RFC3339Nano), "tomorrow", 1))
	for _, field := range []string{"version", "receiver", "status", "alerts"} {
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(base), &object); err != nil {
			t.Fatal(err)
		}
		delete(object, field)
		encoded, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		cases["missing "+field] = encoded
	}
	for name, mutate := range map[string]func(*models.Alert){
		"missing startsAt":     func(a *models.Alert) { a.StartsAt = time.Time{} },
		"missing fingerprint":  func(a *models.Alert) { a.Fingerprint = "" },
		"missing status":       func(a *models.Alert) { a.Status = "" },
		"unknown status":       func(a *models.Alert) { a.Status = "pending" },
		"missing endsAt":       func(a *models.Alert) { a.Status = "resolved" },
		"invalid endsAt order": func(a *models.Alert) { a.Status = "resolved"; a.EndsAt = a.StartsAt.Add(-time.Nanosecond) },
	} {
		a := testAlert("firing", map[string]string{"sla_id": "business"})
		mutate(&a)
		cases[name] = testPayload(t, valid, a)
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			s := newTestService(t)
			w := post(s, data)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%d: %s", w.Code, w.Body)
			}
			if got := counter(t, s, "sla_proxy_notifications_received_total", map[string]string{"status": "4xx"}); got != 1 {
				t.Fatal(got)
			}
			if got := counter(t, s, "sla_proxy_alerts_received_total", map[string]string{"status": "firing"}); got != 0 {
				t.Fatal("partially observed invalid batch", got)
			}
		})
	}
}

func TestHTTPRoutesLimitsAndIndependentRegistries(t *testing.T) {
	s := newTestService(t)
	s.cfg.MaxBodyBytes = 10
	if w := post(s, []byte(strings.Repeat(" ", 11))); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatal(w.Code)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/webhook", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "POST" {
		t.Fatal(w.Code, w.Header())
	}
	s.ready.Store(false)
	if w := post(s, []byte("{}")); w.Code != 503 {
		t.Fatal(w.Code)
	}
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 503, "/missing": 404} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != want {
			t.Fatal(path, w.Code)
		}
	}
	other := newTestService(t) // Must not collide with any global collector.
	if got := counter(t, other, "sla_proxy_notifications_received_total", map[string]string{"status": "4xx"}); got != 0 {
		t.Fatal("registry shared", got)
	}
	cfg := testConfig(t)
	cfg.WebhookPath = "/alerts"
	custom, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(custom.Handler())
	defer server.Close()
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 200, "/webhook": 404, "/metrics": 200} {
		resp, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != want {
			t.Fatal(path, resp.StatusCode, err)
		}
		if path == "/metrics" && (!bytes.Contains(body, []byte("sla_proxy_alerts_skipped_total")) || bytes.Contains(body, []byte("alertreceiver_")) || bytes.Contains(body, []byte("delivery_attempts"))) {
			t.Fatalf("unexpected metrics: %s", body)
		}
	}
	resp, err := server.Client().Post(server.URL+"/alerts", "application/json", bytes.NewReader(testPayload(t, testAlert("firing", map[string]string{"sla_id": "business"}))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
}

func TestNoopEnrichmentAndWireIdentityPreserved(t *testing.T) {
	original := testAlert("resolved", map[string]string{"sla_id": "business", "site_id": "original"})
	data := testPayload(t, original)
	var decoded *models.WebhookPayload
	if err := decodeWebhook(data, &decoded); err != nil {
		t.Fatal(err)
	}
	a := decoded.Alerts[0]
	if !a.StartsAt.Equal(original.StartsAt) || a.StartsAt.Nanosecond() != original.StartsAt.Nanosecond() || !a.EndsAt.Equal(original.EndsAt) || a.Fingerprint != original.Fingerprint || !reflect.DeepEqual(a.Labels, original.Labels) || !reflect.DeepEqual(a.Annotations, original.Annotations) || a.GeneratorURL != original.GeneratorURL || decoded.GroupKey != "original-group" || decoded.CommonLabels["team"] != "ops" {
		t.Fatal("wire identity/metadata changed", decoded)
	}
	metadata, err := (NoopEnricher{}).Enrich(context.Background(), a)
	if err != nil || len(metadata) != 0 || !reflect.DeepEqual(a.Labels, original.Labels) {
		t.Fatal("no-op changed labels", err)
	}
	s := newTestService(t)
	// Observe-only processing includes every notification, even repeats, and does
	// not check whether their synthetic timestamps are inside an SLA window.
	for i := 0; i < 2; i++ {
		if w := post(s, data); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if got := counter(t, s, "sla_proxy_alerts_selected_total", map[string]string{"sla_label": "sla_id", "sla_value": "business", "status": "resolved"}); got != 2 {
		t.Fatal(got)
	}
}
