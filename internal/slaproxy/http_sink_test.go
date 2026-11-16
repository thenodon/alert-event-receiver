package slaproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	sink.config.BasicAuth.Username = "${TEST_SINK_USER}"
	sink.config.BasicAuth.Password = "${TEST_SINK_PASSWORD}"
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
		assertHTTPFailure(t, err, "timeout")
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := testHTTPSink(t, "http://127.0.0.1:1/secret?token=hidden", time.Second).Deliver(ctx, httpEvent())
		assertHTTPFailure(t, err, "canceled")
	})
	t.Run("connection", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		url := server.URL
		server.Close()
		err := testHTTPSink(t, url, time.Second).Deliver(context.Background(), httpEvent())
		assertHTTPFailure(t, err, "connection_error")
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
		err := testHTTPSink(t, server.URL, time.Second).Deliver(context.Background(), httpEvent())
		assertHTTPFailure(t, err, "tls_error")
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
	cfg.BasicAuth.Username = "${NONEXISTENT_SINK_USERNAME}"
	cfg.BasicAuth.Password = "${NONEXISTENT_SINK_PASSWORD}"
	if _, err := newHTTPSink(cfg, nil, nil, nil); err == nil {
		t.Fatal("accepted missing credentials")
	}
	t.Setenv("NONEXISTENT_SINK_USERNAME", "secret:user")
	t.Setenv("NONEXISTENT_SINK_PASSWORD", "secret-password")
	if _, err := newHTTPSink(cfg, nil, nil, nil); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("invalid credentials not safely rejected", err)
	}
}

func assertHTTPFailure(t *testing.T, err error, class string) {
	t.Helper()
	var failure *DeliveryError
	if !errors.As(err, &failure) || failure.FailureClass != class || failure.Permanent || failure.HTTPStatus != 0 || failure.RetryAfter != 0 || deliveryFailure(err) != "temporary_failure" {
		t.Fatalf("unexpected %s failure: %+v", class, failure)
	}
}

func TestHTTPTransportDiagnosticsAreBoundedAndSanitized(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		class string
	}{
		{"fallback", errors.New("secret-password https://private.example/path?token=hidden Authorization: secret"), "transport_error"},
		{"wrapped cancellation", fmt.Errorf("secret: %w", context.Canceled), "canceled"},
		{"wrapped deadline", fmt.Errorf("secret: %w", context.DeadlineExceeded), "timeout"},
		{"network timeout", &net.DNSError{Err: "secret", Name: "private.example", IsTimeout: true}, "timeout"},
		{"dns", &net.DNSError{Err: "secret", Name: "private.example"}, "connection_error"},
		{"certificate hostname", x509.HostnameError{Host: "private.example"}, "tls_error"},
		{"certificate invalid", x509.CertificateInvalidError{}, "tls_error"},
		{"certificate authority", x509.UnknownAuthorityError{}, "tls_error"},
		{"TLS record", tls.RecordHeaderError{Msg: "secret"}, "tls_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			sink := testHTTPSink(t, "https://private.example/path?token=hidden", time.Second)
			sink.username, sink.password = "secret-user", "secret-password"
			sink.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			service := newTestService(t)
			sink.metrics = service.metrics.delivery
			sink.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, &url.Error{Op: "Post", URL: sink.config.URL, Err: tc.cause}
			})}
			err := sink.Deliver(context.Background(), httpEvent())
			assertHTTPFailure(t, err, tc.class)
			var record map[string]any
			if e := json.Unmarshal(logs.Bytes(), &record); e != nil {
				t.Fatal(e)
			}
			if record["failure_class"] != tc.class || record["outcome"] != "temporary_failure" {
				t.Fatal(record)
			}
			if counter(t, service, "sla_proxy_http_requests_total", map[string]string{"sink_id": "servicenow", "status_class": "network_error", "result": "temporary_failure"}) != 1 {
				t.Fatal("metric semantics changed")
			}
			diagnostic := logs.String() + err.Error() + errors.Unwrap(err).Error()
			for _, sensitive := range []string{"private.example", "hidden", "secret", "Authorization", "/path", "original summary"} {
				if strings.Contains(diagnostic, sensitive) {
					t.Fatalf("sensitive diagnostic: %s", diagnostic)
				}
			}
			var raw *url.Error
			if errors.As(err, &raw) {
				t.Fatal("retained raw URL error")
			}
		})
	}
}
