package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/andersh/alertmanager_event_receiver/internal/httpsecurity"
	"github.com/andersh/alertmanager_event_receiver/internal/httpsecurity/securitytest"
	"github.com/andersh/alertmanager_event_receiver/internal/models"
	"github.com/andersh/alertmanager_event_receiver/internal/processor"
	"github.com/andersh/alertmanager_event_receiver/internal/webhook"
)

type securityProcessor struct{ calls atomic.Int32 }

func (p *securityProcessor) Process(context.Context, models.Alert, string, string, string) (processor.ProcessingResult, error) {
	p.calls.Add(1)
	return processor.ProcessingResult{}, nil
}

func TestReceiverEndpointSecurity(t *testing.T) {
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
				security, err := httpsecurity.LoadFromEnv()
				if err != nil {
					t.Fatal(err)
				}
				proc := &securityProcessor{}
				server := newHTTPServer("127.0.0.1:0", security, webhook.NewHandler(proc, slog.New(slog.NewTextHandler(io.Discard, nil))))
				if server.ReadHeaderTimeout != 5*time.Second || server.ReadTimeout != 10*time.Second || server.WriteTimeout != 15*time.Second || server.IdleTimeout != 60*time.Second {
					t.Fatal("timeouts changed")
				}
				listener, err := net.Listen("tcp", server.Addr)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- security.Serve(server, listener) }()
				t.Cleanup(func() {
					server.Close()
					select {
					case err := <-done:
						if err != http.ErrServerClosed {
							t.Error(err)
						}
					case <-time.After(time.Second):
						t.Error("serve did not stop")
					}
				})
				transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
				defer transport.CloseIdleConnections()
				client := &http.Client{Transport: transport, Timeout: time.Second}
				base := security.Scheme() + "://" + listener.Addr().String()
				payload := `{"alerts":[{"status":"firing","fingerprint":"security-test","startsAt":"2026-11-18T08:00:00Z"}]}`
				request := func(method, path, user, password string) int {
					req, err := http.NewRequest(method, base+path, strings.NewReader(payload))
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
				if got := request("POST", "/webhook", "", ""); auth == "both" || auth == "webhook" {
					if got != 401 || proc.calls.Load() != 0 {
						t.Fatal("unauthorized webhook processed", got, proc.calls.Load())
					}
					if got := request("POST", "/webhook", "scraper", "metric-secret"); got != 401 {
						t.Fatal("metrics credentials authorized webhook")
					}
				} else if got != 200 {
					t.Fatal(got)
				}
				if got := request("POST", "/webhook", "alerts", "web-secret"); got != 200 {
					t.Fatal(got)
				}
				want := 200
				if auth == "both" || auth == "metrics" {
					want = 401
				}
				if got := request("GET", "/metrics", "", ""); got != want {
					t.Fatal("metrics", got, want)
				}
				if want == 401 && request("GET", "/metrics", "alerts", "web-secret") != 401 {
					t.Fatal("webhook credentials authorized metrics")
				}
				if request("GET", "/metrics", "scraper", "metric-secret") != 200 {
					t.Fatal("authorized metrics failed")
				}
				if request("GET", "/healthz", "", "") != 200 {
					t.Fatal("health protected")
				}
				if request("GET", "/readyz", "", "") != 404 || request("POST", "/missing", "", "") != 404 || request("GET", "/webhook", "", "") != 405 {
					t.Fatal("routing changed")
				}
				if secure {
					// Plain HTTP cannot reach the application on the TLS listener.
					before := proc.calls.Load()
					resp, err := client.Post("http://"+listener.Addr().String()+"/webhook", "application/json", strings.NewReader(payload))
					if resp != nil {
						resp.Body.Close()
					}
					if err == nil && resp.StatusCode == 200 {
						t.Fatal("plaintext accepted")
					}
					if proc.calls.Load() != before {
						t.Fatal("plaintext side effects")
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := server.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestReceiverInvalidSecurityBeforeStatefulStartup(t *testing.T) {
	securitytest.ClearEnv(t)
	t.Setenv("HTTP_WEBHOOK_BASIC_AUTH_USERNAME", "alerts")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReceiverInvalidSecurityHelper$")
	cmd.Env = append(os.Environ(), "HTTP_SECURITY_STARTUP_TEST=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, output)
	}
	if !strings.Contains(string(output), "invalid HTTP security configuration") || strings.Contains(string(output), "OpenTelemetry shutdown") || strings.Contains(string(output), "Valkey") {
		t.Fatalf("late security failure: %s", output)
	}
}
func TestReceiverInvalidSecurityHelper(t *testing.T) {
	if os.Getenv("HTTP_SECURITY_STARTUP_TEST") == "1" {
		if code := run(); code != 1 {
			t.Fatal(code)
		}
	}
}

func TestReceiverDefaultRoutesInProcess(t *testing.T) {
	server := newHTTPServer("", httpsecurity.Config{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(202) }))
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, httptest.NewRequest("POST", "/webhook", nil))
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
}

// Exercise run itself (including shared ListenAndServe and OTel cleanup), rather
// than only the same mux/Serve API used by the focused handler tests above.
func TestReceiverProcessHTTPAndHTTPS(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{true: "https", false: "http"}[secure], func(t *testing.T) {
			securitytest.ClearEnv(t)
			cert, key, roots := securitytest.Certificate(t)
			if secure {
				t.Setenv("HTTP_TLS_CERT_FILE", cert)
				t.Setenv("HTTP_TLS_KEY_FILE", key)
				securitytest.Auth(t, "WEBHOOK", "alerts", "process-secret")
			}
			fixture := miniredis.RunT(t)
			reservation, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := reservation.Addr().String()
			reservation.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReceiverProcessHelper$")
			cmd.Env = append(os.Environ(), "HTTP_SECURITY_PROCESS_TEST=1", "ADDRESS="+address, "REDIS_ADDR="+fixture.Addr(), "REDIS_PASSWORD=", "REDIS_DB=0", "OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf", "OTEL_EXPORTER_OTLP_HEADERS=", "OTEL_EXPORTER_OTLP_LOGS_HEADERS=", "OTEL_RESOURCE_ATTRIBUTES=")
			var logs bytes.Buffer
			cmd.Stdout = &logs
			cmd.Stderr = &logs
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			stopped := false
			defer func() {
				if !stopped {
					cmd.Process.Kill()
					cmd.Wait()
				}
			}()
			transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 200 * time.Millisecond}
			scheme := "http"
			if secure {
				scheme = "https"
			}
			base := scheme + "://" + address
			deadline := time.Now().Add(5 * time.Second)
			for {
				resp, err := client.Get(base + "/healthz")
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode != 200 {
						t.Fatal(resp.StatusCode)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("receiver failed to start", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			// An empty batch exercises the real handler without generating OTLP exports.
			req, err := http.NewRequest("POST", base+"/webhook", strings.NewReader(`{"alerts":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			want := 200
			if secure {
				want = 401
			}
			if resp.StatusCode != want {
				t.Fatal(resp.StatusCode, want)
			}
			if secure {
				req, err = http.NewRequest("POST", base+"/webhook", strings.NewReader(`{"alerts":[]}`))
				if err != nil {
					t.Fatal(err)
				}
				req.SetBasicAuth("alerts", "process-secret")
				resp, err = client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatal(resp.StatusCode)
				}
			}
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			stopped = true
			if err != nil || !strings.Contains(logs.String(), "OpenTelemetry shutdown completed") {
				t.Fatalf("shutdown: %v %s", err, logs.String())
			}
			if strings.Contains(logs.String(), "process-secret") {
				t.Fatal("secret logged")
			}
		})
	}
}
func TestReceiverProcessHelper(t *testing.T) {
	if os.Getenv("HTTP_SECURITY_PROCESS_TEST") == "1" {
		os.Exit(run())
	}
}
