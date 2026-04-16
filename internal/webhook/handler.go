package webhook

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
	"github.com/andersh/alertmanager_event_receiver/internal/telemetry"
)

// AlertProcessor processes a single alert instance.
type AlertProcessor interface {
	Process(ctx context.Context, alert models.Alert, groupKey, receiver, externalURL string) error
}

// Handler is the HTTP handler for Alertmanager webhook deliveries.
type Handler struct {
	processor AlertProcessor
	logger    *slog.Logger
}

// NewHandler creates a Handler.
func NewHandler(p AlertProcessor, logger *slog.Logger) *Handler {
	return &Handler{processor: p, logger: logger}
}

// ServeHTTP accepts POST /webhook, splits the grouped payload into individual
// alerts, and passes each to the processor.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	statusCode := http.StatusOK
	defer func() {
		class := statusClass(statusCode)
		telemetry.WebhookRequestsTotal.WithLabelValues(class).Inc()
		telemetry.WebhookDuration.WithLabelValues(class).Observe(time.Since(start).Seconds())
	}()

	if r.Method != http.MethodPost {
		statusCode = http.StatusMethodNotAllowed
		http.Error(w, "method not allowed", statusCode)
		return
	}

	var payload models.WebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		h.logger.ErrorContext(r.Context(), "failed to decode webhook payload",
			slog.String("remote_addr", r.RemoteAddr),
			slog.Any("error", err),
		)
		statusCode = http.StatusBadRequest
		http.Error(w, "bad request", statusCode)
		return
	}

	h.logger.InfoContext(r.Context(), "webhook received",
		slog.String("group_key", payload.GroupKey),
		slog.String("receiver", payload.Receiver),
		slog.Int("alert_count", len(payload.Alerts)),
	)

	ctx := r.Context()
	hadError := false
	for _, alert := range payload.Alerts {
		if err := h.processor.Process(ctx, alert, payload.GroupKey, payload.Receiver, payload.ExternalURL); err != nil {
			h.logger.ErrorContext(ctx, "failed to process alert",
				slog.String("fingerprint", alert.Fingerprint),
				slog.String("alertname", alert.Labels["alertname"]),
				slog.String("status", alert.Status),
				slog.Any("error", err),
			)
			hadError = true
		}
	}

	if hadError {
		// Return 500 so Alertmanager retries the batch.
		statusCode = http.StatusInternalServerError
		http.Error(w, "one or more alerts failed to process", statusCode)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	default:
		return "2xx"
	}
}

