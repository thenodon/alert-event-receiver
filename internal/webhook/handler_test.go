package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
	"github.com/andersh/alertmanager_event_receiver/internal/webhook"
)

// ---- test double ----

type mockProcessor struct {
	calls []models.Alert
	err   error
}

func (m *mockProcessor) Process(_ context.Context, alert models.Alert, _, _, _ string) error {
	m.calls = append(m.calls, alert)
	return m.err
}

func newHandler(p *mockProcessor) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return webhook.NewHandler(p, logger)
}

// ---- helpers ----

func payload(alerts ...models.Alert) []byte {
	p := models.WebhookPayload{
		Version:     "4",
		GroupKey:    "gk1",
		Receiver:    "test-receiver",
		ExternalURL: "http://alertmanager",
		Alerts:      alerts,
	}
	b, _ := json.Marshal(p)
	return b
}

func alert(status, fp string) models.Alert {
	return models.Alert{
		Status:      status,
		Fingerprint: fp,
		Labels:      map[string]string{"alertname": "TestAlert", "severity": "warning"},
		StartsAt:    time.Now(),
	}
}

// ---- tests ----

func TestWebhook_ValidFiringPayload_Returns200(t *testing.T) {
	proc := &mockProcessor{}
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payload(alert("firing", "fp1"))))
	rec := httptest.NewRecorder()

	newHandler(proc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if len(proc.calls) != 1 {
		t.Errorf("expected 1 processed alert, got %d", len(proc.calls))
	}
}

func TestWebhook_MultipleAlerts_AllProcessed(t *testing.T) {
	proc := &mockProcessor{}
	req := httptest.NewRequest(http.MethodPost, "/webhook",
		bytes.NewReader(payload(alert("firing", "fp1"), alert("resolved", "fp2"))))
	rec := httptest.NewRecorder()

	newHandler(proc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if len(proc.calls) != 2 {
		t.Errorf("expected 2 processed alerts, got %d", len(proc.calls))
	}
}

func TestWebhook_ProcessorError_Returns500(t *testing.T) {
	proc := &mockProcessor{err: errors.New("emit failure")}
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payload(alert("firing", "fp1"))))
	rec := httptest.NewRecorder()

	newHandler(proc).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on processor error, got %d", rec.Code)
	}
}

func TestWebhook_InvalidJSON_Returns400(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader([]byte("not-json")))
	rec := httptest.NewRecorder()

	newHandler(&mockProcessor{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestWebhook_GetMethod_Returns405(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	rec := httptest.NewRecorder()

	newHandler(&mockProcessor{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestWebhook_EmptyAlerts_Returns200(t *testing.T) {
	proc := &mockProcessor{}
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payload()))
	rec := httptest.NewRecorder()

	newHandler(proc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for empty alert list, got %d", rec.Code)
	}
	if len(proc.calls) != 0 {
		t.Errorf("expected 0 processed alerts, got %d", len(proc.calls))
	}
}

