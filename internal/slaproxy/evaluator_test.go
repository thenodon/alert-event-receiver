package slaproxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func instant(t testing.TB, text string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func engineFor(t *testing.T, policy Policy, horizon int) *CalendarEngine {
	t.Helper()
	cfg := testConfig(t)
	cfg.EvaluationHorizonDays = horizon
	cfg.SLADefinitions = []Definition{{SLALabel: "sla_id", SLAs: map[string]Policy{"business": policy}}}
	e, err := NewCalendarEngine(cfg, CountryHolidays{})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func businessPolicy() Policy {
	return Policy{Timezone: "Europe/Stockholm", Schedule: map[string][]string{
		"monday": {"08:00-12:00", "13:00-17:00"}, "tuesday": {"08:00-17:00"},
		"wednesday": {"08:00-17:00"}, "thursday": {"08:00-17:00"}, "friday": {"08:00-17:00"},
	}}
}

func fullDayPolicy() Policy {
	p := Policy{Timezone: "Europe/Stockholm", Schedule: map[string][]string{}}
	for _, day := range []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"} {
		p.Schedule[day] = []string{"00:00-24:00"}
	}
	return p
}

func assertEvaluation(t *testing.T, e *CalendarEngine, at, wantNext, reason string, eligible bool) Decision {
	t.Helper()
	input := PolicyInput{Alert: testAlert("firing", map[string]string{"sla_id": "business"})}
	d, err := e.Evaluate(context.Background(), input, PolicyRef{"sla_id", "business"}, "", instant(t, at))
	if err != nil {
		t.Fatal(err)
	}
	if d.Eligible != eligible || d.Reason != reason || len(d.PolicyVersion) != 64 {
		t.Fatalf("at %s: %+v", at, d)
	}
	if wantNext == "" {
		if !d.NextEvaluationAt.IsZero() {
			t.Fatalf("unexpected next instant: %v", d.NextEvaluationAt)
		}
	} else if want := instant(t, wantNext); !d.NextEvaluationAt.Equal(want) {
		t.Fatalf("at %s: next=%s, want %s", at, d.NextEvaluationAt.Format(time.RFC3339Nano), wantNext)
	}
	if !d.NextEvaluationAt.IsZero() && d.NextEvaluationAt.Location() != time.UTC {
		t.Fatal("next instant is not canonical UTC")
	}
	return d
}

func TestWeeklyEvaluationAndExactBoundaries(t *testing.T) {
	e := engineFor(t, businessPolicy(), 370)
	for _, tc := range []struct {
		at, next, reason string
		eligible         bool
	}{
		{"2026-11-09T07:59:59.999999999+01:00", "2026-11-09T07:00:00Z", ReasonOutsideWindow, false},
		{"2026-11-09T08:00:00+01:00", "2026-11-09T07:00:00Z", ReasonEligible, true},
		{"2026-11-09T11:59:59.123456789+01:00", "2026-11-09T10:59:59.123456789Z", ReasonEligible, true},
		{"2026-11-09T12:00:00+01:00", "2026-11-09T12:00:00Z", ReasonOutsideWindow, false},
		{"2026-11-09T13:00:00+01:00", "2026-11-09T12:00:00Z", ReasonEligible, true},
		{"2026-11-09T17:00:00+01:00", "2026-11-10T07:00:00Z", ReasonOutsideWindow, false},
		{"2026-11-13T17:00:00+01:00", "2026-11-16T07:00:00Z", ReasonOutsideWindow, false},
	} {
		t.Run(tc.at, func(t *testing.T) { assertEvaluation(t, e, tc.at, tc.next, tc.reason, tc.eligible) })
	}
}

func TestHolidayPrecedenceAndYearBoundary(t *testing.T) {
	p := fullDayPolicy()
	p.Holidays = &Holidays{Country: "SE", AdditionalDates: []string{"2026-12-31"}}
	e := engineFor(t, p, 370)
	assertEvaluation(t, e, "2026-12-31T12:00:00+01:00", "2027-01-01T23:00:00Z", ReasonAdditionalDate, false)
	assertEvaluation(t, e, "2027-01-01T12:00:00+01:00", "2027-01-01T23:00:00Z", ReasonHoliday, false)
	// Additional dates alone need no country and use the policy-local date,
	// even when the evaluation instant still belongs to the previous UTC day.
	p.Holidays = &Holidays{AdditionalDates: []string{"2027-01-02"}}
	e = engineFor(t, p, 370)
	assertEvaluation(t, e, "2027-01-01T23:30:00Z", "2027-01-02T23:00:00Z", ReasonAdditionalDate, false)
	p.Holidays = nil
	assertEvaluation(t, engineFor(t, p, 370), "2027-01-01T12:00:00+01:00", "2027-01-01T11:00:00Z", ReasonEligible, true)
	p.Holidays = &Holidays{AdditionalDates: []string{"2028-02-29"}}
	assertEvaluation(t, engineFor(t, p, 370), "2028-02-29T12:00:00+01:00", "2028-02-29T23:00:00Z", ReasonAdditionalDate, false)
	// Business-hour search skips Friday's holiday and the following weekend.
	p = businessPolicy()
	p.Holidays = &Holidays{Country: "SE"}
	assertEvaluation(t, engineFor(t, p, 370), "2026-12-31T17:00:00+01:00", "2027-01-04T07:00:00Z", ReasonOutsideWindow, false)
}

func TestOvernightWindowsUseCurrentLocalHolidayDate(t *testing.T) {
	p := Policy{Timezone: "Europe/Stockholm", Schedule: map[string][]string{"monday": {"22:00-06:00"}}}
	e := engineFor(t, p, 370)
	assertEvaluation(t, e, "2026-11-09T23:00:00+01:00", "2026-11-09T22:00:00Z", ReasonEligible, true)
	assertEvaluation(t, e, "2026-11-10T05:59:59.999999999+01:00", "2026-11-10T04:59:59.999999999Z", ReasonEligible, true)
	assertEvaluation(t, e, "2026-11-10T06:00:00+01:00", "2026-11-16T21:00:00Z", ReasonOutsideWindow, false)
	p.Holidays = &Holidays{AdditionalDates: []string{"2026-11-10"}}
	e = engineFor(t, p, 370)
	assertEvaluation(t, e, "2026-11-09T23:59:59+01:00", "2026-11-09T22:59:59Z", ReasonEligible, true)
	assertEvaluation(t, e, "2026-11-10T00:00:00+01:00", "2026-11-16T21:00:00Z", ReasonAdditionalDate, false)
	// A holiday on the start date does not suppress the next day's carry.
	p.Holidays.AdditionalDates = []string{"2026-11-09"}
	assertEvaluation(t, engineFor(t, p, 370), "2026-11-09T23:00:00+01:00", "2026-11-09T23:00:00Z", ReasonAdditionalDate, false)
	// Sunday/Monday week wrap.
	p.Schedule = map[string][]string{"sunday": {"22:00-02:00"}}
	p.Holidays = nil
	assertEvaluation(t, engineFor(t, p, 370), "2026-11-09T01:00:00+01:00", "2026-11-09T00:00:00Z", ReasonEligible, true)
}

func TestDSTGapsFoldsAndNextActualInstant(t *testing.T) {
	for _, tc := range []struct {
		name, zone, window, at, next, reason string
		eligible                             bool
	}{
		{"spring gap", "Europe/Stockholm", "02:00-02:30", "2026-03-29T00:59:00Z", "2026-04-05T00:00:00Z", ReasonOutsideWindow, false},
		{"spring partial gap", "Europe/Stockholm", "02:30-03:30", "2026-03-29T00:59:00Z", "2026-03-29T01:00:00Z", ReasonOutsideWindow, false},
		{"spring after jump", "Europe/Stockholm", "02:30-03:30", "2026-03-29T01:00:00Z", "2026-03-29T01:00:00Z", ReasonEligible, true},
		{"fold first", "Europe/Stockholm", "02:15-02:45", "2026-10-25T00:20:00Z", "2026-10-25T00:20:00Z", ReasonEligible, true},
		{"fold second", "Europe/Stockholm", "02:15-02:45", "2026-10-25T01:20:00Z", "2026-10-25T01:20:00Z", ReasonEligible, true},
		{"fold second start", "Europe/Stockholm", "02:15-02:45", "2026-10-25T00:50:00Z", "2026-10-25T01:15:00Z", ReasonOutsideWindow, false},
		{"fold first end", "Europe/Stockholm", "02:15-02:45", "2026-10-25T00:45:00Z", "2026-10-25T01:15:00Z", ReasonOutsideWindow, false},
		{"fold final end", "Europe/Stockholm", "02:15-02:45", "2026-10-25T01:45:00Z", "2026-11-01T01:15:00Z", ReasonOutsideWindow, false},
		{"half hour spring gap", "Australia/Lord_Howe", "02:00-02:45", "2026-10-03T15:29:00Z", "2026-10-03T15:30:00Z", ReasonOutsideWindow, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Policy{Timezone: tc.zone, Schedule: map[string][]string{"sunday": {tc.window}}}
			assertEvaluation(t, engineFor(t, p, 370), tc.at, tc.next, tc.reason, tc.eligible)
		})
	}
}

func TestSkippedLocalDateAndNonWholeHourZone(t *testing.T) {
	p := Policy{Timezone: "Pacific/Apia", Schedule: map[string][]string{"friday": {"09:00-10:00"}}}
	assertEvaluation(t, engineFor(t, p, 370), "2011-12-29T23:00:00-10:00", "2012-01-05T19:00:00Z", ReasonOutsideWindow, false)
	p = fullDayPolicy()
	p.Timezone = "Asia/Kathmandu"
	p.Holidays = &Holidays{AdditionalDates: []string{"2026-11-11"}}
	assertEvaluation(t, engineFor(t, p, 370), "2026-11-11T00:00:00+05:45", "2026-11-11T18:15:00Z", ReasonAdditionalDate, false)
}

func TestHorizonExhaustionAndEmptyWeek(t *testing.T) {
	p := businessPolicy()
	assertEvaluation(t, engineFor(t, p, 1), "2026-11-09T17:00:00+01:00", "", ReasonHorizonExhausted, false)
	assertEvaluation(t, engineFor(t, p, 2), "2026-11-09T17:00:00+01:00", "2026-11-10T07:00:00Z", ReasonOutsideWindow, false)
	p.Schedule = map[string][]string{"monday": {}}
	assertEvaluation(t, engineFor(t, p, 370), "2026-11-09T12:00:00+01:00", "", ReasonHorizonExhausted, false)
}

type brokenHolidays struct{ validationErr, evaluationErr error }

func (brokenHolidays) Version() string                 { return "test-provider-v1" }
func (p brokenHolidays) Validate(string, string) error { return p.validationErr }
func (p brokenHolidays) IsHoliday(context.Context, time.Time, string, string) (bool, error) {
	return false, p.evaluationErr
}

func TestEvaluatorErrorsAreNotAuthoritativeEligibility(t *testing.T) {
	cfg := testConfig(t)
	sentinel := errors.New("calendar unavailable")
	if _, err := NewCalendarEngine(cfg, brokenHolidays{validationErr: sentinel}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	engine, err := NewCalendarEngine(cfg, brokenHolidays{evaluationErr: sentinel})
	if err != nil {
		t.Fatal(err)
	}
	d, err := engine.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", instant(t, "2026-11-09T09:00:00+01:00"))
	if !errors.Is(err, sentinel) || d.Eligible || !d.NextEvaluationAt.IsZero() || d.PolicyVersion != "" {
		t.Fatal("authoritative decision on error", d, err)
	}
	_, err = engine.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"missing", "business"}, "", time.Now())
	var failure *EvaluationError
	if !errors.As(err, &failure) || failure.Reason != "unknown_policy" {
		t.Fatal(err)
	}
	_, err = engine.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", time.Time{})
	if !errors.As(err, &failure) || failure.Reason != "invalid_evaluation_time" {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = engine.Evaluate(ctx, PolicyInput{}, PolicyRef{"sla_id", "business"}, "", time.Now())
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCompiledPoliciesIndependentVersionedAndConcurrent(t *testing.T) {
	cfg := testConfig(t)
	e, err := NewCalendarEngine(cfg, CountryHolidays{})
	if err != nil {
		t.Fatal(err)
	}
	at := instant(t, "2026-11-09T12:30:00+01:00")
	first, err := e.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", at)
	if err != nil || first.Eligible {
		t.Fatal(first, err)
	}
	second, err := e.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_name", "business"}, "", at)
	if err != nil || !second.Eligible || first.PolicyVersion == second.PolicyVersion {
		t.Fatal("namespaces not independent", second, err)
	}
	// Mutating the source configuration cannot mutate compiled policy state.
	p := cfg.SLADefinitions[0].SLAs["business"]
	p.Schedule["monday"] = []string{"00:00-24:00"}
	changed, err := NewCalendarEngine(cfg, CountryHolidays{})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := changed.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", at)
	if err != nil || !updated.Eligible || updated.PolicyVersion == first.PolicyVersion {
		t.Fatal(updated, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			d, err := e.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", at)
			if err != nil || d.Eligible || d.PolicyVersion != first.PolicyVersion {
				t.Error(d, err)
			}
		})
	}
	wg.Wait()
	// Reloading the same YAML preserves versions despite map iteration order.
	fresh, err := NewCalendarEngine(testConfig(t), CountryHolidays{})
	if err != nil {
		t.Fatal(err)
	}
	d, err := fresh.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", at)
	if err != nil || d.PolicyVersion != first.PolicyVersion {
		t.Fatal(d, err)
	}
}
