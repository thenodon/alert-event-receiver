package slaproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/httpsecurity/securitytest"
)

func TestEndpointSecurity(t *testing.T) {
	for _, secure := range []bool{false, true} {
		for _, auth := range []string{"none", "webhook", "metrics", "both"} {
			t.Run(map[bool]string{true: "https", false: "http"}[secure]+"/"+auth, func(t *testing.T) {
				securitytest.ClearEnv(t)
				if auth == "both" || auth == "webhook" {
					securitytest.Auth(t, "WEBHOOK", "alerts", "web-secret")
				}
				if auth == "both" || auth == "metrics" {
					securitytest.Auth(t, "METRICS", "scraper", "metric-secret")
				}
				cert, key, roots := securitytest.Certificate(t)
				if secure {
					t.Setenv("HTTP_TLS_CERT_FILE", cert)
					t.Setenv("HTTP_TLS_KEY_FILE", key)
				}
				cfg := testConfig(t)
				cfg.WebhookPath = "/custom-alerts"
				var logs bytes.Buffer
				s, err := New(cfg, slog.New(slog.NewJSONHandler(&logs, nil)))
				if err != nil {
					t.Fatal(err)
				}
				var calls atomic.Int32
				original := s.evaluator
				s.evaluator = evaluatorFunc(func(ctx context.Context, input PolicyInput, ref PolicyRef, sink string, at time.Time) (Decision, error) {
					calls.Add(1)
					return original.Evaluate(ctx, input, ref, sink, at)
				})
				listener := listenTest(t)
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- s.Serve(ctx, listener) }()
				defer func() {
					cancel()
					if err := awaitStopped(t, done); err != nil {
						t.Error(err)
					}
					if strings.Contains(logs.String(), "web-secret") || strings.Contains(logs.String(), "metric-secret") || strings.Contains(logs.String(), "Authorization") {
						t.Error("credentials in logs")
					}
				}()
				transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
				defer transport.CloseIdleConnections()
				client := &http.Client{Transport: transport, Timeout: time.Second}
				base := s.security.Scheme() + "://" + listener.Addr().String()
				payload := testPayload(t, testAlert("firing", map[string]string{"sla_id": "business"}))
				request := func(method, path, user, password string) int {
					req, err := http.NewRequest(method, base+path, bytes.NewReader(payload))
					if err != nil {
						t.Fatal(err)
					}
					if user != "" {
						req.SetBasicAuth(user, password)
					}
					response, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					io.Copy(io.Discard, response.Body)
					response.Body.Close()
					return response.StatusCode
				}
				want := 200
				if auth == "both" || auth == "webhook" {
					want = 401
				}
				if got := request("POST", cfg.WebhookPath, "", ""); got != want {
					t.Fatal(got, want)
				}
				if want == 401 {
					if calls.Load() != 0 {
						t.Fatal("unauthorized webhook reached evaluator")
					}
					if request("POST", cfg.WebhookPath, "scraper", "metric-secret") != 401 {
						t.Fatal("cross-authorized")
					}
				}
				if request("POST", cfg.WebhookPath, "alerts", "web-secret") != 200 || calls.Load() == 0 {
					t.Fatal("authorized webhook not evaluated")
				}
				want = 200
				if auth == "both" || auth == "metrics" {
					want = 401
				}
				if got := request("GET", "/metrics", "", ""); got != want {
					t.Fatal("metrics", got, want)
				}
				if want == 401 && request("GET", "/metrics", "alerts", "web-secret") != 401 {
					t.Fatal("cross-authorized")
				}
				if request("GET", "/metrics", "scraper", "metric-secret") != 200 {
					t.Fatal("authorized metrics failed")
				}
				if request("GET", "/healthz", "", "") != 200 || request("GET", "/readyz", "", "") != 200 {
					t.Fatal("probes protected")
				}
				if request("POST", "/webhook", "", "") != 404 || request("GET", "/missing", "", "") != 404 {
					t.Fatal("routing changed")
				}
				// Handler() retains auth even when intentionally tested without listener TLS.
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, httptest.NewRequest("POST", cfg.WebhookPath, bytes.NewReader(payload)))
				if auth == "both" || auth == "webhook" {
					if w.Code != 401 {
						t.Fatal("Handler bypasses auth")
					}
				}
				if secure {
					before := calls.Load()
					resp, err := client.Post("http://"+listener.Addr().String()+cfg.WebhookPath, "application/json", bytes.NewReader(payload))
					if resp != nil {
						resp.Body.Close()
					}
					if err == nil && resp.StatusCode == 200 {
						t.Fatal("plaintext accepted")
					}
					if calls.Load() != before {
						t.Fatal("plaintext evaluated")
					}
				}
			})
		}
	}
}

func TestSecurityValidationPrecedesSinksAndValkey(t *testing.T) {
	securitytest.ClearEnv(t)
	t.Setenv("HTTP_TLS_CERT_FILE", "/missing/cert")
	cfg := testConfig(t)
	cfg.Durable.Enabled = true
	cfg.Durable.Valkey.Address = "127.0.0.1:1"
	_, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "inbound HTTP security") || strings.Contains(err.Error(), "state store") {
		t.Fatal("security validated too late", err)
	}
}

func TestSecurityDoesNotChangePolicyIdentity(t *testing.T) {
	securitytest.ClearEnv(t)
	cfg := testConfig(t)
	before, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer before.Close()
	securitytest.Auth(t, "WEBHOOK", "alerts", "one-secret")
	securitytest.Auth(t, "METRICS", "scraper", "another-secret")
	cert, key, _ := securitytest.Certificate(t)
	t.Setenv("HTTP_TLS_CERT_FILE", cert)
	t.Setenv("HTTP_TLS_KEY_FILE", key)
	after, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	left, right := before.evaluator.(*CalendarEngine), after.evaluator.(*CalendarEngine)
	for ref, p := range left.policies {
		if p.version != right.policies[ref].version {
			t.Fatal("security changed policy version")
		}
	}
	if before.cfg.Durable.PolicyEpoch != after.cfg.Durable.PolicyEpoch {
		t.Fatal("security changed epoch")
	}
}
