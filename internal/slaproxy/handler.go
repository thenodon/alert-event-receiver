package slaproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
)

type Observation struct {
	SchemaVersion    int                 `json:"schema_version"`
	Mode             string              `json:"mode"`
	Persisted        bool                `json:"persisted"`
	Forwarded        bool                `json:"forwarded"`
	Received         int                 `json:"received"`
	Selected         int                 `json:"selected"`
	Skipped          int                 `json:"skipped"`
	Evaluated        int                 `json:"evaluated"`
	Eligible         int                 `json:"eligible"`
	Ineligible       int                 `json:"ineligible"`
	Pending          int                 `json:"pending"`
	Unknown          int                 `json:"unknown"`
	EvaluationErrors int                 `json:"evaluation_errors"`
	EvaluatedAt      time.Time           `json:"evaluated_at"`
	Policies         []PolicyObservation `json:"policy_observations"`
}

// PolicyObservation separates the calendar at startsAt from the lifecycle
// evidence in this individual notification. Firing and resolved inputs, and
// resolutions with different endsAt, must not be merged into one conclusion.
type PolicyObservation struct {
	SLALabel            string            `json:"sla_label"`
	SLAValue            string            `json:"sla_value"`
	Fingerprint         string            `json:"fingerprint"`
	StartsAt            time.Time         `json:"starts_at"`
	AlertStatus         string            `json:"alert_status"`
	EndsAt              *time.Time        `json:"ends_at,omitempty"`
	AlertCount          int               `json:"alert_count"`
	EligibleAtStart     *bool             `json:"eligible_at_start,omitempty"`
	CalendarReason      string            `json:"calendar_reason,omitempty"`
	EligibilityStatus   EligibilityStatus `json:"eligibility_status"`
	EligibilityReason   string            `json:"eligibility_reason"`
	NextEligibleAt      *time.Time        `json:"next_eligible_at,omitempty"`
	CalendarSearchUntil *time.Time        `json:"calendar_search_until,omitempty"`
	PolicyVersion       string            `json:"policy_version,omitempty"`
	Error               string            `json:"error,omitempty"`
}

func utcPointer(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	utc := at.UTC()
	return &utc
}

func evaluationErrorReason(err error) string {
	var evaluationErr *EvaluationError
	if errors.As(err, &evaluationErr) {
		switch evaluationErr.Reason {
		case "calendar_error", "unknown_policy", "invalid_evaluation_time", "cancelled":
			return evaluationErr.Reason
		}
	}
	return "evaluation_error"
}

func (s *Service) webhook(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	status := http.StatusOK
	defer func() {
		class := fmt.Sprintf("%dxx", status/100)
		s.metrics.requests.WithLabelValues(class).Inc()
		s.metrics.latency.WithLabelValues(class).Observe(time.Since(started).Seconds())
	}()
	if r.Method != http.MethodPost {
		status = http.StatusMethodNotAllowed
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", status)
		return
	}
	if !s.ready.Load() {
		status = http.StatusServiceUnavailable
		http.Error(w, "observation service is shutting down", status)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	data, err := io.ReadAll(r.Body)
	var payload *models.WebhookPayload
	if err == nil {
		err = decodeWebhook(data, &payload)
	}
	if err == nil {
		err = validateWebhook(payload)
	}
	if err != nil {
		status = http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		// Do not log payloads, labels, annotations, or attacker-controlled decoder
		// error text. The bounded response category suffices for diagnosis here.
		s.logger.WarnContext(r.Context(), "webhook rejected", "component", "sla_webhook", "operation", "validate", "http_status", status)
		http.Error(w, "invalid webhook payload", status)
		return
	}
	result := Observation{SchemaVersion: 2, Mode: "observe_only", Received: len(payload.Alerts), EvaluatedAt: s.now().UTC(), Policies: []PolicyObservation{}}
	// Cache calendar work only. Lifecycle conclusions also depend on status,
	// resolution time and the observation cutoff, never just policy/startsAt.
	type evaluationKey struct {
		Policy   PolicyRef
		StartsAt time.Time
	}
	type observationKey struct {
		evaluationKey
		Fingerprint, Status string
		EndsAt              time.Time
	}
	type calendarResult struct {
		decision Decision
		failure  string
	}
	decisions := make(map[evaluationKey]calendarResult)
	policyIndexes := make(map[observationKey]int)
	for _, alert := range payload.Alerts {
		s.metrics.alerts.WithLabelValues(alert.Status).Inc()
		ref, reason := s.cfg.Select(alert)
		if reason != "selected" {
			result.Skipped++
			s.metrics.skipped.WithLabelValues(reason).Inc()
			continue
		}
		metadata, _ := s.enricher.Enrich(r.Context(), alert) // Concrete no-op.
		result.Selected++
		s.metrics.selected.WithLabelValues(ref.SLALabel, ref.SLAValue, alert.Status).Inc()
		startsAt := alert.StartsAt.UTC()
		key := evaluationKey{Policy: ref, StartsAt: startsAt}
		cached, exists := decisions[key]
		if !exists {
			decision, err := s.evaluator.Evaluate(r.Context(), PolicyInput{Alert: alert, Enrichment: metadata}, ref, "", startsAt)
			if err != nil {
				cached.failure = evaluationErrorReason(err)
				s.metrics.policyErrors.WithLabelValues(cached.failure).Inc()
				s.logger.ErrorContext(r.Context(), "SLA calendar evaluation failed", "component", "sla_policy", "operation", "evaluate",
					"sla_label", ref.SLALabel, "sla_value", ref.SLAValue, "starts_at", startsAt.Format(time.RFC3339Nano), "reason", cached.failure)
			} else {
				cached.decision = decision
			}
			decisions[key] = cached
		}
		occurrence := observationKey{evaluationKey: key, Fingerprint: alert.Fingerprint, Status: alert.Status}
		// Firing endsAt can be an Alertmanager expiry estimate, not evidence of
		// resolution. Only resolved endsAt bounds a known active interval.
		if alert.Status == "resolved" {
			occurrence.EndsAt = alert.EndsAt.UTC()
		}
		index, exists := policyIndexes[occurrence]
		if !exists {
			observation := PolicyObservation{SLALabel: ref.SLALabel, SLAValue: ref.SLAValue, StartsAt: startsAt,
				Fingerprint: alert.Fingerprint, AlertStatus: alert.Status, Error: cached.failure}
			if alert.Status == "resolved" {
				observation.EndsAt = utcPointer(alert.EndsAt)
			}
			if cached.failure != "" {
				observation.EligibilityStatus, observation.EligibilityReason = EligibilityError, "calendar_evaluation_error"
			} else {
				d := cached.decision
				eligibleAtStart := d.Eligible
				observation.EligibleAtStart = &eligibleAtStart
				observation.CalendarReason, observation.PolicyVersion = d.Reason, d.PolicyVersion
				observation.NextEligibleAt, observation.CalendarSearchUntil = utcPointer(d.NextEvaluationAt), utcPointer(d.SearchUntil)
				observation.EligibilityStatus, observation.EligibilityReason = alertEligibility(alert, d, result.EvaluatedAt)
			}
			index = len(result.Policies)
			policyIndexes[occurrence] = index
			result.Policies = append(result.Policies, observation)
		}
		observation := &result.Policies[index]
		observation.AlertCount++
		s.metrics.eligibility.WithLabelValues(ref.SLALabel, ref.SLAValue, alert.Status, string(observation.EligibilityStatus), observation.EligibilityReason).Inc()
		if cached.failure != "" {
			result.EvaluationErrors++
			continue
		}
		result.Evaluated++
		classification := "outside_sla"
		if cached.decision.Eligible {
			classification = "inside_sla"
		}
		s.metrics.calendar.WithLabelValues(ref.SLALabel, ref.SLAValue, classification, observation.CalendarReason).Inc()
		switch observation.EligibilityStatus {
		case EligibilityEligible:
			result.Eligible++
		case EligibilityIneligible:
			result.Ineligible++
		case EligibilityPending:
			result.Pending++
		case EligibilityUnknown:
			result.Unknown++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if result.EvaluationErrors != 0 {
		status = http.StatusInternalServerError
		w.WriteHeader(status)
	}
	s.logger.InfoContext(r.Context(), "webhook observation",
		"component", "sla_webhook", "operation", "observe", "http_status", status,
		"observation", result)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		s.logger.WarnContext(r.Context(), "observation response write failed", "component", "sla_webhook", "operation", "respond")
	}
}

func decodeWebhook(data []byte, payload **models.WebhookPayload) error {
	// encoding/json otherwise accepts repeated keys with last-value-wins
	// behavior, which is undesirable for identities and label selection.
	keys := json.NewDecoder(bytes.NewReader(data))
	keys.UseNumber()
	if err := uniqueJSONKeys(keys); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(payload); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one JSON document")
	}
	return nil
}

func uniqueJSONKeys(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid JSON key")
			}
			seen[name] = true
			if err := uniqueJSONKeys(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := uniqueJSONKeys(dec); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = dec.Token()
	return err
}

func validateWebhook(p *models.WebhookPayload) error {
	if p == nil || p.Version != "4" || strings.TrimSpace(p.Receiver) == "" ||
		(p.Status != "firing" && p.Status != "resolved") || p.Alerts == nil || p.TruncatedAlerts < 0 {
		return fmt.Errorf("expected a v4 webhook with receiver, status and alerts array")
	}
	for _, a := range p.Alerts {
		if a.Status != "firing" && a.Status != "resolved" {
			return fmt.Errorf("alert status must be firing or resolved")
		}
		if strings.TrimSpace(a.Fingerprint) == "" || a.StartsAt.IsZero() {
			return fmt.Errorf("alert fingerprint and startsAt are required")
		}
		if _, err := a.StartsAt.MarshalText(); err != nil {
			return fmt.Errorf("invalid startsAt: %w", err)
		}
		if a.Status == "resolved" && (a.EndsAt.IsZero() || a.EndsAt.Before(a.StartsAt)) {
			return fmt.Errorf("resolved alert requires endsAt >= startsAt")
		}
	}
	return nil
}
