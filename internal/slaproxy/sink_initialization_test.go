package slaproxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestRuntimeSinkInitialization(t *testing.T) {
	for _, mode := range []string{"observe_only", "durable_state_only", "delivery_enabled"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("INIT_SINK_USER", "")
			t.Setenv("INIT_SINK_PASSWORD", "")
			_ = os.Unsetenv("INIT_SINK_USER")
			_ = os.Unsetenv("INIT_SINK_PASSWORD")
			var calls atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer target.Close()
			cfg := testConfig(t)
			cfg.Durable.Enabled = mode != "observe_only"
			cfg.Delivery.Enabled = mode == "delivery_enabled"
			httpConfig := &HTTPSinkConfig{URL: target.URL, Timeout: time.Second}
			httpConfig.BasicAuth.Username = "${INIT_SINK_USER}"
			httpConfig.BasicAuth.Password = "${INIT_SINK_PASSWORD}"
			cfg.Delivery.Sinks = []SinkConfig{{ID: "first", Type: "http", HTTP: httpConfig}, {ID: "audit", Type: "log"}, {ID: "second", Type: "http", HTTP: httpConfig}}
			if cfg.Durable.Enabled {
				cfg.Durable.Valkey.Address = miniredis.RunT(t).Addr()
			}
			logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
			s, err := New(cfg, logger)
			if cfg.Delivery.Enabled {
				if err == nil {
					_ = s.Close()
					t.Fatal("accepted missing credentials")
				}
				// Fail after a first HTTP sink has initialized, then retry successfully.
				first := *httpConfig
				first.BasicAuth.Username, first.BasicAuth.Password = "literal-user", "literal-password"
				cfg.Delivery.Sinks[0].HTTP = &first
				if s, err = New(cfg, logger); err == nil {
					_ = s.Close()
					t.Fatal("accepted missing second sink credentials")
				}
				t.Setenv("INIT_SINK_USER", "runtime-user")
				t.Setenv("INIT_SINK_PASSWORD", "runtime-password")
				s, err = New(cfg, logger)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if s.mode() != mode {
				t.Fatal(s.mode())
			}
			if cfg.Delivery.Enabled {
				if s.dispatcher == nil || s.httpTransport == nil || len(s.dispatcher.sinks) != 3 {
					t.Fatal("missing runtime sinks")
				}
				first := s.dispatcher.sinks["first"].(*HTTPSink)
				second := s.dispatcher.sinks["second"].(*HTTPSink)
				if first.client != second.client || first.client.Transport != s.httpTransport || second.username != "runtime-user" {
					t.Fatal("incorrect transport ownership or credentials")
				}
				if _, ok := s.dispatcher.sinks["audit"].(LogSink); !ok {
					t.Fatal("missing log sink")
				}
			} else {
				if s.dispatcher != nil || s.httpTransport != nil {
					t.Fatal("disabled delivery initialized runtime resources")
				}
				response := post(s, testPayload(t, testAlert("firing", map[string]string{"sla_id": "business"})))
				if response.Code != http.StatusOK {
					t.Fatal(response.Code, response.Body.String())
				}
			}
			if calls.Load() != 0 {
				t.Fatal("initialization or disabled delivery sent HTTP requests")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDisabledDeliveryStillValidatesSinkStructure(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delivery.Sinks = []SinkConfig{{ID: "invalid", Type: "http", HTTP: &HTTPSinkConfig{URL: "not-a-url", Timeout: time.Second}}}
	if _, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil))); err == nil || !strings.Contains(err.Error(), "HTTP") {
		t.Fatal("invalid disabled sink accepted", err)
	}
}
