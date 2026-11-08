package processor_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
	"github.com/andersh/alertmanager_event_receiver/internal/processor"
	"github.com/andersh/alertmanager_event_receiver/internal/state"
	"github.com/andersh/alertmanager_event_receiver/internal/telemetry"
	dto "github.com/prometheus/client_model/go"
)

func durationSnapshot(t *testing.T) *dto.Histogram {
	t.Helper()
	var metric dto.Metric
	if err := telemetry.AlertDurationSeconds.Write(&metric); err != nil {
		t.Fatal(err)
	}
	if len(metric.Label) != 0 {
		t.Fatalf("unexpected duration labels: %v", metric.Label)
	}
	return metric.GetHistogram()
}

func TestClosedDurationValuesAndBuckets(t *testing.T) {
	before := durationSnapshot(t)
	// Include zero, subsecond, exact bucket boundaries, multiple days and a value
	// beyond the largest finite bucket (retained by count / the implicit +Inf).
	durations := []time.Duration{0, 250 * time.Millisecond, time.Second, 5 * time.Second, time.Minute, 5 * time.Minute, 5*time.Minute + time.Nanosecond, time.Hour, 25 * time.Hour, 7 * 24 * time.Hour, 8 * 24 * time.Hour}
	base := time.Date(2026, 11, 8, 10, 0, 0, 123456789, time.UTC)
	for i, duration := range durations {
		store := newMockStore()
		emitter := &mockEmitter{}
		a := models.Alert{Fingerprint: fmt.Sprintf("duration-%d", i), Status: "resolved", StartsAt: base, EndsAt: base.Add(duration)}
		store.states[a.Fingerprint] = &state.AlertState{Status: "firing", StartsAt: base, FirstFiringAt: base}
		result, err := newProcessor(store, emitter).Process(context.Background(), a, "", "", "")
		if err != nil || result.Outcome != processor.Closed || result.Event == nil {
			t.Fatalf("closure: %+v, %v", result, err)
		}
		if result.Event.DurationSeconds != duration.Seconds() {
			t.Fatalf("duration = %v, want %v", result.Event.DurationSeconds, duration.Seconds())
		}
	}
	after := durationSnapshot(t)
	var sum float64
	for _, duration := range durations {
		sum += duration.Seconds()
	}
	if after.GetSampleCount()-before.GetSampleCount() != uint64(len(durations)) || after.GetSampleSum()-before.GetSampleSum() != sum {
		t.Fatalf("incorrect count/sum: before=%v after=%v", before, after)
	}
	bounds := []float64{300, 600, 900, 1800, 3600, 7200, 14400, 21600, 43200, 86400, 172800, 259200, 604800}
	if len(after.Bucket) != len(bounds) || len(before.Bucket) != len(bounds) {
		t.Fatal("unexpected duration bucket configuration")
	}
	for i, bound := range bounds {
		if after.Bucket[i].GetUpperBound() != bound {
			t.Fatalf("bucket %d = %v, want %v", i, after.Bucket[i].GetUpperBound(), bound)
		}
		var expected uint64
		for _, duration := range durations {
			if duration.Seconds() <= bound {
				expected++
			}
		}
		if got := after.Bucket[i].GetCumulativeCount() - before.Bucket[i].GetCumulativeCount(); got != expected {
			t.Errorf("bucket le=%v: count=%d, want %d", bound, got, expected)
		}
	}
}

func TestClosedDurationIndependentOfEmission(t *testing.T) {
	for _, mode := range []string{"accepted", "suppressed", "failed"} {
		t.Run(mode, func(t *testing.T) {
			store := newMockStore()
			emitter := &mockEmitter{}
			a := resolvedAlert()
			a.EndsAt = a.StartsAt.Add(90*time.Second + 500*time.Millisecond)
			store.states[a.Fingerprint] = &state.AlertState{Status: "firing", StartsAt: a.StartsAt, FirstFiringAt: a.StartsAt}
			if mode == "suppressed" {
				store.idempKeys[state.IdempotencyKey(a.Fingerprint, a.Status, a.StartsAt)] = true
			}
			if mode == "failed" {
				emitter.err = errors.New("OTLP emission failed")
			}
			p := newProcessor(store, emitter)
			before := durationSnapshot(t)
			result, err := p.Process(context.Background(), a, "", "", "")
			if result.Outcome != processor.Closed || string(result.EventEmissionStatus) != mode || (err != nil) != (mode == "failed") {
				t.Fatalf("got %+v, %v", result, err)
			}
			after := durationSnapshot(t)
			if after.GetSampleCount()-before.GetSampleCount() != 1 || after.GetSampleSum()-before.GetSampleSum() != 90.5 {
				t.Fatal("closure duration depended on emission result")
			}
			repeated, err := p.Process(context.Background(), a, "", "", "")
			if err != nil || repeated.Outcome != processor.RepeatedResolved {
				t.Fatalf("repeat: %+v, %v", repeated, err)
			}
			if durationSnapshot(t).GetSampleCount() != after.GetSampleCount() {
				t.Fatal("repeated resolution observed duration again")
			}
		})
	}
}

func TestExcludedDurationOutcomes(t *testing.T) {
	base := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, status, storedStatus, operation string
		offset                                time.Duration
		want                                  processor.Outcome
	}{
		{name: "new firing", status: "firing", want: processor.Opened},
		{name: "repeated firing", status: "firing", storedStatus: "firing", want: processor.RepeatedFiring},
		{name: "stale firing", status: "firing", storedStatus: "closed", want: processor.StaleFiring},
		{name: "superseded firing", status: "firing", storedStatus: "firing", offset: time.Second, want: processor.SupersededFiring},
		{name: "reopened", status: "firing", storedStatus: "closed", offset: time.Second, want: processor.Reopened},
		{name: "repeated resolution", status: "resolved", storedStatus: "closed", want: processor.RepeatedResolved},
		{name: "stale resolution with open state", status: "resolved", storedStatus: "firing", offset: -time.Second, want: processor.StaleResolved},
		{name: "stale resolution with closed state", status: "resolved", storedStatus: "closed", offset: -time.Second, want: processor.StaleResolved},
		{name: "orphan without state", status: "resolved", want: processor.ResolvedWithoutOpen},
		{name: "newer orphan over open state", status: "resolved", storedStatus: "firing", offset: time.Second, want: processor.ResolvedWithoutOpen},
		{name: "newer orphan over closed state", status: "resolved", storedStatus: "closed", offset: time.Second, want: processor.ResolvedWithoutOpen},
		{name: "failed state read", status: "resolved", storedStatus: "firing", operation: "get_state", want: processor.Indeterminate},
		{name: "failed repeated refresh", status: "firing", storedStatus: "firing", operation: "update_last_seen", want: processor.Indeterminate},
		{name: "failed firing write", status: "firing", operation: "set_firing", want: processor.Indeterminate},
		{name: "failed closure", status: "resolved", storedStatus: "firing", operation: "set_closed", want: processor.Indeterminate},
		{name: "failed orphan write", status: "resolved", operation: "set_closed", want: processor.Indeterminate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			a := models.Alert{Fingerprint: "duration-excluded", Status: tc.status, StartsAt: base.Add(tc.offset), EndsAt: base.Add(time.Hour)}
			if tc.storedStatus != "" {
				store.states[a.Fingerprint] = &state.AlertState{Status: tc.storedStatus, StartsAt: base, FirstFiringAt: base, EndsAt: base.Add(time.Minute)}
			}
			var dependency state.Store = store
			if tc.operation != "" {
				dependency = &failingStore{mockStore: store, operation: tc.operation, failure: errors.New("Valkey unavailable")}
			}
			before := durationSnapshot(t)
			result, err := newProcessor(dependency, &mockEmitter{}).Process(context.Background(), a, "", "", "")
			if result.Outcome != tc.want || (err != nil) != (tc.operation != "") {
				t.Fatalf("got %+v, %v", result, err)
			}
			after := durationSnapshot(t)
			if after.GetSampleCount() != before.GetSampleCount() || after.GetSampleSum() != before.GetSampleSum() {
				t.Fatalf("%s contributed an observation", tc.name)
			}
			for i, bucket := range after.Bucket {
				if bucket.GetCumulativeCount() != before.Bucket[i].GetCumulativeCount() {
					t.Fatalf("%s changed bucket accounting", tc.name)
				}
			}
		})
	}
}
