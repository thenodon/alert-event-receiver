package slaproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBasicAuthLiteralEnvironmentAndMixed(t *testing.T) {
	t.Setenv("AUTH_TEST_USER", "environment-user")
	t.Setenv("AUTH_TEST_PASS", "environment-password")
	for _, tc := range []struct{ name, user, password, wantUser, wantPassword string }{
		{"literal", "literal-user", "literal-password", "literal-user", "literal-password"},
		{"environment", "${AUTH_TEST_USER}", "${AUTH_TEST_PASS}", "environment-user", "environment-password"},
		{"mixed", "literal-user", "${AUTH_TEST_PASS}", "literal-user", "environment-password"},
		{"literal dollars", "user", "$not-expanded", "user", "$not-expanded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				if !ok || user != tc.wantUser || password != tc.wantPassword {
					t.Error("incorrect resolved credentials")
				}
				w.WriteHeader(204)
			}))
			defer server.Close()
			client, transport := newDeliveryHTTPClient(1)
			defer transport.CloseIdleConnections()
			cfg := HTTPSinkConfig{URL: server.URL, Timeout: time.Second}
			cfg.BasicAuth.Username = tc.user
			cfg.BasicAuth.Password = tc.password
			sink, err := newHTTPSink(cfg, client, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := sink.Deliver(context.Background(), httpEvent()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestCredentialReferencesAreStrictAndSinglePass(t *testing.T) {
	for _, value := range []string{"${}", "${bad-name}", "${NAME", "prefix${NAME}", "${NAME}suffix", "${A}${B}"} {
		if _, _, err := credentialReference(value); err == nil {
			t.Errorf("accepted malformed reference %q", value)
		}
	}
	t.Setenv("AUTH_TEST_EMPTY", "")
	if _, err := resolveCredential("${AUTH_TEST_EMPTY}"); err == nil {
		t.Fatal("accepted empty environment variable")
	}
	t.Setenv("AUTH_TEST_NESTED", "${DO_NOT_EXPAND}")
	if value, err := resolveCredential("${AUTH_TEST_NESTED}"); err != nil || value != "${DO_NOT_EXPAND}" {
		t.Fatal("recursively expanded environment value")
	}
}
func TestLiteralCredentialsExcludedFromDestinationIdentity(t *testing.T) {
	cfg := SinkConfig{ID: "http", Type: "http", HTTP: &HTTPSinkConfig{URL: "https://example.com/webhook", Timeout: time.Second}}
	cfg.HTTP.BasicAuth.Username = "literal-user"
	cfg.HTTP.BasicAuth.Password = "literal-secret"
	before := cfg.fingerprint()
	cfg.HTTP.BasicAuth.Username = "rotated-user"
	cfg.HTTP.BasicAuth.Password = "rotated-secret"
	if cfg.fingerprint() != before {
		t.Fatal("literal credential values changed destination fingerprint")
	}
	if cfg.HTTP.BasicAuth.Password != "rotated-secret" {
		t.Fatal("fingerprinting mutated configured credentials")
	}
	cfg.HTTP.BasicAuth.Username = "${AUTH_TEST_USER}"
	cfg.HTTP.BasicAuth.Password = "${AUTH_TEST_PASS}"
	envFingerprint := cfg.fingerprint()
	t.Setenv("AUTH_TEST_PASS", "rotated-secret")
	if cfg.fingerprint() != envFingerprint {
		t.Fatal("resolved secret affected identity")
	}
	cfg.HTTP.BasicAuth.Password = "${OTHER_AUTH_TEST_PASS}"
	if cfg.fingerprint() == envFingerprint {
		t.Fatal("credential reference change silently accepted")
	}
}
func TestLiteralCredentialErrorsDoNotExposeValues(t *testing.T) {
	cfg := HTTPSinkConfig{}
	cfg.BasicAuth.Username = "user:secret"
	cfg.BasicAuth.Password = "sensitive-password"
	if _, err := newHTTPSink(cfg, nil, nil, nil); err == nil || strings.Contains(err.Error(), "user:secret") || strings.Contains(err.Error(), "sensitive-password") {
		t.Fatal("invalid credentials not safely rejected")
	}
}
