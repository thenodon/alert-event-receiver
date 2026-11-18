package httpsecurity

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/httpsecurity/securitytest"
)

func fromMap(values map[string]string) (Config, error) {
	return load(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
}

func TestEnvironmentDefaultsAndValidation(t *testing.T) {
	for _, endpoint := range []string{"WEBHOOK", "METRICS"} {
		prefix := "HTTP_" + endpoint + "_BASIC_AUTH_"
		cases := []struct {
			name, user, reference, password string
			valid                           bool
		}{
			{"absent", "", "", "", true}, {"valid", "reader", "SECRET", "sensitive-password", true},
			{"user only", "reader", "", "", false}, {"reference only", "", "SECRET", "secret", false},
			{"blank user", " \t", "SECRET", "secret", false}, {"colon", "bad:user", "SECRET", "secret", false},
			{"control user", "bad\x00user", "SECRET", "secret", false}, {"newline user", "bad\nuser", "SECRET", "secret", false},
			{"blank reference", "reader", " ", "secret", false}, {"inline secret", "reader", "${SECRET}", "secret", false},
			{"invalid reference", "reader", "bad-name", "secret", false}, {"empty password", "reader", "SECRET", "", false},
			{"newline password", "reader", "SECRET", "sensitive-password\n", false}, {"control password", "reader", "SECRET", "secret\x7f", false},
		}
		for _, tc := range cases {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				cfg, err := fromMap(map[string]string{prefix + "USERNAME": tc.user, prefix + "PASSWORD_ENV": tc.reference, "SECRET": tc.password})
				if (err == nil) != tc.valid {
					t.Fatalf("valid=%v err=%v", tc.valid, err)
				}
				if err != nil && strings.Contains(err.Error(), "sensitive-password") {
					t.Fatal("password leaked")
				}
				if err == nil && cfg.Scheme() != "http" {
					t.Fatal("unexpected TLS")
				}
			})
		}
		if _, err := fromMap(map[string]string{prefix + "USERNAME": "reader", prefix + "PASSWORD_ENV": "MISSING"}); err == nil {
			t.Fatal("missing secret accepted")
		}
	}
}

func TestMiddleware(t *testing.T) {
	cfg, err := fromMap(map[string]string{"HTTP_WEBHOOK_BASIC_AUTH_USERNAME": "web", "HTTP_WEBHOOK_BASIC_AUTH_PASSWORD_ENV": "WEB_SECRET", "WEB_SECRET": "web-secret",
		"HTTP_METRICS_BASIC_AUTH_USERNAME": "scrape", "HTTP_METRICS_BASIC_AUTH_PASSWORD_ENV": "METRIC_SECRET", "METRIC_SECRET": "metric-secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, realm := range []string{"webhook", "metrics"} {
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(204) })
		var protected http.Handler = cfg.Webhook(handler)
		goodUser, goodPass := "web", "web-secret"
		if realm == "metrics" {
			protected = cfg.Metrics(handler)
			goodUser, goodPass = "scrape", "metric-secret"
		}
		headers := []string{"", "Bearer secret", "Basic !!!", "Basic Og==", "Basic d2Vi", "Basic d3Jvbmc6d2ViLXNlY3JldA=="}
		for _, header := range headers {
			req := httptest.NewRequest("POST", "/", nil)
			req.Header.Set("Authorization", header)
			w := httptest.NewRecorder()
			protected.ServeHTTP(w, req)
			if w.Code != 401 || w.Header().Get("WWW-Authenticate") != `Basic realm="`+realm+`"` || calls != 0 {
				t.Fatalf("invalid auth reached handler: %d %d", w.Code, calls)
			}
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatal("leaked input")
			}
		}
		for _, pair := range [][2]string{{goodUser, "wrong"}, {"wrong", goodPass}, {"web", "metric-secret"}, {"scrape", "web-secret"}} {
			req := httptest.NewRequest("POST", "/", nil)
			req.SetBasicAuth(pair[0], pair[1])
			w := httptest.NewRecorder()
			protected.ServeHTTP(w, req)
			if w.Code != 401 || calls != 0 {
				t.Fatal("wrong credentials accepted")
			}
		}
		for i := 0; i < 2; i++ {
			req := httptest.NewRequest("POST", "/", nil)
			req.SetBasicAuth(goodUser, goodPass)
			w := httptest.NewRecorder()
			protected.ServeHTTP(w, req)
			if w.Code != 204 {
				t.Fatal(w.Code)
			}
		}
		req := httptest.NewRequest("POST", "/", nil)
		w := httptest.NewRecorder()
		protected.ServeHTTP(w, req)
		if w.Code != 401 || calls != 2 {
			t.Fatal("authentication cached")
		}
	}
}

func TestIndependentAuth(t *testing.T) {
	for _, endpoint := range []string{"WEBHOOK", "METRICS"} {
		securitytest.ClearEnv(t)
		securitytest.Auth(t, endpoint, "reader", "secret")
		cfg, err := LoadFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
		for _, other := range []string{"WEBHOOK", "METRICS"} {
			handler := cfg.Webhook(next)
			if other == "METRICS" {
				handler = cfg.Metrics(next)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
			want := 204
			if endpoint == other {
				want = 401
			}
			if w.Code != want {
				t.Fatal(endpoint, other, w.Code)
			}
		}
	}
}

func TestTLSValidationAndSnapshot(t *testing.T) {
	cert, key, _ := securitytest.Certificate(t)
	otherCert, otherKey, _ := securitytest.Certificate(t)
	invalid := t.TempDir() + "/invalid.pem"
	if err := os.WriteFile(invalid, []byte("private-sensitive-material"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{cert, ""}, {"", key}, {" ", key}, {"/missing", key}, {cert, "/missing"}, {invalid, key}, {cert, invalid}, {otherCert, key}, {cert, otherKey}} {
		_, err := fromMap(map[string]string{"HTTP_TLS_CERT_FILE": pair[0], "HTTP_TLS_KEY_FILE": pair[1]})
		if err == nil || strings.Contains(err.Error(), "private-sensitive-material") {
			t.Fatal("invalid TLS accepted or leaked")
		}
	}
	cfg, err := fromMap(map[string]string{"HTTP_TLS_CERT_FILE": cert, "HTTP_TLS_KEY_FILE": key})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.tls.MinVersion != tls.VersionTLS12 || cfg.Scheme() != "https" {
		t.Fatal("unsafe TLS")
	}
	// Files are not read again on each connection.
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if len(cfg.tls.Certificates) != 1 {
		t.Fatal("missing snapshot")
	}
}

func TestServeProtocolsAndListenerOwnership(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[secure], func(t *testing.T) {
			cfg := Config{}
			cert, key, roots := securitytest.Certificate(t)
			if secure {
				var err error
				cfg, err = fromMap(map[string]string{"HTTP_TLS_CERT_FILE": cert, "HTTP_TLS_KEY_FILE": key})
				if err != nil {
					t.Fatal(err)
				}
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), ReadHeaderTimeout: time.Second}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cfg.Serve(server, listener) }()
			defer server.Close()
			transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: time.Second}
			response, err := client.Get(cfg.Scheme() + "://" + listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 204 {
				t.Fatal(response.StatusCode)
			}
			if secure {
				response, err = client.Get("http://" + listener.Addr().String())
				if err == nil {
					response.Body.Close()
					if response.StatusCode == 204 {
						t.Fatal("plaintext served")
					}
				}
				legacy := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}}
				defer legacy.CloseIdleConnections()
				resp, err := (&http.Client{Transport: legacy, Timeout: time.Second}).Get("https://" + listener.Addr().String())
				if resp != nil {
					resp.Body.Close()
				}
				if err == nil {
					t.Fatal("TLS 1.1 accepted")
				}
			}
			server.Close()
			select {
			case err := <-done:
				if err != http.ErrServerClosed {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("server did not stop")
			}
			if _, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second); err == nil {
				t.Fatal("listener not closed")
			}
		})
	}
}

func TestCredentialStartupSnapshot(t *testing.T) {
	securitytest.ClearEnv(t)
	securitytest.Auth(t, "WEBHOOK", "reader", "original-secret")
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	securitytest.Auth(t, "WEBHOOK", "reader", "rotated-secret")
	next, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	for _, tc := range []struct {
		config   Config
		password string
		want     int
	}{{cfg, "original-secret", 204}, {cfg, "rotated-secret", 401}, {next, "original-secret", 401}, {next, "rotated-secret", 204}} {
		req := httptest.NewRequest("POST", "/", nil)
		req.SetBasicAuth("reader", tc.password)
		w := httptest.NewRecorder()
		tc.config.Webhook(handler).ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatal("credentials changed without reloading configuration", w.Code, tc.want)
		}
	}
}
