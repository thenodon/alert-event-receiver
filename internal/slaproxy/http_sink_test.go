package slaproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
)

func httpEvent() DeliveryEvent {
	start := time.Date(2026, 11, 15, 7, 0, 0, 123456789, time.UTC)
	return DeliveryEvent{IntentID: "stable-intent", SinkID: "servicenow", Action: "firing", Attempt: 1, StartsAt: start, Notification: Notification{
		Alert:    models.Alert{Status: "firing", Fingerprint: "original-fingerprint", StartsAt: start, Labels: map[string]string{"alertname": "HighCPU", "sla_id": "production"}, Annotations: map[string]string{"summary": "original summary"}, GeneratorURL: "https://prometheus.example/graph"},
		Metadata: models.WebhookPayload{Version: "4", Receiver: "original-receiver", GroupKey: "original-group", GroupLabels: map[string]string{"job": "prometheus"}, CommonLabels: map[string]string{"team": "ops"}, CommonAnnotations: map[string]string{"summary": "group summary"}, ExternalURL: "https://alertmanager.example", TruncatedAlerts: 3},
	}}
}
func testHTTPSink(t *testing.T, url string, timeout time.Duration) *HTTPSink {
	t.Helper()
	client, transport := newDeliveryHTTPClient(4)
	t.Cleanup(transport.CloseIdleConnections)
	sink, err := newHTTPSink(HTTPSinkConfig{URL: url, Timeout: timeout}, client, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sink
}
func TestHTTPSinkPayloadAuthAndStableIntent(t *testing.T) {
	t.Setenv("TEST_SINK_USER", "test-user")
	t.Setenv("TEST_SINK_PASSWORD", "secret-password")
	var received []models.WebhookPayload
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Query().Get("source") != "prometheus" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Idempotency-Key") != "stable-intent" {
			t.Errorf("invalid request metadata")
		}
		user, password, ok := r.BasicAuth()
		if !ok || user != "test-user" || password != "secret-password" {
			t.Error("missing authentication")
		}
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		var p models.WebhookPayload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Error(err)
		}
		received = append(received, p)
		w.WriteHeader(202)
	}))
	defer server.Close()
	sink := testHTTPSink(t, server.URL+"/webhook?source=prometheus", time.Second)
	sink.config.BasicAuth.UsernameEnv = "TEST_SINK_USER"
	sink.config.BasicAuth.PasswordEnv = "TEST_SINK_PASSWORD"
	var logs bytes.Buffer
	var err error
	sink, err = newHTTPSink(sink.config, sink.client, slog.New(slog.NewJSONHandler(&logs, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	e := httpEvent()
	for range 2 {
		if err := sink.Deliver(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatal("retry payload changed")
	}
	e.Action = "resolved"
	e.Notification.Alert.EndsAt = e.StartsAt.Add(time.Hour)
	e.EndsAt = &e.Notification.Alert.EndsAt
	if err := sink.Deliver(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	for n, p := range received {
		expected := "firing"
		if n == 2 {
			expected = "resolved"
		}
		if p.Status != expected || len(p.Alerts) != 1 || p.Alerts[0].Status != expected || p.Receiver != "original-receiver" || p.GroupKey != "original-group" || p.Version != "4" || p.ExternalURL != "https://alertmanager.example" || p.TruncatedAlerts != 3 || p.CommonLabels["team"] != "ops" || p.GroupLabels["job"] != "prometheus" || p.CommonAnnotations["summary"] != "group summary" {
			t.Fatalf("metadata changed: %+v", p)
		}
		a := p.Alerts[0]
		if a.Fingerprint != e.Notification.Alert.Fingerprint || !a.StartsAt.Equal(e.StartsAt) || a.Annotations["summary"] != "original summary" || a.Labels["sla_id"] != "production" || len(a.Labels) != 2 || a.GeneratorURL != "https://prometheus.example/graph" {
			t.Fatalf("alert changed: %+v", a)
		}
	}
	if !received[2].Alerts[0].EndsAt.Equal(e.Notification.Alert.EndsAt) {
		t.Fatal("resolution lost")
	}
	for _, secret := range []string{"secret-password", "test-user", "original summary", "HighCPU", server.URL} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("sensitive data logged")
		}
	}
}
func TestHTTPSinkStatusClassification(t *testing.T) {
	for _, code := range []int{200, 202, 204, 400, 401, 403, 408, 429, 500, 503, 302} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(code)
				_, _ = io.WriteString(w, "secret-response")
			}))
			defer server.Close()
			err := testHTTPSink(t, server.URL, time.Second).Deliver(context.Background(), httpEvent())
			if code < 300 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var failure *DeliveryError
			permanent := code != 408 && code != 429 && code < 500
			if !errors.As(err, &failure) || failure.Permanent != permanent || failure.HTTPStatus != code || strings.Contains(err.Error(), "secret-response") {
				t.Fatalf("classification: %v", err)
			}
			if (code == 429 || code == 503) && failure.RetryAfter != 2*time.Second {
				t.Fatal("retry hint lost")
			}
		})
	}
}
func TestHTTPSinkTimeoutCancellationRedirectAndTLS(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(100 * time.Millisecond):
			}
		}))
		defer server.Close()
		began := time.Now()
		err := testHTTPSink(t, server.URL, 20*time.Millisecond).Deliver(context.Background(), httpEvent())
		if err == nil || deliveryFailure(err) != "temporary_failure" || time.Since(began) > time.Second {
			t.Fatal(err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := testHTTPSink(t, "http://127.0.0.1:1/secret?token=hidden", time.Second).Deliver(ctx, httpEvent()); err == nil || strings.Contains(err.Error(), "hidden") || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
	})
	t.Run("connection", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		url := server.URL
		server.Close()
		if err := testHTTPSink(t, url, time.Second).Deliver(context.Background(), httpEvent()); err == nil || deliveryFailure(err) != "temporary_failure" || strings.Contains(err.Error(), url) {
			t.Fatal(err)
		}
	})
	t.Run("redirect", func(t *testing.T) {
		var calls atomic.Int64
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
		defer target.Close()
		redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
		defer redirect.Close()
		err := testHTTPSink(t, redirect.URL, time.Second).Deliver(context.Background(), httpEvent())
		if err == nil || deliveryFailure(err) != "permanent_failure" || calls.Load() != 0 {
			t.Fatal(err, calls.Load())
		}
	})
	t.Run("untrusted TLS", func(t *testing.T) {
		server := httptest.NewTLSServer(http.NotFoundHandler())
		defer server.Close()
		if err := testHTTPSink(t, server.URL, time.Second).Deliver(context.Background(), httpEvent()); err == nil || deliveryFailure(err) != "temporary_failure" {
			t.Fatal(err)
		}
	})
}

type countedBody struct {
	read   int
	closed bool
}

func (b *countedBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	b.read += len(p)
	return len(p), nil
}
func (b *countedBody) Close() error { b.closed = true; return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestHTTPSinkBoundedBodyAndNoAutomaticReplay(t *testing.T) {
	body := &countedBody{}
	sink := testHTTPSink(t, "http://example.invalid", time.Second)
	disabled := false
	sink.config.IdempotencyKey = &disabled
	sink.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.GetBody != nil || r.Header.Get("Idempotency-Key") != "" {
			t.Error("replay enabled")
		}
		return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
	})}
	if err := sink.Deliver(context.Background(), httpEvent()); err != nil {
		t.Fatal(err)
	}
	if body.read > maxHTTPResponseBody+1 || !body.closed {
		t.Fatal("unbounded or unclosed response", body)
	}
}
func TestRetryAfterAndCredentialValidation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for value, want := range map[string]time.Duration{"": 0, "invalid": 0, "-1": 0, "5": 5 * time.Second, "999999999": maxRetryAfter, now.Add(7 * time.Second).Format(http.TimeFormat): 7 * time.Second, now.Add(-time.Second).Format(http.TimeFormat): 0} {
		if got := retryAfter(value, now); got != want {
			t.Errorf("%q: %v != %v", value, got, want)
		}
	}
	cfg := HTTPSinkConfig{}
	cfg.BasicAuth.UsernameEnv = "NONEXISTENT_SINK_USERNAME"
	cfg.BasicAuth.PasswordEnv = "NONEXISTENT_SINK_PASSWORD"
	if _, err := newHTTPSink(cfg, nil, nil, nil); err == nil {
		t.Fatal("accepted missing credentials")
	}
	t.Setenv(cfg.BasicAuth.UsernameEnv, "secret:user")
	t.Setenv(cfg.BasicAuth.PasswordEnv, "secret-password")
	if _, err := newHTTPSink(cfg, nil, nil, nil); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("invalid credentials not safely rejected", err)
	}
}
