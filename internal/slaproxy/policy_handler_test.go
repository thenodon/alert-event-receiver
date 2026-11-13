package slaproxy

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
)

type evaluatorFunc func(context.Context, PolicyInput, PolicyRef, string, time.Time) (Decision, error)

func alertAt(t testing.TB, status, label, starts string) models.Alert {
	t.Helper()
	alert := testAlert(status, map[string]string{label: "business"})
	alert.StartsAt = instant(t, starts)
	if status == "resolved" {
		alert.EndsAt = alert.StartsAt.Add(time.Hour)
	}
	return alert
}

func (f evaluatorFunc) Evaluate(ctx context.Context, input PolicyInput, ref PolicyRef, sink string, at time.Time) (Decision, error) {
	return f(ctx, input, ref, sink, at)
}

func TestWebhookClassifiesOccurrenceTimeDespiteProcessingClock(t *testing.T) {
	s := newTestService(t)
	at := instant(t, "2026-11-09T12:30:00+01:00")
	s.now = func() time.Time { return instant(t, "2026-11-12T17:05:00+01:00") }
	alerts := []models.Alert{
		testAlert("firing", map[string]string{"sla_id": "business"}),
		testAlert("resolved", map[string]string{"sla_id": "business"}),
		testAlert("firing", map[string]string{"sla_name": "business"}),
		testAlert("firing", map[string]string{"sla_id": "unknown", "sla_name": "business"}),
	}
	// All three selected alerts started at Monday lunchtime, regardless of
	// when this batch is processed.
	for i := range alerts {
		alerts[i].StartsAt = at
		if alerts[i].Status == "resolved" {
			alerts[i].EndsAt = at.Add(time.Hour)
		}
	}
	w := post(s, testPayload(t, alerts...))
	var result Observation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if result.Mode != "observe_only" || result.Persisted || result.Forwarded || result.Received != 4 || result.Selected != 3 || result.Skipped != 1 || result.Evaluated != 3 || result.Eligible != 2 || result.Ineligible != 0 || result.Unknown != 1 || result.EvaluationErrors != 0 || !result.EvaluatedAt.Equal(s.now()) {
		t.Fatalf("unexpected response %+v", result)
	}
	if len(result.Policies) != 3 || result.Policies[0].EligibilityStatus != EligibilityUnknown || result.Policies[1].EligibilityStatus != EligibilityEligible || result.Policies[2].EligibilityStatus != EligibilityEligible {
		t.Fatalf("different lifecycle evidence was merged: %+v", result.Policies)
	}

	if got := counter(t, s, "sla_proxy_calendar_classifications_total", map[string]string{"sla_label": "sla_id", "sla_value": "business", "classification": "outside_sla", "reason": ReasonOutsideWindow}); got != 2 {
		t.Fatal(got)
	}
	if got := counter(t, s, "sla_proxy_calendar_classifications_total", map[string]string{"sla_label": "sla_name", "sla_value": "business", "classification": "inside_sla", "reason": ReasonEligible}); got != 1 {
		t.Fatal(got)
	}
	// One provider check for this unique pair, despite two alert statuses.
	if got := counter(t, s, "sla_proxy_holiday_evaluations_total", map[string]string{"country": "SE", "result": "not_holiday"}); got != 1 {
		t.Fatal(got)
	}
}

func TestEvaluationFailureReturns500WithoutClaimingEligibility(t *testing.T) {
	s := newTestService(t)
	s.now = func() time.Time { return instant(t, "2026-11-09T09:00:00+01:00") }
	e, err := NewCalendarEngine(s.cfg, observedHolidays{provider: brokenHolidays{evaluationErr: errors.New("unavailable")}, metrics: s.metrics})
	if err != nil {
		t.Fatal(err)
	}
	s.evaluator = e
	w := post(s, testPayload(t,
		alertAt(t, "firing", "sla_id", "2026-11-09T09:00:00+01:00"),
		alertAt(t, "resolved", "sla_id", "2026-11-09T09:00:00+01:00"),
		alertAt(t, "firing", "sla_id", "2026-11-09T09:00:00.000000001+01:00"),
		alertAt(t, "firing", "sla_name", "2026-11-09T09:00:00+01:00")))
	var result Observation
	if w.Code != http.StatusInternalServerError || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if result.Selected != 4 || result.Evaluated != 1 || result.Eligible != 1 || result.Ineligible != 0 || result.EvaluationErrors != 3 || result.Persisted || result.Forwarded {
		t.Fatal(result)
	}
	failed := result.Policies[0]
	if failed.Error != "calendar_error" || failed.EligibleAtStart != nil || failed.EligibilityStatus != EligibilityError || failed.NextEligibleAt != nil || failed.PolicyVersion != "" {
		t.Fatal("authoritative result on failed evaluation", failed)
	}
	if got := counter(t, s, "sla_proxy_policy_errors_total", map[string]string{"reason": "calendar_error"}); got != 2 {
		t.Fatal("error counted per alert instead of call", got)
	}
	if got := counter(t, s, "sla_proxy_holiday_evaluations_total", map[string]string{"country": "SE", "result": "error"}); got != 2 {
		t.Fatal(got)
	}
	if got := counter(t, s, "sla_proxy_notifications_received_total", map[string]string{"status": "5xx"}); got != 1 {
		t.Fatal(got)
	}
}

func TestEvaluationCacheIsPerRequestAndValidationComesFirst(t *testing.T) {
	s := newTestService(t)
	calls, clocks := 0, 0
	at := instant(t, "2026-11-09T09:00:00+01:00")
	s.now = func() time.Time { clocks++; return at }
	s.evaluator = evaluatorFunc(func(_ context.Context, input PolicyInput, ref PolicyRef, sink string, actual time.Time) (Decision, error) {
		calls++
		if !actual.Equal(input.Alert.StartsAt) || actual.Equal(at) || sink != "" || input.Alert.Fingerprint != "original-fingerprint" || len(input.Enrichment) != 0 {
			t.Error("unexpected evaluation input")
		}
		return Decision{Eligible: true, Reason: ReasonEligible, NextEvaluationAt: actual, PolicyVersion: "test"}, nil
	})
	alert := testAlert("firing", map[string]string{"sla_id": "business"})
	for i := 0; i < 2; i++ {
		if w := post(s, testPayload(t, alert, alert)); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if calls != 2 || clocks != 2 {
		t.Fatal("cache leaked across requests or evaluated per alert", calls, clocks)
	}
	bad := alert
	bad.StartsAt = time.Time{}
	if w := post(s, testPayload(t, alert, bad)); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if calls != 2 || clocks != 2 {
		t.Fatal("invalid batch entered evaluation")
	}
	skip := testAlert("firing", map[string]string{"sla_id": "unknown"})
	if w := post(s, testPayload(t, skip)); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if calls != 2 {
		t.Fatal("skipped alert entered evaluation")
	}
}

func TestPolicyObservationsThroughHTTP(t *testing.T) {
	cfg := testConfig(t)
	s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return instant(t, "2026-11-09T09:00:00+01:00") }
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	resp, err := server.Client().Post(server.URL+"/webhook", "application/json", bytes.NewReader(testPayload(t, alertAt(t, "firing", "sla_id", "2026-11-09T09:00:00+01:00"))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result Observation
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || resp.StatusCode != 200 || result.Eligible != 1 || result.Forwarded || result.Persisted {
		t.Fatal(result, err)
	}
}

func TestSamePolicyDifferentStartsAtClassifiesIndependently(t *testing.T) {
	cfg := testConfig(t)
	cfg.SLADefinitions[0].SLAs["business"] = businessPolicy()
	s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	processingTime := instant(t, "2026-11-12T07:42:00Z")
	s.now = func() time.Time { return processingTime }
	outside := alertAt(t, "firing", "sla_id", "2026-11-11T06:00:00Z") // 07:00 CET
	inside := alertAt(t, "firing", "sla_id", "2026-11-11T10:00:00Z")  // 11:00 CET
	w := post(s, testPayload(t, outside, inside, outside))
	var result Observation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if result.Selected != 3 || result.Eligible != 1 || result.Ineligible != 0 || result.Unknown != 2 || !result.EvaluatedAt.Equal(processingTime) || len(result.Policies) != 2 {
		t.Fatal(result)
	}
	first, second := result.Policies[0], result.Policies[1]
	if first.Fingerprint != outside.Fingerprint || !first.StartsAt.Equal(outside.StartsAt) || first.AlertCount != 2 || *first.EligibleAtStart || first.EligibilityStatus != EligibilityUnknown || first.NextEligibleAt == nil || !first.NextEligibleAt.Equal(instant(t, "2026-11-11T07:00:00Z")) {
		t.Fatal(first)
	}
	if second.Fingerprint != inside.Fingerprint || !second.StartsAt.Equal(inside.StartsAt) || second.AlertCount != 1 || !*second.EligibleAtStart || second.EligibilityStatus != EligibilityEligible || second.NextEligibleAt == nil || !second.NextEligibleAt.Equal(inside.StartsAt) {
		t.Fatal(second)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw["policy_observations"], []byte(`"next_evaluation_at"`)) || !bytes.Contains(raw["policy_observations"], []byte(`"next_eligible_at"`)) {
		t.Fatal("response field was not renamed", w.Body)
	}
}

func TestDeliveryLatencyAndHolidayDoNotChangeOccurrenceClassification(t *testing.T) {
	p := businessPolicy()
	p.Holidays = &Holidays{Country: "SE", AdditionalDates: []string{"2026-11-11"}}
	cfg := testConfig(t)
	cfg.SLADefinitions[0].SLAs["business"] = p
	s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// The processing date is a normal working day; the occurrence date can
	// still be a holiday, or before/after the window on an earlier day.
	s.now = func() time.Time { return instant(t, "2026-11-12T17:05:00+01:00") }
	for _, tc := range []struct {
		starts, reason string
		eligible       bool
	}{
		{"2026-11-12T16:55:00+01:00", ReasonEligible, true},
		{"2026-11-12T17:00:00+01:00", ReasonOutsideWindow, false},
		{"2026-11-11T10:00:00+01:00", ReasonAdditionalDate, false},
		{"2027-01-01T10:00:00+01:00", ReasonHoliday, false},
	} {
		for _, status := range []string{"firing", "resolved"} {
			w := post(s, testPayload(t, alertAt(t, status, "sla_id", tc.starts)))
			var result Observation
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Policies) != 1 || *result.Policies[0].EligibleAtStart != tc.eligible || result.Policies[0].CalendarReason != tc.reason {
				t.Fatalf("%s %s: %s", status, tc.starts, w.Body)
			}
		}
	}
}

func TestCacheUsesExactInstantButResponsesPreserveOccurrences(t *testing.T) {
	s := newTestService(t)
	s.now = func() time.Time { return instant(t, "2026-11-12T09:00:00+01:00") }
	calls := 0
	engine := s.evaluator
	s.evaluator = evaluatorFunc(func(ctx context.Context, input PolicyInput, ref PolicyRef, sink string, at time.Time) (Decision, error) {
		calls++
		return engine.Evaluate(ctx, input, ref, sink, at)
	})
	first := alertAt(t, "firing", "sla_id", "2026-11-09T07:59:59.999999999+01:00")
	second := alertAt(t, "firing", "sla_id", "2026-11-09T08:00:00+01:00")
	offsetRepeat := second
	offsetRepeat.StartsAt = second.StartsAt.In(time.FixedZone("different offset", -5*3600))
	otherFingerprint := second
	otherFingerprint.Fingerprint = "different-occurrence"
	subsecond := second
	subsecond.StartsAt = second.StartsAt.Add(time.Nanosecond)
	w := post(s, testPayload(t, first, second, offsetRepeat, otherFingerprint, subsecond))
	var result Observation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Code, w.Body)
	}
	if calls != 3 || result.Eligible != 4 || result.Ineligible != 0 || result.Unknown != 1 || len(result.Policies) != 4 {
		t.Fatal(calls, result)
	}
	if result.Policies[1].AlertCount != 2 || result.Policies[2].Fingerprint != otherFingerprint.Fingerprint {
		t.Fatal("distinct occurrences were merged", result.Policies)
	}
	for _, observation := range result.Policies {
		if observation.StartsAt.Location() != time.UTC {
			t.Fatal("starts_at is not canonical UTC", observation)
		}
	}
}
