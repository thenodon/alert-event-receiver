package slaproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestResolvedActiveIntervalIntersection(t *testing.T) {
	e := engineFor(t, businessPolicy(), 370)
	for _, tc := range []struct {
		start, end string
		status     EligibilityStatus
	}{
		{"07:30:00", "07:45:00", EligibilityIneligible},
		{"07:30:00", "08:00:00", EligibilityIneligible},
		{"07:30:00", "08:00:00.000000001", EligibilityEligible},
		{"07:30:00", "08:01:00", EligibilityEligible},
		{"07:30:00", "08:30:00", EligibilityEligible},
		{"08:30:00", "09:00:00", EligibilityEligible},
		{"16:55:00", "17:30:00", EligibilityEligible},
		{"17:05:00", "18:00:00", EligibilityIneligible},
		{"08:00:00", "08:00:00", EligibilityIneligible},
	} {
		t.Run(tc.start+"-"+tc.end, func(t *testing.T) {
			a := alertAt(t, "resolved", "sla_id", "2026-11-09T"+tc.start+"+01:00")
			a.EndsAt = instant(t, "2026-11-09T"+tc.end+"+01:00")
			d, err := e.Evaluate(context.Background(), PolicyInput{Alert: a}, PolicyRef{"sla_id", "business"}, "", a.StartsAt)
			if err != nil {
				t.Fatal(err)
			}
			for _, processing := range []string{"2026-11-09T08:05:00+01:00", "2026-11-12T17:10:00+01:00"} {
				status, reason := alertEligibility(a, d, instant(t, processing))
				if status != tc.status {
					t.Fatalf("%s: %s (%s), want %s", processing, status, reason, tc.status)
				}
			}
		})
	}
}

func TestFiringEvidenceAndObservationCutoff(t *testing.T) {
	e := engineFor(t, businessPolicy(), 370)
	for _, tc := range []struct {
		start, cutoff string
		status        EligibilityStatus
		reason        string
	}{
		{"07:30:00", "07:35:00", EligibilityPending, "awaiting_first_eligible_instant"},
		{"07:30:00", "08:00:00", EligibilityUnknown, "activity_at_eligibility_unverified"},
		{"07:30:00", "08:05:00", EligibilityUnknown, "activity_at_eligibility_unverified"},
		{"16:55:00", "17:10:00", EligibilityEligible, "start_in_sla"},
		{"08:30:00", "08:00:00", EligibilityUnknown, "start_after_observation_cutoff"},
	} {
		a := alertAt(t, "firing", "sla_id", "2026-11-09T"+tc.start+"+01:00")
		// An expiry estimate on a firing webhook is not a resolution.
		a.EndsAt = instant(t, "2026-11-09T07:45:00+01:00")
		d, err := e.Evaluate(context.Background(), PolicyInput{Alert: a}, PolicyRef{"sla_id", "business"}, "", a.StartsAt)
		if err != nil {
			t.Fatal(err)
		}
		status, reason := alertEligibility(a, d, instant(t, "2026-11-09T"+tc.cutoff+"+01:00"))
		if status != tc.status || reason != tc.reason {
			t.Fatalf("start=%s cutoff=%s: %s/%s", tc.start, tc.cutoff, status, reason)
		}
	}
}

func TestHorizonCoverageBoundsResolvedConclusions(t *testing.T) {
	p := businessPolicy()
	p.Schedule = map[string][]string{"monday": {}}
	e := engineFor(t, p, 1)
	a := alertAt(t, "resolved", "sla_id", "2026-11-09T07:30:00+01:00")
	d, err := e.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", a.StartsAt)
	if err != nil {
		t.Fatal(err)
	}
	want := instant(t, "2026-11-10T00:00:00+01:00")
	if d.Reason != ReasonHorizonExhausted || !d.NextEvaluationAt.IsZero() || !d.SearchUntil.Equal(want) {
		t.Fatal(d)
	}
	for _, tc := range []struct {
		end    time.Time
		status EligibilityStatus
	}{
		{want.Add(-time.Nanosecond), EligibilityIneligible},
		{want, EligibilityIneligible},
		{want.Add(time.Nanosecond), EligibilityUnknown},
	} {
		a.EndsAt = tc.end
		status, _ := alertEligibility(a, d, want.Add(24*time.Hour))
		if status != tc.status {
			t.Fatal(tc.end, status)
		}
	}
	a.Status = "firing"
	if status, _ := alertEligibility(a, d, want); status != EligibilityUnknown {
		t.Fatal(status)
	}
}

func TestIntervalEligibilityAcrossCalendarsAndDST(t *testing.T) {
	for _, tc := range []struct {
		name, zone, day, window, start, end string
		holidays                            *Holidays
		status                              EligibilityStatus
	}{
		{"overnight", "Europe/Stockholm", "monday", "22:00-06:00", "2026-11-09T21:30:00+01:00", "2026-11-10T01:00:00+01:00", nil, EligibilityEligible},
		{"overnight holiday", "Europe/Stockholm", "monday", "22:00-06:00", "2026-11-10T00:00:00+01:00", "2026-11-10T01:00:00+01:00", &Holidays{AdditionalDates: []string{"2026-11-10"}}, EligibilityIneligible},
		{"public holiday", "Europe/Stockholm", "friday", "08:00-17:00", "2027-01-01T07:30:00+01:00", "2027-01-01T10:00:00+01:00", &Holidays{Country: "SE"}, EligibilityIneligible},
		{"spring partial gap", "Europe/Stockholm", "sunday", "02:30-03:30", "2026-03-29T00:30:00Z", "2026-03-29T01:10:00Z", nil, EligibilityEligible},
		{"spring empty gap", "Europe/Stockholm", "sunday", "02:00-02:30", "2026-03-29T00:30:00Z", "2026-03-29T01:30:00Z", nil, EligibilityIneligible},
		{"autumn second fold", "Europe/Stockholm", "sunday", "02:15-02:45", "2026-10-25T00:50:00Z", "2026-10-25T01:20:00Z", nil, EligibilityEligible},
		{"autumn exclusive end", "Europe/Stockholm", "sunday", "02:15-02:45", "2026-10-25T00:50:00Z", "2026-10-25T01:15:00Z", nil, EligibilityIneligible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Policy{Timezone: tc.zone, Schedule: map[string][]string{tc.day: {tc.window}}, Holidays: tc.holidays}
			e := engineFor(t, p, 370)
			a := alertAt(t, "resolved", "sla_id", tc.start)
			a.EndsAt = instant(t, tc.end)
			d, err := e.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", a.StartsAt)
			if err != nil {
				t.Fatal(err)
			}
			if status, _ := alertEligibility(a, d, a.EndsAt.Add(24*time.Hour)); status != tc.status {
				t.Fatal(status, d)
			}
		})
	}
}

func TestWebhookSeparatesDifferentLifecycleEvidenceWithOneCalendarCall(t *testing.T) {
	s := newTestService(t)
	s.now = func() time.Time { return instant(t, "2026-11-09T09:05:00+01:00") }
	firing := alertAt(t, "firing", "sla_id", "2026-11-09T07:30:00+01:00")
	before := firing
	before.Status = "resolved"
	before.EndsAt = instant(t, "2026-11-09T07:45:00+01:00")
	after := before
	after.EndsAt = instant(t, "2026-11-09T08:30:00+01:00")
	w := post(s, testPayload(t, firing, before, after, before))
	var result Observation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Code, w.Body)
	}
	if result.SchemaVersion != 2 || result.Selected != 4 || result.Eligible != 1 || result.Ineligible != 2 || result.Unknown != 1 || result.Pending != 0 || len(result.Policies) != 3 {
		t.Fatal(result)
	}
	if result.Policies[0].EligibilityStatus != EligibilityUnknown || result.Policies[1].EligibilityStatus != EligibilityIneligible || result.Policies[2].EligibilityStatus != EligibilityEligible || result.Policies[1].AlertCount != 2 {
		t.Fatal(result.Policies)
	}
	if got := counter(t, s, "sla_proxy_holiday_evaluations_total", map[string]string{"country": "SE", "result": "not_holiday"}); got != 1 {
		t.Fatal("calendar cache not shared", got)
	}
	if got := counter(t, s, "sla_proxy_alert_eligibility_observations_total", map[string]string{"sla_label": "sla_id", "sla_value": "business", "alert_status": "resolved", "eligibility_status": "eligible", "reason": "active_interval_overlaps_sla"}); got != 1 {
		t.Fatal(got)
	}
	var raw struct {
		Policies []map[string]json.RawMessage `json:"policy_observations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, observation := range raw.Policies {
		if _, old := observation["eligible"]; old {
			t.Fatal("ambiguous boolean remains")
		}
		if _, ok := observation["eligible_at_start"]; !ok {
			t.Fatal("calendar classification missing")
		}
	}
	families, err := s.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "sla_proxy_policy_decisions_total" {
			t.Fatal("legacy metric retained with ambiguous semantics")
		}
	}
}

func TestHorizonUnknownIsSuccessfulObservationNotCalendarError(t *testing.T) {
	cfg := testConfig(t)
	cfg.EvaluationHorizonDays = 1
	cfg.SLADefinitions[0].SLAs["business"] = businessPolicy()
	s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return instant(t, "2026-11-15T09:00:00+01:00") }
	short := alertAt(t, "resolved", "sla_id", "2026-11-13T17:05:00+01:00")
	long := short
	long.EndsAt = instant(t, "2026-11-14T10:00:00+01:00")
	firing := short
	firing.Status = "firing"
	w := post(s, testPayload(t, short, long, firing))
	var result Observation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Code, w.Body)
	}
	if result.Ineligible != 1 || result.Unknown != 2 || result.EvaluationErrors != 0 || result.Evaluated != 3 {
		t.Fatal(result)
	}
	for _, observation := range result.Policies {
		if observation.NextEligibleAt != nil || observation.CalendarSearchUntil == nil || observation.CalendarReason != ReasonHorizonExhausted {
			t.Fatal(observation)
		}
	}
}

func TestPendingWebhookDoesNotClaimFutureEligibility(t *testing.T) {
	s := newTestService(t)
	s.now = func() time.Time { return instant(t, "2026-11-09T07:35:00+01:00") }
	w := post(s, testPayload(t, alertAt(t, "firing", "sla_id", "2026-11-09T07:30:00+01:00")))
	var result Observation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Pending != 1 || result.Eligible != 0 || result.Ineligible != 0 || result.Unknown != 0 {
		t.Fatal(w.Code, w.Body)
	}
	if bytes.Contains(w.Body.Bytes(), []byte(`"eligibility_status":"eligible"`)) {
		t.Fatal("future calendar time treated as observed overlap")
	}
}

func TestExhaustedCoverageRespectsDSTAndSkippedDates(t *testing.T) {
	for _, tc := range []struct{ zone, start, until string }{
		{"Europe/Stockholm", "2026-03-29T01:00:00+01:00", "2026-03-30T00:00:00+02:00"},
		{"Europe/Stockholm", "2026-10-25T02:00:00+02:00", "2026-10-26T00:00:00+01:00"},
		{"Pacific/Apia", "2011-12-29T12:00:00-10:00", "2011-12-31T00:00:00+14:00"},
	} {
		p := Policy{Timezone: tc.zone, Schedule: map[string][]string{"monday": {}}}
		e := engineFor(t, p, 1)
		d, err := e.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", instant(t, tc.start))
		if err != nil || !d.SearchUntil.Equal(instant(t, tc.until)) {
			t.Fatal(tc, d, err)
		}
	}
}
