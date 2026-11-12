package slaproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
)

type DurableObservation struct {
	SchemaVersion  int              `json:"schema_version"`
	Mode           string           `json:"mode"`
	Persisted      bool             `json:"persisted"`
	Forwarded      bool             `json:"forwarded"`
	Received       int              `json:"received"`
	Selected       int              `json:"selected"`
	Skipped        int              `json:"skipped"`
	PersistedCount int              `json:"persisted_count"`
	FailedCount    int              `json:"failed_count"`
	EvaluatedAt    time.Time        `json:"evaluated_at"`
	Occurrences    []DurableSummary `json:"occurrences"`
}

type DurableSummary struct {
	EndsAt       *time.Time       `json:"ends_at,omitempty"`
	OccurrenceID string           `json:"occurrence_id"`
	Fingerprint  string           `json:"fingerprint"`
	StartsAt     time.Time        `json:"starts_at"`
	Lifecycle    LifecycleStatus  `json:"lifecycle"`
	Version      uint64           `json:"version"`
	Bindings     []BindingSummary `json:"bindings"`
}
type BindingSummary struct {
	SLALabel         string            `json:"sla_label"`
	SLAValue         string            `json:"sla_value"`
	SinkID           string            `json:"sink_id"`
	PolicyVersion    string            `json:"policy_version"`
	Eligibility      EligibilityStatus `json:"eligibility"`
	Delivery         DeliveryStatus    `json:"delivery"`
	HoldReason       string            `json:"hold_reason,omitempty"`
	NextEvaluationAt *time.Time        `json:"next_evaluation_at,omitempty"`
}

func (s *Service) durableWebhook(w http.ResponseWriter, r *http.Request, payload models.WebhookPayload) int {
	// A whole batch is bounded too; many unavailable-store inputs must not each
	// consume a fresh timeout beyond the HTTP response deadline.
	batchCtx, stopBatch := context.WithTimeout(r.Context(), s.cfg.Durable.Scheduler.OperationTimeout)
	defer stopBatch()
	result := DurableObservation{SchemaVersion: 3, Mode: s.mode(), Received: len(payload.Alerts), EvaluatedAt: s.now().UTC(), Occurrences: []DurableSummary{}}
	for _, a := range payload.Alerts {
		s.metrics.alerts.WithLabelValues(a.Status).Inc()
		ctx, cancel := context.WithTimeout(batchCtx, s.cfg.Durable.Scheduler.OperationTimeout)
		accepted, err := s.durable.accept(ctx, a, payload)
		cancel()
		if accepted.Skipped != "" {
			result.Skipped++
			s.metrics.skipped.WithLabelValues(accepted.Skipped).Inc()
			continue
		}
		result.Selected++
		for _, ref := range accepted.Policies {
			s.metrics.selected.WithLabelValues(ref.SLALabel, ref.SLAValue, a.Status).Inc()
		}
		if err != nil {
			result.FailedCount++
			s.logger.ErrorContext(r.Context(), "SLA occurrence was not acknowledged", "component", "sla_lifecycle", "operation", "accept", "fingerprint", a.Fingerprint, "starts_at", a.StartsAt, "error", err.Error())
			continue
		}
		row := accepted.Occurrence
		result.PersistedCount++
		summary := DurableSummary{OccurrenceID: row.ID, Fingerprint: row.Fingerprint, StartsAt: row.StartsAt, Lifecycle: row.Lifecycle, EndsAt: row.EndsAt, Version: row.Version, Bindings: []BindingSummary{}}
		for _, b := range row.Bindings {
			summary.Bindings = append(summary.Bindings, BindingSummary{b.Policy.SLALabel, b.Policy.SLAValue, b.SinkID, b.PolicyVersion, b.Eligibility, b.Delivery, b.HoldReason, utcPointer(b.NextEvaluationAt)})
			// Preserve Phase 2 metrics as notification evidence observations,
			// distinct from the durable webhook-authoritative lifecycle state.
			classification := "outside_sla"
			if b.Calendar.Eligible {
				classification = "inside_sla"
			}
			s.metrics.calendar.WithLabelValues(b.Policy.SLALabel, b.Policy.SLAValue, classification, b.Calendar.Reason).Inc()
			observed, reason := alertEligibility(a, b.Calendar, result.EvaluatedAt)
			s.metrics.eligibility.WithLabelValues(b.Policy.SLALabel, b.Policy.SLAValue, a.Status, string(observed), reason).Inc()
		}
		result.Occurrences = append(result.Occurrences, summary)
	}
	status := http.StatusOK
	result.Persisted = result.FailedCount == 0
	if result.FailedCount != 0 {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	s.logger.InfoContext(r.Context(), "webhook observation", "component", "sla_webhook", "operation", "accept", "http_status", status, "observation", result)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		s.logger.WarnContext(r.Context(), "state acceptance response write failed", "component", "sla_webhook", "operation", "respond")
	}
	return status
}
