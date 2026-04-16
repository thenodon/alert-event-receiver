package processor_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
	"github.com/andersh/alertmanager_event_receiver/internal/processor"
	"github.com/andersh/alertmanager_event_receiver/internal/state"
	"github.com/andersh/alertmanager_event_receiver/internal/telemetry"
)

// ---- test doubles ----

type mockStore struct {
	states    map[string]*state.AlertState
	idempKeys map[string]bool
}

func newMockStore() *mockStore {
	return &mockStore{
		states:    make(map[string]*state.AlertState),
		idempKeys: make(map[string]bool),
	}
}

func (m *mockStore) GetState(_ context.Context, fp string) (*state.AlertState, error) {
	return m.states[fp], nil
}
func (m *mockStore) SetFiring(_ context.Context, fp string, s state.AlertState) error {
	m.states[fp] = &s
	return nil
}
func (m *mockStore) UpdateLastSeen(_ context.Context, fp string, at time.Time) error {
	if s := m.states[fp]; s != nil {
		s.LastSeenAt = at
	}
	return nil
}
func (m *mockStore) SetClosed(_ context.Context, fp string, _ time.Duration) error {
	if s := m.states[fp]; s != nil {
		s.Status = "closed"
	} else {
		m.states[fp] = &state.AlertState{Status: "closed"}
	}
	return nil
}
func (m *mockStore) CheckAndSetIdempotency(_ context.Context, key string, _ time.Duration) (bool, error) {
	if m.idempKeys[key] {
		return false, nil
	}
	m.idempKeys[key] = true
	return true, nil
}

type mockEmitter struct {
	events []models.LifecycleEvent
}

func (m *mockEmitter) Emit(_ context.Context, e models.LifecycleEvent) error {
	m.events = append(m.events, e)
	return nil
}

// ---- helpers ----

func newProcessor(store state.Store, emitter *mockEmitter) *processor.Processor {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return processor.New(store, emitter, 24*time.Hour, 7*24*time.Hour, logger)
}

func firingAlert() models.Alert {
	return models.Alert{
		Status:      "firing",
		Fingerprint: "abc123",
		Labels:      map[string]string{"alertname": "TestAlert", "severity": "warning"},
		Annotations: map[string]string{"summary": "something bad"},
		StartsAt:    time.Now().Add(-5 * time.Minute),
	}
}

func resolvedAlert() models.Alert {
	a := firingAlert()
	a.Status = "resolved"
	a.EndsAt = time.Now()
	return a
}

// ---- tests ----

func TestOnFiring_NewAlert_EmitsOpenedEvent(t *testing.T) {
	store := newMockStore()
	emitter := &mockEmitter{}
	p := newProcessor(store, emitter)
	openedBefore := testutil.ToFloat64(telemetry.StateWritesTotal.WithLabelValues(models.StateOpened))
	idempBefore := testutil.ToFloat64(telemetry.IdempotencyKeysCreatedTotal.WithLabelValues(models.TransitionFiring))

	err := p.Process(context.Background(), firingAlert(), "gk", "recv", "http://am")
	if err != nil {
		t.Fatal(err)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(emitter.events))
	}
	ev := emitter.events[0]
	if ev.Transition != models.TransitionFiring {
		t.Errorf("expected transition=firing, got %s", ev.Transition)
	}
	if ev.StateWriteResult != models.StateOpened {
		t.Errorf("expected state_write_result=opened, got %s", ev.StateWriteResult)
	}
	if got := testutil.ToFloat64(telemetry.StateWritesTotal.WithLabelValues(models.StateOpened)) - openedBefore; got != 1 {
		t.Errorf("expected opened metric delta 1, got %v", got)
	}
	if got := testutil.ToFloat64(telemetry.IdempotencyKeysCreatedTotal.WithLabelValues(models.TransitionFiring)) - idempBefore; got != 1 {
		t.Errorf("expected firing idempotency-created delta 1, got %v", got)
	}
}

func TestOnFiring_AlreadyFiring_NoEmit(t *testing.T) {
	store := newMockStore()
	now := time.Now()
	store.states["abc123"] = &state.AlertState{
		Status: "firing", FirstFiringAt: now, LastSeenAt: now, StartsAt: now,
	}
	emitter := &mockEmitter{}
	p := newProcessor(store, emitter)
	alreadyBefore := testutil.ToFloat64(telemetry.StateWritesTotal.WithLabelValues(models.StateAlreadyFiring))

	_ = p.Process(context.Background(), firingAlert(), "gk", "recv", "http://am")
	if len(emitter.events) != 0 {
		t.Errorf("expected no event for already-firing alert, got %d", len(emitter.events))
	}
	if got := testutil.ToFloat64(telemetry.StateWritesTotal.WithLabelValues(models.StateAlreadyFiring)) - alreadyBefore; got != 1 {
		t.Errorf("expected already_firing metric delta 1, got %v", got)
	}
}

func TestOnFiring_Reopen_EmitsReopenedEvent(t *testing.T) {
	store := newMockStore()
	now := time.Now()
	store.states["abc123"] = &state.AlertState{
		Status: "closed", FirstFiringAt: now.Add(-time.Hour), LastSeenAt: now, StartsAt: now.Add(-time.Hour),
	}
	emitter := &mockEmitter{}
	p := newProcessor(store, emitter)
	reopenedBefore := testutil.ToFloat64(telemetry.StateWritesTotal.WithLabelValues(models.StateReopened))

	_ = p.Process(context.Background(), firingAlert(), "gk", "recv", "http://am")
	if len(emitter.events) != 1 {
		t.Fatalf("expected 1 event on reopen, got %d", len(emitter.events))
	}
	if emitter.events[0].StateWriteResult != models.StateReopened {
		t.Errorf("expected reopened, got %s", emitter.events[0].StateWriteResult)
	}
	if got := testutil.ToFloat64(telemetry.StateWritesTotal.WithLabelValues(models.StateReopened)) - reopenedBefore; got != 1 {
		t.Errorf("expected reopened metric delta 1, got %v", got)
	}
}

func TestOnResolved_WithOpenState_EmitsClosedWithDuration(t *testing.T) {
	store := newMockStore()
	now := time.Now()
	store.states["abc123"] = &state.AlertState{
		Status:        "firing",
		FirstFiringAt: now.Add(-10 * time.Minute),
		LastSeenAt:    now,
		StartsAt:      now.Add(-10 * time.Minute),
		Alertname:     "TestAlert",
		Labels:        map[string]string{"alertname": "TestAlert", "severity": "warning"},
	}
	emitter := &mockEmitter{}
	p := newProcessor(store, emitter)
	closedBefore := testutil.ToFloat64(telemetry.StateWritesTotal.WithLabelValues(models.StateClosed))
	idempBefore := testutil.ToFloat64(telemetry.IdempotencyKeysCreatedTotal.WithLabelValues(models.TransitionResolved))

	_ = p.Process(context.Background(), resolvedAlert(), "gk", "recv", "http://am")
	if len(emitter.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(emitter.events))
	}
	ev := emitter.events[0]
	if ev.StateWriteResult != models.StateClosed {
		t.Errorf("expected closed, got %s", ev.StateWriteResult)
	}
	if ev.DurationSeconds <= 0 {
		t.Errorf("expected positive duration, got %f", ev.DurationSeconds)
	}
	if ev.OrphanReason != "" {
		t.Errorf("expected no orphan reason, got %q", ev.OrphanReason)
	}
	if got := testutil.ToFloat64(telemetry.StateWritesTotal.WithLabelValues(models.StateClosed)) - closedBefore; got != 1 {
		t.Errorf("expected closed metric delta 1, got %v", got)
	}
	if got := testutil.ToFloat64(telemetry.IdempotencyKeysCreatedTotal.WithLabelValues(models.TransitionResolved)) - idempBefore; got != 1 {
		t.Errorf("expected resolved idempotency-created delta 1, got %v", got)
	}
}

func TestOnResolved_Orphan_EmitsWithOrphanMarker(t *testing.T) {
	store := newMockStore() // no state for this fingerprint
	emitter := &mockEmitter{}
	p := newProcessor(store, emitter)
	orphanBefore := testutil.ToFloat64(telemetry.ResolvedOrphansTotal)
	stateBefore := testutil.ToFloat64(telemetry.StateWritesTotal.WithLabelValues(models.StateResolvedWithoutOpen))

	_ = p.Process(context.Background(), resolvedAlert(), "gk", "recv", "http://am")
	if len(emitter.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(emitter.events))
	}
	ev := emitter.events[0]
	if ev.StateWriteResult != models.StateResolvedWithoutOpen {
		t.Errorf("expected resolved_without_open_state, got %s", ev.StateWriteResult)
	}
	if ev.OrphanReason != "open_state_missing" {
		t.Errorf("expected orphan_reason=open_state_missing, got %q", ev.OrphanReason)
	}
	if got := testutil.ToFloat64(telemetry.ResolvedOrphansTotal) - orphanBefore; got != 1 {
		t.Errorf("expected resolved orphan metric delta 1, got %v", got)
	}
	if got := testutil.ToFloat64(telemetry.StateWritesTotal.WithLabelValues(models.StateResolvedWithoutOpen)) - stateBefore; got != 1 {
		t.Errorf("expected resolved_without_open_state metric delta 1, got %v", got)
	}
}

func TestIdempotency_DuplicateDropped(t *testing.T) {
	store := newMockStore()
	emitter := &mockEmitter{}
	p := newProcessor(store, emitter)
	dupBefore := testutil.ToFloat64(telemetry.DuplicatesDropped)

	alert := firingAlert()
	_ = p.Process(context.Background(), alert, "gk", "recv", "http://am")
	_ = p.Process(context.Background(), alert, "gk", "recv", "http://am") // exact duplicate

	if len(emitter.events) != 1 {
		t.Errorf("expected 1 event (duplicate dropped), got %d", len(emitter.events))
	}
	if got := testutil.ToFloat64(telemetry.DuplicatesDropped) - dupBefore; got != 1 {
		t.Errorf("expected duplicates dropped delta 1, got %v", got)
	}
}

func TestStateTransition_FullCycle(t *testing.T) {
	store := newMockStore()
	emitter := &mockEmitter{}
	p := newProcessor(store, emitter)

	// 1. First firing
	a := firingAlert()
	_ = p.Process(context.Background(), a, "gk", "recv", "http://am")

	// 2. Repeated firing — no new event
	a2 := firingAlert()
	a2.StartsAt = a2.StartsAt.Add(time.Second) // different startsAt so idempotency key differs
	_ = p.Process(context.Background(), a2, "gk", "recv", "http://am")

	// 3. Resolved
	r := resolvedAlert()
	r.EndsAt = time.Now()
	_ = p.Process(context.Background(), r, "gk", "recv", "http://am")

	// 4. Second firing (new cycle)
	a3 := firingAlert()
	a3.StartsAt = time.Now()
	_ = p.Process(context.Background(), a3, "gk", "recv", "http://am")

	if len(emitter.events) != 3 {
		t.Errorf("expected 3 events (open, close, reopen), got %d", len(emitter.events))
	}
	if emitter.events[0].StateWriteResult != models.StateOpened {
		t.Errorf("[0] expected opened, got %s", emitter.events[0].StateWriteResult)
	}
	if emitter.events[1].StateWriteResult != models.StateClosed {
		t.Errorf("[1] expected closed, got %s", emitter.events[1].StateWriteResult)
	}
	if emitter.events[2].StateWriteResult != models.StateReopened {
		t.Errorf("[2] expected reopened, got %s", emitter.events[2].StateWriteResult)
	}
}

