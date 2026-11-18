//go:build integration

package slaproxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/andersh/alertmanager_event_receiver/internal/httpsecurity/securitytest"
)

func TestInboundSecurityRotationPreservesDurableCatalog(t *testing.T) {
	securitytest.ClearEnv(t)
	f := realValkey(t)
	cfg := integrationConfig(t, f)
	securitytest.Auth(t, "WEBHOOK", "alerts", "initial-secret")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	ctx := context.Background()
	snapshot := func() map[string]string {
		keys, err := service.durable.store.client.Keys(ctx, "*").Result()
		if err != nil {
			t.Fatal(err)
		}
		result := map[string]string{}
		for _, key := range keys {
			value, err := service.durable.store.client.Dump(ctx, key).Result()
			if err != nil {
				t.Fatal(err)
			}
			result[key] = value
		}
		return result
	}
	before := snapshot()
	alert := alertAt(t, "firing", "sla_id", "2026-11-09T09:00:00+01:00")
	payload := testPayload(t, alert)
	w := httptest.NewRecorder()
	service.Handler().ServeHTTP(w, httptest.NewRequest("POST", cfg.WebhookPath, bytes.NewReader(payload)))
	if w.Code != 401 || !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("unauthorized webhook changed durable state", w.Code)
	}
	req := httptest.NewRequest("POST", cfg.WebhookPath, bytes.NewReader(payload))
	req.SetBasicAuth("alerts", "initial-secret")
	w = httptest.NewRecorder()
	service.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	original := snapshot()
	// Change only inbound credentials and certificate material, with the SAME epoch.
	securitytest.Auth(t, "WEBHOOK", "rotated-user", "rotated-secret")
	securitytest.Auth(t, "METRICS", "scraper", "scrape-secret")
	cert, key, _ := securitytest.Certificate(t)
	t.Setenv("HTTP_TLS_CERT_FILE", cert)
	t.Setenv("HTTP_TLS_KEY_FILE", key)
	rotated, err := New(cfg, logger)
	if err != nil {
		t.Fatal("security rotation required a policy epoch", err)
	}
	defer rotated.Close()
	if !reflect.DeepEqual(original, snapshot()) {
		t.Fatal("security rotation changed durable registries or occurrences")
	}
	for _, password := range []string{"initial-secret", "rotated-secret"} {
		req := httptest.NewRequest("POST", cfg.WebhookPath, bytes.NewReader(payload))
		user := "alerts"
		want := 401
		if password == "rotated-secret" {
			user = "rotated-user"
			want = 200
		}
		req.SetBasicAuth(user, password)
		w := httptest.NewRecorder()
		rotated.Handler().ServeHTTP(w, req)
		if w.Code != want {
			t.Fatal(w.Code, want)
		}
	}
}
