package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
	"github.com/andersh/alertmanager_event_receiver/internal/processor"
	"github.com/andersh/alertmanager_event_receiver/internal/telemetry"
)

// AlertProcessor processes a single alert instance. Its value result describes
// the processing outcome even when an error is returned (for example,
// Indeterminate on a state failure or a detected transition on an emit failure).
type AlertProcessor interface {
	Process(ctx context.Context, alert models.Alert, groupKey, receiver, externalURL string) (processor.ProcessingResult, error)
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

	var payload *models.WebhookPayload
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&payload); err != nil {
		h.logger.ErrorContext(r.Context(), "failed to decode webhook payload",
			slog.String("remote_addr", r.RemoteAddr),
			slog.String("component", "webhook"), slog.String("operation", "decode"),
			slog.String("error", telemetry.SafeError(err)),
		)
		statusCode = http.StatusBadRequest
		http.Error(w, "bad request", statusCode)
		return
	}

	if payload == nil {
		h.logger.WarnContext(r.Context(), "invalid webhook payload", "component", "webhook", "operation", "validate", "error", "null payload")
		statusCode = http.StatusBadRequest
		http.Error(w, "bad request", statusCode)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		h.logger.WarnContext(r.Context(), "invalid webhook payload", "component", "webhook", "operation", "decode", "error", "expected one JSON document")
		statusCode = http.StatusBadRequest
		http.Error(w, "bad request", statusCode)
		return
	}
	// Validate the complete batch before any alert can mutate lifecycle state.
	for _, alert := range payload.Alerts {
		if err := processor.ValidateAlert(alert); err != nil {
			h.logger.WarnContext(r.Context(), "invalid alert notification", "component", "webhook", "operation", "validate", "alert_fingerprint", alert.Fingerprint, "alert_transition", alert.Status, "error", telemetry.SafeError(err))
			statusCode = http.StatusBadRequest
			http.Error(w, "invalid alert notification", statusCode)
			return
		}
	}

	// A grouped webhook can contain both firing and resolved alerts, so report
	// individual alert counts as well as Alertmanager's group status.
	firingCount, resolvedCount, unknownStatusCount := 0, 0, 0
	for _, alert := range payload.Alerts {
		switch alert.Status {
		case models.TransitionFiring:
			firingCount++
		case models.TransitionResolved:
			resolvedCount++
		default:
			unknownStatusCount++
		}
	}

	h.logger.InfoContext(r.Context(), "webhook received",
		slog.String("component", "webhook"), slog.String("operation", "receive"),
		slog.String("group_key", payload.GroupKey),
		slog.String("receiver", payload.Receiver),
		slog.String("status", payload.Status),
		slog.Int("alert_count", len(payload.Alerts)),
		slog.Int("firing_count", firingCount),
		slog.Int("resolved_count", resolvedCount),
		slog.Int("unknown_status_count", unknownStatusCount),
	)

	ctx := r.Context()
	hadError := false
	for _, alert := range payload.Alerts {
		result, err := h.processor.Process(ctx, alert, payload.GroupKey, payload.Receiver, payload.ExternalURL)
		// Outcome metadata is independent of the error; ProcessingResult is a value,
		// not a nullable success-only result. GoLand's success-only assumption does
		// not apply to this API, which returns outcome metadata alongside errors.
		//goland:noinspection GoDfaErrorMayBeNotNil
		outcome, emissionStatus := string(result.Outcome), string(result.EventEmissionStatus)
		if err != nil {
			h.logger.ErrorContext(ctx, "failed to process alert",
				slog.String("component", "webhook"), slog.String("operation", "process"),
				slog.String("event_id", models.LifecycleEventID(alert.Fingerprint, alert.StartsAt, alert.Status)),
				slog.String("alert_fingerprint", alert.Fingerprint), slog.String("alert_transition", alert.Status),
				slog.String("outcome", outcome), slog.String("emission_status", emissionStatus),
				slog.String("error", telemetry.SafeError(err)),
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
