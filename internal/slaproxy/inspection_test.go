package slaproxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/andersh/alertmanager_event_receiver/internal/httpsecurity/securitytest"
	"github.com/andersh/alertmanager_event_receiver/internal/models"
)

func TestInspectionAPI(t *testing.T) {
	securitytest.ClearEnv(t)
	securitytest.Auth(t, "INSPECTION", "operator", "inspect-secret")
	securitytest.Auth(t, "WEBHOOK", "sender", "webhook-secret")
	securitytest.Auth(t, "METRICS", "metrics", "metrics-secret")
	fixture := miniredis.RunT(t)
	cfg := testConfig(t)
	cfg.Durable.Enabled = true
	cfg.Durable.Valkey.Address = fixture.Addr()
	cfg.Delivery.Enabled = true
	cfg.Delivery.Sinks = []SinkConfig{{ID: "audit", Type: "log"}, {ID: "servicenow", Type: "log"}}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	request := func(path, user, password string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if user != "" {
			r.SetBasicAuth(user, password)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != want || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("GET %s status=%d cache=%q body=%s", path, w.Code, w.Header().Get("Cache-Control"), w.Body)
		}
		return w
	}
	get := func(path string, want int) *httptest.ResponseRecorder {
		t.Helper()
		return request(path, "operator", "inspect-secret", want)
	}
	list := func(query string) inspectionPage {
		t.Helper()
		var page inspectionPage
		w := get(inspectionPath+"?"+query, http.StatusOK)
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Items == nil {
			t.Fatalf("invalid inspection page: %v %s", err, w.Body)
		}
		return page
	}
	for _, path := range []string{inspectionPath, inspectionPath + "/" + strings.Repeat("a", 64)} {
		if challenge := request(path, "", "", http.StatusUnauthorized).Header().Get("WWW-Authenticate"); challenge != `Basic realm="inspection"` {
			t.Fatal(challenge)
		}
		request(path, "sender", "webhook-secret", http.StatusUnauthorized)
		request(path, "metrics", "metrics-secret", http.StatusUnauthorized)
	}
	get(inspectionPath+"/invalid", http.StatusBadRequest)
	get(inspectionPath+"/"+strings.Repeat("a", 64), http.StatusNotFound)
	get(inspectionPath+"?limit=101", http.StatusBadRequest)
	get(inspectionPath+"?cursor=bad", http.StatusBadRequest)

	alert := testAlert("firing", map[string]string{"sla_id": "business", "alertname": "Demo", "private": "sensitive label"})
	id := occurrenceKeyID(alert)
	ids := map[string]bool{}
	for index := range 5 {
		input := alert
		if index != 0 {
			input.Fingerprint = fmt.Sprintf("other-%d", index)
		}
		payload := testPayload(t, input)
		r := httptest.NewRequest(http.MethodPost, cfg.WebhookPath, strings.NewReader(string(payload)))
		r.SetBasicAuth("sender", "webhook-secret")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		ids[occurrenceKeyID(input)] = true
	}
	query := "lifecycle=firing&alertname=Demo&limit=1"
	page := list(query)
	token := url.QueryEscape(page.NextCursor)
	get(inspectionPath+"?lifecycle=resolved&cursor="+token, http.StatusBadRequest)
	for index := range 5 {
		if len(page.Items) != 1 || !ids[page.Items[0].ID] || len(page.Items[0].Bindings) != 2 {
			t.Fatalf("unexpected page %d: %+v", index, page)
		}
		delete(ids, page.Items[0].ID)
		if page.NextCursor == "" {
			break
		}
		page = list(query + "&cursor=" + url.QueryEscape(page.NextCursor))
	}
	if len(ids) != 0 {
		t.Fatalf("pagination dropped IDs: %v", ids)
	}
	auditID := bindingKeyID(id, PolicyRef{SLALabel: "sla_id", SLAValue: "business"}, "audit")
	if err := s.dispatcher.runOne(context.Background(), deliveryWorkID(id, auditID)); err != nil {
		t.Fatal(err)
	}
	if page := list("sink_id=servicenow&delivery=delivered&fingerprint=" + alert.Fingerprint); len(page.Items) != 0 {
		t.Fatalf("matched filters on different bindings: %+v", page.Items)
	}
	if page := list("sink_id=audit&delivery=delivered&fingerprint=" + alert.Fingerprint); len(page.Items) != 1 {
		t.Fatalf("missing delivered binding: %+v", page.Items)
	}

	detail := get(inspectionPath+"/"+id, http.StatusOK)
	var occurrence occurrenceDetail
	if err := json.Unmarshal(detail.Body.Bytes(), &occurrence); err != nil || occurrence.AlertName != "Demo" || len(occurrence.Bindings) != 2 {
		t.Fatalf("invalid detail: %v %s", err, detail.Body)
	}
	if !strings.Contains(detail.Body.String(), "receipt_committed_at") || strings.Contains(detail.Body.String(), "sensitive label") || strings.Contains(detail.Body.String(), "original annotation") {
		t.Fatal(detail.Body.String())
	}

	servicenowID := bindingKeyID(id, PolicyRef{SLALabel: "sla_id", SLAValue: "business"}, "servicenow")
	if err := s.dispatcher.runOne(context.Background(), deliveryWorkID(id, servicenowID)); err != nil {
		t.Fatal(err)
	}
	alert.Status, alert.EndsAt = "resolved", alert.StartsAt.Add(time.Hour)
	if _, err := s.durable.accept(context.Background(), alert, models.WebhookPayload{}); err != nil {
		t.Fatal(err)
	}
	if p := list("lifecycle=firing&fingerprint=" + alert.Fingerprint); len(p.Items) != 0 {
		t.Fatal("resolved occurrence remained in firing list")
	}
	if p := list("lifecycle=resolved&alertname=Demo&fingerprint=" + alert.Fingerprint); len(p.Items) != 1 {
		t.Fatal("resolved occurrence missing from list")
	}

	if err := s.dispatcher.runOne(context.Background(), deliveryWorkID(id, auditID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.durable.store.update(context.Background(), id, func(row *DurableOccurrence, _ time.Time) (*DurableOccurrence, error) {
		for _, b := range row.Bindings {
			b.HoldReason = "policy_changed"
		}
		return row, nil
	}); err != nil {
		t.Fatal(err)
	}
	holdQuery := "hold_reason=policy_changed&fingerprint=" + alert.Fingerprint
	if p := list(holdQuery + "&sink_id=audit"); len(p.Items) != 0 {
		t.Fatal("completed audit binding matched the hold filter")
	}
	checkHolds := func(bindings []occurrenceBindingView) {
		t.Helper()
		holds := map[string]string{}
		for _, b := range bindings {
			holds[b.SinkID] = b.HoldReason
		}
		if len(holds) != 2 || holds["audit"] != "" || holds["servicenow"] != "policy_changed" {
			t.Fatalf("unexpected binding holds: %v", holds)
		}
	}
	if p := list(holdQuery + "&sink_id=servicenow"); len(p.Items) != 1 {
		t.Fatal("binding with only a firing receipt did not match the hold filter")
	} else {
		checkHolds(p.Items[0].Bindings)
	}
	detail = get(inspectionPath+"/"+id, http.StatusOK)
	if err := json.Unmarshal(detail.Body.Bytes(), &occurrence); err != nil {
		t.Fatal(err)
	}
	var bindings []occurrenceBindingView
	for _, b := range occurrence.Bindings {
		bindings = append(bindings, b.occurrenceBindingView)
	}
	checkHolds(bindings)
	if strings.Count(detail.Body.String(), `"hold_reason"`) != 1 {
		t.Fatal(detail.Body.String())
	}

	if _, err := s.durable.store.update(context.Background(), id, func(row *DurableOccurrence, _ time.Time) (*DurableOccurrence, error) {
		row.Original, row.LastFiring, row.Resolution, row.Compacted = nil, nil, nil, true
		return row, nil
	}); err != nil {
		t.Fatal(err)
	}
	if p := list("lifecycle=resolved&alertname=Demo&fingerprint=" + alert.Fingerprint); len(p.Items) != 0 {
		t.Fatal("compacted occurrence matched an unavailable name")
	}
	if w := get(inspectionPath+"/"+id, http.StatusOK); strings.Contains(w.Body.String(), `"alertname"`) || !strings.Contains(w.Body.String(), `"compacted":true`) {
		t.Fatal(w.Body.String())
	}

	fixture.Close()
	get(inspectionPath, http.StatusServiceUnavailable)
	get(inspectionPath+"/"+id, http.StatusServiceUnavailable)
}

func TestInspectionAvailabilityAndTLS(t *testing.T) {
	securitytest.ClearEnv(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := testConfig(t)
	cfg.Durable.Enabled = true
	cfg.Durable.Valkey.Address = miniredis.RunT(t).Addr()
	checkListStatus := func(cfg Config, want int) {
		t.Helper()
		s, err := New(cfg, logger)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, inspectionPath, nil))
		if w.Code != want {
			t.Fatalf("inspection list returned %d, want %d", w.Code, want)
		}
		detail := httptest.NewRecorder()
		s.Handler().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, inspectionPath+"/invalid", nil))
		wantDetail := http.StatusNotFound
		if cfg.Durable.Enabled {
			wantDetail = http.StatusBadRequest
		}
		if detail.Code != wantDetail {
			t.Fatalf("inspection detail returned %d, want %d", detail.Code, wantDetail)
		}
	}
	securitytest.Auth(t, "WEBHOOK", "sender", "webhook-secret")
	checkListStatus(cfg, http.StatusOK)

	t.Setenv("HTTP_INSPECTION_BASIC_AUTH_USERNAME", "operator")
	if s, err := New(cfg, logger); err == nil {
		s.Close()
		t.Fatal("accepted partial inspection credentials")
	}
	securitytest.Auth(t, "INSPECTION", "operator", "inspect-secret")
	securitytest.Auth(t, "METRICS", "operator", "inspect-secret")
	if s, err := New(cfg, logger); err == nil {
		s.Close()
		t.Fatal("accepted the same credentials for metrics and inspection")
	}
	securitytest.Auth(t, "METRICS", "scraper", "metrics-secret")
	observe := cfg
	observe.Durable.Enabled = false
	checkListStatus(observe, http.StatusNotFound)

	collision := cfg
	collision.WebhookPath = inspectionPath + "/detail"
	securitytest.ClearEnv(t)
	if s, err := New(collision, logger); err == nil {
		s.Close()
		t.Fatal("accepted a conflicting webhook path")
	}

	securitytest.Auth(t, "INSPECTION", "operator", "inspect-secret")
	cert, key, roots := securitytest.Certificate(t)
	t.Setenv("HTTP_TLS_CERT_FILE", cert)
	t.Setenv("HTTP_TLS_KEY_FILE", key)
	s, err := New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	listener := listenTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, listener) }()
	defer func() {
		cancel()
		if err := awaitStopped(t, done); err != nil {
			t.Error(err)
		}
		s.Close()
	}()
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	request, err := http.NewRequest(http.MethodGet, "https://"+listener.Addr().String()+inspectionPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth("operator", "inspect-secret")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("TLS inspection returned %d", response.StatusCode)
	}
}
